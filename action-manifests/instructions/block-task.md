# Block Task

## When to use

When a task can't proceed until a specific other task is done — the
blocker is a real dependency edge in the chain graph, not just a
preference or a vague "later". Recording it via `block_task` flips the
blocked task's status (only when currently `pending` or `active`),
inserts an edge in `task_dependencies`, and gives downstream pickup
tooling something concrete to skip over.

## When not to use

- Do not use to mark a task hard or risky — those stay `active`.
- Do not use to record being blocked on something outside the chain
  (an external review, a meeting, a deploy). The dependency table
  only models task→task edges; non-task blockers belong in handoff
  prose or a separate note.
- Do not use to abandon a task — `cancel_task` is the right call.
- Do not use to retire a closure — `reopen_task` is the right call.

## Steps

Call `block_task` with one of these identifier shapes for the task
being blocked, plus one for the blocker:

- `slug` + optional `chain_slug` (canonical) — `chain_slug` is needed
  only when the slug is ambiguous across chains.
- `task_id` (numeric, from `task_search` output) — globally unique,
  no `chain_slug` required.

For the blocker, the same shape applies: `blocker_slug` + optional
`blocker_chain_slug`, or `blocker_id`. When both an ID and a slug
are supplied for the same task, the ID wins. (The legacy alias
`task_slug` is still accepted as a synonym for `slug` so older
callers don't break.)

Self-blocking is rejected; closed/cancelled tasks cannot act as
blockers. Adding the same edge twice is a silent no-op (INSERT OR
IGNORE on task_blockers' (blocked, blocker) primary key).

Status behaviour:
- If the blocked task is currently `pending` or `active`, status
  flips to `blocked`.
- If the blocked task is `closed` or `cancelled`, the edge is still
  recorded (informational) but status is not changed — closure /
  cancellation are terminal in their own direction.
- If the blocked task is already `blocked`, an additional edge is
  recorded; status remains `blocked`.

A successful response is `{ "ok": true, "slug": "<slug>", "status":
"blocked" }`.

A failure response is `{ "error": "<reason>" }`.

## What this writes

- Inserts a row into `task_blockers` (`blocked_task_id`,
  `blocker_task_id`, `reason`). PRIMARY KEY (blocked_task_id,
  blocker_task_id) makes duplicate inserts idempotent.
- Conditionally updates `tasks.status` to `blocked` and stamps
  `tasks.updated_at`.
- No content fields are touched. Per-write provenance lives in the
  observability event stream.
