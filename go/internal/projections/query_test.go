package projections_test

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/projections"
	"toolkit/internal/telemetry"
	"toolkit/internal/testutil"
)

// ── proj_query_volume_by_source ───────────────────────────────────────

func TestQueryVolumeBySource_Rebuild(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := "2026-05-17T03:00:00Z"
	seedGroundingEvents(t, pool, []groundingSeed{
		{project: "mcp-servers", action: "vault_search", source: "agent_initiated",
			created: now, results: 5, sourceRefs: `["a","b"]`},
		{project: "mcp-servers", action: "vault_search", source: "agent_initiated",
			created: now, results: 0, sourceRefs: `[]`},
		{project: "mcp-servers", action: "kiwix_search", source: "proactive_hook",
			created: now, results: 3, sourceRefs: `["x"]`},
	})

	mustRebuildAll(t, pool, []string{"query_volume_by_source"})

	rows := dumpVolumeRows(t, pool)
	want := map[string]volumeRow{
		"mcp-servers|vault_search|agent_initiated|2026-05-17": {
			QueryCount: 2, ZeroResultCount: 1, SuccessCount: 0, AvgResults: 2.5,
		},
		"mcp-servers|kiwix_search|proactive_hook|2026-05-17": {
			QueryCount: 1, ZeroResultCount: 0, SuccessCount: 0, AvgResults: 3.0,
		},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %d, want %d: %+v", len(rows), len(want), rows)
	}
	for k, v := range want {
		got, ok := rows[k]
		if !ok {
			t.Errorf("missing bucket %s", k)
			continue
		}
		if got != v {
			t.Errorf("bucket %s = %+v, want %+v", k, got, v)
		}
	}
}

// TestQueryVolumeBySource_SuccessCountReflectsInteractions checks the
// success_count column is the COUNT(DISTINCT grounding_event_id) of rows
// with at least one `followed` or `resolved-from` interaction.
func TestQueryVolumeBySource_SuccessCountReflectsInteractions(t *testing.T) {
	pool := testutil.NewTestDB(t)
	geID := seedOneGrounding(t, pool, "mcp-servers", "vault_search", "agent_initiated",
		"2026-05-17T03:00:00Z", 4, `["a","b","c"]`)
	seedInteraction(t, pool, geID, "a", telemetry.ClickFollowed, "span-1")

	mustRebuildAll(t, pool, []string{"query_volume_by_source"})

	rows := dumpVolumeRows(t, pool)
	want := volumeRow{QueryCount: 1, ZeroResultCount: 0, SuccessCount: 1, AvgResults: 4.0}
	got := rows["mcp-servers|vault_search|agent_initiated|2026-05-17"]
	if got != want {
		t.Errorf("bucket = %+v, want %+v", got, want)
	}
}

// TestQueryVolumeBySource_PopulatesLastEvent pins the population fix for
// bug query-telemetry-projections-hardcode-empty-last-event-id-ts: the
// writer hardcoded " for last_event_id/ts on every row, masking the gap
// as populated-with-blank. The aggregated bucket must carry its MOST-
// RECENT grounding_event's id + created_at (MAX over the group). Sibling
// of the reranker fix (881 / 5c8fd43b).
func TestQueryVolumeBySource_PopulatesLastEvent(t *testing.T) {
	pool := testutil.NewTestDB(t)
	const created = "2026-05-17T03:00:00Z"
	geID := seedOneGrounding(t, pool, "mcp-servers", "vault_search", "agent_initiated",
		created, 4, `["a"]`)

	mustRebuildAll(t, pool, []string{"query_volume_by_source"})

	var lastEventID, lastEventTs string
	if err := pool.DB().QueryRow(`
		SELECT last_event_id, last_event_ts FROM proj_query_volume_by_source
		 WHERE project_id='mcp-servers' AND action='vault_search' AND query_source='agent_initiated'`,
	).Scan(&lastEventID, &lastEventTs); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if lastEventTs != created {
		t.Errorf("last_event_ts = %q, want %q (bucket's MAX grounding_event created_at)", lastEventTs, created)
	}
	if lastEventTs == "" {
		t.Errorf("last_event_ts is empty — the masking-default gap regressed")
	}
	if lastEventID != fmt.Sprintf("%d", geID) {
		t.Errorf("last_event_id = %q, want %d (bucket's MAX grounding_event id)", lastEventID, geID)
	}
}

