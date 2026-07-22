# forge_delete

## When to use

- You need to hard-delete an existing artifact: a pitch, a script, a role, a skill, a paper, etc. forge_delete removes the markdown file (and / or DB row for dual-stored shapes) using `forge_op_delete` from artifact-forge.
- For markdown-only shapes, pass `artifact_path` (the relative file path); forge_delete derives a Simple key from the file stem when `key` is omitted.
- For DB-backed shapes, pass `key` — string for simple keys, object for composite.

## When not to use

- **Task or chain shapes** — forge_delete refuses both. Use `cancel_task` for tasks (sets status='cancelled' without removing the row) and `close_chain` for chains (closes the lifecycle without orphaning task rows).
- Editing → `forge_edit`.
- Resolving / closing without removal → `bug_resolve`, `complete_task`, `close_chain`, etc.
- Creating → `forge`.

## Errors

- `error: "schema '<name>' not found"` — schema name doesn't exist.
- `error: "forge_delete refuses task shapes — use cancel_task ..."` — task shape passed.
- `error: "forge_delete refuses chain shapes — use close_chain ..."` — chain shape passed.
- `error: "forge_delete on DB-backed shape '<name>' requires `key` (columns: [...])"` — key missing on DB shape.
- `error: "forge_delete on markdown shape requires key or artifact_path"` — neither key nor path supplied for a markdown shape.
- `error: "key column '<col>' must be a string"` / `"key object missing required column '<col>'"` — composite key shape mismatch.

## Response shape

```json
{ "deleted": true, "schema": "pitch", "artifact_path_removed": "process-docs/pitches/PITCH_my-pitch_2026-04-28.md" }
```

`artifact_path_removed` is present when a markdown file was unlinked. `file_cleanup_error` is set when the DB row was removed but the markdown file couldn't be cleaned up (an inconsistency surface, not a hard failure).

Requires `--signal-table-root`. DB-backed shapes also require `--db`.
