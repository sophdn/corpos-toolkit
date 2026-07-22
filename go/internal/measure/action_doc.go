package measure

// action_doc.go is the descriptor-registry seam for the measure surface's action
// docs (chain migrate-measure-action-docs-to-derive-contract — the third
// per-surface instantiation of the contract established on work, after
// knowledge). It is the single source of the measure surface's action docs: each
// param's TYPE is DERIVED from the handler's typed param struct where one exists,
// and only the irreducible semantics (purpose, param name-list/order/required/
// description, errors, notes, envelope-requirements, examples, returns) are
// authored in a co-located Go descriptor.
//
// measure is a mostly MAP-BOUND surface — the knowledge audit's "verify the
// binding style first; the all-typed premise is false" warning (bug 935) held.
// TWO actions are struct-backed and derive their param types (ParamStruct set,
// authored Type left empty — gate-enforced by
// TestMeasureRegistryDerivedParamsHaveEmptyAuthoredType): bench_run (benchRunParams)
// and benchmark_replay (benchmarkReplayParams, hoisted from an inline struct in
// finalize-action-docs-epic T4). The classify_* family + benchmark_query bind via
// the mcpparam helpers (mcpparam.String/Int64Opt) and benchmark_record binds via
// the tolerant parseBenchmarkResult map — these author their param Types with
// ParamStruct == nil (the forge-family pattern from docs/ACTION_DOC_CONTRACT.md),
// and their authored types are pinned against what the handler reads by
// TestActionDocParamTypes_MeasureMapBoundBinderParity.
//
// The generated corpus + admin.action_describe(measure, X) derive from this
// registry via MeasureActionSpecs(); byte-parity is pinned by the T1
// characterization net (internal/actiondocs/surface_contract_net_test.go). The
// enumerated blessed deltas the derive/doc-completeness introduces are
// bench_run.override_flags object→object[] (the []benchFlagPairCLI slice; the
// batch.ops-class correction) and the benchmark_record/query/replay param
// documentation added in finalize-action-docs-epic T4 (bug 940). See
// docs/ACTION_DOC_CONTRACT.md.

import (
	"reflect"

	"toolkit/internal/actionspec"
)

// ── Benchmark CRUD ──
//
// benchmark_record / benchmark_query / benchmark_replay now document the params
// their handlers actually read (chain finalize-action-docs-epic T4, bug 940 — a
// doc-completeness expansion, the enumerated behavior change the migration
// deliberately scoped out). The required flags match handler enforcement:
// parseBenchmarkResult collects the missing required keys into one error;
// benchmark_replay's `if p.RowID == ""` guard rejects a missing row_id.
//
// benchmark_record is map-bound (parseBenchmarkResult, ParamStruct == nil) — its
// tolerance (a non-string value for a required key is treated as absent; int
// columns accept int OR bool) is deliberate, so it stays authored rather than
// becoming a strict json.Unmarshal target (the forge-family reasoning). The four
// *_score columns are float64 — authored `float`; the spec vocabulary has no
// numeric scalar token (SpecType collapses float64→object), so for an AUTHORED
// map-bound param the precise `float` token is used rather than the lossy `object`
// (see docs/ACTION_DOC_CONTRACT.md §spec-vocabulary). benchmark_record/query's
// authored string/integer types are pinned against the handler binders by
// TestActionDocParamTypes_MeasureMapBoundBinderParity.