// ── proj_retrieval_success_per_query ──────────────────────────────────

// TestRetrievalSuccessPerQuery_PopulatesLastEvent: same bug as above, the
// per-query (one row per grounding_event) projection. last_event_id/ts
// must carry the row's source grounding_event id + created_at directly.
func TestRetrievalSuccessPerQuery_PopulatesLastEvent(t *testing.T) {
	pool := testutil.NewTestDB(t)
	const created = "2026-05-17T03:00:00Z"
	geID := seedOneGrounding(t, pool, "mcp-servers", "vault_search", "agent_initiated",
		created, 5, `["a","b"]`)

	mustRebuildAll(t, pool, []string{"retrieval_success_per_query"})

	var lastEventID, lastEventTs string
	if err := pool.DB().QueryRow(`
		SELECT last_event_id, last_event_ts FROM proj_retrieval_success_per_query
		 WHERE grounding_event_id = ?`, geID,
	).Scan(&lastEventID, &lastEventTs); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if lastEventTs != created {
		t.Errorf("last_event_ts = %q, want %q (source grounding_event created_at)", lastEventTs, created)
	}
	if lastEventID != fmt.Sprintf("%d", geID) {
		t.Errorf("last_event_id = %q, want %d (source grounding_event id)", lastEventID, geID)
	}
}

func TestRetrievalSuccessPerQuery_FoldEmitsRowPerEvent(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ge1 := seedOneGrounding(t, pool, "mcp-servers", "vault_search", "agent_initiated",
		"2026-05-17T03:00:00Z", 5, `["a","b"]`)
	ge2 := seedOneGrounding(t, pool, "mcp-servers", "kiwix_search", "proactive_hook",
		"2026-05-17T03:01:00Z", 0, `[]`)
	seedInteraction(t, pool, ge1, "a", telemetry.ClickFollowed, "span-1")
	seedInteraction(t, pool, ge1, "a", telemetry.ClickMentioned, "span-1")

	mustRebuildAll(t, pool, []string{"retrieval_success_per_query"})

	var (
		ge1Followed, ge1Mentioned, ge1Cited, ge1Resolved int
		ge1Success, ge1Proactive                         int
		ge1MaxWeight                                     float64
		ge1Kinds                                         string
	)
	if err := pool.DB().QueryRow(`
		SELECT had_followed, had_cited, had_mentioned, had_resolved_from,
		       max_click_weight, kinds_fired, success, was_proactive
		FROM proj_retrieval_success_per_query
		WHERE grounding_event_id = ?`, ge1).Scan(
		&ge1Followed, &ge1Cited, &ge1Mentioned, &ge1Resolved,
		&ge1MaxWeight, &ge1Kinds, &ge1Success, &ge1Proactive,
	); err != nil {
		t.Fatalf("scan ge1: %v", err)
	}
	if ge1Followed != 1 || ge1Mentioned != 1 || ge1Cited != 0 || ge1Resolved != 0 {
		t.Errorf("ge1 flags = followed=%d cited=%d mentioned=%d resolved-from=%d",
			ge1Followed, ge1Cited, ge1Mentioned, ge1Resolved)
	}
	if ge1MaxWeight < 0.99 || ge1MaxWeight > 1.01 {
		t.Errorf("ge1 max_click_weight = %v, want ~1.0", ge1MaxWeight)
	}
	if ge1Success != 1 {
		t.Errorf("ge1 success = %d, want 1", ge1Success)
	}
	if ge1Proactive != 0 {
		t.Errorf("ge1 was_proactive = %d, want 0", ge1Proactive)
	}
	if !strings.Contains(ge1Kinds, "followed") || !strings.Contains(ge1Kinds, "mentioned") {
		t.Errorf("ge1 kinds_fired = %q, want followed+mentioned", ge1Kinds)
	}

	// ge2: zero-results query, no interactions; row still lands with all
	// flags zero and was_proactive=1 (query_source=proactive_hook).
	var ge2Success, ge2Proactive int
	if err := pool.DB().QueryRow(`
		SELECT success, was_proactive FROM proj_retrieval_success_per_query
		WHERE grounding_event_id = ?`, ge2).Scan(&ge2Success, &ge2Proactive); err != nil {
		t.Fatalf("scan ge2: %v", err)
	}
	if ge2Success != 0 || ge2Proactive != 1 {
		t.Errorf("ge2 success=%d was_proactive=%d, want 0/1", ge2Success, ge2Proactive)
	}
}

