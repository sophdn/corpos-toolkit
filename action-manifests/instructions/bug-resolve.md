# Bug Resolve

## When to use

When closing an open bug — pick one of four resolution kinds:
- `fixed` — a commit landed that satisfies the AC; pass
  `resolved_commit_sha` to stamp the commit (or call
  `bug_stamp_resolved_sha` later to backfill).
- `wontfix` — out of scope or rejected. `note` should record the
  rationale (not enforced at the DB layer; convention).
- `dup` — duplicate of another bug. `note` is REQUIRED and should
  name the target bug slug.
- `routed` — rolled into a chain task. `routed_chain_slug` and
  `routed_task_slug` are both REQUIRED.

## When not to use

- Use `bug_reopen` to rescind a prior resolution.
- Use `bug_stamp_resolved_sha` to backfill the commit SHA after the
  fix lands (keeps the resolution intact).
- Use `forge(bug, …)` to file a new bug.

## Commit ordering

**Always commit before calling `bug_resolve`.** The correct sequence is:

1. Apply the fix
2. `git commit` — SHA is now available
3. `bug_resolve(…, resolved_commit_sha = <sha>)` — stamp inline

`bug_stamp_sha` exists only as a correction path (e.g. when a prior
session resolved before committing). It is not the default flow.
Resolving before committing means the SHA cannot be stamped inline and
requires a separate pass after the fact.

## Steps

Call `bug_resolve(slug, kind, note?, routed_chain_slug?,
routed_task_slug?, resolved_commit_sha?)`. The handler validates per
the rules above before writing. Only `open` bugs can be resolved —
repeat-close attempts return `{ resolved: false, error: …only open
bugs… }`.

A successful response is `{ "resolved": true, "slug": "<slug>",
"kind": "<kind>" }`.

A failure response is `{ "resolved": false, "error": "<reason>" }`.

## What this writes

Inside one transaction, on the bug row:
- `status = <kind>`
- `resolution_note = <note or "">`
- `routed_chain_slug = <chain or "">`
- `routed_task_slug = <task or "">`
- `resolved_at = datetime('now')`
- `resolved_commit_sha = <sha or NULL>`
- `resolved_dirty = 0`

Diverges from seed-mcp's port-equivalent: drops the patch-confession
gate, deferred-AC auto-forge, recurrence-acknowledged escalation,
and dirty-tree warning. Those are bug-resolve-discipline mechanisms
that layer on top of the base mutation; they belong in a
higher-level discipline chain.