var benchmarkRecordDoc = actionspec.ActionDoc{
	Purpose: "Record a benchmark result row. Captures the provenance bundle (scenario/tool/model identity, timing, outcome flags, optional rubric scores) used by benchmark_replay to reconstruct the run later. project comes from the dispatch envelope, NOT params.",
	Params: []actionspec.DocParam{
		// Required (parseBenchmarkResult collects every missing key into one error).
		{Name: "scenario_id", Required: true, Type: "string", Description: "Scenario identifier the run exercised."},
		{Name: "tool_name", Required: true, Type: "string", Description: "Name of the tool/surface under benchmark."},
		{Name: "model_name", Required: true, Type: "string", Description: "Model that produced the run (e.g. the Qwen-local or Claude-remote id)."},
		{Name: "run_at", Required: true, Type: "int64", Description: "Unix-seconds timestamp of the run. Accepts a JSON integer or boolean (boolean-flavoured INTEGER column)."},
		{Name: "wall_clock_ms", Required: true, Type: "int64", Description: "End-to-end wall-clock latency in milliseconds. Accepts a JSON integer or boolean."},
		{Name: "invocation_ok", Required: true, Type: "int64", Description: "1/0 (or true/false) — whether the invocation itself succeeded (transport/parse OK), independent of answer quality."},
		{Name: "provenance_id", Required: true, Type: "string", Description: "benchmark_provenance(id) joining this result to its replayable provenance row. Required-by-trigger after migration 035 (T6 cutover); a missing value is surfaced as a structured param error rather than a trigger-text failure at INSERT."},
		// Optional identity / timing / token counts.
		{Name: "id", Required: false, Type: "string", Description: "benchmark_results UUID. Auto-generated (v4) when omitted."},
		{Name: "run_id", Required: false, Type: "string", Description: "Caller-supplied run grouping id; falls back to the row id when absent."},
		{Name: "input_tokens", Required: false, Type: "int64", Description: "Prompt token count, when measured."},
		{Name: "output_tokens", Required: false, Type: "int64", Description: "Completion token count, when measured."},
		{Name: "invoked_contextually", Required: false, Type: "int64", Description: "1/0 (or true/false) — whether the tool was invoked in-context vs. forced. Defaults to 1 when omitted."},
		{Name: "args_match", Required: false, Type: "int64", Description: "1/0 (or true/false) — whether the extracted args matched the expected args."},
		{Name: "interpretation_ok", Required: false, Type: "int64", Description: "1/0 (or true/false) — whether the model interpreted the task correctly."},
		{Name: "extracted_args", Required: false, Type: "string", Description: "The args the model actually produced (free-form, for diffing against expectations)."},
		{Name: "detected_tool", Required: false, Type: "string", Description: "The tool the model selected, when the scenario tests tool selection."},
		{Name: "notes", Required: false, Type: "string", Description: "Free-form notes about the run."},
		{Name: "task_shape", Required: false, Type: "string", Description: "Task-shape tag for grouping results across scenarios."},
		{Name: "accuracy_score", Required: false, Type: "float", Description: "Optional rubric accuracy score (float, typically 0.0–1.0)."},
		{Name: "honesty_score", Required: false, Type: "float", Description: "Optional rubric honesty score (float, typically 0.0–1.0)."},
		{Name: "ranking_quality_score", Required: false, Type: "float", Description: "Optional ranking-quality score (float, typically 0.0–1.0)."},
		{Name: "within_budget_score", Required: false, Type: "float", Description: "Optional within-budget score (float, typically 0.0–1.0)."},
	},
}

