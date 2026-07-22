#!/usr/bin/env bash
# scripts/precommit.sh — unified pre-commit gate for mcp-servers.
#
# Single entry point that an agent or developer can invoke before
# committing to run the same checks the workspace expects. Mirrors the
# shape of seed-packet's scripts/precommit.sh so an agent moving
# between the two workspaces does not have to remember two
# invocation conventions.
#
# Stages run, in order, fail-fast on first non-zero:
#   1. gofmt + ASCII-quote normalization (staged blobs) — MUTATES + re-stages,
#      so it runs FIRST to hand corpos-gate a gofmt-clean tree.
#   2. corpos-gate run --tier=pre-push (chain 434 dogfood cutover) — the Go
#      CHECK stages (format / vet / lint / build / coverage-floor / vuln) plus
#      three read-only custom guards (quadlet-directives, cmd-gitignore-parity,
#      runtime-affecting-paths-parity) now run through the stack-agnostic gate
#      orchestrator driven by gate.yml, replacing the former inline
#      gofmt-drift / vet / golangci-lint / build-all / cover-floor / vuln
#      stages AND the three inline guard invocations. See go/cmd/corpos-gate +
#      go/internal/gate + gate.yml at the repo root. vuln is bypassable offline
#      via TOOLKIT_PRECOMMIT_SKIP_VULN=1 (threaded through as `--skip=vuln`).
#   3. make -C go replay-verify — the chain forge byte-identity replay (a
#      specialized stage that stays inline, not modeled by gate.yml).
#
# The MUTATING / restaging / specialized scaffolding (gofmt normalization,
# migration + event-schema sync, codemap regen, replay-verify) STAYS inline in
# this script; only the pure read-only Go CHECK stages moved to corpos-gate.
#
# Pre-T6 had Rust workspace stages here — retired at chain
# rust-retirement-and-db-hardening T7 (2026-05-22) along with the
# workspace itself. The repo is single-language Go from that
# commit forward.
#
# corpos-gate itself delegates to the same TAGS=sqlite_fts5 go toolchain
# invocations the go/Makefile targets use, so the canonical build-tag
# invariant is not duplicated here. The whole Go block is guarded by
# [ -d go ] so the script stays valid if a future workspace split removes
# the go/ subtree.
#
# Invoked from two places: the local .git/hooks/pre-commit symlink, and
# .gitea/workflows/ci.yaml, which runs this same script on a clean
# checkout so CI enforcement can never drift from the local gate. The
# staged-blob stages (stage 3 gofmt + ASCII-quote normalization) no-op in
# CI because a checkout has nothing staged; every other stage is
# whole-tree and runs in both contexts.

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

# Rust source-code guards (0, 0b, 0c) retired at chain
# rust-retirement-and-db-hardening T7 (2026-05-22). No Rust source
# remains in the repo outside archive/. The /home/, CARGO_MANIFEST_DIR,
# and std::env::set_var checks were Rust-specific portability gates.

# ── 0d. CONVENTIONS.md layout integrity ──────────────────────────────────────
# Top-level directories referenced at the root of fenced layout blocks in
# CONVENTIONS.md must exist on disk. Only non-indented entries are checked
# (indented lines describe subdirs of an already-checked top-level dir).
echo "[precommit.sh] checking CONVENTIONS.md top-level directory references"
_conv_fail=0
while IFS= read -r line; do
  # Only match lines with no leading whitespace that look like a directory name.
  dir=$(echo "$line" | grep -oE '^[a-zA-Z][a-zA-Z0-9_-]+/$' | tr -d '/' || true)
  [ -z "$dir" ] && continue
  if [ ! -d "$dir" ]; then
    echo "  CONVENTIONS.md references missing directory: $dir/"
    _conv_fail=1
  fi
done < <(sed -n '/^```$/,/^```$/p' CONVENTIONS.md 2>/dev/null)
if [ "$_conv_fail" -ne 0 ]; then
  echo "ERROR: CONVENTIONS.md File Layout references top-level directories that do not exist."
  exit 1
fi

# ── 0d2. agent-primary Go log discipline ─────────────────────────────────────
# Chain agent-first-substrate T5 (structured-observability) replaces flat
# log.Printf with the obs/ package's structured slog wrapper so every log
# entry carries the span_id the dispatcher mints per MCP request — the
# join key between log entries and the events ledger. The agent-primary
# packages (work, forge, knowledge, measure, admin) MUST emit through
# obs.Logger(ctx); a regression to log.Printf or fmt.Println breaks the
# fold-friendly log property the audit (AGENT_AUDIT_CONVENTIONS.md §11)
# requires. CLI scaffolding under cmd/ stays out of scope — those are
# user-facing stdout, not structured logs.
#
# Test files are excluded by name; production source under the listed
# packages is gated. Failure prints the offending file:line so the
# developer can migrate the call site to obs.
echo "[precommit.sh] checking for retired CRUD table refs after agent-substrate-crud-retirement T6"
# Migration 060 (T6, commit 725e854) dropped the eight artifact-lifecycle
# CRUD tables. Production code reads through projection tables and writes
# through the events log; any non-migration / non-comment SQL hit against
# the retired names is a regression. The check runs against go/internal/,
# go/cmd/, go/smoketest/ (excluding _test files for
# now — they're allowed to direct-INSERT into projections, but never into
# the dropped CRUD tables). The grep matches INSERT/SELECT/UPDATE/DELETE
# against the eight retired names. Exclusions: SQL migration files (which
# legitimately reference the names during the create→use→drop arc),
# vendored dirs (target/, node_modules/), and the smoketest package
# (gated by TOOLKIT_SERVER_BINARY so dead refs would compile but never
# run — they STILL count as drift, but smoketest hits do exit nonzero
# below).
_crud_refs=$(grep -rnE \
    '(INSERT INTO|FROM|UPDATE|DELETE FROM) (bugs|tasks|chains|task_blockers|task_dependencies|benchmark_results|roadmap_items|suggestions)\b' \
    go/internal/ go/cmd/ go/smoketest/ 2>/dev/null \
  | grep -vE '/(migrations/|target/|node_modules/|scenarios_e[14]/)' \
  | grep -vE ':[0-9]+:[[:space:]]*//' \
  || true)
