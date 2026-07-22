#!/usr/bin/env bash
# scripts/post-commit-restart-advisor.sh — classify the most recent
# commit's blast radius, rebuild the binary if needed, and restart
# any running HTTP daemon automatically.
#
# Path-based heuristic; no LLM. Runs in milliseconds. The decision
# table mirrors the actual runtime topology of the Go toolkit-server
# (this repo is Go-only post-rust-retirement T6; the dashboard split to
# sophdn/corpos-toolkit-dashboard, chain auto-startup-dev-services T3):
#
#   stdio MCP      — toolkit-server spawned by every Claude Code IDE
#                    that mounts this backend via .mcp.json. Restart =
#                    restart the IDE (or kill the stdio process, the IDE
#                    respawns it). Picks up: go/cmd + go/internal (the
#                    Go binary) and its dependencies.
#   HTTP daemon    — Go toolkit-server --http-port 3000 --http-only
#                    (canonical), long-running. Serves the dashboard's
#                    /benchmarks, /chains, /tasks, etc. over HTTP (the
#                    dashboard is a separate repo now). Restart =
#                    go/launch.sh. Picks up: go/internal/observehttp
#                    handlers and every internal package they depend on.
#
# Usage:
#   ./scripts/post-commit-restart-advisor.sh                 # run on HEAD
#   ./scripts/post-commit-restart-advisor.sh HEAD~3..HEAD   # range form
#
# Environment:
#   TOOLKIT_PRECOMMIT_QUIET=1   collapse happy-path output to a single
#                                summary line ('advisor: rebuilt rust+go,
#                                preserved 2 stdio sessions, restarted
#                                HTTP daemon (log: /tmp/toolkit-http.log)').
#                                Destructive-action warnings (foreground
#                                CLI kill, orphan stdio kill) ALWAYS
#                                surface even in quiet mode. Failed
#                                rebuilds dump their captured output.
#   TOOLKIT_PRECOMMIT_FORCE_STDIO_KILL=1
#                                kill stdio MCP processes attached to
#                                live sessions instead of preserving
#                                them (see bug 1322 for the discipline).
#   TOOLKIT_PRECOMMIT_SKIP_SMOKE=1
#                                skip the post-rebuild smoke check
#                                (bug 1331). The smoke step boots the
#                                fresh binary against a tempfile DB to
#                                catch migration/startup failures before
#                                they take down the live MCP. Useful in
#                                test harnesses that pre-stage a binary
#                                whose runtime path is out of scope.
#
# Rebuild dispatch:
#   - Go diff   → `make -C go build` (Makefile pins -tags sqlite_fts5).
# HTTP daemon restart: kills the old process (fresh pgrep, not the pid
#   from classify time) and relaunches via go/launch.sh.
# Stdio restart: prints a one-line IDE restart prompt (the only action
#   the script genuinely cannot take itself).
#
# Exit code: always 0 (advisory, not gating). Wired as a git post-commit hook:
#   ln -s ../../scripts/post-commit-restart-advisor.sh .git/hooks/post-commit
# .git/hooks/ is not version-controlled, so the symlink is per-clone.

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

# ── Linked-worktree detection + daemon-restart opt-out (bug 936) ───────
# The :3000 HTTP daemon (and the stdio MCP) load the MAIN checkout's
# go/bin/toolkit-server. When this commit lands in a LINKED worktree,
# restarting :3000 here would relaunch it from the worktree — hijacking
# the canonical daemon from the user and any other agent, and under
# concurrent multi-agent work flip-flopping :3000 in a last-writer-wins
# binary race (bug 936; bug 924 named the topology). A worktree's Go
# change isn't even deployable this way — it must reach main first (see
# the "built in a LINKED WORKTREE" warning below). So the daemon restart
# is suppressed by default for linked-worktree commits. Detect by
# comparing the absolute common git dir's parent against REPO_ROOT: in the
# main checkout they match (not linked); in a linked worktree they differ.
MAIN_ROOT=""
_git_common="$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null || true)"
case "$_git_common" in
    */.git) MAIN_ROOT="$(dirname "$_git_common")" ;;
esac
if [[ -n "$MAIN_ROOT" && "$MAIN_ROOT" != "$REPO_ROOT" ]]; then
    IS_LINKED_WORKTREE=true
else
    IS_LINKED_WORKTREE=false
fi
# Operator override: skip the :3000 restart for ANY checkout. Set by the
# gate-only worktree commit path (docs/MULTI_AGENT_WORKTREE_WORKFLOW.md),
# and usable from the main checkout when a commit shouldn't recycle the
# shared daemon.
NO_DAEMON_RESTART="${TOOLKIT_PRECOMMIT_NO_DAEMON_RESTART:-0}"