// study_run_record is map-bound (parseStudyRunInput, ParamStruct == nil): it
// tolerantly unmarshals the posted run record and flattens the nested verdict
// object, so its param types are authored rather than derived. rows is
// documented `object[]` (the score grid) and materials_hashes `object` (the
// filename→sha256hex map); neither is a gated primitive family, so the
// map-bound binder-parity gate skips them.
var studyRunRecordDoc = actionspec.ActionDoc{
	Purpose: "Persist one corpos-lab behavioral-assay study run (parent provenance record + N child score-grid rows). Emits a StudyRunRecorded event whose fold writes proj_study_runs + proj_study_run_scores; on commit publishes an artifact_created SSE event so the dashboard refreshes live. Stores hashes/pointers only — responses_dir is a filesystem path, materials_hashes are SHA-256 digests; raw responses stay on disk. project comes from the dispatch envelope, NOT params.",
	Params: []actionspec.DocParam{
		// Required (parseStudyRunInput collects every missing key into one error).
		{Name: "name", Required: true, Type: "string", Description: "Human-facing run name (e.g. 'casg-direct-v3-smoke')."},
		{Name: "assay", Required: true, Type: "string", Description: "The behavioral assay exercised (e.g. 'grounded-glyph-probe'). Primary dashboard filter."},
		{Name: "status", Required: true, Type: "string", Description: "Terminal status — 'completed' or 'failed'. Any other value is rejected with a structured param error."},
		{Name: "run_at", Required: true, Type: "string", Description: "RFC 3339 (UTC) timestamp of the run. Stored verbatim; the observe surface orders by run_at DESC."},
		// Score grid.
		{Name: "rows", Required: false, Type: "object[]", Description: "Score grid — one entry per condition×run cell. Each carries item, condition, run (index), rationale, and a verdict: EITHER flat (verdict_kind/verdict_reason) OR nested ({verdict:{kind,reason}}) — the handler flattens the nested form."},
		// Optional identity / provenance.
		{Name: "run_id", Required: false, Type: "string", Description: "Stable run id (entity slug + parent primary key). Auto-generated (UUIDv4) when omitted."},
		{Name: "item_id", Required: false, Type: "string", Description: "Study item / scenario id (e.g. 'casg-direct')."},
		{Name: "image", Required: false, Type: "string", Description: "Container image reference the run executed under."},
		{Name: "image_digest", Required: false, Type: "string", Description: "Immutable image digest (sha256:...) pinning the image bytes."},
		{Name: "error", Required: false, Type: "string", Description: "Failure detail; set when status='failed'."},
		{Name: "study_digest", Required: false, Type: "string", Description: "SHA-256 (hex) digest of study.json."},
		{Name: "materials_hashes", Required: false, Type: "object", Description: "Map of material filename → SHA-256 hex digest. Small hashes, NOT blobs."},
		{Name: "model_id", Required: false, Type: "string", Description: "Model identifier the run used (e.g. 'Qwen2.5-32B-Instruct-Q4_K_M.gguf')."},
		{Name: "model_version", Required: false, Type: "string", Description: "Model version / quantization tag (e.g. 'q4km')."},
		{Name: "responses_dir", Required: false, Type: "string", Description: "On-disk POINTER to the raw responses directory. Path only — blobs never enter the ledger."},
	},
	Returns: &actionspec.ActionReturn{
		Shape:       "StudyRunRecordResult",
		Description: "On success: {run_id, status}. On param error: {error} naming the missing/invalid params.",
	},
	Examples: []actionspec.ActionExample{
		{Description: "Persist a completed smoke run with a two-cell score grid", Call: `{"action":"study_run_record","project":"corpos-lab","params":{"name":"casg-direct-v3-smoke","assay":"grounded-glyph-probe","item_id":"casg-direct","status":"completed","run_at":"2026-07-09T00:33:10Z","model_id":"Qwen2.5-32B-Instruct-Q4_K_M.gguf","model_version":"q4km","responses_dir":"/abs/path/out/responses","rows":[{"item":"casg-direct","condition":"baseline","run":1,"verdict_kind":"fail","verdict_reason":"","rationale":"…"},{"item":"casg-direct","condition":"glyph_only","run":1,"verdict_kind":"pass","verdict_reason":"","rationale":"…"}]}}`},
	},
}

