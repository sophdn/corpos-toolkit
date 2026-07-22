# Bug Reopen

## When to use

When a previously-resolved bug needs to come back to `open` — the
bug recurred, the wontfix decision was reversed, the dup target was
wrong, or the routed chain didn't actually cover the work.

## When not to use

- The bug is already `open` — `bug_reopen` rejects open bugs.
- The resolution stands but the SHA is wrong — use
  `bug_stamp_resolved_sha` instead. Reopening clears the SHA stamp.
- You want a fresh bug — use `forge(bug, …)`.

## Steps

Call `bug_reopen(slug)`. The handler validates the bug is currently
non-open before flipping.

A successful response is `{ "reopened": true, "slug": "<slug>" }`.

A failure response is `{ "reopened": false, "error": "<reason>" }`.

## What this writes

- `status = 'open'`
- `resolved_at = NULL`
- `resolved_commit_sha = NULL`
- `resolved_dirty = NULL`

The `resolution_note` and routed pointers are preserved on the row
so reopen history is recoverable; subsequent `bug_resolve` calls
overwrite them.

No reason annotation is written (distinct from task `reopen_task`
which prepends `[REOPENED YYYY-MM-DD: <reason>]` to handoff_output).
