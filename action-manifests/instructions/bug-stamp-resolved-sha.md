# Bug Stamp Resolved SHA

## When to use

When a previously-resolved bug's `resolved_commit_sha` needs
backfilling — typically because `bug_resolve` ran against a dirty
working tree (the resolution happened, but the SHA at that moment
didn't yet contain the fix). Calling this with the post-commit SHA
corrects the audit row so `bug_list(status=fixed)` reports a SHA
that actually contains the fix.

Also clears `resolved_dirty` (sets it to 0) — the implication being
that the freshly-named SHA is from a clean commit.

## When not to use

- The bug is currently `open` — only resolved bugs carry a
  resolved_commit_sha. Resolve via `bug_resolve` first.
- You want to rescind the whole resolution — `bug_reopen` clears
  the stamp by setting it to NULL.
- The resolution kind itself was wrong — `bug_reopen` + a fresh
  `bug_resolve` with the right kind.

## Steps

Call `bug_stamp_resolved_sha(slug, commit_sha)`. The handler
rejects open bugs.

A successful response is `{ "stamped": true, "slug": "<slug>",
"commit_sha": "<sha>" }`.

A failure response is `{ "stamped": false, "error": "<reason>" }`.

## What this writes

- `resolved_commit_sha = <sha>`
- `resolved_dirty = 0`

No other fields touched. This is a single-column corrective write.
The handler does NOT validate the SHA against the repository
(diverges from seed-mcp's port-equivalent); validate before
stamping if needed.