// gate_run + gate_trend are struct-backed (their params derive from
// gateRunParams / gateTrendParams via reflect.TypeOf in the registry), so the
// descriptors author order + descriptions + returns while Types derive. gate_run
// REUSES the corpos-gate CLI core (internal/gate) — same gate.Run over the
// repo's gate.yml — so its verdict is identical to `corpos-gate run`; the run is
// persisted as an event-sourced trend row (proj_gate_runs) that gate_trend reads
// back. See gate_run.go.
var gateRunDoc = actionspec.ActionDoc{
	Purpose: "Run corpos-gate over a repo (loads its gate.yml, runs the reused internal/gate core for the tier) and return the aggregated verdict: overall_ok + per-check results + parsed coverage/mutation. The verdict is IDENTICAL to `corpos-gate run --tier=<tier>` on the same repo/commit (same core, not a fork). Persists the run as EVENT-SOURCED trend data (a GateRunCompleted event → proj_gate_runs + proj_gate_check_results) so coverage/mutation/verdict become a time series per project, read back via gate_trend. Persistence is ADDITIVE + DB-optional: if no DB is available or no project is supplied, the verdict is still returned with persisted=false and a note (a gate run must work with storage unavailable, e.g. in CI).",
	Params: []actionspec.DocParam{
		{Name: "repo_dir", Required: true, Description: "Absolute (or CWD-relative) path to the repo root — the directory holding gate.yml. The gate runs against this tree."},
		{Name: "tier", Required: false, Description: "Gate tier to run: 'pre-commit', 'pre-push', or 'ci' (tiers are a superset: ci ⊃ pre-push ⊃ pre-commit). Defaults to 'pre-push' when omitted."},
		{Name: "project", Required: false, Description: "Project the trend row is recorded under. Falls back to the dispatch envelope's project. When empty (and none in the envelope), the verdict is still returned but the trend row is skipped (persisted=false)."},
		{Name: "commit_sha", Required: false, Description: "The commit SHA the gate ran against, stored verbatim on the trend row so it can be joined back to a revision. Empty for a dirty / uncommitted working-tree run."},
	},
	Returns: &actionspec.ActionReturn{
		Shape:       "GateRunResult",
		Description: "Always carries the verdict: overall_ok, tier, per-check checks[] (name/tier/ok/skipped/duration_ms/note), coverage_pct, branch_pct, mutation_score (-1 = N/A), duration_ms. persisted reports whether a trend row was written; persist_note explains a skip/failure. error is set only on a param error or a gate-core infra failure (missing gate.yml, a check that could not run) — a check that merely FAILS is a verdict (overall_ok=false), not an error.",
	},
	Examples: []actionspec.ActionExample{
		{Description: "Run the pre-push gate over a repo and record the trend row", Call: `{"action":"gate_run","project":"corpos-toolkit","params":{"repo_dir":"/home/user/dev/corpos-toolkit","tier":"pre-push","commit_sha":"95eeca1"}}`},
	},
}

var gateTrendDoc = actionspec.ActionDoc{
	Purpose: "Read the corpos-gate trend for a project from proj_gate_runs — the coverage / mutation / verdict time series recorded by gate_run, most-recent runs first (ran_at DESC). Each point carries ran_at, commit_sha, tier, overall_ok, coverage_pct, and mutation_score.",
	Params: []actionspec.DocParam{
		{Name: "project", Required: true, Description: "Project whose gate trend to read. Falls back to the dispatch envelope's project."},
		{Name: "metric", Required: false, Description: "Optional filter: 'coverage' returns only runs with a coverage metric, 'mutation' only runs with a mutation score, 'verdict' the full series (default when omitted). Any other value is a param error."},
		{Name: "limit", Required: false, Description: "Cap the number of runs returned (most recent first). No limit when omitted or <= 0."},
	},
	Returns: &actionspec.ActionReturn{
		Shape:       "GateTrendResult",
		Description: "Carries project, metric (echo), and points[] — each {ran_at, commit_sha, tier, overall_ok, coverage_pct, mutation_score} ordered most-recent first. On param error: error names the missing/invalid param.",
	},
	Examples: []actionspec.ActionExample{
		{Description: "Read the last 20 gate runs for a project", Call: `{"action":"gate_trend","project":"corpos-toolkit","params":{"limit":20}}`},
		{Description: "Read only runs that carry a coverage metric", Call: `{"action":"gate_trend","project":"corpos-toolkit","params":{"metric":"coverage"}}`},
	},
}

