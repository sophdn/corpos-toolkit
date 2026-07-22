# find-chain

## When to use

Use when you know part of a chain's name but not the exact slug:
- User says "find the port nav chain" or "which chain covers tier2 reads"
- You have a partial name fragment and need to discover the canonical slug
- You want to rank a set of candidate chains by relevance to a query

## When not to use

- You already know the exact chain slug — call `get_chain_state` directly
- You want all chains listed — use `chain_status`
- You want to search inside task content (problem statements, AC, etc.) — use `search_task_content`

## How scoring works

Query is tokenized on whitespace, hyphens, and underscores. Each chain is scored:

- **Base score:** `matched_tokens / total_tokens` (fraction of query tokens that appear as substrings in the slug)
- **+0.5 bonus** if the normalized slug exactly equals the normalized query
- **+0.25 bonus** if the slug starts with the normalized query

Results are ordered by score descending, then slug ascending as a tiebreaker.

## Parameters

- `query` — free-text phrase; hyphens and underscores tokenize like spaces
- `max_results` — default 10; clamp to at least 1
- `include_closed` — default true; set false to restrict to open chains only

## Typical follow-up

After `find_chain`, call `get_chain_state` with the top-ranked slug to see the chain's task table and prose.
