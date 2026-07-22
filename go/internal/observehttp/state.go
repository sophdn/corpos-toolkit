// Package observehttp implements the HTTP observe surface that the
// dashboard reads from: /chains, /tasks, /bugs, /benchmarks,
// /emotive, /tool-health, /events, /projects, /roadmap,
// /knowledge/index-card, /inference/health-cards,
// /inference/sparklines, /inference/retrieval-health.
//
// Port-by-port replacement for crates/observe-http. Handlers are
// read-only views over the canonical toolkit.db; no writes happen here.
package observehttp

import (
	"toolkit/internal/actiondocs"
	"toolkit/internal/db"
	"toolkit/internal/eventbus"
	"toolkit/internal/obs"
)

// AppState bundles the pool and event bus that handlers need.
// Mirrors observe_http::AppState in crates/observe-http/src/lib.rs.
//
// SpanTail is optional — when non-nil, BuildRouter mounts /events/spans
// to stream observability spans (span_open / span_close events emitted
// by [obs.SpanStart] and persisted via [obs.DBSpanSink]) to dashboard
// subscribers. The tail reads from the shared span_events table, so the
// stream covers every toolkit-server process writing to the same DB —
// stdio MCPs included. Nil means no span stream is exposed (CLI-only
// deployments, smoketests, etc.).
//
// DispatchPolicyPath is the absolute path to action-manifests/dispatch-policy.toml
// resolved at server startup; the /admin/dispatch-policy handler reads
// the file fresh on each request so an admin.schema_reload (or any
// edit) shows up without server restart. Empty string disables the
// endpoint (it returns 503).
//
// ActionDocs is the startup-loaded per-action documentation corpus
// surfaced through /admin/action-docs. Shared with admin.action_describe
// (MCP) — the same in-process registry serves both consumers. Nil is
// acceptable: the handler returns a zero-value response with the
// dashboard rendering "no docs loaded".
//
// ActionDocsDir is the absolute path of the corpus directory used to
// load ActionDocs. Surfaced in the response (corpus_path field) and
// consumed by the handler's ?reload=1 path to re-read the corpus
// fresh from disk without a server restart.
// GitSHA / BuiltAtUnix / PackageVer carry the build-time identity of
// the running binary, ldflags-injected by go/Makefile. Surfaced
// through GET /version (bug 1415) so the dashboard can detect a daemon
// that's drifted behind the source the dashboard bundle was built
// from. Zero / "unversioned" values are tolerated — the /version
// endpoint returns whatever is set; the dashboard's banner gate
// decides whether to treat the absence as a mismatch.
type AppState struct {
	Pool               *db.Pool
	Bus                *eventbus.Bus
	SpanTail           *obs.SpanTail
	DispatchPolicyPath string
	ActionDocs         *actiondocs.Registry
	ActionDocsDir      string
	GitSHA             string
	BuiltAtUnix        int64
	PackageVer         string
	// RepoRoot is the toolkit-server checkout used as the working
	// directory for git invocations (chain parse-context-lean-orienting
	// T9: `git rev-parse HEAD` for the /admin/stdio-drift-state
	// endpoint). Empty string means "use the process cwd" — the
	// snapshot helper degrades to leaving HeadSHA empty if the cwd
	// isn't a repo.
	RepoRoot string
	// DriftMarkerPathOverride / DriftProcRootOverride mirror the
	// HandlerDeps fields for tests. Production callers leave both
	// empty so stdiodrift.MarkerPath ("/tmp/toolkit-server-restart-needed")
	// and "/proc" win.
	DriftMarkerPathOverride string
	DriftProcRootOverride   string
}
