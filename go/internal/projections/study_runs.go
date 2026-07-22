package projections

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"toolkit/internal/events"
)

// studyRuns folds StudyRunRecorded events into the study-run projection
// pair: proj_study_runs (parent, one row per run) + proj_study_run_scores
// (child, one row per condition×run score cell). See
// go/internal/db/migrations/088_study_runs.sql for the design + the
// dedicated-tables-vs-forge-schema decision. The event is cross-cutting
// (entity_kind='study_run', envelope project_id null); project_id rides in
// the payload so the fold can populate the namespaced columns.
//
// The parent uses ON CONFLICT(id) DO UPDATE so re-folding an event is
// idempotent; the child rows have no natural key, so the fold DELETEs every
// existing score row for the run and re-INSERTs the payload grid — also
// idempotent. That idempotency is what makes RebuildFromEmpty byte-identical
// to the incremental fold.
type studyRuns struct{}

func init() { Register(studyRuns{}) }

func (studyRuns) Name() string      { return "study_runs" }
func (studyRuns) TableName() string { return "proj_study_runs" }

func (studyRuns) Fold(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	// Literal-filter contract: the writer (study_run_record handler) emits
	// exactly this entity kind via events.NewCrossCuttingEntityRef("study_run", …).
	if evt.EntityKind != "study_run" {
		return nil
	}
	if evt.Type == "StudyRunRecorded" {
		return foldStudyRunRecorded(ctx, tx, evt)
	}
	return nil
}

// RebuildFromEmpty replays StudyRunRecorded events. It deletes the child
// score table up front (RebuildAll's TRUNCATE only clears TableName() — the
// parent — and the shared DSN runs with foreign_keys off, so the ON DELETE
// CASCADE doesn't fire) so a targeted rebuild starts from a genuinely empty
// pair.
func (studyRuns) RebuildFromEmpty(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM proj_study_run_scores`); err != nil {
		return fmt.Errorf("truncate proj_study_run_scores: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM proj_study_runs`); err != nil {
		return fmt.Errorf("truncate proj_study_runs: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug,
		       entity_project_id, payload, rationale, caused_by_event_id,
		       related_entities, span_id, schema_version
		FROM events
		WHERE entity_kind = 'study_run'
		  AND type = 'StudyRunRecorded'
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
		if err := (studyRuns{}).Fold(ctx, tx, evt); err != nil {
			return fmt.Errorf("rebuild study_run fold %s: %w", evt.EventID, err)
		}
	}
	return rows.Err()
}

func foldStudyRunRecorded(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	var p events.StudyRunRecordedPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("study_run %s payload: %w", evt.EventID, err)
	}
	if p.RunID == "" || p.ProjectID == "" {
		return fmt.Errorf("study_run %s: run_id and project_id are required in the payload", evt.EventID)
	}

	// materials_hashes → JSON object text. nil map serialises as "null";
	// normalise to "{}" so the column always holds a valid JSON object.
	materialsJSON := "{}"
	if p.MaterialsHashes != nil {
		b, err := json.Marshal(p.MaterialsHashes)
		if err != nil {
			return fmt.Errorf("study_run %s materials_hashes: %w", evt.EventID, err)
		}
		materialsJSON = string(b)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO proj_study_runs (
			id, project_id, name, assay, item_id, image_ref, image_digest,
			study_digest, materials_hash_json, model_id, model_version,
			status, error, responses_dir, run_at, last_event_id, last_event_ts
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			project_id = excluded.project_id,
			name = excluded.name,
			assay = excluded.assay,
			item_id = excluded.item_id,
			image_ref = excluded.image_ref,
			image_digest = excluded.image_digest,
			study_digest = excluded.study_digest,
			materials_hash_json = excluded.materials_hash_json,
			model_id = excluded.model_id,
			model_version = excluded.model_version,
			status = excluded.status,
			error = excluded.error,
			responses_dir = excluded.responses_dir,
			run_at = excluded.run_at,
			last_event_id = excluded.last_event_id,
			last_event_ts = excluded.last_event_ts`,
		p.RunID, p.ProjectID, p.Name, p.Assay, p.ItemID, p.Image, p.ImageDigest,
		p.StudyDigest, materialsJSON, p.ModelID, p.ModelVersion,
		p.Status, p.Error, p.ResponsesDir, p.RunAt, evt.EventID, evt.Ts,
	); err != nil {
		return fmt.Errorf("study_run %s parent upsert: %w", evt.EventID, err)
	}

	// Child score grid: DELETE-then-INSERT keeps re-folds idempotent.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM proj_study_run_scores WHERE run_id = ?`, p.RunID); err != nil {
		return fmt.Errorf("study_run %s clear scores: %w", evt.EventID, err)
	}
	for _, row := range p.Rows {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO proj_study_run_scores (
				run_id, project_id, condition, run_idx, verdict_kind,
				verdict_reason, item, rationale, last_event_id, last_event_ts
			) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			p.RunID, p.ProjectID, row.Condition, row.Run, row.VerdictKind,
			row.VerdictReason, row.Item, row.Rationale, evt.EventID, evt.Ts,
		); err != nil {
			return fmt.Errorf("study_run %s score insert: %w", evt.EventID, err)
		}
	}
	return nil
}