var benchmarkQueryDoc = actionspec.ActionDoc{
	Purpose: "Return previously-recorded benchmark result rows, with optional filters. Each classify_* call writes one row implicitly, so this is the entry point for inspecting what was classified, when, and what came back. project comes from the dispatch envelope (empty project = cross-project); all params below are optional filters.",
	Params: []actionspec.DocParam{
		{Name: "tool_name", Required: false, Type: "string", Description: "Filter to rows whose tool_name matches exactly."},
		{Name: "model_name", Required: false, Type: "string", Description: "Filter to rows whose model_name matches exactly."},
		{Name: "run_id", Required: false, Type: "string", Description: "Filter to rows whose run_id matches exactly."},
		{Name: "since", Required: false, Type: "int64", Description: "Lower-bound run_at (unix seconds); returns only rows with run_at >= since."},
		{Name: "limit", Required: false, Type: "int64", Description: "Cap the number of rows returned (most recent first by run_at)."},
	},
}

var benchmarkReplayDoc = actionspec.ActionDoc{
	Purpose: "Re-execute a prior run from its captured provenance bundle and return identical/diff against the original. Loads the original benchmark_results row by row_id, re-emits its result_columns under a new run, and diffs the score-shaped columns.",
	// row_id derives from benchmarkReplayParams (ParamStruct set in the registry);
	// its required-ness is authored to match the handler's `if p.RowID == ""` guard.
	Params: []actionspec.DocParam{
		{Name: "row_id", Required: true, Description: "benchmark_results(id) of the original run to replay. The row must have a non-empty provenance_id (pre-T6-cutover legacy rows are not replayable)."},
	},
	Errors: []actionspec.ActionError{
		{Condition: "missing row_id", Message: "Returns error envelope: missing required params: params.row_id."},
		{Condition: "row_id not found", Message: "Returns error envelope: benchmark row \"<id>\" not found."},
		{Condition: "row has no provenance (legacy pre-cutover row)", Message: "Returns error envelope: benchmark row \"<id>\" has no provenance (pre-T6-cutover legacy row); not replayable."},
	},
	EnvelopeRequirements: []actionspec.ActionEnvelopeReq{{
		Field:               "rationale",
		Required:            true,
		Reason:              "Dispatcher policy gate (action-manifests/dispatch-policy.toml). Lives at the call envelope level (next to action/params/project), NOT inside params. benchmark_replay is mutating (writes a new result row); the rationale should explain why the replay is being run (debugging non-determinism, validating a model swap). Rejected on empty / whitespace / boilerplate / <6-char rationales with error=rationale_required.",
		AppliesToActorKinds: []string{"agent"},
	}},
}

