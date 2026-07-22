# mcp-servers hook install snippets

This directory now holds **install-snippet documentation** for the
hooks that mcp-servers ships. The hook **scripts themselves** moved to
`mcp-servers/hooks/` during chain reference-resolution-migration T3
(self-containment migration); they're symlinked into `~/.claude/hooks/`
by `scripts/install-into-claude.sh` per the manifest at
`mcp-servers/skills/_manifest.toml`.

Use this README for the settings.json snippets you merge into
`~/.claude/settings.json` after running the install script.

## `arc-close-filing-review-hook.sh`

**Surface:** `Stop` (user-level)

**Purpose:** On every Stop event, run T3's arc-close-detector to check
for trigger conditions (counter threshold + user-shape regex), and on
trigger, call `work.review_arc_for_filing` against the toolkit-server.
The MCP action returns a typed `partition` of filing decisions; the
hook auto-executes high-confidence `forge_bug` / `forge_vault_note` /
`memory_write` via direct forge calls (or filesystem write for memory)
and surfaces the rest via a system-reminder text block on stdout for
the next user turn. Chain `arc-close-filing-review` T5.

**Critical invariant** (per
[`docs/ARC_CLOSE_FILING_REVIEW.md`](../../docs/ARC_CLOSE_FILING_REVIEW.md)
§Filing-dispatch "Scope of auto-execute"): the auto-execute path
fires forges and writes auto-memory entries only — NEVER writes code,
NEVER edits skill files. `skill_update` decisions always surface for
confirm regardless of confidence; the MCP action's
`partitionDecisions` already enforces this, and the hook trusts the
partition shape.

**Behavior:**

| Response status | Action |
|---|---|
| `fired` | Iterate `partition.auto_execute` → forge / write memory. Iterate `partition.surface_for_confirm` → emit one `<system-reminder>` block on stdout. Log `partition.skip` counts to drift. |
| `debounced` | No-op; drift-log the prior fire timestamp. |
| `skipped` | No-op; drift-log the reason (missing field / empty snapshot / etc.). |
| `qwen_unreachable` | Fail-open; drift-log. The current discipline (parse_context + skill bodies) keeps working. |
| MCP unreachable | Fail-open; drift-log. |
| `stop_hook_active=true` | Exit 0 immediately (anti-loop guard mirrors the detector's). |

**Knobs (env vars):**

- `TOOLKIT_HTTP_PORT` — toolkit-server HTTP port (default `3000`).
- `TOOLKIT_PROJECT` — project_id passed on the MCP envelope
  (default `mcp-servers`).
- `TOOLKIT_ARC_REVIEW_DIR` — directory for the detector's per-session
  counter state (default `$HOME/.claude/.arc-review`).
- `TOOLKIT_ARC_REVIEW_TURN_THRESHOLD` — user-turn count that fires
  the counter trigger (default `5`).
- `TOOLKIT_HOOK_DRIFT_LOG` — single-line drift log path
  (default `/tmp/toolkit-hook-drift.log`).
- `TOOLKIT_ARC_REVIEW_DETECTOR` — path to the detector script;
  override for tests (default sibling `arc-close-detector.sh`).

### Install

Append a `Stop` block to `~/.claude/settings.json`:

```json
{
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "$HOME/.claude/hooks/arc-close-filing-review-hook.sh"
          }
        ]
      }
    ]
  }
}
```

Merge alongside any existing `Stop` block (e.g.
`grounding-events-processor.sh`). Stop hooks run in array order;
this hook is safe to chain anywhere because it always exits 0 and
never blocks.

### Verify

Manual run against the regression scaffold:

```
bash hooks/test-arc-close-filing-review-hook.sh
```

Expected: 25 PASS lines, 0 FAIL. The scaffold spawns a mock
toolkit-server HTTP listener per case (single-shot
`python3 http.server.HTTPServer`) and asserts the right partition
dispatch behavior.

### Drift signal

If the MCP action's response shape changes (status enum widens, field
names drift, etc.), the hook will fail-open on unknown-status paths
and log to `/tmp/toolkit-hook-drift.log`. Tail that file when running
in anger; an empty log is steady state. The integration test
`go/internal/arcreview/integration_test.go` pins the response shape;
breaking changes show there first.