if [ -n "$_crud_refs" ]; then
  echo "ERROR: retired CRUD table references found post-T6 (migration 060)."
  echo "Repoint to the projection tables (proj_current_bugs, proj_current_tasks,"
  echo "proj_chain_status, proj_task_blockers, proj_benchmark_results,"
  echo "proj_roadmap_view, proj_current_suggestions). See agent-substrate-crud-"
  echo "retirement T6 closure (725e854) + docs/SUBSTRATE_CRUD_RETIREMENT.md."
  echo "Offenders:"
  echo "$_crud_refs" | sed 's/^/  /'
  exit 1
fi

echo "[precommit.sh] checking for ORDER BY event_id in Go SQL strings (chronology must use ts)"
# Bug `audit-ledger-orders-by-event-id-buries-real-events-behind-synthetic-
# backfill` (2026-05-23): event_id is an identifier, NOT a chronological key.
# Synthetic-backfill IDs (`started-<uuid>` / `completed-<uuid>`) lex-sort
# AFTER ULIDs and so dominate the top of any `ORDER BY event_id DESC`
# listing. The invariant is pinned in go/internal/events/doc.go: chronology
# queries MUST order on `(ts, event_id)` with ts primary. This lint stage
# rejects every `ORDER BY event_id` (case-insensitive) in production AND
# test Go source. Point lookups (`WHERE event_id = ?`) are unaffected —
# the pattern only matches the ORDER BY shape.
#
# If you have a legitimate need to bypass (e.g. a one-off forensic query
# in a cmd/ tool), prefer fixing the call site to use ts ordering. If
# there's a genuine corner case, document it inline and add the path to
# the exclusion list below — never reach for an inline `// nolint`-style
# escape, the pattern is grep-based and doesn't recognize one.
_event_id_order_offenders=$(grep -rniE \
    'ORDER[[:space:]]+BY[[:space:]]+event_id\b' \
    go/internal/ go/cmd/ go/smoketest/ 2>/dev/null \
  | grep -vE '/(migrations/|target/|node_modules/)' \
  | grep -vE ':[0-9]+:[[:space:]]*//' \
  || true)
if [ -n "$_event_id_order_offenders" ]; then
  echo "ERROR: 'ORDER BY event_id' found in Go SQL strings."
  echo "event_id is an identifier, not a chronological key — synthetic-backfill"
  echo "rows (started-/completed- prefixed) lex-sort AFTER ULIDs and break the"
  echo "ordering invariant. Use ts as the primary sort with event_id as the"
  echo "tiebreaker:"
  echo ""
  echo "    ORDER BY ts DESC, event_id DESC    (newest-first)"
  echo "    ORDER BY ts ASC, event_id ASC      (oldest-first, e.g. fold replay)"
  echo ""
  echo "See go/internal/events/doc.go §'Invariant: ts is the chronological"
  echo "authority' for the full rationale + the canonical bug fix that"
  echo "introduced this discipline (commit 02462ecd)."
  echo "Offenders:"
  echo "$_event_id_order_offenders" | sed 's/^/  /'
  exit 1
fi

echo "[precommit.sh] checking for the archived toolkit/internal/forge top-level import"
# Chain 311 T7 Stage 6 P2-C.2 archived forge's top-level package to
# archive/forge/ — the agent write surface is now construct + the record-sugar
# action. The forge/registry + forge/fieldvalue subpackages STAY in the build
# (they're leaves construct depends on), so this guard bans ONLY the bare
# `"toolkit/internal/forge"` import (the archived dispatch/strategy/handler
# package), not the still-live subpackage imports. A new bare-forge import is a
# regression: that package is no longer compiled (it lives outside the go/
# module under archive/). Repoint to construct (Prepare*/Handle*/Create/Update/
# Delete) or to forge/registry / forge/fieldvalue as appropriate.
_forge_import_offenders=$(grep -rnE \
    '"toolkit/internal/forge"' \
    go/internal/ go/cmd/ go/smoketest/ 2>/dev/null \
  | grep -vE '/(migrations/|target/|node_modules/)' \
  | grep -vE ':[0-9]+:[[:space:]]*//' \
  || true)
