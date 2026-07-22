# chain-status

## When to use

Use when you need a project-wide overview of chain health:
- User asks how many chains exist or what the overall project status is
- You need to pick a chain to work on but don't know which chains are active
- You want to see aggregate task completion across all chains before starting a session
- You need to check whether a chain is open or closed without knowing its exact slug

## When not to use

- You need the task table for a specific chain — use `get_chain_state` instead
- You want to fuzzy-match a chain by name — use `find_chain` instead
- You need task content — use `read_task` instead
- You want to search task fields for a keyword — use `search_task_content` instead

## What you get

`chain_status` returns a `chains` array ordered by `updated_at` descending. Each entry has:
- `slug` — the chain identifier
- `status` — `open`, `closed`, or `retired`
- `tasks_total` / `tasks_pending` / `tasks_closed` / `tasks_cancelled` — task count breakdown
- `updated_at` — ISO-8601 timestamp of last state change

And a `total_chains` integer at the top level.

## Typical follow-up

After `chain_status`, you typically call `get_chain_state` with the slug of the chain you want to inspect in detail.
