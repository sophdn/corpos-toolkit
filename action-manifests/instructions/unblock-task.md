# Unblock Task

## When to use

When a previously-recorded `task_dependencies` edge is no longer
real — the blocker landed, the dependency dissolved, or the
relationship was wrong in the first place. Removing the edge
returns the blocked task to `pending` (and pickable) only when no
other blockers remain on it.

## When not to use

- Do not use to mark a task done — `complete_task` is the right call.
- Do not use to abandon a task — `cancel_task` is the right call.
- Do not use to flip a `blocked` task directly to `active`. Unblock
  takes it back to `pending`; the agent must then `start_task` to
  pick it up. The two-step flow is intentional — the active state
  is the interruption-recovery signal.
- Do not use to clear all blockers at once. Pass one
  (blocked, blocker) pair per call; loop in the caller for bulk.

## Steps

Call `unblock_task` with `task_slug` + `chain_slug` (the task being
unblocked) and `blocker_slug` + `blocker_chain_slug` (the blocker
edge to remove). Both pairs are required because task slugs are not
globally unique.

Status behaviour:
- If removing this edge leaves zero blockers AND the row is
  currently `blocked`, status resets to `pending`.
- If other blockers remain, status stays `blocked`.
- If the row is not `blocked` (e.g. someone closed it after the
  block was recorded), the edge is removed but the row's status is
  left untouched. The unblock does not resurrect closed/cancelled
  rows.
- Removing a non-existent edge is a silent no-op (matches the
  `INSERT OR IGNORE` symmetry on `block_task`).

A successful response is `{ "unblocked": true, "task_slug": "<slug>" }`.

A failure response is `{ "unblocked": false, "error": "<reason>" }`.

## What this writes

- DELETE on the matching row in `task_dependencies`.
- Conditionally UPDATE on `tasks.status` (`blocked → pending`) when
  zero blockers remain and the row is currently `blocked`.
- Stamps `tasks.updated_at` only when the status update fires.
- No content fields are touched. Per-write provenance lives in the
  observability event stream.