# Quiet-mode helpers. TOOLKIT_PRECOMMIT_QUIET=1 suppresses informational
# lines but never destructive-action warnings; _summary_parts accumulates
# fragments for the consolidated end-of-run line.
_quiet="${TOOLKIT_PRECOMMIT_QUIET:-0}"
_summary_parts=()
_say()        { [[ "$_quiet" == "1" ]] || echo "$@"; }
_say_always() { echo "$@"; }
_summary_add(){ _summary_parts+=("$@"); }
# Run a build/restart subcommand; in quiet mode, buffer its output and
# only surface it if it failed. In verbose mode, pass through unchanged.
_run_buffered() {
    if [[ "$_quiet" == "1" ]]; then
        local logfile rc
        logfile=$(mktemp)
        if "$@" >"$logfile" 2>&1; then
            rm -f "$logfile"
            return 0
        else
            rc=$?
            cat "$logfile" >&2
            rm -f "$logfile"
            return $rc
        fi
    else
        "$@"
    fi
}

# ── Sequencing-operation guard (bug 920) ──────────────────────────────
# During git rebase / cherry-pick / revert / am, the post-commit hook
# fires once per replayed commit. Rebuilding the binary + restarting the
# daemon on every pick is slow (~30-60s each), blows the command timeout
# mid-operation (leaving the rebase half-applied), and churns the shared
# HTTP daemon other agents may be using. None of the replayed commits is
# a fresh branch-tip landing — each was already advised when first
# authored — so the correct move is to no-op now and let the operator
# rebuild ONCE when the whole operation finishes.
#
# Detection is worktree-aware: the rebase/sequencer state lives in the
# per-worktree git dir, which `git rev-parse --git-path` resolves
# correctly (it is NOT always $REPO_ROOT/.git under linked worktrees).
_sequencing_op=""
for _st in rebase-merge rebase-apply CHERRY_PICK_HEAD REVERT_HEAD sequencer; do
    _p="$(git rev-parse --git-path "$_st" 2>/dev/null || true)"
    [[ -n "$_p" && -e "$_p" ]] || continue
    case "$_st" in
        rebase-merge|rebase-apply) _sequencing_op="rebase/am" ;;
        CHERRY_PICK_HEAD)          _sequencing_op="cherry-pick" ;;
        REVERT_HEAD)               _sequencing_op="revert" ;;
        sequencer)                 _sequencing_op="cherry-pick/revert sequence" ;;
    esac
    break
done
if [[ -n "$_sequencing_op" ]]; then
    _say_always "advisor: mid-$_sequencing_op — skipping per-commit rebuild/restart (would fire once per replayed commit). Rebuild once when the operation completes: make -C go build  (or re-run this advisor on HEAD)."
    exit 0
fi

RANGE="${1:-HEAD~1..HEAD}"
CHANGED=$(git diff --name-only "$RANGE" 2>/dev/null || true)

if [[ -z "$CHANGED" ]]; then
    _say "advisor: no files changed in $RANGE — nothing to rebuild."
    exit 0
fi

# Classification flags. Each path category contributes to one or both
# restart needs. Foundational internal packages are dependencies of
# BOTH the stdio MCP and the HTTP daemon, so they trigger both restarts.
NEED_GO_REBUILD=false    # Go rebuild — canonical
NEED_STDIO_RESTART=false
NEED_HTTP_RESTART=false
TEST_ONLY_PATHS=()
DOC_ONLY=true