if [ -n "$_forge_import_offenders" ]; then
  echo "ERROR: bare 'toolkit/internal/forge' import found — that package archived"
  echo "to archive/forge/ in chain 311 T7 Stage 6 P2-C.2 and is no longer compiled."
  echo "Use construct (HandleForgeCreate/Edit/Delete, PrepareForge*, Create/Update/"
  echo "Delete, the index/markdown seams) or the forge/registry + forge/fieldvalue"
  echo "subpackages. Offenders:"
  echo "$_forge_import_offenders" | sed 's/^/  /'
  exit 1
fi

echo "[precommit.sh] checking for log.Printf / fmt.Println in agent-primary Go paths"
_log_offenders=$(find \
    go/internal/work \
    go/internal/forge \
    go/internal/knowledge \
    go/internal/measure \
    go/internal/admin \
    -type f -name '*.go' ! -name '*_test.go' 2>/dev/null \
  | xargs grep -nE '\blog\.(Printf|Println|Fatalf|Fatal)\b|\bfmt\.Print(f|ln)\b' 2>/dev/null || true)
if [ -n "$_log_offenders" ]; then
  echo "ERROR: log.Printf / log.Fatalf / fmt.Println found in agent-primary Go paths."
  echo "Replace with obs.Logger(ctx) (or obs.L() at init time) — see go/internal/obs/."
  echo "Offenders:"
  echo "$_log_offenders" | sed 's/^/  /'
  exit 1
fi

# ── 0d3. quadlet deployment-artifact integrity ──────────────────────────────
# MOVED to corpos-gate: the quadlet-directives custom guard
# (scripts/validate-quadlet-units.sh) now runs via `corpos-gate run` (gate.yml
# custom[]), invoked from the Go block below. See chain 434 dogfood cutover.

# ── 0e. migrations single-source sync ────────────────────────────────────────
# Post-T6: crates/shared-db is gone, so go/internal/db/migrations/ is
# now canonical. The testutil mirror at go/internal/testutil/migrations/
# stays a real-copy convention because Go embed rejects symlinks; the
# precommit gate copies the canonical → mirror direction inline rather
# than via a separate script.
echo "[precommit.sh] sync migrations from go/internal/db/migrations/ → testutil mirror"
for src in go/internal/db/migrations/*.sql; do
    dst="go/internal/testutil/migrations/$(basename "$src")"
    if [ ! -f "$dst" ] || ! cmp -s "$src" "$dst"; then
        cp "$src" "$dst"
    fi
done
# Delete any .sql in the testutil mirror that no longer exists in canonical.
for dst in go/internal/testutil/migrations/*.sql; do
    src="go/internal/db/migrations/$(basename "$dst")"
    if [ ! -f "$src" ]; then
        rm "$dst"
    fi
done
# Re-stage any mirror-dir .sql files the sync touched (parallel to the
# go-fmt re-stage below).
git diff --name-only -- \
    'go/internal/testutil/migrations/*.sql' \
    | xargs -r git add

# The dashboard TS-types codegen freshness stage (former 0e2) was dropped
# when toolkit split out of the monorepo (chain auto-startup-dev-services
# T2): it hashed apps/dashboard/src/api/types.gen.ts, which lives in the
# frontend repo now. The Go→TS codegen (tygo) moves with the dashboard.

# ── 0f. event-schemas single-source sync ────────────────────────────────────
# blueprints/events/ is the canonical source-of-truth for event payload
# schemas; go/internal/events/schemas/ is the Go events-package embed
# mirror. Same discipline as migrations: Go embed rejects symlinks, so
# the mirror is a real copy kept in sync by scripts/sync-event-schemas.sh.
echo "[precommit.sh] sync event schemas from canonical to Go embed mirror"
bash scripts/sync-event-schemas.sh
git diff --name-only -- 'go/internal/events/schemas/*.json' | xargs -r git add

# ── 0g / 0g'. runtime-affecting-paths + cmd-gitignore parity ─────────────────
# MOVED to corpos-gate: the runtime-affecting-paths-parity
# (scripts/test-runtime-affecting-paths-parity.sh) and cmd-gitignore-parity
# (scripts/test-cmd-gitignore-parity.sh) custom guards now run via
# `corpos-gate run` (gate.yml custom[]), invoked from the Go block below. See
# chain 434 dogfood cutover.

# ── 0g''. cutover global/non-fleet MCP-config coverage (bug 986) ────────────
# scripts/cutover-canonical-db.sh must swap the GLOBAL ~/.claude.json mcpServers
# block + ~/dev/.mcp.json to the proxy, not only the ~/dev/*/.mcp.json fleet glob
# — else a session rooted outside a fleet dir keeps opening the canonical DB
# natively post-flip (a second writer, breaking the T7 single-writer invariant).
# This self-test drives check/migrate-configs/restore-configs against a sandbox
# HOME and fails if the global/dev-level configs aren't reported + swapped.
echo "[precommit.sh] cutover global MCP-config coverage check"
bash scripts/test-cutover-global-mcp-coverage.sh

# ── 0g'''. gradient-question-guard regression ───────────────────────────────
# hooks/gradient-question-guard.sh is the PreToolUse wall that blocks
# completeness-gradient AskUserQuestion menus ("do it fully / partially / not at
# all") — the anti-pattern that three feedback memories failed to stop. Its
# value is entirely in firing correctly: a false-negative re-opens the
# productivity sink, a false-positive blocks legitimate forks and gets the guard
# disabled. Gate the detector so neither half can silently rot.
echo "[precommit.sh] gradient-question-guard regression check"
bash hooks/test-gradient-question-guard.sh