// ── proj_training_data_for_reranker ───────────────────────────────────

// TestTrainingData_FiveLabelKinds drives one ingest cycle producing one
// row per label_kind value. Asserts the TT1.5 5-value enum classifier
// works as documented in docs/TELEMETRY_LABEL_SPIKE.md §5.
func TestTrainingData_FiveLabelKinds(t *testing.T) {
	pool := testutil.NewTestDB(t)

	// ge_a: results_count=5 with 4 source_refs (a, b, c, d).
	//   a → followed → max_weight=1.0    → positive
	//   b → mentioned → max_weight=0.4   → weakly_positive
	//   c (position 3) → no tier         → hard_negative (results_count=5, position<=3)
	//   d (position 4) → no tier         → negative (position<=10)
	geA := seedOneGrounding(t, pool, "mcp-servers", "vault_search", "agent_initiated",
		"2026-05-17T03:00:00Z", 5, `["a","b","c","d"]`)
	seedInteraction(t, pool, geA, "a", telemetry.ClickFollowed, "span-A")
	seedInteraction(t, pool, geA, "b", telemetry.ClickMentioned, "span-A")

	// ge_b: results_count=3 with one source_ref. position=1 but
	// results_count<5 means hard_negative does NOT fire — falls to
	// negative because position<=10.
	geB := seedOneGrounding(t, pool, "mcp-servers", "vault_search", "agent_initiated",
		"2026-05-17T03:01:00Z", 3, `["e"]`)

	mustRebuildAll(t, pool, []string{"training_data_for_reranker"})

	rows := dumpTrainingRows(t, pool)
	want := map[string]string{
		mkPair(geA, "a"): "positive",
		mkPair(geA, "b"): "weakly_positive",
		mkPair(geA, "c"): "hard_negative",
		mkPair(geA, "d"): "negative",
		mkPair(geB, "e"): "negative",
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %d, want %d: %v", len(rows), len(want), rows)
	}
	for k, v := range want {
		if rows[k] != v {
			t.Errorf("pair %s label_kind = %q, want %q", k, rows[k], v)
		}
	}
}

// Regression for bug `reranker-projection-last-event-ts-never-populated`.
// last_event_ts was hardcoded " on every projection row (the SQL wrote a
// literal empty string), masking the never-written gap and blocking
// chain 272 T1's most-recent-~15% time-based held-out split. The writer
// now populates it from the source grounding_event's real created_at and
// last_event_id from the grounding_event id.
func TestTrainingData_PopulatesLastEventTs(t *testing.T) {
	pool := testutil.NewTestDB(t)
	const createdAt = "2026-05-18T07:30:00Z"
	geID := seedOneGrounding(t, pool, "mcp-servers", "vault_search", "agent_initiated",
		createdAt, 1, `["a"]`)

	mustRebuildAll(t, pool, []string{"training_data_for_reranker"})

	var lastEventTs, lastEventID string
	if err := pool.DB().QueryRow(`
		SELECT last_event_ts, last_event_id FROM proj_training_data_for_reranker
		 WHERE grounding_event_id = ? AND source_ref = ?`,
		geID, "a").Scan(&lastEventTs, &lastEventID); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if lastEventTs != createdAt {
		t.Errorf("last_event_ts = %q, want %q (must carry the grounding event's real created_at)", lastEventTs, createdAt)
	}
	if lastEventTs == "" {
		t.Errorf("last_event_ts is empty — the masking-default gap regressed")
	}
	if lastEventID != fmt.Sprintf("%d", geID) {
		t.Errorf("last_event_id = %q, want %d (the source grounding_event id)", lastEventID, geID)
	}
}

