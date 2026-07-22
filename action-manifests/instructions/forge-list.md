# forge_list

## When to use

- Before calling `forge`, when you don't remember which fields a schema requires or what their types are. The response lists every field with its type and required-flag, so you can construct the `forge` call without guessing.
- To audit the registry of artifact shapes — orientation, schema review, or surfacing schemas with parse errors that would silently break `forge` calls.
- To check whether a draft schema has been promoted to stable — `is_draft=true` rows live under `drafts/` and are only returned with `include_drafts=true`.

## When not to use

- You already know the schema and the field set → call `forge` directly.
- You want to delete / edit an existing artifact → `forge_delete` / `forge_edit`.
- You want to list skills / tasks / chains / bugs (different lists, different tools).

## Behavior

- Reads `<root>/blueprints/forge-schemas/*.toml`, plus `drafts/` when `include_drafts=true`.
- Each schema row carries `name`, `prefix`, `output_dir`, `is_draft`, plus a `fields` array. Each field row has `name`, `type` (one of `string`, `string_list`, `optional_string`, `optional_string_list`, `string_or_list`, `optional_string_or_list`), `required` (true unless type starts with `optional_`), and `description`.
- A schema TOML that fails to parse — missing `[schema]` table, unknown field type, malformed TOML — lands in `parse_errors` rather than being silently dropped from the listing. Each parse-error row carries the source filename, draft flag, and the underlying error string.
- `schema_name` filter narrows to a single schema by exact name match. With no matching schema, both `schemas` and `parse_errors` are empty.
- Requires `--signal-table-root`. Without it, the response is `{ error: ... }`.

## Response shape

```json
{
  "schemas": [
    {
      "name": "task",
      "prefix": "TASK",
      "output_dir": "process-docs/tasks",
      "is_draft": false,
      "fields": [
        { "name": "chain_slug", "type": "string", "required": true, "description": "Parent chain slug…" }
      ]
    }
  ],
  "parse_errors": []
}
```