# ── 0g''''. install-into-claude regression ──────────────────────────────────
# scripts/install-into-claude.sh is the disaster-recovery path for ~/.claude's
# skills + hooks. It sat DEAD for weeks — it read a _manifest.toml this repo
# never had, so it exited 2 on line one — and nothing noticed, because nothing
# ran it (chain 444 T6). The harness is hermetic: temp targets only, never
# ~/.claude, and its corpos-dependent assertions degrade instead of failing when
# no sibling corpos checkout exists (CI / container).
echo "[precommit.sh] install-into-claude regression check"
bash scripts/test-install-into-claude.sh

# ── 0h. action-docs corpus no-diff gate (single-source-action-describe T7;
# extended to knowledge by migrate-knowledge-action-docs-to-derive-contract, then
# to measure, admin, and ml by their migrate-<surface>-action-docs-to-derive-
# contract chains) ──
# go/internal/actiondocs/corpus/<surface>/*.toml is GENERATED from each migrated
# surface's co-located descriptor registry (work.actionRegistry,
# knowledge.knowledgeActionRegistry, measure.measureActionRegistry,
# admin.adminActionRegistry, ml.mlActionRegistry) and go:embed'd into the binary. The
# generator's --check mode regenerates every generated surface's corpus in memory
# and exits non-zero if any on-disk chunk drifts — i.e. a hand-edit of a generated
# chunk, or a registry change shipped without rerunning the generator. Runs the
# generator exactly as devs do (same wrapper). Fix on failure:
# scripts/action-docs-corpus-gen && git add the regenerated chunks. The parity
# gates (param_type_parity / param_tag_gate) validate the still-hand-authored
# surfaces' docs↔handler structs; this gate validates corpus↔registry.
echo "[precommit.sh] action-docs corpus no-diff check (work + knowledge + measure + admin + ml corpus ↔ registries)"
bash scripts/action-docs-corpus-gen --check

# ── 0i. orphan precommit-fmt stash detection (bug 1425) ────────────────────
# A prior precommit invocation that was killed between `git stash push` and
# `git stash pop` (e.g. parent Claude Code crashed, SIGKILL, OOM, power
# loss) leaves a `precommit-fmt-<pid>-<label>` entry in `git stash list`.
# The EXIT trap installed by restage_after_fmt closes most kill paths
# (SIGTERM / INT / HUP / normal error exit), but SIGKILL and hard host
# failure bypass trap handlers. This check surfaces such orphans on the
# next precommit run so they don't silently accumulate.
#
# WARN-only — doesn't block the commit. A user with unrelated stashes
# they're deliberately holding shouldn't have their commit gated on
# unrelated cleanup. The user recovers via `git checkout stash@{N} -- ...`
# (extract work) + `git stash drop stash@{N}` (clean up).
echo "[precommit.sh] checking for orphan precommit-fmt stashes"
_orphan_count=0
_orphan_lines=""
_orphan_empty_indices=()
_orphan_nonempty_count=0
while IFS= read -r line; do
  [ -z "$line" ] && continue
  pid=$(echo "$line" | grep -oE 'precommit-fmt-[0-9]+' | grep -oE '[0-9]+' || true)
  [ -z "$pid" ] && continue
  if kill -0 "$pid" 2>/dev/null; then continue; fi
  # Orphan detected.
  _orphan_count=$((_orphan_count + 1))
  _orphan_lines+="  $line"$'\n'
  # Classify empty vs non-empty via `git stash show <ref>`. Empty
  # diff = the format pass was a no-op; safe to autoreap. Non-empty
  # may hold recoverable work per bug 1425's recovery flow.
  idx=$(echo "$line" | grep -oE '^stash@\{[0-9]+\}' | grep -oE '[0-9]+' || true)
  if [ -n "$idx" ]; then
    if [ -z "$(git stash show "stash@{$idx}" 2>/dev/null)" ]; then
      _orphan_empty_indices+=("$idx")
    else
      _orphan_nonempty_count=$((_orphan_nonempty_count + 1))
    fi
  fi
done < <(git stash list | grep -F 'precommit-fmt-' || true)
if [ "$_orphan_count" -gt 0 ]; then
  echo "[precommit.sh] WARN: $_orphan_count orphan precommit-fmt stash(es) detected (parent PID is dead):"
  printf '%s' "$_orphan_lines"
  echo "[precommit.sh]   Recover any work with:  git checkout stash@{N} -- <files>"
  echo "[precommit.sh]   Drop after recovery:    git stash drop stash@{N}"
  echo "[precommit.sh]   See bug 1425 for the crash-window analysis."
  # Opt-in auto-reap of empty (no-op format-only) orphans. Default
  # OFF so users who haven't read about this feature aren't surprised
  # by silent stash deletions. Non-empty orphans are NEVER
  # auto-reaped — they may hold recoverable work per bug 1425.
  _autoreap="${TOOLKIT_PRECOMMIT_AUTOREAP_EMPTY_ORPHANS:-0}"
  case "$_autoreap" in
    1|true|yes|on)
      if [ "${#_orphan_empty_indices[@]}" -gt 0 ]; then
        # Sort descending so drops don't shift remaining targets'
        # indices. (Dropping stash@{3} leaves stash@{1} unchanged;
        # dropping stash@{1} would shift stash@{3} → stash@{2}.)
        _sorted_indices=$(printf '%s\n' "${_orphan_empty_indices[@]}" | sort -rn)
        for _idx in $_sorted_indices; do
          if git stash drop "stash@{$_idx}" > /dev/null 2>&1; then
            echo "[precommit.sh]   auto-reaped empty orphan stash@{$_idx}"
          else
            echo "[precommit.sh]   WARN: failed to drop empty orphan stash@{$_idx} — manual cleanup needed"
          fi
        done
      fi
      if [ "$_orphan_nonempty_count" -gt 0 ]; then
        echo "[precommit.sh]   $_orphan_nonempty_count non-empty orphan(s) left intact — manual review required"
      fi
      ;;
  esac
