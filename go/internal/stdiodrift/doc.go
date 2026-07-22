// Package stdiodrift detects the case where a stdio toolkit-server
// process is running an older binary than the on-disk artifact and
// surfaces a typed report. Two distinct drift kinds live under one
// roof:
//
//   - stdio_fd_pinned: Linux holds the binary's fd open inside the
//     stdio process even after the post-commit advisor replaces the
//     file on disk (bug 1322's preserve-stdio policy is load-bearing —
//     killing the process would drop in-flight tool calls). The
//     process keeps executing OLD code until /mcp reconnect. Detected
//     via /proc/<pid>/exe ending in " (deleted)".
//
//   - compile_time_stale: the running binary's compile-time gitSHA
//     no longer matches the git HEAD that produced the on-disk
//     artifact. Detected via comparing the per-process gitSHA report
//     against `git rev-parse HEAD`. Subsumes the surface that
//     commit 41ff355 (advisor: rebuild when restarting) addressed —
//     the diagnostic stays so future drift sources don't conflate.
//
// ## Intended use
//
// **Workflow served:** Surfaces "the stdio MCP binary your agent is
// talking to is older than HEAD" — the structurally-invisible gap
// CLAUDE.md §"Agent-side staleness check" documents as a manual
// ritual. parse_context's discipline_skill surface consumes it on
// every prompt; the /admin/stdio-drift-state HTTP endpoint exposes
// the same state to dashboards and external tooling. Chain
// parse-context-lean-orienting T9.
//
// **Invocation pattern:** Snapshot(ctx, SnapshotInputs{RepoRoot,
// OnDiskGitSHA, ...}) returns a typed State the caller can serialize
// or evaluate. Pure-Go and dependency-free aside from stdlib +
// os/exec; safe to call from any process that needs the diagnostic.
//
// **Success shape:** State with DriftDetected=false on the dominant
// happy path (no marker, no preserved stdio); DriftDetected=true
// with one StdioProcess entry per preserved PID when the
// post-commit advisor has flagged drift. Snapshot errors are
// best-effort (the empty-marker case is not an error).
//
// **Non-goals:** Does NOT kill stdio processes (bug 1322's
// preserve rule is load-bearing); does NOT mutate the marker file
// (the post-commit advisor owns writes); does NOT subsume the
// harness-level TaskCreate/TodoWrite over-firer reminders (different
// layer — see PARSE_CONTEXT.md §13.8).
package stdiodrift
