# forge_edit

## When to use

- You need to update or append to a single field on an existing artifact (a markdown file or a DB row authored via `forge`).
- For markdown / dual shapes (pitch, script, task-decomp), pass `artifact_path` (relative to the project root).
- For DB-only shapes (task, chain), pass `key` — a string for simple keys, or an object like `{chain_slug, slug}` for composite keys.

## When not to use

- Creating a new artifact → `forge`.
- Deleting → `forge_delete`.
- Discovering what schemas exist → `forge_list`.
- Task-row mutations where dedicated tools exist (`start_task`, `complete_task`, `populate_task_content`, `edit_task_content`, etc.) — those handle lifecycle invariants forge_edit doesn't.

## Operations

- `operation: "update"` — replace the field's value with `value`. `value` is a string or array, matching the field's declared type.
- `operation: "append"` — add a single string to a list field. `value` must be a single string (or a one-element list, which is coerced).

## Errors

- `error: "schema '<name>' not found"` — schema name doesn't exist.
- `error: "forge_edit accepts exactly one of artifact_path or key — ..."` — caller passed both.
- `error: "forge_edit requires artifact_path (markdown/dual shapes) or key (DB shapes)"` — caller passed neither.
- `error: "artifact not found: <path>"` — the markdown file does not exist on disk.
- `error: "append expects a single string value"` / `"... not a list"` — wrong shape for an append.
- `error: "composite key column '<col>' must be a string, got <value>"` — composite key entry has a non-string value.

## Response shape

```json
{ "edited": true, "field": "problem_statement", "edit_kind": "update", "artifact_path": "..." }
```

`artifact_path` is omitted on DB-key edits.

Requires `--signal-table-root`. DB-key edits also require `--db`.