// bench_run is the ONE struct-backed measure action: its params derive from
// benchRunParams (reflect.TypeOf in the registry). slug→string, update_baseline→
// bool, override_flags ([]benchFlagPairCLI)→object[]. The descriptor authors
// order + descriptions + the rest; Types stay empty so they derive. The rationale
// envelope reason is bench_run-specific (mutating: writes events + may overwrite a
// file), so it is authored inline rather than via a shared helper.
var benchRunDoc = actionspec.ActionDoc{
	Purpose: "Execute a registered bench harness subprocess and diff its output against the stored baseline. Reads the row from bench_harnesses (forged via forge(bench)), runs the binary with the recorded flag_set (plus any override_flags), parses stdout per parse_output_as (v1: json only), compares against baseline_json_path, prints + persists a per-metric diff as a BenchmarkDiff event. With update_baseline=true, overwrites the baseline file BEFORE diffing so the diff is zero by construction; a BenchmarkBaselineUpdated event records the explicit baseline shift.",
	Params: []actionspec.DocParam{
		{Name: "slug", Required: true, Description: "The bench's kebab/snake-case slug as registered in bench_harnesses (e.g. 'parse-context'). Must exist in the project_id scope — reject otherwise."},
		{Name: "update_baseline", Required: false, Description: "Default false (compare-against-existing-baseline path). True overwrites baseline_json_path with the observed values BEFORE computing the diff; the resulting BenchmarkDiff has zero deltas by construction, and a sibling BenchmarkBaselineUpdated event records the baseline shift with previous + new SHA-256 for round-trip audit."},
		{Name: "override_flags", Required: false, Description: "List of {flag, value} entries that override or augment the registered flag_set for this single run. Matching flag names replace the registered value (preserving order); new flag names append. Used for one-off probes (e.g. --session-id <other>) without re-forging the bench. NOT persisted — only affects this invocation."},
	},
	EnvelopeRequirements: []actionspec.ActionEnvelopeReq{{
		Field:               "rationale",
		Required:            true,
		Reason:              "Dispatcher policy gate (action-manifests/dispatch-policy.toml). bench_run is mutating (writes events; with update_baseline=true also overwrites a file on disk). Rationale should explain why this run is happening — perf-touch validation, baseline refresh after a known-fixed regression, post-deploy sanity.",
		AppliesToActorKinds: []string{"agent"},
	}},
	Returns: &actionspec.ActionReturn{
		Shape:       "BenchRunResult",
		Description: "Carries ok + slug + metrics[] + markdown_table + run_latency_ms + baseline_updated + stderr_log_path + baseline_path + bench_event_id. On error: error names the failure mode (subprocess timeout, non-zero exit, parse failure, baseline_not_found); metrics may be empty if the parse stage didn't reach the diff. The markdown_table field is the human-readable per-metric diff suitable for the operator's view + future reports.",
	},
	Examples: []actionspec.ActionExample{
		{Description: "Standard run — compare against the stored baseline, emit BenchmarkDiff", Call: `{"action":"bench_run","project":"mcp-servers","rationale":"post-T6 sanity bench against parse-context baseline","params":{"slug":"parse-context"}}`},
		{Description: "Refresh the baseline (e.g. after a fixed regression where the baseline is the new ground truth)", Call: `{"action":"bench_run","project":"mcp-servers","rationale":"refresh parse-context baseline after bug 866 fix","params":{"slug":"parse-context","update_baseline":true}}`},
		{Description: "Override the session-id flag for a one-off cache-disabled probe", Call: `{"action":"bench_run","project":"mcp-servers","rationale":"probe cache-disabled latency vs cache-warm baseline","params":{"slug":"parse-context","override_flags":[{"flag":"--session-id","value":""}]}}`},
	},
}

// ── Qwen-local rubric-classify family (map-bound: mcpparam.String;
// ParamStruct == nil; param Types authored "string") ──

var classifyChainTaskProportionalityDoc = actionspec.ActionDoc{
	Purpose: "Dispatches the chain-assessment rubric via Qwen — derives team-context from telemetry (closures-per-week + vault-decision-keyword-scan) when no override is supplied, calls the rubric registry, writes a benchmark_results row, and returns {label, latency_ms, model_name, team_context_prose}.",
	Params: []actionspec.DocParam{
		{Name: "task_spec", Required: true, Description: "The task specification to assess proportionality against.", Type: "string"},
		{Name: "team_context_override", Required: false, Description: "Override the auto-derived team context. When absent, team context is derived from telemetry: closures-per-week + vault-decision-keyword-scan.", Type: "string"},
	},
}

var classifyRetirementObservationDoc = actionspec.ActionDoc{
	Purpose: "Dispatches the retirement-signal rubric via Qwen — classifies a project activity observation by retirement artifact-type; label ∈ {tool-retirement, skill-retirement, workflow-retirement, not-retirement}; writes a benchmark_results row; returns {label, latency_ms, model_name}.",
	Params: []actionspec.DocParam{
		{Name: "observation_text", Required: true, Description: "The observation text describing project activity to classify.", Type: "string"},
	},
}

var classifyArtifactTierDoc = actionspec.ActionDoc{
	Purpose: "Dispatches the tiered-context-loading rubric via Qwen — classifies an artifact by the session tier at which it should be loaded; label ∈ {tier-zero, tier-one, tier-two, tier-three} (word-form; digit-suffix form trips a parser bug); writes a benchmark_results row; returns {label, latency_ms, model_name}.",
	Notes:   "Use the word-form labels (tier-zero, tier-one, …); the digit-suffix form (tier-0, tier-1) trips a parser bug.",
	Params: []actionspec.DocParam{
		{Name: "artifact_descriptor", Required: true, Description: "The artifact descriptor to classify by tier.", Type: "string"},
	},
}

