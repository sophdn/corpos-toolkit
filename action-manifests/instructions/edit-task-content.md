# Edit Task Content

## When to use

When an already-populated content field needs overwriting — a model
decision invalidated the original framing, an acceptance criterion
needs revising mid-flight, or a handoff_output needs correcting after
review. The write-side complement to `populate_task_content` (which
only fills empties).

Lifecycle-agnostic — closed and cancelled tasks can have their content
edited too (closure or cancellation stands; the content is refined).

## When not to use

- Use `populate_task_content` when the field is currently empty and
  you want default-protect semantics. (Edit is fine on empties too,
  but populate's skip-nonempty default is safer for backfill flows.)
- Use the dedicated lifecycle tools to change status (`start_task`,
  `complete_task`, `cancel_task`, `reopen_task`).
- Use `move_task` / `reorder_tasks` to change chain or position.
- Edit cannot change `task_slug` or `chain_slug` — those are
  identity, not content.

## Steps

Call `edit_task_content` with `slug`, `chain_slug`, and at least
one content-field override. Omitted fields are left untouched
(partial-update). All provided fields are written regardless of the
current value (no force flag — overwriting is the intent).

The five editable fields and the JSON shapes each accepts:
- `problem_statement` — string
- `acceptance_criteria` — string OR array of strings (list renders
  to `"\n- "`-joined storage form, matching forge-create)
- `context_required` — string OR array of strings (same as above)
- `constraints` — string
- `handoff_output` — string

Per-field validation is independent. If one field's value has the
wrong JSON shape, the other valid fields still write and the bad
field surfaces in `field_errors`. The call only aborts when zero
fields validate (then `field_errors` carries every failure).

A successful response is `{ "ok": true, "slug": "<slug>",
"fields_written": [...] }`. `field_errors` is omitted unless one
or more supplied fields failed validation.

A failure response is `{ "error": "<reason>" }`, optionally with
`field_errors: { "<name>": "<hint>" }` when the failure was at
the per-field decode stage.

## What this writes

Inside one transaction:
- For each provided field: `UPDATE tasks SET <field> = ?,
  updated_at = datetime('now') WHERE id = ?`.
- No status change. No write to provenance columns (per chain
  audit-column retirement decision).
