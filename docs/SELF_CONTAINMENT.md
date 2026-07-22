# Self-Containment Architecture

**Chain:** reference-resolution-migration (id 598)
**Task:** T2 — self-containment-architecture-design
**Date:** 2026-05-18

Path X from the chain's `design_decisions`: mcp-servers is the
authoritative source for every dependency it consumes at runtime.
Skills, hooks, personas, and memory-format-or-discipline files live
inside the repo as canonical copies. Runtime paths Claude Code expects
(`~/.claude/skills/<name>/`, `~/.claude/hooks/<name>.sh`,
`~/.claude/personas/<name>.md`) become **symlinks into the repo**,
not separate copies. Pulling remote main + running the install script
restores the workbench's mcp-servers-relevant runtime environment on
a fresh machine.

This doc is the load-bearing output of T2. T3 executes against it.

---

> ## ⚠ SUPERSEDED IN PART (2026-07-22, chain 444 T6) — read this first
>
> This is a **dated 2026-05-18 design record**, kept for its rationale and left
> otherwise unedited. Three of its central claims are no longer true, and the
> repo it names (`mcp-servers`) has been split and renamed to `corpos-toolkit`.
>
> 1. **`_manifest.toml` does not exist and never did in this repo.** §3's registry
>    schema was designed but never built here; `scripts/install-into-claude.sh`
>    read it anyway and therefore exited 2 on its first check for weeks. The
>    installer is now manifest-free and discovers skills from the source trees
>    directly. corpos's loader also defers manifest bucket support
>    (`internal/skills/skills.go`), so nothing downstream honors one either. Any
>    §3 / §5 text about registering a skill in `_manifest.toml` — including the
>    §"deploying a skill" checklist — is inert.
> 2. **Skills are COPIES now, not symlinks into the repo.** The symlink farm went
>    stale when `mcp-servers` was renamed, leaving 22 dangling links with real
>    untracked content beside them — the exact opposite of the disaster-recovery
>    guarantee this doc promises. Skills install as gitignored copies (chain 444
>    T4); the canonical→overlay direction is documented in `~/.claude/CLAUDE.md`
>    § Skills and in the installer's header. **Hooks are still symlinks**, and
>    for hooks this doc's reasoning still holds exactly.
> 3. **`~/.claude` is canonical for 19 skills**, not downstream for all of them.
>    The operator stack skills live in corpos's gitignored `internal/skills/
>    userlib/`, so they have no committed upstream to install from and the flow
>    reverses. "The repo is authoritative for everything it consumes" is true
>    only of the half that has a repo home.
>
> Personas remain unsolved: this doc assumed a `personas/` dir in the repo, and
> there isn't one. Tracked separately.

---

## 1. Directory layout

```
mcp-servers/
├── skills/                  # canonical skill bodies (existing dir, expanded)
│   ├── _manifest.toml       # registry per §3
│   ├── reference-resolution/         (already present)
│   │   ├── SKILL.md
│   │   └── SKILL.toml
│   ├── parse-context-first-call/    (NEW — T6 authors)
│   │   ├── SKILL.md
│   │   └── SKILL.toml
│   ├── bug-filing-discipline/        (MIGRATED from ~/.claude/skills/)
│   ├── vault-pull-discipline/        (CANONICAL; deduplicates with ~/.claude/skills/)
│   ├── ... (per inventory §3.1 of REFERENCE_RESOLUTION_MIGRATION_PLAN.md)
│
├── hooks/                   # NEW — consolidates hooks scattered today
│   ├── grounding-events-processor.sh    (MIGRATED from ~/.claude/hooks/)
│   ├── friction-filing-reminder.sh      (MIGRATED — rollback artifact)
│   └── intercept-task-tools-reminder.sh (REMOVED 2026-05-25 — superseded by a permissions.deny fix; see REFERENCE_RESOLUTION.md §8)
│
├── personas/                # NEW — for the ~9 role personas
│   ├── MANIFEST.md
│   ├── ROLE_SPEC.md
│   ├── advisory-panel.md
│   ├── code-auditor.md
│   ├── daemon.md
│   ├── librarian.md
│   ├── prospector.md
│   ├── role-keeper.md
│   ├── synthesist.md
│   ├── technowizard.md
│   └── white-hat.md
│
├── memory/                  # NEW — format + discipline ONLY; no personal content
│   ├── _schema.md           # frontmatter shape, body conventions, MEMORY.md index format
│   └── (any project-side memory-discipline files — currently none distinct from the general
│        auto-memory discipline that lives in the harness)
│
└── scripts/
    ├── install-into-claude.sh    # NEW — the install script per §5
    ├── hooks/                    # existing dir
    │   └── README.md             (install snippets for settings.json; intercept-task-tools-reminder.sh + its test REMOVED 2026-05-25)
    └── (other existing scripts)