var classifyAuditFindingSeverityDoc = actionspec.ActionDoc{
	Purpose: "Dispatches the agentic-architecture-audit severity rubric via Qwen — classifies ONE audit finding by consequence severity; label ∈ {critical, high, medium, low}; severity tracks consequence NOT effort (critical=actively harmful trust/data violation, high=structurally wrong compounds over time, medium=naive misses opportunity, low=hygiene no architectural consequence); writes a benchmark_results row; returns {label, latency_ms, model_name}.",
	Notes:   "Severity tracks CONSEQUENCE, not effort. critical = actively harmful trust/data violation. high = structurally wrong, compounds over time. medium = naive, misses opportunity. low = hygiene, no architectural consequence.",
	Params: []actionspec.DocParam{
		{Name: "finding_prose", Required: true, Description: "The audit finding prose to classify.", Type: "string"},
	},
}

var classifyArtifactReviewCriterionDoc = actionspec.ActionDoc{
	Purpose: "Dispatches the artifact-review rubric via Qwen — evaluates ONE criterion against ONE artifact excerpt under a named review purpose; purpose ∈ {safety, completeness, scope-fit, quality, coherence, scope-drift, custom}; label ∈ {pass, fail, mixed, n-a}; bias-toward-mixed clause: use mixed over fail when violation is partial; writes a benchmark_results row; returns {label, latency_ms, model_name}.",
	Notes:   "Bias-toward-mixed clause: use mixed over fail when violation is partial.",
	Params: []actionspec.DocParam{
		{Name: "artifact_excerpt", Required: true, Description: "The artifact excerpt to evaluate.", Type: "string"},
		{Name: "purpose", Required: true, Description: "Review purpose. One of: safety, completeness, scope-fit, quality, coherence, scope-drift, custom.", Type: "string"},
		{Name: "criterion", Required: true, Description: "The single criterion to evaluate against this excerpt under this purpose.", Type: "string"},
	},
}

var classifySessionRoutingTriggerDoc = actionspec.ActionDoc{
	Purpose: "Dispatches the session-routing rubric via Qwen; label ∈ {context-handoff, execute-document, retirement-dispatch, chain-execution, tool-suggest, no-trigger}; writes a benchmark_results row; returns {label, latency_ms, model_name}.",
	Params: []actionspec.DocParam{
		{Name: "user_input", Required: true, Description: "The user input text to classify for session-routing intent.", Type: "string"},
	},
}

var classifyPreCommitFailureDoc = actionspec.ActionDoc{
	Purpose: "Dispatches the pre-commit-failure rubric via Qwen — classifies the dominant failure cause in a pre-commit hook stderr dump; label ∈ {lint, typecheck, test, lifecycle, unclassifiable}; writes a benchmark_results row; returns {label, latency_ms, model_name}.",
	Params: []actionspec.DocParam{
		{Name: "stderr", Required: true, Description: "The pre-commit hook stderr dump to classify.", Type: "string"},
	},
}

var classifyDocstringDriftDoc = actionspec.ActionDoc{
	Purpose: "Dispatches the docstring-drift detection rubric via Qwen — given a function body and its doc comment, decides whether the docstring has drifted; label ∈ {matches, doesn't_match, unclear}; matches=drift detected, doesn't_match=still accurate, unclear=insufficient evidence (use as fallback trigger); writes a benchmark_results row; returns {label, latency_ms, model_name}.",
	Notes:   "Label semantics are counter-intuitive: matches = drift DETECTED, doesn't_match = doc STILL ACCURATE, unclear = insufficient evidence (use as fallback trigger).",
	Params: []actionspec.DocParam{
		{Name: "function_snippet", Required: true, Description: "The function body plus its doc comment to check for drift.", Type: "string"},
	},
}

