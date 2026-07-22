package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"toolkit/internal/events"
)

// ClassifyResult holds the output of a single classify inference call.
type ClassifyResult struct {
	Label        string // empty when invocation failed
	RawResponse  string
	LatencyMS    int64
	InputTokens  *int64
	OutputTokens *int64
	InvocationOK bool
	// NotesOverride, when non-empty, replaces RawResponse as the value
	// written to the benchmark_results.notes column. Use when call-site
	// metadata (fallback_used, final_label, etc.) needs to travel with
	// the row — mirrors Rust's record_benchmark_dispatch_noted.
	NotesOverride string
	// RunShape labels the row's origin. Empty defaults to "production"
	// (the live dispatch path). Smoke binaries pass "smoke"; regression
	// binaries pass "regression". Matches Rust shared_db::record_benchmark_dispatch
	// vs ..._regression vs ..._smoke shape.
	RunShape string
}

func newRandomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// RecordBenchmarkDispatch writes a production benchmark_results row for a
// classify dispatch call. Mirrors shared_db::record_benchmark_dispatch in
// the Rust server.
//
// taskID is the rubric name (e.g. "chain-assessment"). project may be empty
// in which case the row is skipped — matching Rust's `if let Some(proj)` guard.
func RecordBenchmarkDispatch(ctx context.Context, pool *Pool, project, taskID, modelName string, result ClassifyResult) error {
	if project == "" {
		return nil
	}
	id := newRandomID()
	now := time.Now().Unix()
	hourBucket := now / 3600
	runID := fmt.Sprintf("prod-%s-%d", taskID, hourBucket)
	scenarioID := fmt.Sprintf("prod-%s-%s", taskID, id[:8])

	_ = result.InvocationOK // T5-benchmarks: encoded in payload.result_columns.invocation_ok
	detectedTool := sql.NullString{}
	if result.Label != "" {
		detectedTool = sql.NullString{String: result.Label, Valid: true}
	}

	notes := result.RawResponse
	if result.NotesOverride != "" {
		notes = result.NotesOverride
	}

	// Emit BenchmarkRunStarted + INSERT benchmark_provenance + emit
	// BenchmarkRunCompleted, all inside one transaction. The Go
	// production classify path has fewer reproducibility inputs than
	// the Rust harness (no fixed corpus, no retriever for a Classify
	// call), so the provenance bundle captures (model + rubric prompt-
	// template-as-task_id + input as ad-hoc corpus). This is best-
	// effort provenance: every run becomes addressable, but classify
	// outputs vary with input rather than scenario, so replay
	// determinism is bounded by the classifier model's own non-
	// determinism. The benchmark_results CRUD write that used to
	// follow Started+provenance is gone (T5-benchmarks landed at
	// 94618fe; T6 dropped the table outright) — the Completed event's
	// result_columns + identifying columns are what the projection
	// fold now uses to build proj_benchmark_results.
	provenanceID := newRandomID()
	startedEventID := ""
	provenance := events.BenchmarkProvenance{
		ModelID:             modelName,
		ModelVersion:        modelName, // no separate version surfaced by the router here
		PromptTemplateHash:  sha256Hex("rubric:" + taskID),
		CorpusHash:          sha256Hex("input:" + notes), // notes carries the model input or response trail
		RetrieverVersion:    "no-retriever",
		RetrieverConfigHash: sha256Hex("retriever-config:none"),
		Seed:                0,
		EnvHash:             sha256Hex("env:go-classify-dispatch:v1"),
	}

	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		evID, emitErr := events.Emit(ctx, tx, events.EmitArgs{
			Entity: events.NewCrossCuttingEntityRef("benchmark_run", runID),
			Payload: events.BenchmarkRunStartedPayload{
				ScenarioID: scenarioID,
				Provenance: provenance,
			},
		})
		if emitErr != nil {
			return fmt.Errorf("emit BenchmarkRunStarted: %w", emitErr)
		}
		startedEventID = evID
		const provInsert = `INSERT INTO benchmark_provenance (
			id, run_id, model_id, model_version,
			prompt_template_hash, corpus_hash,
			retriever_version, retriever_config_hash,
			seed, env_hash, started_event_id
		) VALUES (?,?,?,?,?,?,?,?,?,?,?)`
		if _, err := tx.ExecContext(ctx, provInsert,
			provenanceID, runID,
			provenance.ModelID, provenance.ModelVersion,
			provenance.PromptTemplateHash, provenance.CorpusHash,
			provenance.RetrieverVersion, provenance.RetrieverConfigHash,
			provenance.Seed, provenance.EnvHash, startedEventID,
		); err != nil {
			return fmt.Errorf("insert benchmark_provenance: %w", err)
		}
		// T5-benchmarks: CRUD INSERT INTO benchmark_results dropped; the
		// projection fold for BenchmarkRunCompleted INSERTs proj_benchmark_results
		// from the payload (identifying columns added in this commit's
		// additive payload bump).
		// Per T3 of agent-substrate-crud-retirement (§9.6 audit finding),
		// the benchmark_results row write is paired with a
		// BenchmarkRunCompleted emit carrying the rubric-side columns so
		// payload-only fold reconstruction (T5's contract) can rebuild
		// proj_benchmark_results without joining the CRUD table. The
		// caused_by_event_id ties this Completed event to the matching
		// Started event emitted at the top of the same tx.
		layer := "e4"
		taskShape := "Classify"
		runShape := result.RunShape
		if runShape == "" {
			runShape = "production"
		}
		taskIDPtr := taskID
		var detectedToolPtr *string
		if detectedTool.Valid {
			s := detectedTool.String
			detectedToolPtr = &s
		}
		notesPtr := notes
		inputTokensInt, outputTokensInt := toIntPtr(result.InputTokens), toIntPtr(result.OutputTokens)
		wallClockMS := int(result.LatencyMS)
		resultCols := &events.BenchmarkResultColumns{
			ToolName:            taskID,
			ModelName:           modelName,
			Layer:               &layer,
			TaskShape:           &taskShape,
			TaskID:              &taskIDPtr,
			RunShape:            &runShape,
			InvocationOK:        result.InvocationOK,
			DetectedTool:        detectedToolPtr,
			Notes:               &notesPtr,
			InvokedContextually: true,
		}
		idCopy := id
		projCopy := project
		scenarioIDCopy := scenarioID
		provIDCopy := provenanceID
		runAt := now
		if _, emitErr := events.Emit(ctx, tx, events.EmitArgs{
			Entity: events.NewCrossCuttingEntityRef("benchmark_run", runID),
			Payload: events.BenchmarkRunCompletedPayload{
				RunID:             runID,
				WallClockMS:       wallClockMS,
				InputTokens:       inputTokensInt,
				OutputTokens:      outputTokensInt,
				ResultColumns:     resultCols,
				BenchmarkResultID: &idCopy,
				ProjectID:         &projCopy,
				ScenarioID:        &scenarioIDCopy,
				ProvenanceID:      &provIDCopy,
				RunAt:             &runAt,
			},
			Refs: events.Refs{CausedByEventID: &startedEventID},
		}); emitErr != nil {
			return fmt.Errorf("emit BenchmarkRunCompleted: %w", emitErr)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("record benchmark dispatch: %w", err)
	}
	return nil
}

// toIntPtr converts *int64 to *int for events.BenchmarkRunCompletedPayload's
// token fields. Returns nil when the source pointer is nil; otherwise
// dereferences and narrows. The events payload uses int rather than int64
// because the JSON schema declares integer with no width hint and the Go
// embed validator accepts either; matching the events package's existing
// convention keeps the surface uniform.
func toIntPtr(v *int64) *int {
	if v == nil {
		return nil
	}
	n := int(*v)
	return &n
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
