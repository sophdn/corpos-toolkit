package observehttp

import (
	"encoding/json"
	"net/http"
)

// BuildRouter wires the observe HTTP surface onto a stdlib ServeMux and
// wraps the result in permissive CORS. Endpoints land in subsequent
// commits; this scaffold registers /healthz plus the SSE /events handler
// from the event bus so a single port replaces the dual-server window.
func BuildRouter(state AppState) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	// /version exposes the binary's build-time GitSHA + BuiltAtUnix so
	// the dashboard can detect a daemon that's drifted behind the
	// source its bundle was built from (bug 1415). Independent of the
	// state.Pool gate below because the daemon's identity is
	// meaningful even when the DB-backed handlers are absent (smoke,
	// CLI-only deployments).
	mux.HandleFunc("GET /version", state.version)
	// Chain parse-context-lean-orienting T9: stdio MCP binary drift
	// state. Reads /tmp/toolkit-server-restart-needed + /proc; the
	// parse_context handler reads the same source via the shared
	// stdiodrift package without HTTP round-tripping. Mounted outside
	// the state.Pool gate because the snapshot is fs-only (no DB).
	mux.HandleFunc("GET /admin/stdio-drift-state", state.stdioDriftState)
	// arc-close-filing-review T5: HTTP MCP dispatch. Single POST route
	// dispatches to whichever surface's table is registered via
	// observehttp.RegisterDispatchTable (called from main after each
	// dispatch table is built). Shell hooks (e.g.
	// arc-close-filing-review-hook.sh) call this in lieu of stdio MCP.
	mux.HandleFunc("POST /mcp/{surface}", state.mcpDispatch)
	if state.Bus != nil {
		mux.Handle("GET /events", state.Bus.Handler())
	}
	if state.SpanTail != nil {
		mux.Handle("GET /events/spans", state.SpanTail.Handler())
	}
	if state.Pool != nil {
		mux.HandleFunc("GET /chains", state.chainsList)
		mux.HandleFunc("GET /chains/counts", state.chainsCounts)
		mux.HandleFunc("GET /chains/{slug}", state.chainsDetail)
		mux.HandleFunc("GET /tasks", state.tasksList)
		mux.HandleFunc("GET /tasks/counts", state.tasksCounts)
		mux.HandleFunc("GET /tasks/search", state.tasksSearch)
		mux.HandleFunc("GET /bugs", state.bugsList)
		mux.HandleFunc("GET /bugs/counts", state.bugsCounts)
		mux.HandleFunc("GET /suggestions", state.suggestionsList)
		mux.HandleFunc("GET /suggestions/counts", state.suggestionsCounts)
		mux.HandleFunc("GET /roadmap", state.roadmapList)
		mux.HandleFunc("GET /roadmap/diff", state.roadmapDiff)
		mux.HandleFunc("GET /projects", state.projectsList)
		// /inference endpoints (chain telemetry-substrate-cleanup T3).
		// The pre-T3 /inference/stats + /session-routing/stats endpoints
		// were retired in T3d; their dashboard surfaces are now subsumed
		// by /inference's per-task health cards + per-model breakdown.
		mux.HandleFunc("GET /inference/health-cards", state.inferenceHealthCards)
		mux.HandleFunc("GET /inference/sparklines", state.inferenceSparklines)
		mux.HandleFunc("GET /inference/retrieval-health", state.inferenceRetrievalHealth)
		// Per-tool-per-model ranking from proj_inference_tool_model_performance
		// (chain per-tool-per-model-observability T12).
		mux.HandleFunc("GET /inference/tool-model-performance", state.inferenceToolModelPerformance)
		mux.HandleFunc("GET /knowledge/index-card", state.knowledgeIndexCard)
		// chain memory-substrate-within-vault T8: telemetry for the
		// vault-mediated memory substrate (proj_memories + MemoryWritten
		// events). First-pass read ahead of the telemetry-unification push.
		mux.HandleFunc("GET /knowledge/memory-substrate", state.memorySubstrate)
		mux.HandleFunc("GET /benchmarks", state.benchmarksList)
		mux.HandleFunc("GET /benchmarks/timeseries", state.benchmarksTimeseries)
		mux.HandleFunc("GET /benchmarks/cards", state.benchmarksCards)
		mux.HandleFunc("GET /benchmarks/rubric-cards", state.benchmarksRubricCards)
		mux.HandleFunc("GET /benchmarks/tasks", state.benchmarksTasks)
		// study-run-persistence: corpos-lab behavioral-assay runs persisted
		// via the measure study_run_record action. /study-runs lists parent
		// rows (proj_study_runs); /study-runs/{run_id} returns the parent plus
		// its score grid (proj_study_run_scores).
		mux.HandleFunc("GET /study-runs", state.studyRunsList)
		mux.HandleFunc("GET /study-runs/{run_id}", state.studyRunDetail)
		// agent-substrate-frontend F2: event-ledger readers. The SSE
		// stream stays at GET /events (state.Bus.Handler above); the JSON
		// list endpoint lives at /events/list to avoid path collision.
		// See docs/SUBSTRATE_FRONTEND.md §2.4.
		mux.HandleFunc("GET /events/list", state.eventsList)
		mux.HandleFunc("GET /events/{event_id}", state.eventsDetail)
		mux.HandleFunc("GET /entities/{kind}/{slug}/events", state.entityEvents)
		// query-telemetry-substrate-frontend QF2: read-side telemetry
		// surface. See docs/TELEMETRY_FRONTEND.md §3.6 for placement.
		// Trajectory has two router rules: path-by-query_id is the
		// primary deep-link; the {span_id}-query-param form supports
		// span-tail and span-detail panel cross-links.
		mux.HandleFunc("GET /telemetry/trajectories/{query_id}", state.telemetryTrajectoryByID)
		mux.HandleFunc("GET /telemetry/trajectories", state.telemetryTrajectoryBySpan)
		mux.HandleFunc("GET /telemetry/analytics/volume-by-source", state.telemetryVolumeBySource)
		mux.HandleFunc("GET /telemetry/analytics/success-rate", state.telemetrySuccessRate)
		mux.HandleFunc("GET /telemetry/training-pairs", state.telemetryTrainingPairs)
		mux.HandleFunc("GET /telemetry/training-pairs/stats", state.telemetryTrainingPairsStats)
		// arc-close-snapshot-corpus-capture T6: snapshot-corpus readiness
		// telemetry (arcreview_snapshot_corpus). Read-only aggregate.
		mux.HandleFunc("GET /telemetry/snapshot-corpus/stats", state.arcCorpusStats)
		// reference-resolution-substrate-frontend RF2: Context Pull
		// Inspector readers. See docs/REFERENCE_RESOLUTION_FRONTEND.md
		// §3 for the endpoint catalog. The /by-entity/{kind}/{slug}
		// endpoint is mounted under /context-pulls/by-entity/ rather
		// than /entities/{kind}/{slug}/context-pulls to keep the
		// surface clustered (the inspector reads its own scoping
		// affordances under /context-pulls/*) — the agent-substrate
		// /entities/{kind}/{slug}/events convention is the timeline
		// pattern, not the cross-substrate scoping convention.
		mux.HandleFunc("GET /context-pulls", state.contextPullsList)
		mux.HandleFunc("GET /context-pulls/stats", state.contextPullsStats)
		mux.HandleFunc("GET /context-pulls/stats/timeseries", state.contextPullsStatsTimeseries)
		mux.HandleFunc("GET /context-pulls/by-entity/{kind}/{slug}", state.contextPullsByEntity)
		mux.HandleFunc("GET /context-pulls/{grounding_event_id}", state.contextPullsDetail)
		// agent-substrate-frontend F5: dispatch policy peek. Reads fresh
		// from disk; reload-from-disk is implicit (no caching at this
		// layer).
		mux.HandleFunc("GET /admin/dispatch-policy", state.dispatchPolicy)
		// Bug 1452: parse-context-first-call reflex compliance surface.
		// Answers "what fraction of recent user prompts didn't fire
		// parse_context?" from grounding_events.prompt_id stamped by
		// the Stop-hook post-session processor. Visibility-only — the
		// metric flags when the reflex is silently degrading without
		// blocking anything.
		mux.HandleFunc("GET /admin/parse-context-skip-rate", state.parseContextSkipRate)
		// action-docs-corpus-frontend AF2: per-action documentation
		// browse. Serves the startup-loaded actiondocs.Registry by
		// default; ?reload=1 forces a fresh disk load for that response.
		mux.HandleFunc("GET /admin/action-docs", state.actionDocs)
	}
	return withCORS(mux)
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// writeJSON encodes v as JSON and writes it with the given status. If
// encoding fails, the response is 500 with a generic error body — the
// only way encode can fail here is a non-serialisable type, which is a
// programmer error worth surfacing.
//
// Generic over T so call sites pass concrete typed payloads (every
// caller in this package does). The `any` constraint is the structural
// JSON-encoding boundary (`json.Encoder.Encode(any)`) — same shape as
// `dispatch.Adapt[T any]` from chain T1. Per the typed-returns reference
// (vault `reference/2026-05-15_go-mcp-dispatch-typed-returns-pattern.md`),
// generic constraints are the right way to name this boundary; nolint
// comments are explicitly disallowed.
func writeJSON[T any](w http.ResponseWriter, status int, v T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, `{"error":"encode"}`, http.StatusInternalServerError)
	}
}
