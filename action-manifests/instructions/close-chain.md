# Close Chain

## When to use

When every task in a chain has reached a terminal state (`closed` or
`cancelled`) and the chain itself should flip from `open` to `closed`.
This is the final write in the chain lifecycle.

## When not to use

- If any task is still `pending`, `active`, or `blocked`, close those
  out first (`complete_task` / `cancel_task` / `unblock_task` →
  `complete_task`). The all-tasks-terminal guard rejects the close.
- If you want to pause work without closing — chains have no "paused"
  state; leave it `open`.
- The work-server schema does not carry a `design_doc_path`; manage
  documentation moves separately.

## Steps

Call `close_chain` with `chain_slug` (or `slug` / `chain` — all three
spellings are accepted). Optionally pass a closing summary via either
`summary` or `closure_summary` (the schema field name); both are
accepted and `closure_summary` wins when both appear. The handler
runs three checks inside one transaction before flipping status:

1. Chain exists (else `chain '<slug>' not found`).
2. Chain status is `open` (else `cannot close: chain '<slug>' is
   already '<status>'`).
3. All tasks in the chain are `closed` or `cancelled` (else
   `cannot close: chain '<slug>' has N non-terminal task(s):
   ["task-a", "task-b", …]`, with the specific slugs named).

A chain with zero tasks is vacuously terminal and can be closed
(useful for chains forged but never populated).

A successful response is `{ "closed": true, "chain_slug": "<slug>" }`.
When a closing summary is supplied, the response also carries
`closure_summary_chars: <int>` echoing the stored length.

A failure response is `{ "closed": false, "error": "<reason>" }`.

## What this writes

Inside one transaction:
- `UPDATE chains SET status = 'closed', closure_summary = ?,
  updated_at = datetime('now') WHERE id = ?` (closure_summary clause
  omitted when no summary was supplied).
- A `ChainClosed` event with the matching closure_summary payload.
- No task rows are touched. No content fields rewritten. No design
  doc moves.