```

Notes:
- `scripts/hooks/` stays — its README documents settings.json install snippets. The hook scripts themselves move to `mcp-servers/hooks/` for consolidation; `scripts/hooks/` keeps only the test scaffold + README.
- `_manifest.toml` lives at `mcp-servers/skills/_manifest.toml` because that's where readers will look first. T3 may move it to `mcp-servers/_dependency_manifest.toml` if cross-cutting concerns warrant — for now skills/ is fine.

---

## 2. Symlink-not-copy rule

The doc's Option C drift hazard is real (copies of the same skill across N projects WILL drift, fast — see vault `2026-05-17_doc-organization-frames-age-faster-than-items`). Path X avoids this by maintaining **a single canonical body in the repo + symlinks at the runtime paths**.

```
~/.claude/skills/bug-filing-discipline/SKILL.md
    → symlink to → /home/user/dev/mcp-servers/skills/bug-filing-discipline/SKILL.md
```

Properties:

- **No duplication.** Edit the canonical body in the repo; the runtime path sees the change immediately. No propagation step.
- **Git is the source of truth.** Disaster recovery: `git clone <gitea-remote>/mcp-servers && bash scripts/install-into-claude.sh` rebuilds `~/.claude/skills/` from scratch.
- **Other workbench projects can co-opt the same canonical.** seed-packet wants `rust-conventions`? Symlink `seed-packet/skills/rust-conventions/` → `mcp-servers/skills/rust-conventions/`. No copy, no drift.
- **Cross-machine portability.** Each machine that wants the mcp-servers runtime: `git clone` + `install-into-claude.sh`. Same canonical, no per-machine handcrafted state.

Exceptions when symlinks are infeasible:
- **Build-artifact ingestion paths.** None currently — mcp-servers's skills/hooks/personas are all readable-as-files at the runtime path. Document any exception in the manifest with rationale; copy + propagation discipline becomes the fallback only when a symlink would semantically break (e.g. paths embedded in compiled artifacts).

---

## 3. Manifest schema (`mcp-servers/skills/_manifest.toml`)

The single source of truth for what mcp-servers depends on and where it installs. Read by:
- `scripts/install-into-claude.sh` (decides what to symlink where)
- `parse_context`'s skill-trigger resolver (per T4 design — reads trigger keywords per entry)
- Future tooling (a `make doctor` to verify symlink health, etc.)

### 3.1 Top-level

```toml
# Manifest version for forward-compat. Bump major when the entry schema
# changes incompatibly.
schema_version = 1

