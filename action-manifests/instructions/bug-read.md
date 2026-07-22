# bug_read

## When to use

Call `bug_read` when you have a specific bug slug and need its full detail:
problem statement, acceptance criteria, constraints, source, severity, surface,
status, resolution note, and routed chain/task pointers.

Typical triggers:
- After `bug_list` returned a slug and you need the full context to investigate.
- When a task or handoff references a known bug slug and you need to understand it.
- When a routed bug's chain/task pointers need verification.
- When reviewing a resolved bug's resolution note or commit SHA.

## When not to use

- **Browsing or filtering bugs** — use `bug_list` with status/surface/severity filters.
- **Resolving or reopening a bug** — use `bug_resolve` or `bug_reopen`.
- **Finding bugs by keyword** — use `bug_list` with `surface` filter; `bug_read`
  requires an exact slug.

## Response shape

Successful lookup returns a flat JSON object with all fields:

- `slug`, `title`, `problem_statement`, `acceptance_criteria`, `constraints`,
  `source`, `severity`, `surface`, `status`
- `resolution_note`, `routed_chain_slug`, `routed_task_slug` — empty strings
  when the bug is open or not routed
- `filed_at` — ISO-8601 filing timestamp
- `resolved_at`, `resolved_commit_sha`, `resolved_dirty` — null for open bugs;
  populated when the bug was resolved
- `spawned_successor_slug`, `recurrence_candidates`, `resolution_kind` — null
  unless the deferred-AC or recurrence gates fired at resolve time

Not-found returns `{ "error": "bug '<slug>' not found" }`.
