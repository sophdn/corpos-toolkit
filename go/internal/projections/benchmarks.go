package projections

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"toolkit/internal/events"
)

// benchmarkResults folds BenchmarkRun* events into proj_benchmark_results.
// Post-T5-benchmarks (2026-05-21) the fold constructs rows from the
// event payload alone. Identifying columns (id, project_id, scenario_id,
// provenance_id, run_at) added via T5-benchmarks's payload bump.
type benchmarkResults struct{}

func init() { Register(benchmarkResults{}) }

func (benchmarkResults) Name() string      { return "benchmark_results" }
func (benchmarkResults) TableName() string { return "proj_benchmark_results" }

func (benchmarkResults) Fold(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	if evt.EntityKind != "benchmark_run" {
		return nil
	}
	switch evt.Type {
	case "BenchmarkRunCompleted":
		return foldBenchmarkRunCompleted(ctx, tx, evt)
	case "BenchmarkRunFailed":
		return foldBenchmarkRunFailed(ctx, tx, evt)
	}
	return nil
}

// RebuildFromEmpty replays BenchmarkRun{Completed,Failed} events.
// BenchmarkRunStarted doesn't produce a projection row directly.
func (benchmarkResults) RebuildFromEmpty(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug,
		       entity_project_id, payload, rationale, caused_by_event_id,
		       related_entities, span_id, schema_version
		FROM events
		WHERE entity_kind = 'benchmark_run'
		  AND (type = 'BenchmarkRunCompleted' OR type = 'BenchmarkRunFailed')
		ORDER BY ts ASC, event_id ASC`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var evt RawEvent
		var entityProjectID, rationale, causedBy sql.NullString
		var payloadStr, relatedStr string
		if err := rows.Scan(&evt.EventID, &evt.Ts, &evt.ActorKind, &evt.ActorID,
			&evt.Type, &evt.EntityKind, &evt.EntitySlug, &entityProjectID,
			&payloadStr, &rationale, &causedBy, &relatedStr,
			&evt.SpanID, &evt.SchemaVersion); err != nil {
			return err
		}
		evt.Payload = json.RawMessage(payloadStr)
		evt.RelatedEntities = json.RawMessage(relatedStr)
		if entityProjectID.Valid {
			s := entityProjectID.String
			evt.EntityProjectID = &s
		}
		if rationale.Valid {
			s := rationale.String
			evt.Rationale = &s
		}
		if causedBy.Valid {
			s := causedBy.String
			evt.CausedByEventID = &s
		}
		if err := (benchmarkResults{}).Fold(ctx, tx, evt); err != nil {
			return fmt.Errorf("rebuild benchmark fold %s: %w", evt.EventID, err)
		}
	}
	return rows.Err()
}

func foldBenchmarkRunCompleted(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	var p events.BenchmarkRunCompletedPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("benchmark %s payload: %w", evt.EventID, err)
	}
	if p.BenchmarkResultID == nil || p.ProjectID == nil {
		// Pre-T5-benchmarks events lack identifying columns; the post-T5
		// rebuild can't reconstruct these rows from event alone. The live
		// system's pre-T5 rows persist in proj_benchmark_results from the
		// snapshot-seed in migration 058; rebuild-from-empty cannot
		// reproduce them. Skip (forensic-only).
		return nil
	}
	rc := p.ResultColumns
	if rc == nil {
		return fmt.Errorf("benchmark %s: result_columns missing", evt.EventID)
	}
	var runAt int64
	if p.RunAt != nil {
		runAt = *p.RunAt
	}
	invokedContextually := 0
	if rc.InvokedContextually {
		invokedContextually = 1
	}
	invocationOK := 0
	if rc.InvocationOK {
		invocationOK = 1
	}
	argsMatch := boolPtrToInt(rc.ArgsMatch)
	interpretationOK := boolPtrToInt(rc.InterpretationOK)
	scenarioID := ""
	if p.ScenarioID != nil {
		scenarioID = *p.ScenarioID
	}
	provID := ""
	if p.ProvenanceID != nil {
		provID = *p.ProvenanceID
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO proj_benchmark_results (
			id, project_id, scenario_id, tool_name, model_name, run_id, run_at,
			wall_clock_ms, input_tokens, output_tokens, invoked_contextually,
			invocation_ok, args_match, extracted_args, interpretation_ok,
			detected_tool, notes, layer, task_shape, accuracy_score, honesty_score,
			ranking_quality_score, within_budget_score, task_id, run_shape,
			provenance_id, last_event_id, last_event_ts
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			project_id = excluded.project_id,
			scenario_id = excluded.scenario_id,
			tool_name = excluded.tool_name,
			model_name = excluded.model_name,
			run_id = excluded.run_id,
			run_at = excluded.run_at,
			wall_clock_ms = excluded.wall_clock_ms,
			input_tokens = excluded.input_tokens,
			output_tokens = excluded.output_tokens,
			invoked_contextually = excluded.invoked_contextually,
			invocation_ok = excluded.invocation_ok,
			args_match = excluded.args_match,
			extracted_args = excluded.extracted_args,
			interpretation_ok = excluded.interpretation_ok,
			detected_tool = excluded.detected_tool,
			notes = excluded.notes,
			layer = excluded.layer,
			task_shape = excluded.task_shape,
			accuracy_score = excluded.accuracy_score,
			honesty_score = excluded.honesty_score,
			ranking_quality_score = excluded.ranking_quality_score,
			within_budget_score = excluded.within_budget_score,
			task_id = excluded.task_id,
			run_shape = excluded.run_shape,
			provenance_id = excluded.provenance_id,
			last_event_id = excluded.last_event_id,
			last_event_ts = excluded.last_event_ts`,
		*p.BenchmarkResultID, *p.ProjectID, scenarioID,
		rc.ToolName, rc.ModelName, p.RunID, runAt,
		p.WallClockMS,
		intPtrOrNil(p.InputTokens), intPtrOrNil(p.OutputTokens),
		invokedContextually, invocationOK,
		argsMatch, nullableStringFromPtr(rc.ExtractedArgs),
		interpretationOK, nullableStringFromPtr(rc.DetectedTool),
		nullableStringFromPtr(rc.Notes),
		nullableStringFromPtr(rc.Layer), nullableStringFromPtr(rc.TaskShape),
		floatPtrOrNil(rc.AccuracyScore), floatPtrOrNil(rc.HonestyScore),
		floatPtrOrNil(rc.RankingQualityScore), floatPtrOrNil(rc.WithinBudgetScore),
		nullableStringFromPtr(rc.TaskID), nullableStringFromPtr(rc.RunShape),
		provID,
		evt.EventID, evt.Ts,
	)
	return err
}

func foldBenchmarkRunFailed(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	// Failures don't currently produce a benchmark_results row in
	// production (the row is only written on successful completion).
	// If a future emit path produces a failure row, this fold would
	// need to INSERT it; for now, no-op.
	_ = evt
	return nil
}

func boolPtrToInt(p *bool) interface{} {
	if p == nil {
		return nil
	}
	if *p {
		return 1
	}
	return 0
}

func intPtrOrNil(p *int) interface{} {
	if p == nil {
		return nil
	}
	return *p
}

func floatPtrOrNil(p *float64) interface{} {
	if p == nil {
		return nil
	}
	return *p
}