# Default install root. Install script can be invoked with an alternate
# target (sandbox testing); defaults to ~/.claude.
default_install_root = "~/.claude"
```

### 3.2 Per-entry schema

Each entry is a `[[skill]]`, `[[hook]]`, `[[persona]]`, or `[[memory_file]]` table-array:

```toml
[[skill]]
name = "bug-filing-discipline"
# Canonical body path, relative to the repo root.
body_path = "skills/bug-filing-discipline/SKILL.md"
# Optional companion files (TOML metadata, references/, etc.) that
# travel together. Symlinked-or-installed alongside the body.
companion_paths = ["skills/bug-filing-discipline/SKILL.toml"]
# Install target at the runtime path. Relative to default_install_root.
install_target = "skills/bug-filing-discipline/"
# Bucket per the migration plan's three-bucket model. Drives startup
# payload generation (when the harness consumes this manifest).
bucket = "condense-lazy"  # one of: keep-ambient | condense-lazy | pure-lazy
# Trigger keywords for parse_context's skill-trigger resolver.
# Per the parity-test vault learning, prefer specific identifiers over
# generic English. 3-5 keywords per entry; conflicts flagged in §6.
trigger_keywords = ["friction", "paper-cut", "could-file", "bug-filing"]
# One-line description for the manifest's discovery surface (shown by
# the harness's available-skills list for keep-ambient + condense-lazy
# entries).
description = "File observed friction in the toolkit DB."
# Optional: which project originally authored this skill. mcp-servers
# owns canonical; this field documents heritage for cross-project
# coordination if it becomes relevant.
origin = "user-global"  # or "mcp-servers" / "seed-packet" / etc.
```

Hook entries are simpler:

```toml
[[hook]]
name = "grounding-events-processor"
body_path = "hooks/grounding-events-processor.sh"
install_target = "hooks/grounding-events-processor.sh"
# Hook type tells the install script's settings.json snippet generator
# which block to suggest.
hook_type = "Stop"  # or UserPromptSubmit | PreToolUse | PostToolUse | etc.
description = "Process session jsonl into grounding_events on Stop."
```

Personas + memory files follow the same shape with different install_target prefixes.

### 3.3 Bucket field — load-bearing for harness integration

The `bucket` field is the handle that lets the startup-payload generator decide what to include:

- `keep-ambient` → full description in the available-skills list at startup
- `condense-lazy` → one-line pointer (`name + description`) in startup; full body lazy via parse_context
- `pure-lazy` → no startup presence at all; discoverable via slash-command or parse_context skill-trigger resolver

T7 may promote condense-lazy → pure-lazy entries after verifying parse_context's discipline-resolver coverage. The manifest is the source of truth; bucket changes are bucket-field edits.

### 3.4 Why TOML

Matches the prevailing convention in this repo (forge-schemas, action-docs, dispatch-policy, action-manifests). Easy to author by hand. Comment-friendly. No dynamic-typing surprises like JSON's stringly-typed bools.

---

## 4. Install-script contract (`scripts/install-into-claude.sh`)

### 4.1 Behavior

```
$ bash scripts/install-into-claude.sh [--target=<path>] [--dry-run] [--force]
```

For each `[[skill]]`, `[[hook]]`, `[[persona]]` entry in the manifest:
1. Resolve `install_target` against `default_install_root` (or `--target` override).
2. Resolve `body_path` against the repo root (absolute path to the canonical).
3. Create the install_target's parent directory if missing (idempotent).
4. If install_target doesn't exist → create symlink. ✓
5. If install_target exists and IS a symlink to the canonical → no-op. ✓ (idempotent)
6. If install_target exists and is a symlink to a different path → re-link (after explicit `--force` OR by prompting). Warn otherwise.
7. If install_target exists and is NOT a symlink (real file/dir) → REFUSE TO CLOBBER. Surface as a conflict; user must resolve manually (move the existing entry aside, then re-run).

### 4.2 Idempotency

Running the script twice with no underlying changes produces no errors and no duplicate links. Verified by T3's sandbox test.

### 4.3 Settings.json snippets

The install script does NOT auto-write `~/.claude/settings.json`. Auto-mode self-modification guard. Instead, on completion it prints (or writes to a `INSTALL_NEXT_STEPS.md`) the JSON snippets the user should merge into their settings.json. Per-hook snippets:

> **Note (2026-05-25):** the `UserPromptSubmit` → `intercept-task-tools-reminder.sh` snippet shown below was REMOVED — the task-tools reminder is now handled by a `permissions.deny`, not a hook (see REFERENCE_RESOLUTION.md §8 / bug `task-tools-reminder-overfires-when-mcp-work-tools-are-active`). Disregard that block; the install script no longer emits it.

```json
{
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "$HOME/.claude/hooks/grounding-events-processor.sh"
          }
        ]
      }
    ],
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "$HOME/.claude/hooks/intercept-task-tools-reminder.sh"
          }
        ]
      }
    ]
  }
}
```

Source of these snippets: derived from the manifest's `[[hook]]` entries + their `hook_type` field. The script generates the snippets from the manifest; doesn't hand-craft them.

### 4.4 Dry-run mode

`--dry-run` reports what the script WOULD do without making changes. Useful for sandbox testing + for the user to verify before running for real.

### 4.5 Conflict handling — example output

```
$ bash scripts/install-into-claude.sh
[install] resolving manifest at mcp-servers/skills/_manifest.toml
[install] reading 26 skill entries, 3 hook entries, 9 persona entries, 1 memory-schema entry
[install] target root: /home/user/.claude