// TestTrainingData_RoundTripPositiveSatisfiesInvariants is the chain
// substrate-health-audit-projections T3 acceptance test: emit a synthetic
// grounding event + a positive label and assert the resulting projection
// row satisfies every population invariant the chain locks — query_text
// AND last_event_ts populated on the positive row (the class the original
// bug `reranker-projection-drops-query-text-on-positive-labels` dropped).
func TestTrainingData_RoundTripPositiveSatisfiesInvariants(t *testing.T) {
	pool := testutil.NewTestDB(t)
	const createdAt = "2026-05-23T11:00:00Z"
	geID := seedOneGrounding(t, pool, "mcp-servers", "vault_search", "agent_initiated",
		createdAt, 1, `["cand"]`)
	// followed → click_weight 1.0 → label_kind positive
	seedInteraction(t, pool, geID, "cand", telemetry.ClickFollowed, "span-rt")

	mustRebuildAll(t, pool, []string{"training_data_for_reranker"})

	var queryText, lastEventTs, labelKind string
	if err := pool.DB().QueryRow(`
		SELECT query_text, last_event_ts, label_kind
		  FROM proj_training_data_for_reranker
		 WHERE grounding_event_id = ? AND source_ref = ?`, geID, "cand").
		Scan(&queryText, &lastEventTs, &labelKind); err != nil {
		t.Fatalf("scan positive row: %v", err)
	}
	if queryText == "" {
		t.Error("query_text empty on positive row — invariant violated")
	}
	if lastEventTs != createdAt {
		t.Errorf("last_event_ts = %q, want %q (the source grounding event's created_at)", lastEventTs, createdAt)
	}
	if labelKind != "positive" {
		t.Errorf("label_kind = %q, want positive", labelKind)
	}
}

// TestTrainingData_DropsNullAndEmptyQueryRows pins the writer-side half of
// the T3 invariant: a (query, candidate, label) row with no query is
// untrainable, so the writer must DROP grounding events that carry no
// query_text (the 458 legacy processor-created rows, across all label
// classes) rather than emit rows that would trip migration 071's
// query_text NOT NULL CHECK on every rebuild. Not a backfill — excluded.
func TestTrainingData_DropsNullAndEmptyQueryRows(t *testing.T) {
	pool := testutil.NewTestDB(t)

	// (a) query-bearing grounding event → lands in the projection.
	geGood := seedOneGrounding(t, pool, "mcp-servers", "vault_search", "agent_initiated",
		"2026-05-23T10:00:00Z", 1, `["good"]`)

	// (b) NULL-query grounding event (mirrors a processor-created row that
	// never had query_text extracted) and (c) empty-string query — both
	// must be DROPPED by the writer.
	for _, seed := range []struct {
		callID, sourceRefs, created string
		qt                          sql.NullString
	}{
		{"toolu_nullq", `["nullq"]`, "2026-05-23T10:01:00Z", sql.NullString{}},
		{"toolu_emptyq", `["emptyq"]`, "2026-05-23T10:02:00Z", sql.NullString{String: "", Valid: true}},
	} {
		if _, err := pool.DB().Exec(`
			INSERT INTO grounding_events
				(project_id, session_id, call_id, action, results_count,
				 source_refs, next_turn_has_output, span_id, query_source, created_at, query_text)
			VALUES ('mcp-servers','sess-seed',?,'vault_search',1,?,0,?,'agent_initiated',?,?)`,
			seed.callID, seed.sourceRefs, "span-"+seed.callID, seed.created, seed.qt); err != nil {
			t.Fatalf("seed %s: %v", seed.callID, err)
		}
	}

	mustRebuildAll(t, pool, []string{"training_data_for_reranker"})

	var goodCount, untrainableCount int
	pool.DB().QueryRow(`SELECT COUNT(*) FROM proj_training_data_for_reranker WHERE grounding_event_id = ?`, geGood).Scan(&goodCount)
	if goodCount == 0 {
		t.Error("query-bearing grounding event produced 0 training rows, want >= 1")
	}
	// No row anywhere has NULL/empty query_text (the writer filtered the
	// untrainable source events out entirely).
	pool.DB().QueryRow(`SELECT COUNT(*) FROM proj_training_data_for_reranker WHERE query_text IS NULL OR query_text = ''`).Scan(&untrainableCount)
	if untrainableCount != 0 {
		t.Errorf("found %d projection rows with NULL/empty query_text, want 0 (writer must drop untrainable source events)", untrainableCount)
	}
}