var classifyBugSeverityDoc = actionspec.ActionDoc{
	Purpose: "Dispatches the bug-severity rubric via Qwen — classifies a filed bug report along the two-axis observer-impact × blast-radius matrix from skill:bug-filing-discipline; label ∈ {low, medium, high, unclear}; writes a benchmark_results row; returns {label, latency_ms, model_name}.",
	Notes:   "Severity is two-axis (observer-impact × blast-radius) per skill:bug-filing-discipline. unclear = either axis is insufficiently determined; use as a signal to surface the bug for human review rather than auto-routing it.",
	Params: []actionspec.DocParam{
		{Name: "bug_report", Required: true, Description: "The filed bug report to classify.", Type: "string"},
	},
}

// measureActionRegistry is the ordered, co-located descriptor registry — the
// single source of the measure surface's action docs. MeasureActionSpecs()
// derives the catalog the corpus generator + admin.action_describe consume. The
// T1 characterization net (internal/actiondocs/surface_contract_net_test.go) is
// the byte-parity oracle. bench_run + benchmark_replay set ParamStruct (their
// types derive); every other action is map-bound and authors its types
// (ParamStruct == nil).
// Order mirrors measure.BuildTable; for measure (describe-only consumer) order is
// cosmetic, but kept aligned with the handler wiring.
var measureActionRegistry = []actionspec.ActionEntry{
	// ── Benchmark CRUD ──
	{Name: "benchmark_record", Doc: benchmarkRecordDoc, ParamStruct: nil},
	{Name: "benchmark_query", Doc: benchmarkQueryDoc, ParamStruct: nil},
	{Name: "benchmark_replay", Doc: benchmarkReplayDoc, ParamStruct: reflect.TypeOf(benchmarkReplayParams{})},
	{Name: "bench_run", Doc: benchRunDoc, ParamStruct: reflect.TypeOf(benchRunParams{})},

	// ── Study runs (corpos-lab behavioral assays) ──
	{Name: "study_run_record", Doc: studyRunRecordDoc, ParamStruct: nil},

	// ── Gate runs (corpos-gate trend storage) ──
	{Name: "gate_run", Doc: gateRunDoc, ParamStruct: reflect.TypeOf(gateRunParams{})},
	{Name: "gate_trend", Doc: gateTrendDoc, ParamStruct: reflect.TypeOf(gateTrendParams{})},

	// ── Qwen-local rubric-classify family (map-bound) ──
	{Name: "classify_chain_task_proportionality", Doc: classifyChainTaskProportionalityDoc, ParamStruct: nil},
	{Name: "classify_retirement_observation", Doc: classifyRetirementObservationDoc, ParamStruct: nil},
	{Name: "classify_artifact_tier", Doc: classifyArtifactTierDoc, ParamStruct: nil},
	{Name: "classify_audit_finding_severity", Doc: classifyAuditFindingSeverityDoc, ParamStruct: nil},
	{Name: "classify_artifact_review_criterion", Doc: classifyArtifactReviewCriterionDoc, ParamStruct: nil},
	{Name: "classify_session_routing_trigger", Doc: classifySessionRoutingTriggerDoc, ParamStruct: nil},
	{Name: "classify_pre_commit_failure", Doc: classifyPreCommitFailureDoc, ParamStruct: nil},
	{Name: "classify_docstring_drift", Doc: classifyDocstringDriftDoc, ParamStruct: nil},
	{Name: "classify_bug_severity", Doc: classifyBugSeverityDoc, ParamStruct: nil},
}

// MeasureActionSpecs returns the measure surface's full action catalog, derived
// from the co-located descriptor registry. bench_run + benchmark_replay derive
// their param types from their structs; every other action's types are authored
// (map-bound). This is
// what the corpus generator projects into corpus/measure/*.toml and what
// admin.action_describe(measure, X) serves once the corpus is generated.
func MeasureActionSpecs() []actionspec.ActionSpec {
	return actionspec.DeriveSpecs(measureActionRegistry)
}
