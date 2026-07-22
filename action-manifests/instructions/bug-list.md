# bug-list

## When to use

Use when you need a survey of bugs matching a set of criteria:
- Triage session: "show all open high-severity bugs"
- Area sweep: "what bugs are tagged with the `library` surface?"
- Audit: "list routed bugs so I can check the chain pointers"
- Date-bounded review: "bugs filed since 2026-04-01"

## When not to use

- You need the full problem statement, AC, or constraints for a specific bug — use `bug_read`
- You want to resolve a bug — use `bug_resolve`
- You want the doc/source resolution ratio over a time window — use `bug_resolution_mix`
- You want to search task content for a keyword — use `search_task_content`

## Surface filter — multi-tag semantics

The stored `surface` field is a comma-separated tag list (e.g. `"seed-mcp,library,references"`).
The `surface` parameter is itself comma-separated: each token is substring-matched against
the stored value via SQL LIKE. A bug matches if ANY query token matches:

- `surface="library"` — finds `"library"`, `"seed-mcp,library"`, `"library,forge"`, etc.
- `surface="library,forge"` — finds bugs tagged with either `library` OR `forge`

## Parameters

| Parameter | Type | Default | Notes |
|---|---|---|---|
| `status` | string? | all | `open`, `fixed`, `wontfix`, `routed`, `dup` |
| `surface` | string? | all | comma-separated, any-match LIKE |
| `severity` | string? | all | `low`, `medium`, `high` |
| `since` | string? | all | ISO 8601 date/datetime lower bound on `filed_at` |
| `routed_only` | bool | false | shortcut for `status=routed` |
| `dirty_unstamped` | bool | false | resolved bugs where the fix SHA wasn't clean |
| `has_successor` | bool | false | parent bugs with a spawned follow-up |
| `is_successor` | bool | false | deferred-AC follow-up bugs |

## Response shape

```json
{
  "bugs": [
    {
      "slug": "...",
      "title": "...",
      "status": "open",
      "surface": "seed-mcp,library",
      "severity": "high",
      "filed_at": "2026-04-20T10:00:00",
      "resolved_at": null
    }
  ],
  "count": 1
}
```

Results are ordered by `filed_at DESC` (most recently filed first).

## Typical follow-up

After `bug_list`, use `bug_read` with a specific slug to read the full problem statement,
acceptance criteria, and constraints before deciding on a fix.
