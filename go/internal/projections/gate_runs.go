package projections

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"toolkit/internal/events"
)

// gateRuns folds GateRunCompleted events into the gate-run projection pair:
// proj_gate_runs (parent, one row per run) + proj_gate_check_results (child,
// one row per executed check). See go/internal/db/migrations/089_proj_gate_runs.sql
// for the design + the dedicated-tables-vs-forge-schema decision. The event is
// cross-cutting (entity_kind='gate_run', envelope project_id null); project
// rides in the payload so the fold can populate the namespaced columns.
//
// The parent uses ON CONFLICT(id) DO UPDATE so re-folding an event is
// idempotent; the child rows have no natural key beyond execution order, so
// the fold DELETEs every existing check row for the run and re-INSERTs the
// payload grid — also idempotent. That idempotency is what makes
// RebuildFromEmpty byte-identical to the incremental fold. The parent id is
// the event id (each gate run is exactly one event).
type gateRuns struct{}

func init() { Register(gateRuns{}) }

func (gateRuns) Name() string      { return "gate_runs" }
func (gateRuns) TableName() string { return "proj_gate_runs" }

func (gateRuns) Fold(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	// Literal-filter contract: the writer (gate_run handler) emits exactly
	// this entity kind via events.NewCrossCuttingEntityRef("gate_run", …).
	if evt.EntityKind != "gate_run" {
		return nil
	}
	if evt.Type == "GateRunCompleted" {
		return foldGateRunCompleted(ctx, tx, evt)
	}
	return nil
}

// RebuildFromEmpty replays GateRunCompleted events. It deletes the child check
// table up front (RebuildAll's TRUNCATE only clears TableName() — the parent —
// and the shared DSN runs with foreign_keys off, so the ON DELETE CASCADE
// doesn't fire) so a targeted rebuild starts from a genuinely empty pair.
func (gateRuns) RebuildFromEmpty(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM proj_gate_check_results`); err != nil {
		return fmt.Errorf("truncate proj_gate_check_results: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM proj_gate_runs`); err != nil {
		return fmt.Errorf("truncate proj_gate_runs: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug,
		       entity_project_id, payload, rationale, caused_by_event_id,
		       related_entities, span_id, schema_version
		FROM events
		WHERE entity_kind = 'gate_run'
		  AND type = 'GateRunCompleted'
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
		if err := (gateRuns{}).Fold(ctx, tx, evt); err != nil {
			return fmt.Errorf("rebuild gate_run fold %s: %w", evt.EventID, err)
		}
	}
	return rows.Err()
}

func foldGateRunCompleted(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	var p events.GateRunCompletedPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("gate_run %s payload: %w", evt.EventID, err)
	}
	if p.Project == "" {
		return fmt.Errorf("gate_run %s: project is required in the payload", evt.EventID)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO proj_gate_runs (
			id, project_id, commit_sha, tier, overall_ok, coverage_pct,
			branch_pct, mutation_score, duration_ms, ran_at,
			last_event_id, last_event_ts
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			project_id = excluded.project_id,
			commit_sha = excluded.commit_sha,
			tier = excluded.tier,
			overall_ok = excluded.overall_ok,
			coverage_pct = excluded.coverage_pct,
			branch_pct = excluded.branch_pct,
			mutation_score = excluded.mutation_score,
			duration_ms = excluded.duration_ms,
			ran_at = excluded.ran_at,
			last_event_id = excluded.last_event_id,
			last_event_ts = excluded.last_event_ts`,
		evt.EventID, p.Project, p.CommitSHA, p.Tier, boolToInt(p.OverallOK),
		p.CoveragePct, p.BranchPct, p.MutationScore, p.DurationMS, evt.Ts,
		evt.EventID, evt.Ts,
	); err != nil {
		return fmt.Errorf("gate_run %s parent upsert: %w", evt.EventID, err)
	}

	// Child check grid: DELETE-then-INSERT keeps re-folds idempotent.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM proj_gate_check_results WHERE run_id = ?`, evt.EventID); err != nil {
		return fmt.Errorf("gate_run %s clear checks: %w", evt.EventID, err)
	}
	for i, c := range p.Checks {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO proj_gate_check_results (
				run_id, project_id, run_seq, name, tier, ok, skipped,
				duration_ms, note, last_event_id, last_event_ts
			) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			evt.EventID, p.Project, i, c.Name, c.Tier, boolToInt(c.OK),
			boolToInt(c.Skipped), c.DurationMS, c.Note, evt.EventID, evt.Ts,
		); err != nil {
			return fmt.Errorf("gate_run %s check insert: %w", evt.EventID, err)
		}
	}
	return nil
}

// boolToInt maps a Go bool to the 1/0 INTEGER convention the proj_gate_*
// tables use for their boolean columns.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