while IFS= read -r path; do
    [[ -z "$path" ]] && continue
    case "$path" in
        # Go orchestration layer (canonical HTTP daemon post-T68).
        # cmd/* + internal/* produce the running binary; module config
        # affects deps and build flags. Either path requires a fresh
        # `go build` and BOTH a stdio and HTTP daemon relaunch — the
        # same binary serves both surfaces, so any rebuild leaves both
        # process instances stale.
        go/cmd/*|go/internal/*|go/go.mod|go/go.sum|go/Makefile)
            NEED_GO_REBUILD=true
            NEED_STDIO_RESTART=true
            NEED_HTTP_RESTART=true
            DOC_ONLY=false
            ;;
        # Launcher script — relaunch but no rebuild needed.
        go/launch.sh)
            NEED_HTTP_RESTART=true
            DOC_ONLY=false
            ;;
        # Smoke tests — exercises the built binary but doesn't ship in
        # it. Surface so the operator knows to `go test ./smoketest/...`
        # but skip the rebuild + restart.
        go/smoketest/*)
            TEST_ONLY_PATHS+=("$path")
            DOC_ONLY=false
            ;;
        # NB: the per-action documentation corpus no longer has a special
        # arm. It moved under the Go module (go/internal/actiondocs/corpus)
        # and is go:embed'd into the binary (chain single-source-action-
        # describe T6), so a chunk add/edit now requires a rebuild and is
        # correctly caught by the go/internal/* arm above (NEED_GO_REBUILD +
        # both restarts). The old disk-load model — "no rebuild, next
        # startup re-Loads" — no longer holds; that's why the dedicated arm
        # (and the bug it addressed, advisor-misclassifies-action-docs-
        # chunks-as-docs-only) is gone.
        # Forge schemas: loaded from disk at toolkit-server startup via
        # registry.Load(blueprintsDir) at go/cmd/toolkit-server/main.go
        # (~line 378). A schema add / edit (new field, new schema,
        # changed validation) doesn't take effect until restart, OR
        # until the caller runs admin.schema_reload — but the advisor
        # can't assume the operator knows about schema_reload. Bug 857.
        blueprints/forge-schemas/*)
            NEED_STDIO_RESTART=true
            NEED_HTTP_RESTART=true
            DOC_ONLY=false
            ;;
        # Rubrics: loaded from disk at toolkit-server startup via the
        # --rubrics-dir flag. Same load-at-startup pattern as forge
        # schemas; same restart-needed disposition. Bug 857.
        blueprints/rubrics/*)
            NEED_STDIO_RESTART=true
            NEED_HTTP_RESTART=true
            DOC_ONLY=false
            ;;
        # Claude Code hook scripts (Stop / SessionStart / SubmitPrompt /
        # PreToolUse / etc.). Read by the Claude Code harness at hook
        # firing time, NOT by the toolkit-server daemon — no rebuild /
        # restart needed. Set DOC_ONLY=false (it is not a docs-only diff)
        # so the run reaches the final "no daemon restart needed (no-
        # runtime-impact edits)" message instead of the docs-only exit;
        # a hook edit takes effect on the next session start, not via a
        # daemon restart. Bug 857.
        hooks/*)
            DOC_ONLY=false
            ;;
        # Scripts, docs, blueprints/events (spec-only — runtime uses
        # the embedded go/internal/events/schemas/), action-manifests,
        # .git-hooks (pre-commit etc.), top-level .toml / .gitignore /
        # README / CONVENTIONS — no daemon impact.
        scripts/*|*.md|blueprints/events/*|action-manifests/*|.git-hooks/*|*.toml|.gitignore|README*|CONVENTIONS*)
            ;;
        # Anything else: be conservative — flag for inspection but
        # don't auto-recommend a restart. The operator decides.
        *)
            DOC_ONLY=false
            ;;
    esac
done <<< "$CHANGED"

# toolkit-proxy staleness reminder (chain finish-sophdn-repo-split T5): the stdio
# proxy (cmd/toolkit-proxy) is a SEPARATE binary from toolkit-server — hand-installed
# at ~/.local/bin/toolkit-proxy (scripts/install-proxy.sh) and invoked by every
# .mcp.json. The go/cmd + go/internal arms above rebuild the SERVER, not the proxy,
# so a change to the proxy or a package it embeds (dispatch meta-schema, actiondocs
# descriptions) leaves the installed proxy stale and undetected. Remind rather than
# auto-build: the proxy is a per-session SPOF the operator refreshes deliberately
# (and a build from the post-commit hook would surprise). The staleness check
# (scripts/check-proxy-staleness.sh) confirms drift on demand.
if echo "$CHANGED" | grep -qE '^go/cmd/toolkit-proxy/|^go/internal/dispatch/|^go/internal/actiondocs/'; then
    DOC_ONLY=false
    _say "advisor: ⚠ toolkit-proxy source changed — the installed ~/.local/bin/toolkit-proxy is now STALE (every .mcp.json uses it). Refresh: scripts/install-proxy.sh ; verify: scripts/check-proxy-staleness.sh"
fi

# Bug `advisor-restart-without-rebuild-leaves-stale-compile-time-sha`:
# any daemon restart needs a fresh ldflag-stamped main.gitSHA so the
# /version endpoint reflects HEAD. Without this, a restart-only commit
# (forge-schemas / rubrics edits, or go/launch.sh — disk-loaded config
# that sets the restart flags but not NEED_GO_REBUILD) relaunches the
# daemon against a binary whose compile-time gitSHA still points at the
# last .go-touching commit, so /version under-reports HEAD even though
# the config content IS fresh (re-read from disk on startup). (The
# monorepo-era dashboard drift banner that consumed this SHA was removed
# with the frontend split — bug 983 — but an accurate /version is the
# image-lags-HEAD contract's source of truth in its own right.)
# Incremental `go build` is near-free when no .go files changed — Go's
# build cache skips the compilation, the link step refreshes the ldflag
# substitution.
if $NEED_STDIO_RESTART || $NEED_HTTP_RESTART; then
    NEED_GO_REBUILD=true
fi

_file_count=$(echo "$CHANGED" | wc -l | tr -d ' ')
_say "advisor: classified $RANGE ($_file_count file(s) changed)"
_say ""

if $DOC_ONLY; then
    _say "  no runtime-affecting changes — docs / scripts / configs only."
    [[ "$_quiet" == "1" ]] && echo "advisor: docs-only diff ($_file_count file(s)) — no rebuild."
    exit 0
fi

if [[ ${#TEST_ONLY_PATHS[@]} -gt 0 ]] && ! $NEED_GO_REBUILD; then
    _say "  tests-only diff:"
    for p in "${TEST_ONLY_PATHS[@]}"; do _say "    - $p"; done
    _say ""
    # TEST_ONLY_PATHS only ever collects go/smoketest/* now (the Rust
    # test/bench arm retired with the crates split, bug 985).
    _say "  recommended:  go -C go test -tags sqlite_fts5 ./smoketest/..."
    _say "  no daemon restart needed."
    if [[ "$_quiet" == "1" ]]; then
        echo "advisor: tests-only diff (${#TEST_ONLY_PATHS[@]} path(s)) — run: go test; no daemon restart."
    fi
    exit 0
fi

if $NEED_GO_REBUILD; then
    _say "  running go build:"
    _run_buffered make -C go build
    _say "  go build complete."
    _summary_add "rebuilt go"
    _say ""

    # Worktree-deploy warning (bug worktree-commits-dont-deploy-to-stdio-
    # binary-path-no-staleness-signal). The build above wrote into THIS
    # checkout's go/bin. The deployed binary that .mcp.json's stdio command +
    # go/launch.sh load is the MAIN checkout's go/bin/toolkit-server. In a
    # LINKED worktree those are different paths, so this commit's Go change does
    # NOT reach the live MCP/HTTP services via the normal commit → /mcp
    # reconnect flow — it must be merged/cherry-picked to main and the MAIN
    # checkout rebuilt. Reuses the linked-worktree detection computed at the
    # top (IS_LINKED_WORKTREE / MAIN_ROOT); in the main checkout this stays
    # silent. Always-on (a deploy footgun, not chatter).
    if $IS_LINKED_WORKTREE; then
        _say_always "  warning: built in a LINKED WORKTREE ($REPO_ROOT)."
        _say_always "           the stdio MCP + HTTP daemon load the MAIN checkout's binary"
        _say_always "           ($MAIN_ROOT/go/bin/toolkit-server) — this commit's Go change will NOT"
        _say_always "           go live via commit + /mcp reconnect. To deploy: merge/cherry-pick to"
        _say_always "           main, then 'make -C go build' in $MAIN_ROOT, then /mcp reconnect."
        _say_always "           (See docs/MULTI_AGENT_WORKTREE_WORKFLOW.md.)"
        _summary_add "worktree-build-not-deployed"
    fi
fi

# Smoke gate (bug 1331). Boot the freshly built binary against a
# throwaway DB before signalling the daemons. If the binary panics on
# startup (broken migration, missing flag, embed mismatch) we abort here
# — better than taking down the live MCP service. The smoke is bounded
# to 4 seconds: `starting db=` is the first log line `main` emits, so a
# healthy binary reaches it in tens of milliseconds; if we don't see it
# within the bound the rebuild is presumed broken.
#
# The smoke runs with --http-port 0 --http-only so neither transport
# binds; --rubrics-dir "" / --blueprints-dir "" skip the optional
# registry loads so the gate stays sub-5s under hot-cache and tolerable
# under cold. The binary parks on `select {}` after init; the timeout
# tears it down.
if $NEED_GO_REBUILD && [[ "${TOOLKIT_PRECOMMIT_SKIP_SMOKE:-0}" != "1" ]]; then
    SMOKE_BINARY="${TOOLKIT_SMOKE_BINARY:-$REPO_ROOT/go/bin/toolkit-server}"
    if [[ ! -x "$SMOKE_BINARY" ]]; then
        _say_always "  smoke: binary not found at $SMOKE_BINARY — aborting restart."
        _summary_add "smoke aborted (binary missing)"
        exit 0
    fi
    SMOKE_DB="$(mktemp -t toolkit-smoke-XXXXXX.db)"
    SMOKE_LOG="$(mktemp -t toolkit-smoke-XXXXXX.log)"
    # shellcheck disable=SC2064
    trap "rm -f '$SMOKE_DB' '$SMOKE_LOG' '$SMOKE_DB-wal' '$SMOKE_DB-shm'" EXIT

    _say "  running smoke (boot binary against fresh DB)..."
    set +e
    timeout --preserve-status --signal=TERM 4 \
        "$SMOKE_BINARY" \
        --db "$SMOKE_DB" \
        --default-project test \
        --rubrics-dir "" \
        --blueprints-dir "" \
        --http-port 0 \
        --http-only \
        > "$SMOKE_LOG" 2>&1
    smoke_rc=$?
    set -e

    # Acceptable rcs:
    #  0   binary exited cleanly before timeout (atypical with --http-only;
    #      tolerated for forward compat).
    #  124 timeout sent SIGTERM and the binary did not respond — fine, we
    #      only care that 'starting db=' landed; the timeout itself isn't
    #      a failure signal.
    #  143 binary exited from the SIGTERM cleanly (128 + 15).
    # Anything else is a real failure.
    smoke_ok=true
    case "$smoke_rc" in
        0|124|143) ;;
        *)
            smoke_ok=false
            ;;
    esac
    # Match either the pre-T5 plaintext shape (`starting db=...`) or the
    # post-T5 structured-slog JSON shape (`"msg":"toolkit-server starting"`).
    # The signal is "main reached the post-init log line"; the wire shape
    # is a separate concern. Chain agent-first-substrate T5 swapped the
    # boot log to JSON.
    if ! grep -qE 'starting db=|"msg":"toolkit-server starting"' "$SMOKE_LOG"; then
        smoke_ok=false
    fi

    if ! $smoke_ok; then
        _say_always "  smoke FAILED (rc=$smoke_rc) — aborting daemon restart."
        _say_always "  binary output:"
        # Indent the captured output so it visually nests under the warning.
        sed 's/^/    /' "$SMOKE_LOG" >&2 || true
        _summary_add "smoke failed (rc=$smoke_rc) — daemons NOT restarted"
        if [[ "$_quiet" == "1" ]]; then
            IFS=', '
            echo "advisor: ${_summary_parts[*]}"
            unset IFS
        fi
        exit 0
    fi
    _say "  smoke ok."
    _say ""
fi

if $NEED_STDIO_RESTART; then
    # Restart-policy decision tree for stdio MCP processes (bug 1322):
    #
    #   parent alive AND NOT init/systemd (PPID > 1, /proc/PPID exists)?
    #     → "attached": a live Claude Code session is using this stdio
    #       server. Preserve it. Killing here would disrupt the agent
    #       mid-task; the user would have to /mcp reconnect manually
    #       and the failed tool calls would surface in the conversation
    #       before the disconnect notice lands. The active session
    #       continues on the old binary until the user voluntarily
    #       restarts; new sessions naturally pick up the new binary.
    #     → User can force-kill by setting TOOLKIT_PRECOMMIT_FORCE_STDIO_KILL=1.
    #
    #   parent dead OR is init (orphaned)?
    #     → kill: nobody is using this stdio process, it's a leak.
    #
    # The HTTP daemon restart (next block) is unchanged — its FD lifetime
    # is independent of any agent session.
    #
    # Bug 1314 history: explored SIGHUP-based self-re-exec to avoid the
    # disconnect entirely. OS-level mechanic works (process image swaps,
    # PID preserved) but MCP's `initialize` handshake state cannot be
    # preserved across exec; the client is mid-session, the new binary
    # expects a fresh handshake, and every tool call fails with
    # "invalid during session initialization" until /mcp reconnects.
    # Worse than a clean disconnect. SIGHUP handler stays installed as
    # optionality for future protocol-aware re-exec work.
    #
    # The Go server has no --stdio-only flag (canonical post-T68); a stdio
    # process is identified as any toolkit-server cmdline that does NOT
    # carry --http-only or --http-port. The legacy --stdio-only is matched
    # too for transitional builds.
    # `|| true`: pipefail + an empty pgrep result (no toolkit-server on the
    # host, or a narrowed test shim — see bug 1332) propagates grep's exit-1
    # to the pipeline and aborts under `set -e`. Empty STDIO_PIDS is a valid
    # state; the marker-write branch below handles it.
    STDIO_PIDS=$(pgrep -af toolkit-server 2>/dev/null \
        | grep -v -- '--http-only' \
        | grep -v -- '--http-port' \
        | awk '{print $1}' \
        | tr '\n' ' ' | sed 's/ *$//' || true)

    ATTACHED_PIDS=()
    ORPHAN_PIDS=()
    for pid in $STDIO_PIDS; do
        # /proc/<pid>/stat field 4 is PPID. Read the stat line; the
        # second field is the (parenthesised, possibly-spaced) command
        # name. Splitting on the closing paren keeps the field numbers
        # stable regardless of command-name spacing.
        if [[ ! -r "/proc/$pid/stat" ]]; then
            continue
        fi
        stat_after_paren="$(awk -F') ' '{print $2}' "/proc/$pid/stat" 2>/dev/null)"
        ppid="$(echo "$stat_after_paren" | awk '{print $2}')"
        if [[ -z "$ppid" || "$ppid" == "1" || ! -d "/proc/$ppid" ]]; then
            ORPHAN_PIDS+=("$pid")
        else
            ATTACHED_PIDS+=("$pid:$ppid")
        fi
    done

    FORCE_KILL="${TOOLKIT_PRECOMMIT_FORCE_STDIO_KILL:-0}"

    if [[ ${#ATTACHED_PIDS[@]} -gt 0 && "$FORCE_KILL" != "1" ]]; then
        _say "  preserving stdio MCP processes attached to live sessions:"
        for entry in "${ATTACHED_PIDS[@]}"; do
            apid="${entry%%:*}"
            appid="${entry##*:}"
            parent_comm="$(cat "/proc/$appid/comm" 2>/dev/null || echo "?")"
            _say "    - pid $apid (parent pid $appid, '$parent_comm') — active session, not killed"
        done
        _say "  the active session continues on the OLD binary until voluntary restart;"
        _say "  new sessions will pick up the new binary. Set TOOLKIT_PRECOMMIT_FORCE_STDIO_KILL=1"
        _say "  to override and kill anyway. Per-session /mcp reconnect picks up the new binary."
        _summary_add "preserved ${#ATTACHED_PIDS[@]} stdio session(s)"
        # Marker so any session that DOES check can know the binary rebuilt.
        {
            echo "stdio toolkit-server rebuilt at $(date -u +%FT%TZ)"
            echo "the active session retained the OLD binary on disk fd; /mcp reconnect to pick up the new one"
            for entry in "${ATTACHED_PIDS[@]}"; do
                echo "preserved stdio pid: ${entry%%:*}"
            done
        } > /tmp/toolkit-server-restart-needed 2>/dev/null || true
    fi

    if [[ ${#ORPHAN_PIDS[@]} -gt 0 ]] || [[ "$FORCE_KILL" == "1" && ${#ATTACHED_PIDS[@]} -gt 0 ]]; then
        if [[ "$FORCE_KILL" == "1" && ${#ATTACHED_PIDS[@]} -gt 0 ]]; then
            # Destructive action — surface even in quiet mode.
            _say_always "  TOOLKIT_PRECOMMIT_FORCE_STDIO_KILL=1 — killing attached stdio MCP processes too"
            for entry in "${ATTACHED_PIDS[@]}"; do
                ORPHAN_PIDS+=("${entry%%:*}")
            done
        fi
        # Destructive action — surface even in quiet mode.
        _say_always "  killing stdio MCP processes (pids: ${ORPHAN_PIDS[*]}) for clean disconnect..."
        for pid in "${ORPHAN_PIDS[@]}"; do
            kill "$pid" 2>/dev/null || true
        done
        _say_always "  stdio processes killed — run /mcp to reconnect with the new binary."
        _summary_add "killed ${#ORPHAN_PIDS[@]} stdio pid(s)"
        # Clear the marker only if every stdio is gone now.
        if [[ ${#ATTACHED_PIDS[@]} -eq 0 || "$FORCE_KILL" == "1" ]]; then
            rm -f /tmp/toolkit-server-restart-needed 2>/dev/null || true
        fi
    fi

    if [[ -z "$STDIO_PIDS" ]]; then
        # No stdio process matched. The running agent may still be talking
        # to a pre-rebuild binary (file descriptor held open after the
        # binary was overwritten). Drop a marker that any session can read
        # to know its handlers may be stale.
        {
            echo "stdio toolkit-server restart needed: rebuilt at $(date -u +%FT%TZ)"
            echo "diagnostic: pgrep -af toolkit-server | ls -l /proc/<pid>/exe (look for '(deleted)')"
            echo "remediation: restart Claude Code (or kill the stdio toolkit-server pid and let the IDE respawn)"
        } > /tmp/toolkit-server-restart-needed 2>/dev/null || true
        # The marker is the actionable artifact; the message can quiet down.
        _say "  ⚠  No stdio processes found — restart Claude Code IDE to pick up the new binary."
        _say "  (Marker written to /tmp/toolkit-server-restart-needed for the next session to read.)"
        _summary_add "no stdio processes (marker written)"
    fi
    _say ""
fi

if $NEED_HTTP_RESTART && [[ "$NO_DAEMON_RESTART" == "1" ]]; then
    # Operator opt-out (bug 936) — honored for ANY checkout.
    _say_always "  skipping :3000 daemon restart (TOOLKIT_PRECOMMIT_NO_DAEMON_RESTART=1)."
    _say "           the rebuilt binary is at $REPO_ROOT/go/bin/toolkit-server; restart :3000"
    _say "           yourself when ready, or /mcp reconnect to pick it up in-session."
    _summary_add "skipped :3000 restart (override)"
elif $NEED_HTTP_RESTART && $IS_LINKED_WORKTREE; then
    # Default for linked-worktree commits (bug 936): do NOT hijack the shared
    # :3000 daemon. It serves the MAIN checkout and is shared with the user +
    # any other agent; restarting it here with this worktree's binary is the
    # last-writer-wins race the chain exists to remove. View this branch on a
    # PRIVATE port instead (docs/MULTI_AGENT_WORKTREE_WORKFLOW.md §2).
    _say_always "  skipping :3000 daemon restart — commit is from a LINKED WORKTREE ($REPO_ROOT)."
    _say_always "           :3000 serves the MAIN checkout ($MAIN_ROOT), shared with the user + other"
    _say_always "           agents — restarting it here would hijack it with this worktree's binary."
    _say_always "           To view THIS branch live, run a private port (leaves :3000 untouched):"
    _say_always "             HTTP_PORT=3999 TOOLKIT_DB=/tmp/toolkit-\$(git branch --show-current).db go/launch.sh"
    _say_always "           To DEPLOY: merge/cherry-pick to main, 'make -C go build' in $MAIN_ROOT."
    _say_always "           (See docs/MULTI_AGENT_WORKTREE_WORKFLOW.md §§2-3.)"
    _summary_add "skipped :3000 restart (linked worktree)"
elif $NEED_HTTP_RESTART && systemctl --user is-active --quiet toolkit-server.service 2>/dev/null; then
    # POST-CUTOVER (finish-sophdn-repo-split T3; resolves bug 1028). The canonical
    # toolkit-server is now the CONTAINER on :3001 (image-lags-HEAD), and it is the
    # SOLE opener of the canonical DB. Relaunching a native :3000 daemon here would
    # make a SECOND opener of that file — the cross-mount-namespace WAL / dual-writer
    # hazard the container flip exists to avoid. So: rebuild the binary (the retained
    # native fallback) but do NOT auto-launch :3000. The canonical surface advances
    # ONLY by an explicit image rebuild — that lag IS the image-lags-HEAD boundary.
    _say_always "  skipping native :3000 restart — the CONTAINER (:3001) is canonical (image-lags-HEAD)."
    _say_always "           auto-launching :3000 would dual-open the canonical DB (WAL hazard, bug 1028)."
    _say_always "           the rebuilt binary at $REPO_ROOT/go/bin/toolkit-server is the retained fallback."
    _say_always "           advance the CANONICAL surface explicitly:"
    _say_always "             scripts/build-toolkit-image.sh && systemctl --user restart toolkit-server"
    _summary_add "skipped native :3000 restart (container canonical, image-lags-HEAD)"
elif $NEED_HTTP_RESTART; then
    # Re-read pid at restart time so a stale pid from classify time doesn't
    # cause a no-op kill if the daemon was already recycled. Identify the
    # holder by what's actually bound to :3000, not by `pgrep -f` substring
    # match — a transient shell or launch.sh wrapper whose cmdline contains
    # "toolkit-server.*--http-only" can match without holding the port, and
    # killing it leaves the real holder alive so launch.sh's pre-flight
    # then refuses to bind. ss prints `users:(("name",pid=NNN,fd=K))`; sed
    # extracts the pid.
    # `|| true`: pipefail + a host missing iproute2 (ss not installed —
    # bare-bones CI containers, the harness shims ss to `exit 0`) propagates
    # ss's exit-127 to the pipeline and aborts under `set -e`. Empty HTTP_PID
    # is a valid state: the else branch below starts a fresh daemon.
    HTTP_PID=$(ss -ltnp "sport = :3000" 2>/dev/null | sed -n 's/.*pid=\([0-9]*\).*/\1/p' | head -1 || true)
    if [[ -n "$HTTP_PID" ]]; then
        # Warn if the process has a controlling terminal — the user is running
        # a foreground CLI instance that is about to be killed. The background
        # relaunch below won't show up in that terminal. Destructive action,
        # ALWAYS surfaced — even in quiet mode.
        # `|| true`: pipefail + a race where the daemon exited between the ss
        # read above and this ps invocation propagates ps's exit-1 (PID missing)
        # to the pipeline and aborts under `set -e`. Empty HTTP_TTY is a valid
        # state — the `-n $HTTP_TTY` check below handles it.
        HTTP_TTY=$(ps -p "$HTTP_PID" -o tty= 2>/dev/null | tr -d ' ' || true)
        if [[ -n "$HTTP_TTY" && "$HTTP_TTY" != "?" ]]; then
            _say_always "  ⚠  Killing foreground CLI instance (pid $HTTP_PID, tty $HTTP_TTY)."
            _say_always "     A background daemon will be started in its place."
            _say_always "     Log: /tmp/toolkit-http.log"
            _say_always ""
        fi

        _say "  restarting HTTP daemon (pid $HTTP_PID)..."

        # Kill the entire process group so both the go/launch.sh parent and
        # the toolkit-server binary child exit together. A single `kill $PID`
        # hits only one process, leaving the other alive and holding port 3000 —
        # the launch.sh pre-flight then sees "port already bound" and exits
        # silently, leaving nothing running.
        # `|| true`: same race as HTTP_TTY above — the daemon may have exited
        # between the ss read and this ps invocation. Empty HTTP_PGID is a
        # valid state; the else branch below falls back to a bare kill.
        HTTP_PGID=$(ps -p "$HTTP_PID" -o pgid= 2>/dev/null | tr -d ' ' || true)
        if [[ -n "$HTTP_PGID" && "$HTTP_PGID" != "0" ]]; then
            kill -- -"$HTTP_PGID" 2>/dev/null || kill "$HTTP_PID" || true
        else
            kill "$HTTP_PID" || true
        fi

        # Poll until the port is free rather than sleeping a fixed guess.
        # Max 30 x 0.1s = 3s; after that, launch anyway and let launch.sh
        # report if the port is still bound.
        _wait=0
        while ss -ltn "sport = :3000" 2>/dev/null | tail -n +2 | grep -q . && [[ $_wait -lt 30 ]]; do
            sleep 0.1
            (( _wait++ ))
        done
        if [[ $_wait -ge 30 ]]; then
            # Warning condition — always surface.
            _say_always "  warning: port 3000 still bound after 3s — relaunching anyway"
        fi
    else
        _say "  no HTTP daemon running — starting one..."
    fi
    nohup ./go/launch.sh >/tmp/toolkit-http.log 2>&1 &
    _say "  HTTP daemon relaunched via go/launch.sh (log: /tmp/toolkit-http.log)"
    _summary_add "restarted HTTP daemon (log: /tmp/toolkit-http.log)"
    _say ""