[install] skill bug-filing-discipline:
[install]   target: /home/user/.claude/skills/bug-filing-discipline/SKILL.md
[install]   action: SYMLINK → /home/user/dev/mcp-servers/skills/bug-filing-discipline/SKILL.md
[install]   ✓ linked

[install] skill rust-conventions:
[install]   target: /home/user/.claude/skills/rust-conventions/SKILL.md
[install]   action: CONFLICT — target exists and is NOT a symlink
[install]   ⚠  refusing to clobber. resolve manually:
[install]      mv /home/user/.claude/skills/rust-conventions /home/user/.claude/skills/rust-conventions.bak
[install]      bash scripts/install-into-claude.sh   # re-run

[install] 24 of 26 skill entries installed.  2 conflicts.  See above.
[install] write settings.json snippets to INSTALL_NEXT_STEPS.md? [y/N]
```

### 4.6 Reverse direction — uninstall (optional)

`scripts/install-into-claude.sh --uninstall` removes only the symlinks the manifest declared. Doesn't touch user-authored entries. Useful for a clean state during sandbox testing.

---

## 5. Personas + memory directories

Same shape as skills:

- `mcp-servers/personas/<name>.md` is canonical; `~/.claude/personas/<name>.md` is the symlink.
- `mcp-servers/personas/MANIFEST.md` and `ROLE_SPEC.md` are the registry + schema; symlinks at the runtime path.
- `mcp-servers/memory/_schema.md` documents the frontmatter format; symlinked into `~/.claude/projects/...` only if the harness reads it from there (otherwise it stays in-repo as reference doc).

Memory entries themselves (personal content) **are NOT in the manifest**. Only schema + discipline files travel. The split is intentional: format + rules are project-owned; personal data is user-owned.

---

## 6. Trigger-keyword conflict policy

Multiple skills could share a keyword (e.g. "vault" is in both vault-pull-discipline and vault-filing-discipline). parse_context's resolver returns ALL matching skills as Candidates per the design. The manifest's job is just to declare keywords; the resolver disambiguates by confidence tier + Candidate ranking.

Per the [parity-tests vault learning](file:~/.claude/vault/learnings/general/2026-05-18_parity-tests-between-live-classifiers-assert-outputs-not-rule-equality.md): trigger-keyword discipline is about output-equivalence (representative inputs surface the right skill), not list-equality. T7 verifies via representative inputs; conflicting keywords that consistently return the wrong skill get renamed in the manifest.

---

## 7. Cross-project sharing — deferred

mcp-servers owns canonical for the workbench. If seed-packet, atomic-tasks, dm-toolkit, or self-compile need the same skill, they symlink to `mcp-servers/skills/<name>/` from their own runtime path. Examples:

```
seed-packet/skills/rust-conventions → mcp-servers/skills/rust-conventions/
atomic-tasks/skills/rust-conventions → mcp-servers/skills/rust-conventions/
```

Both pull from the same canonical. Edits in mcp-servers propagate to both.

If a skill semantically diverges per-project (seed-packet wants a different rust-conventions than mcp-servers), the resolution is **not** to copy and drift — it's to evolve the canonical to cover both cases OR to fork explicitly with a clear name. Fork-with-rename is rare; the workbench is single-author and the conventions converge naturally.

Cross-project canonicalization (a separate workbench-skills repo) is a future concern if drift emerges. Path X today; Path Y if needed.

---

## 8. Disaster-recovery story

On a fresh machine OR after `~/.claude/` corruption:

```bash
# 1. Clone the repo from gitea
git clone https://gitea.example/sophdn/mcp-servers ~/dev/mcp-servers
cd ~/dev/mcp-servers

# 2. Build the binary (separate from skills install)
make -C go build

# 3. Install the dependency surface into ~/.claude/
bash scripts/install-into-claude.sh

# 4. Merge the printed settings.json snippets into ~/.claude/settings.json
# (manual step; auto-mode guard prevents auto-write)

