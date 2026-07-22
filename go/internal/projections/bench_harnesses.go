package projections

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"toolkit/internal/events"
)

// benchHarnesses folds BenchmarkForged events into the bench_harnesses table.
// Added chain 311 T7 Stage 6 P2-A (2026-05-29): event-sourcing bench so forge's
// direct INSERT (forge.createBenchInTx) is no longer the sole writer and forge
// can be archived. bench_harnesses pre-dated event-sourcing as a direct-write
// artifact table (migration 067; gate_metrics + updated_at in 076), and already
// carried the last_event_id / last_event_ts watermark columns in anticipation
// of this fold — so no schema migration is needed for the table itself.
//
// Grandfather contract. The bench payload was bumped in P2-A to carry
// flag_set_json + gate_metrics (the two columns the row needs that the
// pre-bump payload omitted). The two historical pre-bump BenchmarkForged events
// lack flag_set_json, so this fold SKIPS them (an empty flag_set_json marks a
// pre-bump event) — the live row they describe is reproduced by the
// migration-085 Option-A synthetic-event backfill instead (same shape as
// migration 058's proj_benchmark_results post-035 backfill). A from-empty
// rebuild therefore reconstructs the row from the synthetic full-payload event,
// not the grandfathered originals.
//
// Idempotency. The INSERT is ON CONFLICT(project_id, slug) DO NOTHING, mirroring
// forge's behavior where a re-forge of an existing slug skips the write and
// returns the existing identity untouched. Re-forge events (idempotent=true)
// therefore never overwrite a registered harness, fresh or replayed.
type benchHarnesses struct{}

func init() { Register(benchHarnesses{}) }

func (benchHarnesses) Name() string      { return "bench_harnesses" }
func (benchHarnesses) TableName() string { return "bench_harnesses" }

func (benchHarnesses) Fold(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	if evt.EntityKind != "bench" || evt.Type != "BenchmarkForged" {
		return nil
	}
	var p events.BenchmarkForgedPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("bench_harnesses %s payload: %w", evt.EventID, err)
	}
	if p.FlagSetJSON == "" {
		// Pre-bump (pre-P2-A) BenchmarkForged event: it predates flag_set_json,
		// so the row can't be reconstructed from it. Skip — the live row is
		// reproduced by the migration-085 synthetic backfill (see the type doc).
		return nil
	}
	project := ""
	if evt.EntityProjectID != nil {
		project = *evt.EntityProjectID
	}
	timeoutMs := 60000
	if p.TimeoutMs != nil {
		timeoutMs = *p.TimeoutMs
	}
	parseOutputAs := p.ParseOutputAs
	if parseOutputAs == "" {
		parseOutputAs = "json"
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO bench_harnesses (
			project_id, slug, binary_path, flag_set_json,
			baseline_json_path, parse_output_as, timeout_ms,
			gate_metrics, created_at, updated_at,
			last_event_id, last_event_ts
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_id, slug) DO NOTHING`,
		project, p.Slug, p.BinaryPath, p.FlagSetJSON,
		p.BaselineJSONPath, parseOutputAs, timeoutMs,
		p.GateMetrics, evt.Ts, evt.Ts,
		evt.EventID, evt.Ts,
	)
	return err
}

// RebuildFromEmpty replays every BenchmarkForged event (entity_kind='bench')
// in ts order through Fold. Pre-bump events self-skip (empty flag_set_json);
// the migration-085 synthetic backfill carries the full payload for any row
// that existed before P2-A, so the rebuilt table converges on the live state.
func (benchHarnesses) RebuildFromEmpty(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug,
		       entity_project_id, payload, rationale, caused_by_event_id,
		       related_entities, span_id, schema_version
		FROM events
		WHERE entity_kind = 'bench' AND type = 'BenchmarkForged'
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
		if err := (benchHarnesses{}).Fold(ctx, tx, evt); err != nil {
			return fmt.Errorf("rebuild bench_harnesses fold %s: %w", evt.EventID, err)
		}
	}
	return rows.Err()
}