fi

# Catch the case where the diff touched runtime-eligible paths (so DOC_ONLY
# turned false) but none ended up triggering a rebuild or restart.
if ! $NEED_STDIO_RESTART && ! $NEED_HTTP_RESTART && [[ ${#TEST_ONLY_PATHS[@]} -eq 0 ]]; then
    _say "  no daemon restart needed (no-runtime-impact edits)."
    _summary_add "no daemon restart needed"
fi

# Emit a CommitLanded event through the substrate so the arcreview
# listener (chain arc-close-filing-review-substrate-listener-wiring T6)
# can fire a session-scoped review on the freshly-landed commit.
#
# The binary posts to the daemon's HTTP MCP endpoint rather than opening
# its own DB pool — only events emitted INSIDE the daemon's process
# trigger the daemon's fold hook (SubstrateReviewObserver). A
# binary-owned-pool emit would land the row but bypass the listener.
#
# Fail-open per advisor discipline: the emit binary itself absorbs all
# error paths (daemon unreachable, schema validator failure, git command
# failure) and exits 0; if the binary is somehow missing or unexecutable,
# the `|| true` here covers that case too. The post-commit hook must
# NEVER block.
COMMIT_LANDED_EMIT="$REPO_ROOT/go/bin/commit-landed-emit"
COMMIT_LANDED_PROJECT="${TOOLKIT_PROJECT:-mcp-servers}"
if [[ -x "$COMMIT_LANDED_EMIT" ]]; then
    "$COMMIT_LANDED_EMIT" --project "$COMMIT_LANDED_PROJECT" >/dev/null 2>&1 || true
fi

# Emit the consolidated summary line in quiet mode. The accumulator may be
# empty if every code path was a _say (suppressed) without a _summary_add —
# in that case still emit a 'nothing to do' line so the operator can see
# the hook ran.
if [[ "$_quiet" == "1" ]]; then
    if [[ ${#_summary_parts[@]} -gt 0 ]]; then
        IFS=', '
        echo "advisor: ${_summary_parts[*]}"
        unset IFS
    else
        echo "advisor: classified $_file_count file(s), no action taken."
    fi
fi
