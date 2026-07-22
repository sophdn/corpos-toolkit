# Start Task

## When to use

When you've decided to pick up a task and are about to do its work. Call
`start_task` at the **start** of the task, not right before
`complete_task` — the `active` status is the interruption-recovery
signal for future agents resuming after a crash or session end.

## When not to use

- Do not call to *look at* a task — use `read_task` for that.
- Do not call right before `complete_task`. A back-to-back start →
  complete sequence with no real work in between defeats the purpose
  of the `active` window.
- Do not call on tasks that are already `active`, `closed`, or
  `cancelled`. The state machine rejects re-entry into a non-pending
  state — pick a different task or use `reopen_task` if the task was
  closed in error.

## Steps

Call `start_task` with `task_slug` and `chain_slug`. Both are required;
task slugs are not globally unique.

A successful response is `{ "started": true, "task_slug": "<slug>" }`.

A failure response is `{ "started": false, "error": "<reason>" }`.
Common reasons:
- `task '...' not found in chain '...'` — slug mismatch
- `invalid transition: '<state>' → 'active'` — task is already past
  pending; pick a different one or close out the prior session first

## What this writes

- Sets `tasks.status` from `pending` to `active`.
- Sets `tasks.updated_at` to the current UTC datetime — this drives
  the back-to-back warning that `complete_task` emits when the active
  window is too short.

No content fields are touched. No provenance columns are stamped on
the row itself; per-write provenance lives in the observability event
stream.
