# Read Task

## When to use

When you need the full content of a specific task before starting work,
reviewing requirements, or understanding what a task produces. Call this
at the start of any task-execution session to load the problem statement,
acceptance criteria, constraints, and expected handoff output.

Also useful for reviewing a completed task's handoff output before using
it as context for the next task.

## When not to use

- Do not use to browse available tasks — call `chain_status` or
  `get_chain_state` to see which tasks exist.
- Do not use to find tasks by keyword — call `search_task_content`.
- Do not use to change task state — call `start_task` or `complete_task`.

## Steps

Call `read_task` with the `task_slug` and `chain_slug`. Both are required;
the same task slug can appear in multiple chains.

Use `format: "structured"` (default) when you need the content fields
programmatically. Use `format: "markdown"` when you want a human-readable
rendering to include in a summary or handoff note.

A successful response contains all five content fields. An empty field
(`""` or `[]`) means that section was not populated when the task was
authored — this is expected for pending tasks that have not yet had their
context or handoff filled in.
