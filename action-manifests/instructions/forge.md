# forge

## When to use

- You need to create a new artifact: a task, a chain, a pitch, a bug, a role, a skill, etc. Forge reads the schema from `blueprints/forge-schemas/<name>.toml`, validates the field set you supply, and writes the artifact (DB row, markdown file, or both, depending on the schema's storage backend).
- Pass `after_slug` or `before_slug` (mutually exclusive, task-only) to control insertion position within a chain.
- For a one-off shape that doesn't have an existing schema, pass `declare_fields` + `prefix` + `output_dir` and forge will generate a draft schema before forging the artifact.

## When not to use

- Editing an existing artifact → `forge_edit`.
- Deleting → `forge_delete`.
- Discovery (which schemas exist? what fields? what types?) → `forge_list`.
- Reading existing artifacts → use the read tool for that artifact's surface (e.g. `read_task`, `bug_read`, `get_chain_state`).

## Field shapes

`fields` is a key-value map. Each value is a string (for `string` / `optional_string`) or an array of strings (for `string_list` / `optional_string_list` / `string_or_list` / `optional_string_or_list`). When `optional_string_or_list` accepts a single string, forge coerces it to a one-element list at validation time.

If a required field is missing or a type doesn't match, the response carries `error` plus `valid_fields: [{name, type, required, description}]` — the full schema field set, so you can fix the payload in one shot rather than calling `forge_list` first.

## Response shape

DB-backed shapes (task, chain):

```json
{ "created": true, "schema": "task", "is_draft": false, "source": "database", "slug": "...", "chain_slug": "..." }
```

Markdown-backed shapes:

```json
{ "created": true, "schema": "pitch", "is_draft": false, "artifact_path": "process-docs/pitches/PITCH_my-pitch_2026-04-28.md" }
```

Chain shapes also carry `tasks_inserted` (count of task skeletons forged from the pipe-delimited `tasks` field).

Ad-hoc generation also carries `ad_hoc: true` and (when a draft schema was generated) `draft_schema_path`.

## Errors

- `error: "no schema found for ..."` — schema name doesn't exist on disk and no `declare_fields` was supplied.
- `error: "schema '...' failed to parse at scan time: ..."` — the schema TOML is malformed; surface to the operator and rebuild the binary if the FieldType set drifted.
- Field-validation error (missing required, unknown field, type mismatch) — `error` carries a human-readable description; `valid_fields` is the full schema field set.
- `error: "after_slug and before_slug are mutually exclusive — pass only one"` — caller passed both anchors.
- `error: "after_slug / before_slug position anchors are only valid for schema_name='task'"` — caller passed an anchor on a non-task schema.

Requires `--signal-table-root` to locate `blueprints/forge-schemas/` AND `--db` to write DB-backed artifacts. Markdown-only forges still need `--signal-table-root`.