### Rollback

Remove the `Stop` block entry for this hook from
`~/.claude/settings.json`. Hook script + detector + MCP action stay
in the repo as historical artifacts; the `arc_review_debouncer` table
remains harmless when no fires land.

## `edit-drift-detector.sh`

**Surface:** `PostToolUse` + `UserPromptSubmit` (user-level; same
script handles both events, dispatching on the `hook_event_name`
field of the input payload)

**Purpose:** Forensic catch for the
"file edited by agent but reverted before the next turn" pattern
documented in bug
`mid-session-file-edits-silently-reverted-by-unidentified-mechanism`.
The hook does not PREVENT reverts; it surfaces them with structured
diff data so the next occurrence is investigable rather than
hand-wavy. Filed as option B of the chain
`arc-close-filing-review` T7 follow-up.

**Behavior:**

| Event | Action |
|---|---|
| `PostToolUse` on `Edit` / `Write` / `MultiEdit` / `NotebookEdit` | Snapshot the target file's md5 + size to `~/.claude/.edit-watch/<session_id>/<sanitized_path>.json`. |
| `PostToolUse` on other tools | No-op. |
| `UserPromptSubmit` | For each watched file, compute current md5; if it differs from the recorded snapshot (or the file is gone), append a `drift_detected` line to `/tmp/toolkit-hook-drift.log` with `path`, `session`, `hash_before`, `hash_after`, `size_before`, `size_after`, `delta_bytes`, `recorded_at`. Clear the session's watch dir. |
| Missing `jq` / `md5sum` / `session_id` | Fail-open (log to drift). |

The hook is observation-only — it never reverts, replays, or refuses
edits. Steady state is an empty watch dir + a quiet drift log. A drift
line is the trip-wire to investigate.

**Knobs (env vars):**

- `TOOLKIT_HOOK_DRIFT_LOG` — single-line drift log path
  (default `/tmp/toolkit-hook-drift.log`)
- `TOOLKIT_EDIT_WATCH_DIR` — session-scoped watch dir base
  (default `$HOME/.claude/.edit-watch`)

### Install

Append both event entries to `~/.claude/settings.json`. The same
script handles both — the `hook_event_name` discriminator in the
input payload routes the logic.

```json
{
  "hooks": {
    "PostToolUse": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "$HOME/.claude/hooks/edit-drift-detector.sh"
          }
        ]
      }
    ],
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "$HOME/.claude/hooks/edit-drift-detector.sh"
          }
        ]
      }
    ]
  }
}
```

Merge alongside existing entries.

### Verify

```
bash hooks/test-edit-drift-detector.sh
```

Expected: 15 PASS, 0 FAIL. Covers PostToolUse snapshot + tool
filtering + drift detection (content + file-removed) + clean-on-no-
drift + fail-open paths.

### Rollback

Remove both event entries. The script + watch dir + drift log are
all unversioned / per-machine; no migration rollback needed.

## `materialize-memory.sh`

**Surface:** `SessionStart` (user-level)

**Purpose:** Memory substrate v1 ships the auto-memory entries from
their canonical vault home at `~/.claude/vault/memory/<kind>/*.md` into
the harness's expected per-project dirs at
`~/.claude/projects/<project-dir>/memory/`. The harness's auto-load
contract stays unchanged; the storage source-of-truth flips.

Chain `memory-substrate-within-vault` T3. Sister to the forge-side T2
that owns the write path; this hook owns the read-time materialization.

**Routing:**

| kind | Materialization scope |
|---|---|
| `user` | Every project's harness dir (cross-project semantics — facts about the user span projects). |
| `feedback` / `project` / `reference` | Project matching the entry's `metadata.project:` frontmatter field (forge stamps it from the dispatch envelope at write time). Entries with no metadata.project route to the SessionStart's cwd-derived project as a fallback. |

