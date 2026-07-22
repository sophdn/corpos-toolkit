package measure

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"toolkit/internal/benchmarks"
)

// BenchmarkReplayResult is the response shape for benchmark_replay.
// Identical=true means the replay produced byte-equivalent scoring fields
// across (accuracy_score, honesty_score, ranking_quality_score,
// within_budget_score, extracted_args, notes). Diff is a human-readable
// per-field summary populated when Identical=false.
type BenchmarkReplayResult struct {
	OK            bool   `json:"ok,omitempty"`
	OriginalRunID string `json:"original_run_id,omitempty"`
	ReplayRunID   string `json:"replay_run_id,omitempty"`
	OriginalRowID string `json:"original_row_id,omitempty"`
	ReplayRowID   string `json:"replay_row_id,omitempty"`
	Identical     bool   `json:"identical"`
	Diff          string `json:"diff,omitempty"`
	StderrTail    string `json:"stderr_tail,omitempty"`
	Error         string `json:"error,omitempty"`
}

// benchmarkReplayParams is the typed benchmark_replay request body — the
// json.Unmarshal target AND the action-doc TYPE source: measureActionRegistry
// reflects it (reflect.TypeOf(benchmarkReplayParams{})) so row_id's type derives
// from the field kind (string) rather than being re-authored (chain
// finalize-action-docs-epic T4, bug 940; docs/ACTION_DOC_CONTRACT.md). Hoisted
// from the prior inline anonymous struct — same single field, json tag, and strict
// unmarshal, so the binding is byte-for-byte unchanged. row_id's required-ness is
// enforced by the handler's `if p.RowID == ""` guard (not the unmarshal), so the
// descriptor authors Required=true.
type benchmarkReplayParams struct {
	RowID string `json:"row_id"`
}

// HandleBenchmarkReplay implements the measure.benchmark_replay action.
//
// Flow:
//  1. Resolve the original benchmark_results row by id (params.row_id).
//     Fail if the row is missing OR its provenance_id is empty (legacy
//     pre-cutover row — not replayable).
//  2. Call benchmarks.RunReplay (in-process Go) which re-emits the
//     original row's result_columns under a new run_id + provenance_id +
//     row_id tagged run_shape='replay'.
//  3. Load the new row from proj_benchmark_results.
//  4. Compute a per-field diff of the score-shaped columns. Identical
//     iff every field matches byte-for-byte.
//
// Rationale (per dispatch-policy): replay is a mutating call (writes a
// new result row) — the rationale arg should explain *why* the replay is
// being run (debugging non-determinism, validating a model swap, etc.).
//
// Replaces the prior subprocess-spawn shape (target/release/benchmarks
// --replay) per chain rust-retirement-and-db-hardening T5 Phase 7.
func HandleBenchmarkReplay(ctx context.Context, deps BenchmarkDeps, project string, params json.RawMessage) (BenchmarkReplayResult, error) {
	if project == "" {
		return BenchmarkReplayResult{Error: "project is required"}, nil
	}
	var p benchmarkReplayParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return BenchmarkReplayResult{Error: fmt.Sprintf("params: %s", err)}, nil
		}
	}
	if p.RowID == "" {
		return BenchmarkReplayResult{Error: "missing required params: params.row_id"}, nil
	}

	original, err := loadResultRow(ctx, deps.Pool, p.RowID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BenchmarkReplayResult{Error: fmt.Sprintf("benchmark row %q not found", p.RowID)}, nil
		}
		return BenchmarkReplayResult{}, fmt.Errorf("load original: %w", err)
	}
	if original.ProvenanceID == "" {
		return BenchmarkReplayResult{
			Error: fmt.Sprintf("benchmark row %q has no provenance (pre-T6-cutover legacy row); not replayable", p.RowID),
		}, nil
	}

	replayResult, err := benchmarks.RunReplay(ctx, deps.Pool, p.RowID)
	if err != nil {
		return BenchmarkReplayResult{
			OriginalRowID: p.RowID,
			Error:         fmt.Sprintf("replay failed: %v", err),
		}, nil
	}

	replay, err := loadResultRow(ctx, deps.Pool, replayResult.ReplayRowID)
	if err != nil {
		return BenchmarkReplayResult{
			OriginalRowID: p.RowID,
			ReplayRowID:   replayResult.ReplayRowID,
			Error:         fmt.Sprintf("load replay row %q: %v", replayResult.ReplayRowID, err),
		}, nil
	}

	diff := diffScoreFields(original, replay)
	identical := diff == ""
	return BenchmarkReplayResult{
		OK:            true,
		OriginalRunID: optStrValue(original.RunID),
		ReplayRunID:   replayResult.ReplayRunID,
		OriginalRowID: p.RowID,
		ReplayRowID:   replayResult.ReplayRowID,
		Identical:     identical,
		Diff:          diff,
	}, nil
}

