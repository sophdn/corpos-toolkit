# Complete Task

## When to use

When the work described by the task is genuinely done and you're ready to
record the handoff output that downstream consumers (next task, reviewer,
post-hoc inspection) will read. This is the canonical close path: status
flips active → closed and the handoff_output is written atomically.

## When not to use

- Do not use to roll back a closure — use `reopen_task` (with a reason).
- Do not use to cancel a task that no longer applies — use `cancel_task`.
- Do not call right after `start_task` with no real work in between. The
  handler emits a `warning` when the active window is < 5 seconds, and
  the warning is a nudge: the active state is the interruption-recovery
  signal, and a back-to-back start → complete defeats it. Legitimate
  fast-closes (stale tasks, instant work) are fine — the warning does
  not block.

## Steps

Call `complete_task` with `task_slug`, `chain_slug`, and optional
`handoff_output`. If `handoff_output` is omitted, the existing
handoff_output column on the row is left untouched (useful for
re-confirming a closed task already had its handoff written elsewhere).

Allowed transitions:
- `active → closed` (the happy path)
- `pending → closed` (the single-call shortcut for fast closes that
  deliberately skip the active window)

Rejected transitions:
- `closed → closed` (terminal-state re-entry)
- `cancelled → closed` (terminal-state)
- `blocked → closed` (must unblock first)
- task not found

A successful response is `{ "completed": true, "task_slug": "<slug>" }`,
optionally with a `"warning"` field when the back-to-back nudge fires.

A failure response is `{ "completed": false, "error": "<reason>" }`.

## What this writes

- Sets `tasks.status` to `closed`.
- Sets `tasks.handoff_output` to the supplied value (when provided).
- Sets `tasks.updated_at` to the current UTC datetime.

No content fields other than handoff_output are touched. Per-write
provenance lives in the observability event stream.