fi

# ── 0h. CODEMAP regen + Go package doc.go lint ──────────────────────────────
# scripts/codemap-gen reads the authoritative discovery sources (action
# manifests, forge schemas, go/internal/*/doc.go, crates/*/src/lib.rs,
# skills, scripts) and emits CODEMAP.md at the repo root. The --lint flag
# fails when any go/internal/* package is missing doc.go or its
# four-field intended-use block. See chain agent-first-substrate T7.
if [ -d go ] && command -v go > /dev/null 2>&1; then
  echo "[precommit.sh] codemap-gen --lint (Go doc.go four-field block)"
  bash scripts/codemap-gen --lint
  echo "[precommit.sh] codemap-gen (regenerate CODEMAP.md)"
  bash scripts/codemap-gen
  git diff --name-only -- CODEMAP.md | xargs -r git add
fi

# Helper: run a formatter that touches the working tree (gofmt),
# then re-stage ONLY the files originally staged — without
# sweeping unstaged hunks in those files into the commit. Bug 1380
# was: prior `git add <file>` re-stage destroyed `git add -p` partial
# staging by re-adding the whole file post-fmt. The fix: stash the
# unstaged tracked changes (with --keep-index so the staged content
# stays in the worktree where fmt operates), run the formatter,
# re-stage, then pop the stash to restore the unstaged overlay.
#
# Bug 1425: the gap between `git stash push` and `git stash pop` is
# non-atomic across a process boundary. If the parent (Claude Code,
# the user's shell) is killed mid-gap, the stash entry orphans and the
# user's worktree appears stripped of their unstaged work. The trap
# below pops the stash on any normal exit path (EXIT) plus the catchable
# signals (INT/TERM/HUP) so the script's own death doesn't leak an
# orphan. SIGKILL and hard host failure still bypass trap handlers —
# stage 0i's orphan check catches what slipped through.
#
# Trap discipline: the message is the unique identifier (stash@{N}
# positions shift on push/pop, so position-based recovery is racy).
# _emergency_stash_pop looks up the stash by message, pops it, and
# logs a one-line notice. The trap clears on the function's happy
# path before the normal pop runs so the trap doesn't fire twice.
#
# Args: $1 = display label, $2 = formatter command, $3 = glob for re-stage
restage_after_fmt() {
  local _label="$1" _cmd="$2" _glob="$3"
  # Bug 1468: when nothing is staged, the stash-pop dance has no
  # partial-staging to protect — there's nothing to re-stage. Skipping
  # avoids the failure mode where the formatter touches an unstaged
  # file, pop refuses on 3-way merge conflict, and the user's unstaged
  # edits get stranded in an orphan stash. Run the formatter directly;
  # its edits coalesce with the user's unstaged work — which is what
  # they'd see anyway in a no-commit invocation.
  if git diff --cached --quiet; then
    eval "$_cmd" || return 1
    return 0
  fi
  local _stash_message="precommit-fmt-$$-$_label"
  local _stashed=0
  if ! git diff --quiet; then
    git stash push --keep-index --message "$_stash_message" --quiet
    _stashed=1
    # Arm the emergency pop. Trap fires on:
    #   EXIT — set -e errors anywhere downstream, normal script end
    #   INT  — ctrl-C
    #   TERM — parent kill, Claude Code shutdown cascade
    #   HUP  — terminal hangup
    # The trap is cleared on the happy path below before the normal
    # pop runs, so a successful run pops exactly once.
    trap "_emergency_stash_pop '$_stash_message'" EXIT INT TERM HUP
  fi
  # Run the formatter; on failure, the trap above handles the stash
  # pop on the script's exit (set -e cascades out of this function).
  if ! eval "$_cmd"; then
    return 1
  fi
  # --diff-filter=ACM skips deletions (D) so that committing a removed
  # file doesn't cause `git add` to fail on missing paths.
  # shellcheck disable=SC2086
  git diff --cached --name-only --diff-filter=ACM -- $_glob | xargs -r git add
  if [ "$_stashed" = "1" ]; then
    # Happy path: disarm the trap so the normal pop doesn't double-pop,
    # then pop. If the pop fails (rare — formatter touched lines the
    # user also has unstaged), the trap is already cleared so the user
    # gets the explicit WARN below rather than a silent re-attempt.
    trap - EXIT INT TERM HUP
    local _stash_ref
    _stash_ref=$(git stash list 2>/dev/null | grep -F "$_stash_message" | head -1 | sed -E 's/:.*//')
    # Suggestion 14: redundant-stash early-drop. If every file in the
    # stash now byte-matches the worktree (gofmt happened to apply
    # exactly the same fix the user had unstaged — comma-spacing,
    # const-block alignment, blank-line cleanup), the stash is a no-op
    # and the pop would either succeed silently or — quirk of `git
    # stash pop --keep-index` — conflict on identical-content files.
    # Detect-and-drop avoids both the spurious conflict and the orphan
    # accumulation pattern observed in chains running 3+ commits where
    # the user kept a gofmt-collapsible unstaged change in the worktree.
    # Falls through to the normal pop only when the stash contains
    # something the worktree DOESN'T already have — preserving recovery
    # paths for genuine unstaged work that gofmt didn't reach.
    if _stash_is_redundant_against_worktree "$_stash_ref"; then
      git stash drop --quiet "$_stash_ref" 2>/dev/null
      echo "[precommit.sh] $_label collapsed your unstaged edits into the staged tree; auto-dropped redundant $_stash_ref"
      return 0
    fi
    if ! git stash pop --quiet 2>/dev/null; then
      # Bug 1440: happy-path pop conflict. Identify the specific stash
      # entry (positions shift across pops; the message we set on push
      # is the stable key) and the files in conflict, then print the
      # exact two-step recovery so the user doesn't have to know the
      # ritual: drop the formatter's version (it'll re-apply on the
      # next pre-commit run anyway), then re-pop the stash to restore
      # the unstaged WIP.
      local _conflicts
      _conflicts=$(git diff --name-only --diff-filter=U 2>/dev/null | tr '\n' ' ')
      # Bug 1468: a "WARN + keep going" return here lets the script
      # exit 0 with the user's unstaged work stranded in an orphan
      # stash and the working tree at HEAD (looks like a silent
      # revert). Hard-fail so the loss is impossible to miss.
      echo "[precommit.sh] ERROR: stash pop after $_label failed — your unstaged work is stranded in $_stash_ref." >&2
      if [ -n "$_conflicts" ]; then
        echo "[precommit.sh]   conflicting paths: $_conflicts" >&2
        echo "[precommit.sh]   recover with:  git checkout HEAD -- $_conflicts && git stash pop ${_stash_ref:-}" >&2
      else
        echo "[precommit.sh]   recover unstaged with:  git checkout $_stash_ref -- <files>; git stash drop $_stash_ref" >&2
        echo "[precommit.sh]   (inspect the stash first:  git stash show -p $_stash_ref)" >&2
      fi
      echo "[precommit.sh]   ('git checkout HEAD --' drops the formatter's edits; they re-apply on the next pre-commit run.)" >&2
      return 1
    fi
  fi
}