// TestTrainingData_SurvivesParallelKnowledgePointers pins the chain
// 617 T1 substrate alignment / dup-pointer regression. Two
// knowledge_pointers rows sharing source_ref but differing on
// project_id (the historical drift `vault` vs `general` for
// learnings/general/* files) used to make the projection rebuild fail
// with UNIQUE constraint on (grounding_event_id, source_ref) because
// the LEFT JOIN multiplied rows. The dedup'd subquery now collapses
// to lowest-id-wins; the rebuild succeeds and the candidate_pointer_id
// resolves deterministically to one of the parallel rows.
func TestTrainingData_SurvivesParallelKnowledgePointers(t *testing.T) {
	pool := testutil.NewTestDB(t)

	geID := seedOneGrounding(t, pool, "mcp-servers", "vault_search", "agent_initiated",
		"2026-05-20T03:00:00Z", 1, `["learnings/general/2026-05-07_dup-target.md"]`)

	// Seed two parallel knowledge_pointers rows with the same source_ref
	// but different project_id values — replays the drift shape from
	// chain 617 (`vault` from the legacy seeder + `general` from the
	// post-rework forge).
	for i, projectID := range []string{"vault", "general"} {
		_, err := pool.DB().Exec(`
			INSERT INTO knowledge_pointers (
				project_id, source_type, source_ref, slug,
				question, invoke_when, tags
			) VALUES (?, 'vault', 'learnings/general/2026-05-07_dup-target.md',
			          ?, 'q', 'iw', '[]')`,
			projectID, fmt.Sprintf("dup-target-%d", i))
		if err != nil {
			t.Fatalf("seed pointer %d: %v", i, err)
		}
	}

	// Rebuild must NOT trip the UNIQUE constraint on the projection.
	mustRebuildAll(t, pool, []string{"training_data_for_reranker"})

	// Exactly one projection row should land per (grounding_event_id,
	// source_ref) pair regardless of how many pointers shared the ref.
	var count int
	if err := pool.DB().QueryRow(`
		SELECT COUNT(*) FROM proj_training_data_for_reranker
		 WHERE grounding_event_id = ? AND source_ref = ?`,
		geID, "learnings/general/2026-05-07_dup-target.md").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("projection rows for dup'd source_ref = %d, want 1", count)
	}
}

// TestTrainingData_SurvivesDuplicateSourceRefInArray pins the regression
// for the reranker-projection rebuild emptying itself when a grounding
// event's source_refs array carries the same source_ref more than once.
// This was observed when parse_context surfaced a memory file as several
// identical candidates because the harness MEMORY.md index carried
// duplicate lines for it. Grouping by the array index used to split the
// duplicate into rows that collide on UNIQUE(grounding_event_id,
// source_ref), aborting the full rebuild and zeroing the projection. The
// rebuild now collapses duplicate refs to one row at the earliest position.
func TestTrainingData_SurvivesDuplicateSourceRefInArray(t *testing.T) {
	pool := testutil.NewTestDB(t)
	// Same source_ref three times in the array (indices 0, 1, 2).
	geID := seedOneGrounding(t, pool, "mcp-servers", "vault_search", "agent_initiated",
		"2026-05-24T03:00:00Z", 1, `["memory:dup.md","memory:dup.md","memory:dup.md"]`)

	// Rebuild must NOT trip the UNIQUE constraint or empty the projection.
	mustRebuildAll(t, pool, []string{"training_data_for_reranker"})

	// Exactly one row for the duplicated pair, at the earliest position.
	var count, pos int
	if err := pool.DB().QueryRow(`
		SELECT COUNT(*), COALESCE(MIN(candidate_position), -1)
		  FROM proj_training_data_for_reranker
		 WHERE grounding_event_id = ? AND source_ref = ?`,
		geID, "memory:dup.md").Scan(&count, &pos); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("projection rows for 3x-duplicated source_ref = %d, want 1", count)
	}
	if pos != 1 {
		t.Errorf("candidate_position = %d, want 1 (earliest of the duplicate positions)", pos)
	}
}