// loadResultRow fetches one benchmark_results row by primary id.
func loadResultRow(ctx context.Context, pool interface {
	DB() *sql.DB
}, id string) (BenchmarkResult, error) {
	const q = `SELECT id, project_id, scenario_id, tool_name, model_name, run_id, run_at,
	                  wall_clock_ms, input_tokens, output_tokens,
	                  invoked_contextually, invocation_ok, args_match, extracted_args,
	                  interpretation_ok, detected_tool, notes,
	                  task_shape, accuracy_score, honesty_score,
	                  ranking_quality_score, within_budget_score,
	                  COALESCE(provenance_id, '')
	           FROM proj_benchmark_results WHERE id = ?`
	row := pool.DB().QueryRowContext(ctx, q, id)
	var r BenchmarkResult
	var runID, extracted, detected, notes, taskShape sql.NullString
	var inTok, outTok, argsMatch, interp sql.NullInt64
	var acc, honest, ranking, budget sql.NullFloat64
	if err := row.Scan(
		&r.ID, &r.ProjectID, &r.ScenarioID, &r.ToolName, &r.ModelName,
		&runID, &r.RunAt,
		&r.WallClockMS, &inTok, &outTok,
		&r.InvokedContextually, &r.InvocationOK,
		&argsMatch, &extracted,
		&interp, &detected, &notes,
		&taskShape, &acc, &honest, &ranking, &budget,
		&r.ProvenanceID,
	); err != nil {
		return BenchmarkResult{}, err
	}
	r.RunID = nullableStringPtr(runID)
	r.InputTokens = nullableInt64Ptr(inTok)
	r.OutputTokens = nullableInt64Ptr(outTok)
	r.ArgsMatch = nullableInt64Ptr(argsMatch)
	r.ExtractedArgs = nullableStringPtr(extracted)
	r.InterpretationOK = nullableInt64Ptr(interp)
	r.DetectedTool = nullableStringPtr(detected)
	r.Notes = nullableStringPtr(notes)
	r.TaskShape = nullableStringPtr(taskShape)
	r.AccuracyScore = nullableFloat64Ptr(acc)
	r.HonestyScore = nullableFloat64Ptr(honest)
	r.RankingQualityScore = nullableFloat64Ptr(ranking)
	r.WithinBudgetScore = nullableFloat64Ptr(budget)
	return r, nil
}

// diffScoreFields returns "" when the two rows' score-shaped columns
// match byte-for-byte; otherwise returns a human-readable per-field
// diff. Compares the same fields a future dashboard "replay diff" view
// would surface.
func diffScoreFields(orig, replay BenchmarkResult) string {
	var out strings.Builder
	addF := func(name string, a, b *float64) {
		if !floatEq(a, b) {
			fmt.Fprintf(&out, "  %s: original=%s replay=%s\n", name, ptrFloat(a), ptrFloat(b))
		}
	}
	addS := func(name string, a, b *string) {
		if !strPtrEq(a, b) {
			fmt.Fprintf(&out, "  %s: original=%q replay=%q\n", name, ptrStr(a), ptrStr(b))
		}
	}
	addI := func(name string, a, b *int64) {
		if !intPtrEq(a, b) {
			fmt.Fprintf(&out, "  %s: original=%s replay=%s\n", name, ptrInt(a), ptrInt(b))
		}
	}
	addF("accuracy_score", orig.AccuracyScore, replay.AccuracyScore)
	addF("honesty_score", orig.HonestyScore, replay.HonestyScore)
	addF("ranking_quality_score", orig.RankingQualityScore, replay.RankingQualityScore)
	addF("within_budget_score", orig.WithinBudgetScore, replay.WithinBudgetScore)
	addS("extracted_args", orig.ExtractedArgs, replay.ExtractedArgs)
	addS("notes", orig.Notes, replay.Notes)
	addI("args_match", orig.ArgsMatch, replay.ArgsMatch)
	addI("interpretation_ok", orig.InterpretationOK, replay.InterpretationOK)
	if out.Len() == 0 {
		return ""
	}
	return "diff between original and replay:\n" + out.String()
}

func floatEq(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// strPtrEq treats nil and *"" as equal — the projection writeback
// collapses nil to "" on disk, so the comparison must too lest legacy
// rows seeded with NULL silently diff against fresh projection writes.
func strPtrEq(a, b *string) bool {
	av := ""
	bv := ""
	if a != nil {
		av = *a
	}
	if b != nil {
		bv = *b
	}
	return av == bv
}

func intPtrEq(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func ptrFloat(p *float64) string {
	if p == nil {
		return "nil"
	}
	return strconv.FormatFloat(*p, 'g', -1, 64)
}

func ptrStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func ptrInt(p *int64) string {
	if p == nil {
		return "nil"
	}
	return strconv.FormatInt(*p, 10)
}

func optStrValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func tailString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return "…" + s[len(s)-max:]
}