# Suggestion 14: returns 0 when every file in the stash has the same
# content in the current worktree (the stash is now a no-op against
# disk because the formatter just applied the same edits the user had
# unstaged), 1 otherwise. Used by restage_after_fmt() to drop redundant
# stashes before attempting the pop — avoids both the spurious conflict
# and the orphan-stash accumulation pattern.
#
# Empty stash refs return 1 (we can't prove redundancy of nothing).
# Missing files in either side count as "different" — conservative;
# the stash gets popped via the normal path and the user sees any
# real recovery state.
_stash_is_redundant_against_worktree() {
  local _ref="$1"
  if [ -z "$_ref" ]; then
    return 1
  fi
  local _files
  _files=$(git stash show --name-only "$_ref" 2>/dev/null)
  if [ -z "$_files" ]; then
    return 1
  fi
  while IFS= read -r _file; do
    [ -z "$_file" ] && continue
    if [ ! -f "$_file" ]; then
      return 1
    fi
    if ! cmp -s <(git show "$_ref:$_file" 2>/dev/null) "$_file" 2>/dev/null; then
      return 1
    fi
  done <<< "$_files"
  return 0
}

# Emergency pop invoked from the trap when restage_after_fmt exits
# unexpectedly (signal, error cascade, parent death). Finds the stash
# entry by its unique message — positions shift on pop, but messages
# are unique per call — and pops it. Idempotent: a no-op when the
# stash has already been popped on the happy path (trap fires but
# finds no matching entry). Writes its notice to stderr so it isn't
# eaten by stdout pipes (the failure case is exactly when stdout may
# already be lost).
_emergency_stash_pop() {
  local _message="$1"
  local _ref
  _ref=$(git stash list 2>/dev/null | grep -F "$_message" | head -1 | sed -E 's/:.*//')
  if [ -n "$_ref" ]; then
    if git stash pop --quiet "$_ref" 2>/dev/null; then
      echo "[precommit.sh] emergency stash pop of '$_message' on exit (your unstaged work is back in the worktree)" >&2
    else
      echo "[precommit.sh] WARN: emergency pop of '$_message' failed; recover with: git checkout $_ref -- <files>; git stash drop $_ref" >&2
    fi
  fi
}

# Rust workspace stages retired at chain rust-retirement-and-db-hardening
# T7 (2026-05-22). The workspace is single-language Go from this commit
# forward.