// TestTrainingData_LabelSourcesArray asserts the label_sources column
// preserves every contributing click_kind per (grounding_event_id,
// source_ref) pair.
func TestTrainingData_LabelSourcesArray(t *testing.T) {
	pool := testutil.NewTestDB(t)
	geID := seedOneGrounding(t, pool, "mcp-servers", "vault_search", "agent_initiated",
		"2026-05-17T03:00:00Z", 1, `["a"]`)
	seedInteraction(t, pool, geID, "a", telemetry.ClickFollowed, "span-1")
	seedInteraction(t, pool, geID, "a", telemetry.ClickCited, "span-1")

	mustRebuildAll(t, pool, []string{"training_data_for_reranker"})

	var sources string
	if err := pool.DB().QueryRow(`SELECT label_sources FROM proj_training_data_for_reranker
		WHERE grounding_event_id = ? AND source_ref = ?`, geID, "a").Scan(&sources); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !strings.Contains(sources, "followed") || !strings.Contains(sources, "cited") {
		t.Errorf("label_sources = %q, want followed+cited", sources)
	}
}

// TestQueryProjections_FoldEqualsRebuild proves Fold == RebuildFromEmpty
// for the read-side projections — they're implemented as full re-snapshot
// so the byte-identical-rebuild invariant holds vacuously.
func TestQueryProjections_FoldEqualsRebuild(t *testing.T) {
	pool := testutil.NewTestDB(t)
	geID := seedOneGrounding(t, pool, "mcp-servers", "vault_search", "agent_initiated",
		"2026-05-17T03:00:00Z", 5, `["a","b"]`)
	seedInteraction(t, pool, geID, "a", telemetry.ClickFollowed, "span-1")

	mustRebuildAll(t, pool, []string{"query_volume_by_source", "retrieval_success_per_query", "training_data_for_reranker"})
	preVol := countRows(t, pool, "proj_query_volume_by_source")
	preSucc := countRows(t, pool, "proj_retrieval_success_per_query")
	preTrain := countRows(t, pool, "proj_training_data_for_reranker")

	// Drive Fold via FoldAllReadSide directly (telemetry-emit hook
	// invocation path) and confirm row counts converge.
	if err := pool.WithWrite(context.Background(), func(tx *sql.Tx) error {
		return projections.FoldAllReadSide(context.Background(), tx)
	}); err != nil {
		t.Fatalf("FoldAllReadSide: %v", err)
	}
	if got := countRows(t, pool, "proj_query_volume_by_source"); got != preVol {
		t.Errorf("query_volume_by_source: rebuild=%d, fold=%d", preVol, got)
	}
	if got := countRows(t, pool, "proj_retrieval_success_per_query"); got != preSucc {
		t.Errorf("retrieval_success_per_query: rebuild=%d, fold=%d", preSucc, got)
	}
	if got := countRows(t, pool, "proj_training_data_for_reranker"); got != preTrain {
		t.Errorf("training_data_for_reranker: rebuild=%d, fold=%d", preTrain, got)
	}
}

// TestQueryProjections_TelemetryEmitTriggersFold drives the production
// emit path with the fold hook wired (mirroring main.go's
// telemetry.SetFoldHook(projections.FoldAllReadSide) bootstrap). The
// emit lands the query_interactions row AND folds the read-side
// projections inside the same tx; the assertion is that
// proj_retrieval_success_per_query reflects the just-inserted row
// without a separate rebuild step.
func TestQueryProjections_TelemetryEmitTriggersFold(t *testing.T) {
	pool := testutil.NewTestDB(t)
	telemetry.SetFoldHook(projections.FoldAllReadSide)
	t.Cleanup(func() { telemetry.SetFoldHook(nil) })

	geID := seedOneGrounding(t, pool, "mcp-servers", "vault_search", "agent_initiated",
		"2026-05-17T03:00:00Z", 1, `["a"]`)
	if err := pool.WithWrite(context.Background(), func(tx *sql.Tx) error {
		_, err := telemetry.EmitInteraction(context.Background(), tx, telemetry.InteractionArgs{
			GroundingEventID: geID,
			SourceRef:        "a",
			ClickKind:        telemetry.ClickFollowed,
			SpanID:           "span-emit-1",
			SessionID:        "sess-emit-1",
			DetectedAt:       time.Now().UTC().Format(time.RFC3339Nano),
		})
		return err
	}); err != nil {
		t.Fatalf("EmitInteraction: %v", err)
	}
	var hadFollowed, success int
	if err := pool.DB().QueryRow(`
		SELECT had_followed, success FROM proj_retrieval_success_per_query
		WHERE grounding_event_id = ?`, geID).Scan(&hadFollowed, &success); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if hadFollowed != 1 || success != 1 {
		t.Errorf("after emit: had_followed=%d success=%d, want 1/1", hadFollowed, success)
	}
}

