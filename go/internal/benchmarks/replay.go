package benchmarks

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/events"
)

// ReplayResult names the new row + new run id produced by RunReplay.
type ReplayResult struct {
	ReplayRowID string
	ReplayRunID string
}

// RunReplay reconstructs a benchmark run from its provenance and re-emits
// the row as a copy-of-original tagged run_shape='replay'. Returns the
// new (replay_row_id, replay_run_id).
//
// Ported from benchmarks/src/replay.rs::run_replay per chain
// rust-retirement-and-db-hardening T5 Phase 7. The Rust replay was an
// in-process subprocess invoked by measure.HandleBenchmarkReplay; the Go
// port collapses the boundary so a single transaction owns both reads
// (original row + provenance) and writes (replay BenchmarkRunStarted +
// new benchmark_provenance + BenchmarkRunCompleted), avoiding the
// rusqlite-vs-sqlx process-boundary serialization the Rust source
// papered over.
//
// Behavioral parity with Rust (which the docstring labels "replay-mvp"):
// re-emits the original row's result_columns verbatim (no model re-run);
// the actual "re-run + new-grade" path is a future feature. The new row
// carries new id/run_id/provenance_id/scenario_id columns; the result
// columns are copied with run_shape rewritten to "replay".
func RunReplay(ctx context.Context, pool *db.Pool, originalRowID string) (ReplayResult, error) {
	if originalRowID == "" {
		return ReplayResult{}, errors.New("RunReplay: originalRowID is empty")
	}

	ctx = events.WithActor(ctx, events.Actor{Kind: "system", ID: "benchmarks-replay"})

	orig, err := loadOriginal(ctx, pool, originalRowID)
	if err != nil {
		return ReplayResult{}, err
	}

	replayRunID := newReplayID()
	replayRowID := newReplayID()
	replayProvenanceID := newReplayID()
	runAt := time.Now().Unix()

	rationale := fmt.Sprintf("replay of row %s", originalRowID)

	err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
		startedEvID, emitErr := events.Emit(ctx, tx, events.EmitArgs{
			Entity: events.NewCrossCuttingEntityRef("benchmark_run", replayRunID),
			Payload: events.BenchmarkRunStartedPayload{
				ScenarioID: orig.scenarioID,
				Provenance: events.BenchmarkProvenance{
					ModelID:             orig.modelID,
					ModelVersion:        orig.modelVersion,
					PromptTemplateHash:  orig.promptTemplateHash,
					CorpusHash:          orig.corpusHash,
					RetrieverVersion:    orig.retrieverVersion,
					RetrieverConfigHash: orig.retrieverConfigHash,
					Seed:                int(orig.seed),
					EnvHash:             orig.envHash,
				},
			},
			Rationale: &rationale,
			Refs:      events.Refs{CausedByEventID: &orig.startedEventID},
		})
		if emitErr != nil {
			return fmt.Errorf("emit BenchmarkRunStarted: %w", emitErr)
		}

		const provInsert = `INSERT INTO benchmark_provenance (
			id, run_id, model_id, model_version,
			prompt_template_hash, corpus_hash,
			retriever_version, retriever_config_hash,
			seed, env_hash, started_event_id
		) VALUES (?,?,?,?,?,?,?,?,?,?,?)`
		if _, execErr := tx.ExecContext(ctx, provInsert,
			replayProvenanceID, replayRunID,
			orig.modelID, orig.modelVersion,
			orig.promptTemplateHash, orig.corpusHash,
			orig.retrieverVersion, orig.retrieverConfigHash,
			orig.seed, orig.envHash, startedEvID,
		); execErr != nil {
			return fmt.Errorf("insert benchmark_provenance: %w", execErr)
		}

		replayShape := "replay"
		resultCols := orig.resultColumns
		resultCols.RunShape = &replayShape
		idCopy := replayRowID
		projCopy := orig.projectID
		scenarioCopy := orig.scenarioID
		provCopy := replayProvenanceID
		runAtCopy := runAt

		if _, emitErr := events.Emit(ctx, tx, events.EmitArgs{
			Entity: events.NewCrossCuttingEntityRef("benchmark_run", replayRunID),
			Payload: events.BenchmarkRunCompletedPayload{
				RunID:             replayRunID,
				WallClockMS:       orig.wallClockMS,
				InputTokens:       orig.inputTokens,
				OutputTokens:      orig.outputTokens,
				ResultColumns:     &resultCols,
				BenchmarkResultID: &idCopy,
				ProjectID:         &projCopy,
				ScenarioID:        &scenarioCopy,
				ProvenanceID:      &provCopy,
				RunAt:             &runAtCopy,
			},
			Rationale: &rationale,
			Refs:      events.Refs{CausedByEventID: &startedEvID},
		}); emitErr != nil {
			return fmt.Errorf("emit BenchmarkRunCompleted: %w", emitErr)
		}
		return nil
	})
	if err != nil {
		return ReplayResult{}, err
	}

	return ReplayResult{
		ReplayRowID: replayRowID,
		ReplayRunID: replayRunID,
	}, nil
}

// originalRow bundles the fields the replay path needs from the
// original benchmark_results row + its joined benchmark_provenance row.
type originalRow struct {
	scenarioID          string
	projectID           string
	wallClockMS         int
	inputTokens         *int
	outputTokens        *int
	resultColumns       events.BenchmarkResultColumns
	modelID             string
	modelVersion        string
	promptTemplateHash  string
	corpusHash          string
	retrieverVersion    string
	retrieverConfigHash string
	seed                int64
	envHash             string
	startedEventID      string
}