if [ -d go ]; then
  # Go stages delegate to go/Makefile targets so the hook and `make test`
  # stay in lockstep — the canonical TAGS=sqlite_fts5 lives in the Makefile,
  # not duplicated here. If TAGS or build flags change, the hook follows
  # automatically.

  # ── 3. gofmt + ASCII-quote normalization (blob-only) ─────────────────────
  # Bug 863: the prior stash-and-pop approach (restage_after_fmt below)
  # could fail when gofmt's edits conflicted with the user's unstaged
  # changes in the same file, stranding the unstaged work in stash@{0}.
  # The cleaner fix is to never touch the worktree at all: format each
  # staged blob in-place via git plumbing. Bug 1380's partial-staging
  # concern is naturally satisfied — we format the staged blobs, which
  # IS the partial-staging content. The worktree's unstaged hunks stay
  # untouched; the next edit/save/precommit cycle picks them up
  # normally.
  #
  # ASCII-quote normalization closes bug
  # `typographic-quotes-keep-getting-converted-in-go-source-files-by-
  # unidentified-hook`: agent text generation alternates between Unicode
  # smart quotes (U+2018/U+2019/U+201C/U+201D) and ASCII (' ")
  # depending on whether the context is narrative-prose docstring or
  # code transcription. Two Write calls on the same comment can land
  # different bytes. Normalizing every `//` comment line to ASCII
  # quotes at gate time makes the bytes deterministic regardless of
  # which way the author wrote them. Scope-limited to lines whose
  # whitespace-trimmed prefix is `//` to avoid clobbering legitimate
  # Unicode in string literals (rare in Go, but possible).
  #
  # Also writes the normalized blob to the worktree IFF the worktree
  # file matches the original-staged content — i.e. there are no
  # unstaged hunks to clobber. This keeps working-tree vs HEAD at
  # zero post-commit. Bug 863's "don't mutate worktree" invariant
  # stays satisfied for the partial-staging case.
  echo "[precommit.sh] gofmt + ASCII-quote normalization (staged blobs only)"
  _staged_go=$(git diff --cached --name-only --diff-filter=ACM -- '*.go' || true)
  if [ -n "$_staged_go" ]; then
    while IFS= read -r _f; do
      [ -z "$_f" ] && continue
      _tmp=$(mktemp)
      _orig=$(mktemp)
      # shellcheck disable=SC2064
      trap "rm -f $_tmp $_orig" RETURN
      git show ":$_f" > "$_orig"
      # Stage 1: gofmt the staged blob.
      if ! gofmt < "$_orig" > "$_tmp"; then
        echo "[precommit.sh] gofmt error on $_f" >&2
        rm -f "$_tmp" "$_orig"
        exit 1
      fi
      # Stage 2: normalize Unicode smart quotes inside `//` comment
      # lines to ASCII. Python is used over sed/awk because it handles
      # multi-byte UTF-8 reliably independent of LC_ALL.
      python3 -c "
import sys
src = sys.stdin.read()
out = []
for line in src.splitlines(keepends=True):
    if line.lstrip().startswith('//'):
        line = line.translate(str.maketrans({
            '‘': \"'\",
            '’': \"'\",
            '“': '\"',
            '”': '\"',
        }))
    out.append(line)
