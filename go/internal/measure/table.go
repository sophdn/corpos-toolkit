// Package measure's table.go assembles the dispatch.Table for the
// measure meta-tool: benchmark CRUD plus the Qwen-local rubric-classify
// family. Pairs with the parallel BuildTable functions in internal/admin,
// internal/work, internal/knowledge — keeping the action wiring next to
// the handlers (classify.go, benchmark.go) localizes future changes.
//
// Every entry adapts a typed-return handler into dispatch.Handler via
// dispatch.AdaptParamsOnly — that adapter is the single JSON-marshaling
// seam where the typed result widens to `any` for the dispatcher.
package measure

import (
	"context"
	"encoding/json"

	"toolkit/internal/dispatch"
)

// BuildTable returns the measure surface's dispatch.Table. Benchmark
// actions are always registered. The classify_* family registers only
// when classifyDeps.Rubrics is non-nil — degraded mode when no rubric
// registry is configured still serves the benchmark actions.
func BuildTable(classifyDeps ClassifyDeps, benchDeps BenchmarkDeps, studyDeps StudyRunDeps, gateDeps GateRunDeps) dispatch.Table {
	table := dispatch.Table{
		"benchmark_record": dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (BenchmarkRecordResult, error) {
			return HandleBenchmarkRecord(ctx, benchDeps, project, params)
		}),
		"study_run_record": dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (StudyRunRecordResult, error) {
			return HandleStudyRunRecord(ctx, studyDeps, project, params)
		}),
		"gate_run": dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (GateRunResult, error) {
			return HandleGateRun(ctx, gateDeps, project, params)
		}),
		"gate_trend": dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (GateTrendResult, error) {
			return HandleGateTrend(ctx, gateDeps, project, params)
		}),
		"benchmark_query": dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) ([]BenchmarkResult, error) {
			return HandleBenchmarkQuery(ctx, benchDeps, project, params)
		}),
		"benchmark_replay": dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (BenchmarkReplayResult, error) {
			return HandleBenchmarkReplay(ctx, benchDeps, project, params)
		}),
		"bench_run": dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (BenchRunResult, error) {
			return HandleBenchRun(ctx, benchDeps, project, params)
		}),
	}
	if classifyDeps.Rubrics == nil {
		return table
	}

	// classifyAdapt is a small local adapter: the classify handlers
	// take a per-call ClassifyDeps and params, ignore the dispatch
	// project string (the rubric registry is shared across projects).
	classifyAdapt := func(h func(context.Context, ClassifyDeps, json.RawMessage) (ClassifyResponse, error)) dispatch.Handler {
		return dispatch.AdaptParamsOnly(func(ctx context.Context, params json.RawMessage) (ClassifyResponse, error) {
			return h(ctx, classifyDeps, params)
		})
	}

	table["classify_chain_task_proportionality"] = classifyAdapt(HandleClassifyChainTaskProportionality)
	table["classify_retirement_observation"] = classifyAdapt(HandleClassifyRetirementObservation)
	table["classify_artifact_tier"] = classifyAdapt(HandleClassifyArtifactTier)
	table["classify_audit_finding_severity"] = classifyAdapt(HandleClassifyAuditFindingSeverity)
	table["classify_artifact_review_criterion"] = classifyAdapt(HandleClassifyArtifactReviewCriterion)
	table["classify_session_routing_trigger"] = classifyAdapt(HandleClassifySessionRoutingTrigger)
	table["classify_pre_commit_failure"] = classifyAdapt(HandleClassifyPreCommitFailure)
	table["classify_docstring_drift"] = classifyAdapt(HandleClassifyDocstringDrift)
	table["classify_bug_severity"] = classifyAdapt(HandleClassifyBugSeverity)

	return table
}