**Sentinel + cleanup:** Materialized files end with the trailer
`<!-- materialized-from-vault -->`. The cleanup pass walks each
project's harness dir and removes sentinel-marked files whose backing
vault entry is gone. **Non-sentinel files (user-curated) are NEVER
touched.**

**MEMORY.md regeneration:** Each project's `MEMORY.md` is rebuilt
between the markers
```
<!-- materialized-from-vault:start -->
<!-- materialized-from-vault:end -->
```
Lines outside the markers (user-curated index entries pointing at
non-vault files) are preserved across rebuilds.

**Fail-open:** Always exits 0. Missing vault, missing jq, write
failures, malformed payloads — all drift-log to
`/tmp/toolkit-materialize-memory-drift.log` and continue.

### settings.json snippet

```json
{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "$HOME/.claude/hooks/materialize-memory.sh"
          }
        ]
      }
    ]
  }
}
```

### Verify

```
bash hooks/test-materialize-memory.sh
```

Expected: 13 PASS, 0 FAIL. Covers user-kind fan-out, project-scoped
routing, fallback to cwd, sentinel cleanup, user-curated preservation,
MEMORY.md region replacement, idempotency, and missing-vault silent
exit.

### Rollback

Remove the `SessionStart` event entry from `~/.claude/settings.json`.
The vault entries at `~/.claude/vault/memory/` persist untouched; T4's
one-shot migration script (chain T4) is the inverse if you want the
harness dirs to become source-of-truth again.

## `toolkit-binary-staleness-check.sh`

**Surface:** `SessionStart` (user-level)

**Purpose:** Make the "binary lags HEAD → /mcp reconnect" reflex
**structural** instead of a procedural convention a fresh agent can't
see. On session start it compares the **deployed** toolkit-server
binary's `gitSHA` — the one `.mcp.json`'s stdio command + `go/launch.sh`
actually load, i.e. the MAIN checkout's `go/bin/toolkit-server`, read via
`toolkit-server --version` (a cheap pre-flag-parse subcommand that prints
`<gitSHA> built <date>` and exits without touching the DB) — against the
**session checkout's** `git HEAD`, and emits a SessionStart
`additionalContext` warning on drift. Closes suggestion
`session-start-staleness-structural-signal` and the in-session-signal
half of bug `worktree-commits-dont-deploy-to-stdio-binary-path-no-staleness-signal`.

**Drift classes** (via git ancestry of the deployed SHA vs session HEAD):

| Class | Meaning | Remedy surfaced |
|---|---|---|
| `BEHIND` | deployed SHA is an ancestor of HEAD — the binary predates your code | `make -C go build` then `/mcp reconnect` |
| `DIVERGENT` | deployed SHA is NOT an ancestor — built from a different branch (worktree / concurrent agent) | commit + reconnect will NOT deploy your change; the main checkout must build your branch |

**Silent** (no emission) when in sync, when the session isn't an
mcp-servers checkout, when the binary is an `unversioned` dev build, or
on any error. Always exits 0 (non-blocking); misses drift-log to
`/tmp/toolkit-binary-staleness-drift.log`. Does **not** cover
within-session staleness after you commit mid-session — the post-commit
advisor's own message + `admin.server_version` handle that; this hook
catches HEAD-moved-but-binary-not-rebuilt (branch switch, worktree,
rebase, prior-session commit without rebuild).

### settings.json snippet

Add a second `SessionStart` command alongside `materialize-memory.sh`
(order-independent — both are fail-open and emit their own
`additionalContext`):

```json
{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "$HOME/.claude/hooks/materialize-memory.sh"
          },
          {
            "type": "command",
            "command": "$HOME/.claude/hooks/toolkit-binary-staleness-check.sh"
          }
        ]
      }
    ]
  }
}
```

### Verify

```
bash hooks/test-toolkit-binary-staleness-check.sh
```

Expected: `PASS`. Covers in-sync silence, BEHIND, DIVERGENT, unversioned
dev build, missing binary, and non-mcp-servers session (all via a
throwaway git repo + a fake `--version` stub; no real binary).

### Rollback

Remove the `toolkit-binary-staleness-check.sh` command from the
`SessionStart` hooks array in `~/.claude/settings.json`.

