# search-task-content

## When to use

Use when you need to find tasks by content rather than by slug:
- User says "find all tasks that mention search_task_content", "which tasks reference the old API", "sweep for CHAIN_ tokens"
- You need to locate a stale reference across many tasks without knowing which chain it's in
- You want to discover cross-chain patterns — e.g., all tasks with a common constraint or shared keyword
- You're hunting for a term in acceptance criteria, handoff output, constraints, or context

## When not to use

- You already know the chain slug — use `get_chain_state` to see the task table directly
- You know the exact task slug — use `read_task` to fetch its full content
- You want to find chains by approximate name — use `find_chain` for slug discovery
- You want a project overview — use `chain_status`

## Parameters

- `pattern` — required; substring to search for (case-insensitive, SQL LIKE %pattern%)
- `fields` — optional; restrict to a subset of `problem_statement`, `context_required`, `acceptance_criteria`, `constraints`, `handoff_output`. Omit to search all five
- `chain_status` — optional; `open` or `closed` to filter by chain lifecycle
- `task_status` — optional; `pending`, `active`, `closed`, `cancelled`, `blocked`
- `chain_slug` — optional; restrict to one chain
- `max_results` — optional; default 100. One task can produce multiple rows (one per matching field)

## Response shape

```json
{
  "count": 3,
  "truncated": false,
  "pattern": "foo",
  "matches": [
    {
      "chain_slug": "work-port-tier2-reads",
      "chain_status": "open",
      "task_slug": "port-search-task-content",
      "task_status": "active",
      "field": "problem_statement",
      "snippet": "…Port foo to work-server end-to-end…"
    }
  ]
}
```

`truncated=true` means results were capped at `max_results`; increase the limit or add filters.

## Typical follow-up

After `search_task_content`, call `read_task` with the returned `task_slug` + `chain_slug` to read the full content of an interesting match. Call `get_chain_state` with the `chain_slug` to see surrounding task context.
