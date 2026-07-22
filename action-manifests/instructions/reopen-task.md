# Reopen Task

## When to use

When a task was closed and that closure needs to be rescinded — an
acceptance criterion was missed, a regression was introduced, scope
changed after the close, or review surfaced a problem the closure
hadn't addressed. The prior `handoff_output` is preserved beneath a
`[REOPENED YYYY-MM-DD: <reason>]` marker, so the original closure
context remains readable.

## When not to use

- Do not use to revive a `cancelled` task. Cancellation is
  intentionally irreversible — create a fresh task with a new slug
  instead. The state machine rejects `cancelled → active`.
- Do not use on `active` or `pending` tasks. There is nothing to
  rescind; `start_task` is the right call for picking up a pending
  task.
- Do not use to override a closure you simply disagree with. Reopen
  exists for material problems with the close, not for retrospective
  preference changes.

## Steps

Call `reopen_task` with `task_slug`, `chain_slug`, and a `reason`.
The reason must be non-empty (whitespace-only is rejected); it lands
in the `[REOPENED ...]` marker prepended to `handoff_output`.

Allowed transitions:
- `closed → active` (the only valid path)

Rejected transitions:
- `cancelled → active` (cancellation is irreversible)
- `active → active` (nothing to rescind)
- `pending → active` (use `start_task` instead)
- `blocked → active` (use `unblock_task` instead)
- task not found
- empty / whitespace-only reason

A successful response is `{ "reopened": true, "task_slug": "<slug>" }`.

A failure response is `{ "reopened": false, "error": "<reason>" }`.

## What this writes

- Sets `tasks.status` from `closed` to `active`.
- Prepends `[REOPENED YYYY-MM-DD: <reason>]\n\n` to
  `tasks.handoff_output`. If the prior handoff was empty, just the
  marker line is written.
- Sets `tasks.updated_at` to the current UTC datetime.

The date in the marker comes from SQLite's `date('now')` (UTC).
Per-write provenance lives in the observability event stream.
