// Package work serves the work meta-tool — task, chain, and bug
// lifecycle actions plus roadmap CRUD and discovery.
//
// ## Intended use
//
// **Workflow served:** agents and humans drive task / chain / bug
// lifecycles (start, complete, cancel, reopen, block / unblock, stamp
// SHA), read and edit the roadmap, and search tasks and bugs; this
// package implements every non-CRUD action on the work meta-tool.
//
// **Invocation pattern:** `work` meta-tool actions like `task_start`,
// `task_complete`, `bug_resolve`, `chain_close`, `roadmap_set`,
// `task_search`; each action's parameter spec lives in this package's
// actions_discovery catalog. The pseudo-action `__actions__` returns
// the whole catalog so cold callers can discover parameter shapes
// without trial and error.
//
// **Success shape:** per-action named result structs (TaskStartResult,
// BugResolveResult, etc.); every mutating action emits a typed event
// inside the same transaction via internal/events, so the events log
// has a row for each lifecycle change.
//
// **Non-goals:** does not own the initial-create CRUD path (internal/forge
// creates the rows; this package transitions them), not a notification
// system (internal/eventbus handles SSE broadcast), does not own
// roadmap planning UI — work serves the data; the dashboard renders it.
package work