// loadOriginal reads the proj_benchmark_results row + its joined
// benchmark_provenance in a single query. Returns sql.ErrNoRows when
// the original row is missing; an explicit "no provenance" error when
// the row exists but is missing its provenance pointer (pre-T6-cutover
// legacy rows aren't replayable).
func loadOriginal(ctx context.Context, pool *db.Pool, rowID string) (originalRow, error) {
	const q = `
		SELECT
			pb.scenario_id,
			pb.project_id,
			pb.wall_clock_ms,
			pb.input_tokens,
			pb.output_tokens,
			pb.tool_name,
			pb.model_name,
			pb.layer,
			pb.task_shape,
			pb.task_id,
			pb.run_shape,
			pb.accuracy_score,
			pb.honesty_score,
			pb.ranking_quality_score,
			pb.within_budget_score,
			pb.invocation_ok,
			pb.args_match,
			pb.extracted_args,
			pb.interpretation_ok,
			pb.detected_tool,
			pb.notes,
			pb.invoked_contextually,
			pb.provenance_id,
			bp.model_id,
			bp.model_version,
			bp.prompt_template_hash,
			bp.corpus_hash,
			bp.retriever_version,
			bp.retriever_config_hash,
			bp.seed,
			bp.env_hash,
			bp.started_event_id
		FROM proj_benchmark_results pb
		LEFT JOIN benchmark_provenance bp ON bp.id = pb.provenance_id
		WHERE pb.id = ?`

	row := pool.DB().QueryRowContext(ctx, q, rowID)

	var (
		r              originalRow
		inTok, outTok  sql.NullInt64
		argsMatch      sql.NullInt64
		interp         sql.NullInt64
		invoked        int64
		invokedCtx     int64
		layer          sql.NullString
		taskShape      sql.NullString
		taskID         sql.NullString
		runShape       sql.NullString
		acc            sql.NullFloat64
		hon            sql.NullFloat64
		ranking        sql.NullFloat64
		budget         sql.NullFloat64
		extracted      sql.NullString
		detected       sql.NullString
		notes          sql.NullString
		provenanceID   sql.NullString
		modelID        sql.NullString
		modelVersion   sql.NullString
		promptHash     sql.NullString
		corpusHash     sql.NullString
		retrieverVer   sql.NullString
		retrieverHash  sql.NullString
		seed           sql.NullInt64
		envHash        sql.NullString
		startedEventID sql.NullString
		toolName       string
		modelName      string
	)
	if err := row.Scan(
		&r.scenarioID, &r.projectID, &r.wallClockMS,
		&inTok, &outTok,
		&toolName, &modelName,
		&layer, &taskShape, &taskID, &runShape,
		&acc, &hon, &ranking, &budget,
		&invoked, &argsMatch, &extracted, &interp,
		&detected, &notes, &invokedCtx,
		&provenanceID,
		&modelID, &modelVersion, &promptHash, &corpusHash,
		&retrieverVer, &retrieverHash, &seed, &envHash,
		&startedEventID,
	); err != nil {
		return originalRow{}, err
	}

	if !provenanceID.Valid || provenanceID.String == "" {
		return originalRow{}, fmt.Errorf("benchmark row %q has no provenance (pre-T6-cutover legacy row); not replayable", rowID)
	}
	if !startedEventID.Valid {
		return originalRow{}, fmt.Errorf("benchmark row %q has no started_event_id", rowID)
	}

	r.inputTokens = nullableIntFromInt64(inTok)
	r.outputTokens = nullableIntFromInt64(outTok)
	r.modelID = nullableString(modelID)
	r.modelVersion = nullableString(modelVersion)
	r.promptTemplateHash = nullableString(promptHash)
	r.corpusHash = nullableString(corpusHash)
	r.retrieverVersion = nullableString(retrieverVer)
	r.retrieverConfigHash = nullableString(retrieverHash)
	r.seed = nullableInt64(seed)
	r.envHash = nullableString(envHash)
	r.startedEventID = startedEventID.String

	r.resultColumns = events.BenchmarkResultColumns{
		ToolName:            toolName,
		ModelName:           modelName,
		Layer:               nullableStringPtr(layer),
		TaskShape:           nullableStringPtr(taskShape),
		TaskID:              nullableStringPtr(taskID),
		RunShape:            nullableStringPtr(runShape),
		AccuracyScore:       nullableFloat64Ptr(acc),
		HonestyScore:        nullableFloat64Ptr(hon),
		RankingQualityScore: nullableFloat64Ptr(ranking),
		WithinBudgetScore:   nullableFloat64Ptr(budget),
		InvocationOK:        invoked != 0,
		ArgsMatch:           nullableBoolPtr(argsMatch),
		ExtractedArgs:       nullableStringPtr(extracted),
		InterpretationOK:    nullableBoolPtr(interp),
		DetectedTool:        nullableStringPtr(detected),
		Notes:               nullableStringPtr(notes),
		InvokedContextually: invokedCtx != 0,
	}

	return r, nil
}

func newReplayID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ── sql.Null* projections ──────────────────────────────────────────────

func nullableString(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

func nullableInt64(v sql.NullInt64) int64 {
	if !v.Valid {
		return 0
	}
	return v.Int64
}

func nullableStringPtr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

func nullableFloat64Ptr(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}

func nullableBoolPtr(v sql.NullInt64) *bool {
	if !v.Valid {
		return nil
	}
	b := v.Int64 != 0
	return &b
}

func nullableIntFromInt64(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}
