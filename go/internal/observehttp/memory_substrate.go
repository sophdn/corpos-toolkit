package observehttp

import (
	"context"
	"database/sql"
	"net/http"
)

// MemoryKindCount is one (key, count) row of the memory-substrate kind /
// source distributions.
type MemoryKindCount struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

// MemoryRatePoint is a per-day MemoryWritten event count for the
// event-rate timeseries (oldest day first).
type MemoryRatePoint struct {
	Day   string `json:"day"`
	Count int64  `json:"count"`
}

// MemorySubstrateStats is the telemetry payload for the vault-mediated
// memory substrate (chain memory-substrate-within-vault), sourced from
// proj_memories + MemoryWritten events. Deliberate first pass ahead of the
// planned telemetry-unification push: it favours a working, tested read of
// the real data over final information architecture, so it stays
// DB-derived (no harness-dir filesystem reconciliation here).
type MemorySubstrateStats struct {
	// TotalMemories is the live proj_memories row count — one per
	// vault/memory/<kind>/<name>.md entry.
	TotalMemories int64 `json:"total_memories"`
	// ByKind is the user / feedback / project / reference distribution.
	ByKind []MemoryKindCount `json:"by_kind"`
	// MemoryWrittenTotal is the lifetime MemoryWritten event count. It
	// exceeds TotalMemories because edits and since-deleted entries also
	// emitted (e.g. the migration re-forged entries later pruned).
	MemoryWrittenTotal int64 `json:"memory_written_total"`
	// BySource breaks MemoryWritten events down by their `source`
	// discriminator (migration / manual / user-correction / unset) — the
	// migration-vs-real-work split the substrate's validation surfaced.
	BySource []MemoryKindCount `json:"by_source"`
	// EventRate is the per-day MemoryWritten count, oldest day first.
	EventRate []MemoryRatePoint `json:"event_rate"`
	// ParseContextHits is the count of grounding_events that surfaced at
	// least one memory/* candidate in source_refs — the reach of
	// parse_context's memory-aware resolution.
	ParseContextHits int64 `json:"parse_context_hits"`
	// OldestFiledAt / NewestFiledAt bound the live corpus (empty when no
	// memories exist).
	OldestFiledAt string `json:"oldest_filed_at"`
	NewestFiledAt string `json:"newest_filed_at"`
}

// memorySubstrate serves GET /knowledge/memory-substrate — the
// memory-substrate telemetry card. Global (not project-scoped): user-kind
// memories are cross-project and the MemoryWritten / grounding signals are
// not cleanly per-project, so a single global view is the honest first
// pass.
func (s AppState) memorySubstrate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	db := s.Pool.DB()

	var stats MemorySubstrateStats
	var err error

	if stats.TotalMemories, err = scalarUnscoped(ctx, db,
		`SELECT COUNT(*) FROM proj_memories`); err != nil {
		dbErr(w, err)
		return
	}
	if stats.ByKind, err = memoryGrouped(ctx, db,
		`SELECT kind, COUNT(*) FROM proj_memories GROUP BY kind ORDER BY COUNT(*) DESC`); err != nil {
		dbErr(w, err)
		return
	}
	if stats.MemoryWrittenTotal, err = scalarUnscoped(ctx, db,
		`SELECT COUNT(*) FROM events WHERE type='MemoryWritten'`); err != nil {
		dbErr(w, err)
		return
	}
	if stats.BySource, err = memoryGrouped(ctx, db,
		`SELECT COALESCE(NULLIF(json_extract(payload,'$.source'), ''), '(unset)') AS source,
		        COUNT(*)
		   FROM events WHERE type='MemoryWritten'
		  GROUP BY source ORDER BY COUNT(*) DESC`); err != nil {
		dbErr(w, err)
		return
	}
	if stats.EventRate, err = memoryEventRate(ctx, db); err != nil {
		dbErr(w, err)
		return
	}
	if stats.ParseContextHits, err = scalarUnscoped(ctx, db,
		`SELECT COUNT(DISTINCT ge.id)
		   FROM grounding_events ge, json_each(ge.source_refs) je
		  WHERE je.value LIKE 'memory:%'`); err != nil {
		dbErr(w, err)
		return
	}

	var oldest, newest sql.NullString
	if err = db.QueryRowContext(ctx,
		`SELECT MIN(filed_at), MAX(filed_at) FROM proj_memories`).Scan(&oldest, &newest); err != nil {
		dbErr(w, err)
		return
	}
	stats.OldestFiledAt = oldest.String
	stats.NewestFiledAt = newest.String

	writeJSON(w, http.StatusOK, stats)
}

// memoryGrouped runs a two-column (key, count) aggregate and returns a
// non-nil slice so the JSON renders [] rather than null on an empty corpus.
func memoryGrouped(ctx context.Context, db *sql.DB, q string) ([]MemoryKindCount, error) {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MemoryKindCount{}
	for rows.Next() {
		var c MemoryKindCount
		if err := rows.Scan(&c.Key, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// memoryEventRate returns per-day MemoryWritten counts, oldest day first.
func memoryEventRate(ctx context.Context, db *sql.DB) ([]MemoryRatePoint, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT substr(ts, 1, 10) AS day, COUNT(*)
		   FROM events WHERE type='MemoryWritten'
		  GROUP BY day ORDER BY day`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MemoryRatePoint{}
	for rows.Next() {
		var p MemoryRatePoint
		if err := rows.Scan(&p.Day, &p.Count); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
