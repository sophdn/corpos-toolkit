# Cancel Task

## When to use

When a task no longer applies and won't be done — the work was scoped
out, the chain pivoted, the dependency dissolved. Cancelling preserves
the row in the chain history with a terminal `cancelled` status so
downstream readers see why the task didn't close normally.

## When not to use

- Do not use to mark a task done — use `complete_task` (status flips
  to `closed`, not `cancelled`).
- Do not use for temporary unavailability — use `block_task` (the task
  comes back to `pending` after `unblock_task`).
- Do not use to reverse a wrongful closure — use `reopen_task` instead.
- Do not call on tasks that are already `closed` or `cancelled`. The
  state machine rejects terminal-state re-entry.
- For a batch of tasks to cancel in one sweep, use `cancel_tasks`.

## Steps

Call `cancel_task` with `task_slug` and `chain_slug`. Both are required;
task slugs are not globally unique.

Allowed transitions:
- `pending → cancelled`
- `active → cancelled`
- `blocked → cancelled`

Rejected transitions:
- `closed → cancelled` (terminal-state re-entry)
- `cancelled → cancelled` (terminal-state re-entry)
- task not found

A successful response is `{ "cancelled": true, "task_slug": "<slug>" }`.

A failure response is `{ "cancelled": false, "error": "<reason>" }`.

## What this writes

- Sets `tasks.status` to `cancelled`.
- Sets `tasks.updated_at` to the current UTC datetime.

No content fields are touched. Per-write provenance lives in the
observability event stream.
