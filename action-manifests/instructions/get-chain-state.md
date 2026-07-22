# Get Chain State

## When to use

When you need to see the task table for a specific chain — which tasks exist,
their order, and their current status. Call this before picking up a chain to
understand what's pending, active, or closed, and to read the chain's
completion condition and design decisions.

Also useful mid-session to recheck progress after completing a task, or to
verify that the chain structure matches your expectations before planning
the next step.

Call with `include_context: true` only if a caller is checking for a Shared
context block — post-Phase-5 this is always an empty string, so omit it unless
wire-compatibility with older callers requires it.

## When not to use

- Do not use to survey all chains at once — call `chain_status` for a
  cross-chain summary with task counts.
- Do not use to read the full content of a specific task — call `read_task`
  for problem statement, AC, constraints, and handoff.
- Do not use to find a chain by fuzzy name — call `find_chain` first if you
  only have a rough slug.
- Do not use to search task content across chains — call `search_task_content`.

## Steps

Call `get_chain_state` with `chain_slug`. The response contains:

- `found` — `true` if the chain exists, `false` otherwise (check this first).
- `tasks` — array of `{ order, slug, status }` sorted by position.
- `completion_condition` — the chain's definition of done.
- `design_decisions` — recorded design choices that constrain task work.
- `output` — what the completed chain produces.
- `chain_path` — always `null` post-Phase-5 (chain markdown files retired).

A `found: false` response includes an `error` field describing why.