// ── helpers ───────────────────────────────────────────────────────────

type groundingSeed struct {
	project, action, source, created string
	results                          int
	sourceRefs                       string
}

func seedGroundingEvents(t *testing.T, pool *db.Pool, seeds []groundingSeed) {
	t.Helper()
	for i, s := range seeds {
		callID := callIDFor(i)
		// query_text is non-empty: migration 071 requires it on every
		// proj_training_data_for_reranker row and the writer filters
		// NULL/empty-query grounding events OUT, so seeds must carry a
		// query to land in the training projection.
		if _, err := pool.DB().ExecContext(context.Background(), `
			INSERT INTO grounding_events
				(project_id, session_id, call_id, action, results_count,
				 source_refs, next_turn_has_output, span_id, query_source, created_at, query_text)
			VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)`,
			s.project, "sess-seed", callID, s.action, s.results,
			s.sourceRefs, callID, s.source, s.created, "q:"+s.action,
		); err != nil {
			t.Fatalf("seed grounding[%d]: %v", i, err)
		}
	}
}

func seedOneGrounding(t *testing.T, pool *db.Pool, project, action, source, created string, results int, sourceRefs string) int64 {
	t.Helper()
	res, err := pool.DB().ExecContext(context.Background(), `
		INSERT INTO grounding_events
			(project_id, session_id, call_id, action, results_count,
			 source_refs, next_turn_has_output, span_id, query_source, created_at, query_text)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)`,
		project, "sess-seed", callIDFor(int(time.Now().UnixNano())),
		action, results, sourceRefs, "span-"+action+"-"+created, source, created, "q:"+action,
	)
	if err != nil {
		t.Fatalf("seed one grounding: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func seedInteraction(t *testing.T, pool *db.Pool, groundingID int64, sourceRef string, kind telemetry.ClickKind, spanID string) {
	t.Helper()
	weight := telemetry.DefaultClickWeights[kind]
	if _, err := pool.DB().ExecContext(context.Background(), `
		INSERT INTO query_interactions
			(grounding_event_id, source_ref, click_kind, click_weight,
			 span_id, session_id, detected_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		groundingID, sourceRef, string(kind), weight, spanID, "sess-seed",
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("seed interaction: %v", err)
	}
}

func mustRebuildAll(t *testing.T, pool *db.Pool, names []string) {
	t.Helper()
	if err := pool.WithWrite(context.Background(), func(tx *sql.Tx) error {
		_, err := projections.RebuildAll(context.Background(), tx, names)
		return err
	}); err != nil {
		t.Fatalf("RebuildAll(%v): %v", names, err)
	}
}

func countRows(t *testing.T, pool *db.Pool, table string) int64 {
	t.Helper()
	var n int64
	if err := pool.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

type volumeRow struct {
	QueryCount      int64
	ZeroResultCount int64
	SuccessCount    int64
	AvgResults      float64
}

func dumpVolumeRows(t *testing.T, pool *db.Pool) map[string]volumeRow {
	t.Helper()
	rows, err := pool.DB().Query(`
		SELECT project_id, action, query_source, day,
		       query_count, zero_result_count, success_count, avg_results_count
		FROM proj_query_volume_by_source`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	out := map[string]volumeRow{}
	for rows.Next() {
		var (
			p, a, s, d string
			vr         volumeRow
		)
		if err := rows.Scan(&p, &a, &s, &d, &vr.QueryCount, &vr.ZeroResultCount, &vr.SuccessCount, &vr.AvgResults); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[p+"|"+a+"|"+s+"|"+d] = vr
	}
	return out
}

func dumpTrainingRows(t *testing.T, pool *db.Pool) map[string]string {
	t.Helper()
	rows, err := pool.DB().Query(`
		SELECT grounding_event_id, source_ref, label_kind
		FROM proj_training_data_for_reranker`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var (
			ge   int64
			s, l string
		)
		if err := rows.Scan(&ge, &s, &l); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[mkPair(ge, s)] = l
	}
	return out
}

func callIDFor(i int) string {
	return "toolu_seed_" + time.Now().UTC().Format("150405.000000000") + "_" + strconv.Itoa(i)
}

func mkPair(ge int64, sourceRef string) string {
	return strconv.FormatInt(ge, 10) + "|" + sourceRef
}
