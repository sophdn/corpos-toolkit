# suggestion-read

## When to use

Use when you need the full content of a single suggestion — the compact projection from
`suggestion_list` omits problem_statement, acceptance_criteria, constraints, and
resolution_note to keep list responses small.

## When not to use

- Scanning a set of suggestions for triage — use `suggestion_list`
- Resolving the suggestion — use `suggestion_resolve`
- Reading a bug — use `bug_read` (separate entity, separate surface)

## Parameters

| Parameter | Type | Notes |
|---|---|---|
| `slug` | string | Identifier (slug or numeric id) |
| `id` | int64 | Alias of `slug` — id-by-default flows (suggestion_list → suggestion_read) skip a lookup hop |

## Response shape

The full Suggestion row, including:
- identity: `id`, `project_id`, `slug`, `title`
- content: `problem_statement`, `surface`, `priority`, `source`, `acceptance_criteria`, `constraints`
- lifecycle: `status`, `resolution_note`, `resolution_kind`
- routing: `routed_chain_slug`, `routed_task_slug`, `routed_bug_slug`
- audit: `resolved_commit_sha`, `tags`, `filed_at`, `resolved_at`, `updated_at`