# 5. Verify
ls -la ~/.claude/skills/   # all symlinks pointing into ~/dev/mcp-servers/skills/
ls -la ~/.claude/hooks/    # all symlinks pointing into ~/dev/mcp-servers/hooks/
ls -la ~/.claude/personas/ # all symlinks pointing into ~/dev/mcp-servers/personas/
```

Restored. The runtime environment Claude Code expects is intact; mcp-servers's substrate workflows function as they did pre-disaster.

---

## 9. Sample manifest entries

Three representative entries spanning the buckets, to validate the schema captures the distinctions T3 will work against:

### 9.1 Keep-ambient (the one survivor)

```toml
[[skill]]
name = "parse-context-first-call"
body_path = "skills/parse-context-first-call/SKILL.md"
companion_paths = ["skills/parse-context-first-call/SKILL.toml"]
install_target = "skills/parse-context-first-call/"
bucket = "keep-ambient"
trigger_keywords = ["parse_context", "first-call", "orienting-call"]
description = "Call parse_context as your first action on every user prompt; surfaces all relevant context in one envelope."
origin = "mcp-servers"
```

### 9.2 Condense-lazy (discipline-skill surfaced by parse_context on shape match)

```toml
[[skill]]
name = "bug-filing-discipline"
body_path = "skills/bug-filing-discipline/SKILL.md"
companion_paths = ["skills/bug-filing-discipline/SKILL.toml"]
install_target = "skills/bug-filing-discipline/"
bucket = "condense-lazy"
trigger_keywords = ["friction", "paper-cut", "could-file", "worth-filing", "noted-but-not-filed"]
description = "File observed friction as a bug in the toolkit DB. Surfaces via parse_context's friction-shape detector."
origin = "user-global"  # migrated from ~/.claude/skills/ during T3
```

### 9.3 Pure-lazy (convention skill discoverable via parse_context skill-trigger OR slash-command)

```toml
[[skill]]
name = "rust-conventions"
body_path = "skills/rust-conventions/SKILL.md"
companion_paths = [
    "skills/rust-conventions/SKILL.toml",
    "skills/rust-conventions/references/",  # references/ subdir travels with the skill
]
install_target = "skills/rust-conventions/"
bucket = "pure-lazy"
trigger_keywords = ["rust", "cargo", "clippy", "rustfmt", "tokio"]
description = "Sophi's Rust coding standards — style, crate anatomy, errors, testing."
origin = "user-global"
```

---

## 10. Open items + caveats

1. **Harness's startup-payload generator:** the `bucket` field is only load-bearing IF the Claude Code harness consumes the manifest. Today the harness scans `~/.claude/skills/` to populate available-skills. T7's paring may require upstream Claude Code to read the manifest's bucket field; document the gap if encountered. Until upstream supports it, paring works on the substrate side (parse_context surfaces lazy bodies) even if the harness still over-loads the description list.

2. **Manifest authoring at T3 time:** T3 generates the initial manifest from T1's dependency inventory. The schema here is the contract; T3 fills in entries.

3. **Trigger-keyword conflicts** are accepted (parse_context returns all matches). T7 verifies the surfaced behavior on representative inputs; documented misses become keyword refinements.

4. **Memory tracking is schema-only.** Personal entries stay at `~/.claude/projects/.../memory/` and are NOT in the manifest. If a future need arises to track project-specific memory bodies in the repo, that's a follow-on decision.

5. **Vault content tracking is out of scope.** The disciplines (vault-pull-discipline, vault-filing-discipline) travel via the manifest; vault content stays at `~/.claude/vault/`.

6. **Install script unimplemented at T2 close.** T3 ships the script + the actual migration. T2 ships the contract.

---

## 11. Chain handoff

T2 closes. T3 (`migrate-skills-hooks-personas-into-repo`) becomes unblocked AND IS THE FIRST POINT WHERE USER INPUT IS REQUIRED — the file-migration step touches `~/.claude/skills/`, `~/.claude/hooks/`, `~/.claude/personas/` (self-modification of the user's home directory beyond mcp-servers itself). The auto-mode classifier will (correctly) block the agent from doing this unilaterally. T3 needs explicit authorization from Sophi to proceed.

T4 (`parse-context-design`) remains unblocked and parallel-safe. Can proceed without waiting on T3.

---

## 12. Runbook — deploying or registering a new skill

Authored 2026-05-23 after deploying `refactoring-discipline` and registering `code-migration-discipline`. These are the steps that make a skill BOTH harness-loadable AND resolvable by `parse_context`. Authoring a `SKILL.md` under `~/.claude/skills/<name>/` alone gets the former but NOT the latter — the `skill_trigger` / `discipline_skill` resolvers only see skills registered in `_manifest.toml`. (A skill that's harness-visible but manifest-absent is the silent gap that left `code-migration-discipline` un-resolvable for weeks.)

### 12.1 Steps

1. **Canonical body in the repo.** Author or move the skill body to `skills/<name>/SKILL.md` — the repo is the single source of truth (§2); `~/.claude/skills/<name>` becomes a symlink. If the skill already exists as a standalone dir under `~/.claude/skills/`, copy it in first: `cp ~/.claude/skills/<name>/SKILL.md skills/<name>/SKILL.md`.

2. **Register in `_manifest.toml`.** Add a `[[skill]]` entry (§3.2): `name`, `body_path = "skills/<name>"`, `install_target = "skills/<name>"`, `bucket` (§3.3), `trigger_keywords` (3–10 *specific* phrases — see the §6 conflict policy; prefer specific identifiers over generic English), `description`, `origin`.

3. **Symlink into `~/.claude/`.** The install script refuses to clobber a real (non-symlink) dir (§4.5), so remove any standalone copy first, then run the installer:
   ```sh
   rm -rf ~/.claude/skills/<name>        # ONLY if a standalone (non-symlink) dir exists
   bash scripts/install-into-claude.sh   # links the new entry; idempotent for the rest
   ```
   Expect `1 linked, N already-linked, 0 conflicts`.

4. **Add a discoverability test case.** In `go/internal/refresolve/discoverability_test.go`, add a case: a representative message containing one trigger keyword → `wantSkillName: "<name>"`, `wantShape: refresolve.ShapeSkillTrigger` (or `ShapeDisciplineSkill` for friction-promoted disciplines). The test builds its own registry from the live manifest, so it pins trigger-keyword coverage against regression.

5. **Commit.** The pre-commit gate runs the full suite (including the discoverability test); the post-commit advisor rebuilds the binary and restarts the HTTP daemon (live stdio sessions are deliberately preserved).

6. **Restart to rebuild the resolver registry — the load-bearing gotcha.** `parse_context` reloads the *detection* catalog per call (`handler.go`), so the new keyword is DETECTED immediately — but resolution dispatches against `deps.Registry`, which is **built once at startup**. Until the daemon process restarts, the new skill comes back as a `no_hit_token` (detected-but-unresolvable), which masks as an unrelated failure (e.g. the co-firing `stdio-drift` surface). Run `/mcp reconnect` (or start a new session), then verify:
   ```
   parse_context(message_text: "<message containing a trigger keyword>")
   # expect: skill_trigger / single_exact / use_directly, body inlined
   ```
   Tracked as bug `parse-context-detects-new-manifest-skills-but-cannot-resolve-until-restart`. If/when that lands, detection and resolution become symmetric and step 6's restart is no longer required for resolution (the binary rebuild + new-session pickup still applies to the harness side).

### 12.2 Concurrent-session caveat

Multiple Claude sessions can share this working directory; a `git checkout` in one moves the shared HEAD for all (memory: `concurrent-skill-deploy-shares-workdir`). When deploying via a branch, verify `git branch --show-current` before AND after each commit, and **cherry-pick rather than merge** when branches have diverged via duplicated commits.

### 12.3 Intent-mapping is a separate, governed path

The steps above give keyword-triggered resolution. Surfacing a discipline on directive-INTENT (a refactor-shaped prompt with no literal trigger keyword) is a different path through `intentDisciplineMap` plus the closed `IntentShape` vocabulary (`intent_detect.go`; docs/PARSE_CONTEXT.md §13.2). Both are closed sets whose extension **requires a new chain task that revisits the design** — do not add intent shapes or map entries inline. (In-flight example: chain `refactor-intent-discipline-surfacing`.)