sys.stdout.write(''.join(out))
" < "$_tmp" > "$_tmp.norm"
      mv "$_tmp.norm" "$_tmp"
      # Only re-stage if anything changed. Saves churn on
      # already-formatted files.
      if ! cmp -s "$_orig" "$_tmp"; then
        _mode=$(git ls-files -s -- "$_f" | awk '{print $1}')
        _blob=$(git hash-object -w "$_tmp")
        git update-index --cacheinfo "$_mode,$_blob,$_f"
        # Mirror the rewrite to the worktree IFF the worktree file
        # matches the original staged blob (no unstaged hunks).
        # Bug 863's invariant: never clobber unstaged work.
        if cmp -s "$_orig" "$_f"; then
          cp "$_tmp" "$_f"
        fi
      fi
      rm -f "$_tmp" "$_orig"
    done <<< "$_staged_go"
  fi

  # ── Go CHECK stages via corpos-gate (chain 434 dogfood cutover) ──────────
  # The pure read-only Go CHECK stages that USED to live inline here —
  # whole-tree gofmt-drift, `go vet`, golangci-lint (forbidigo any-rule),
  # build-all, and the cover-floor test+coverage gate — plus the three
  # read-only custom guards (quadlet-directives, cmd-gitignore-parity,
  # runtime-affecting-paths-parity, formerly invoked at the top of this
  # script) now run through the stack-agnostic gate orchestrator driven by
  # gate.yml. `run --tier=pre-push` is the SUPERSET tier: format / vet / lint
  # / build (pre-commit) PLUS coverage-floor (66%) / vuln (pre-push), plus
  # every custom guard. The canonical TAGS=sqlite_fts5 + coverage-floor
  # invariants live in gate.yml, mirroring the retired Makefile targets.
  #
  # The gate binary is built fresh each run (Go's build cache makes the
  # no-change case cheap) so a source change to cmd/corpos-gate or
  # internal/gate can never run against a stale binary.
  echo "[precommit.sh] make -C go corpos-gate (build the gate binary)"
  make -C go corpos-gate

  # vuln bypass: preserve the exact TOOLKIT_PRECOMMIT_SKIP_VULN truthiness the
  # retired inline `make -C go vuln` stage honored (1 / true / yes). gate.yml
  # has vuln ENABLED, so corpos-gate would run it; when the toggle is set we
  # thread `--skip=vuln` through to drop just that one check. govulncheck hits
  # the network (vuln.go.dev), so this is the documented offline escape — an
  # EXPLICIT bypass, not a silent tool-missing skip. osv-scanner is
  # deliberately NOT in the gate (it false-cleans Go advisories, bug
  # osv-scanner-v2-silently-false-cleans-go-advisories); govulncheck is the
  # authoritative Go scanner.
  _gate_run_args=(run --tier=pre-push)
  _skip_vuln="${TOOLKIT_PRECOMMIT_SKIP_VULN:-0}"
  if [ "$_skip_vuln" = "1" ] || [ "$_skip_vuln" = "true" ] || [ "$_skip_vuln" = "yes" ]; then
    echo "[precommit.sh] ⚠  vuln gate SKIPPED (TOOLKIT_PRECOMMIT_SKIP_VULN=$_skip_vuln)"
    _gate_run_args+=(--skip=vuln)
  fi

  # Strip the inherited git env for the gate subprocess, so the coverage
  # stage's hermetic git-spawning tests (e.g. internal/work stamp_validate,
  # which `git commit`s in throwaway /tmp repos) start from a clean git
  # environment regardless of how THIS commit was invoked. Two sibling
  # channels are scrubbed:
  #
  #   • GIT_DIR / GIT_WORK_TREE / GIT_INDEX_FILE (bug 921): inside the pre-commit
  #     hook git exports an absolute GIT_DIR (always absolute for a *worktree*
  #     checkout). A hermetic test inheriting it would pick up this repo's
  #     absolute core.hooksPath, fire the real pre-commit hook against its /tmp
  #     repo, and fail resolving scripts/precommit.sh there. In a main checkout
  #     GIT_DIR is the relative ".git" so it harmlessly resolves to the temp
  #     repo — which is why this only bit worktree commits.
  #   • GIT_CONFIG_PARAMETERS / GIT_CONFIG_COUNT (bug 937): the gate-only
  #     worktree commit path runs `git -c core.hooksPath=… commit` (skip the
  #     post-commit advisor — see docs/MULTI_AGENT_WORKTREE_WORKFLOW.md). `git -c`
  #     exports GIT_CONFIG_PARAMETERS, which every descendant git inherits — so a
  #     hermetic test's `git commit` would pick up the overridden core.hooksPath
  #     and exec a missing hook → non-zero → test fails. GIT_CONFIG_COUNT gates
  #     the GIT_CONFIG_KEY_<n>/VALUE_<n> array form; unsetting the count makes git
  #     ignore those entirely, so the indexed vars need no per-element scrub.
  #
  # This mirrors the env scrub that wrapped the retired `make -C go cover-floor`
  # stage — corpos-gate runs the SAME coverage suite, so it needs the SAME
  # scrub. The gate's own stash/add/restage git calls above run as direct git
  # invocations in this script (not children of the gate) and keep their env.
  # The binary is at go/bin/corpos-gate (the Makefile's -C go bin/ target); it
  # finds gate.yml by walking up from the repo root.
  echo "[precommit.sh] go/bin/corpos-gate ${_gate_run_args[*]} (format/vet/lint/build/coverage/vuln + custom guards)"
  env -u GIT_DIR -u GIT_WORK_TREE -u GIT_INDEX_FILE \
      -u GIT_CONFIG_PARAMETERS -u GIT_CONFIG_COUNT \
      go/bin/corpos-gate "${_gate_run_args[@]}"

  # ── 7b. chain forge byte-identity replay ───────────────────────────────────
  # T3 of work-batching-and-forge-templates-followons: re-forge the
  # work-batching-and-forge-templates chain via forge(chain, full-objects)
  # into a temp DB and assert the chain row + 9 task rows are byte-identical
  # to the live production rows (modulo ids/timestamps/lifecycle). A
  # regression in parseFullObjectEntry or the projection fold trips this.
  # SKIPs (exit 0) when the live DB / chain is absent, so a fresh checkout
  # without a production DB still commits.
  echo "[precommit.sh] chain forge byte-identity replay (chain-replay-verify)"
  make -C go replay-verify

  # NOTE: the former inline 7c dependency-vulnerability stage (`make -C go
  # vuln` = go mod verify + govulncheck, bypassable via
  # TOOLKIT_PRECOMMIT_SKIP_VULN=1) is now OWNED by corpos-gate above: gate.yml
  # enables the vuln check at pre-push tier, and the skip toggle is threaded
  # through as `--skip=vuln` (see the _gate_run_args logic in the corpos-gate
  # stage). Do NOT re-add an inline vuln stage here.
fi

# The dashboard CSS-token-drift and tsc --noEmit stages (former stages 8
# and 9) were dropped when toolkit split out of the monorepo (chain
# auto-startup-dev-services T2). They gate apps/dashboard, which now lives
# in the frontend repo with its own scoped gate (CSS-token + tsc + lint).
# This gate is Go-only.

echo "[precommit.sh] all stages passed."
