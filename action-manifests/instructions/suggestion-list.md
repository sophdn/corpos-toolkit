# suggestion-list

## When to use

Use when you need a survey of suggestions matching a set of criteria:
- Session retro: "show all open high-priority suggestions"
- Area sweep: "what suggestions are tagged with the `arcreview` surface?"
- Audit: "list adopted suggestions so I can check the chain pointers"
- Date-bounded review: "suggestions filed since 2026-04-01"

## When not to use

- You need the full problem statement, AC, or constraints for a specific suggestion — use `suggestion_read`
- You want to adopt / defer / reject a suggestion — use `suggestion_resolve`
- You want to list observed-friction bugs — use `bug_list` (suggestions are forward-looking proposals; bugs are observed friction)

## Surface filter — multi-tag semantics

The stored `surface` field is a comma-separated tag list (e.g. `"arcreview,prompt-shape"`).
The `surface` parameter is itself comma-separated: each token is substring-matched against
the stored value via SQL LIKE. A suggestion matches if ANY query token matches:

- `surface="arcreview"` — finds `"arcreview"`, `"arcreview,prompt-shape"`, etc.
- `surface="arcreview,forge"` — finds suggestions tagged with either `arcreview` OR `forge`

## Parameters

| Parameter | Type | Default | Notes |
|---|---|---|---|
| `status` | string? | all | `open`, `adopted`, `deferred`, `rejected` |
| `priority` | string? | all | `low`, `medium`, `high` — NOT `severity` |
| `surface` | string? | all | comma-separated, any-match LIKE |
| `since` | string? | all | ISO 8601 date/datetime lower bound on `filed_at` |
| `verbose` | bool | false | Include problem_statement + AC + constraints |
| `all` | bool | false | Confirm a full-table dump when no project/filter passed |

## Response shape

```json
[
  {
    "id": 42,
    "slug": "add-fts5-coverage-to-roadmap-list",
    "title": "roadmap_list lacks FTS5 coverage other lists have",
    "status": "open",
    "surface": "roadmap,fts5",
    "priority": "medium",
    "filed_at": "2026-05-20T10:00:00",
    "resolved_at": null,
    "routed_bug_slug": ""
  }
]
```

Results are ordered by `filed_at DESC` (most recently filed first).

## Typical follow-up

After `suggestion_list`, use `suggestion_read` with a specific slug to read the full problem
statement, acceptance criteria, and constraints before deciding on adopted/deferred/rejected.
