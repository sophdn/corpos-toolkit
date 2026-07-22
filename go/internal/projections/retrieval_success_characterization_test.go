package projections_test

import (
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/telemetry"
	"toolkit/internal/testutil"
)

// Chain 2 (success-model-unification) net densification for the RAG side.
// proj_retrieval_success_per_query.success is
//
//	max_click_weight >= 0.8 OR had_resolved_from = 1
//
// — a two-arm OR. The existing FoldEmitsRowPerEvent test exercises only the
// followed case (weight 1.0, which satisfies BOTH arms) and the empty case
// (no interaction → 0), so neither the >= 0.8 boundary nor the resolved-from
// rescue arm is pinned in isolation. Chain 2 reconciles this definition with
// the inference predicate registry, so each arm must be characterized first.
// All describe CURRENT behavior; none may be edited to make the unification pass.

// seedInteractionWithWeight inserts one query_interactions row with an
// EXPLICIT click_weight (the package's seedInteraction always uses the tier
// default, which can't express a resolved-from row weighted below 0.8).
func seedInteractionWithWeight(t *testing.T, pool *db.Pool, groundingID int64, sourceRef string, kind telemetry.ClickKind, weight float64, spanID string) {
	t.Helper()
	if _, err := pool.DB().Exec(`
		INSERT INTO query_interactions
			(grounding_event_id, source_ref, click_kind, click_weight,
			 span_id, session_id, detected_at)
		VALUES (?, ?, ?, ?, ?, 'sess-seed', datetime('now'))`,
		groundingID, sourceRef, string(kind), weight, spanID,
	); err != nil {
		t.Fatalf("seed interaction (%s @ %v): %v", kind, weight, err)
	}
}

func retrievalSuccessRow(t *testing.T, pool *db.Pool, geID int64) (success, hadResolvedFrom int, maxWeight float64) {
	t.Helper()
	if err := pool.DB().QueryRow(`
		SELECT success, had_resolved_from, max_click_weight
		  FROM proj_retrieval_success_per_query
		 WHERE grounding_event_id = ?`, geID).
		Scan(&success, &hadResolvedFrom, &maxWeight); err != nil {
		t.Fatalf("scan retrieval-success row: %v", err)
	}
	return
}

// B1: a cited-only event sits at the >= 0.8 boundary exactly (cited's default
// weight is 0.8). max_click_weight = 0.8 satisfies the weight arm → success 1,
// with had_resolved_from = 0 (so it's the weight arm, not the rescue arm).
func TestCharacterization_RetrievalSuccess_WeightBoundaryAtPoint8(t *testing.T) {
	pool := testutil.NewTestDB(t)
	geID := seedOneGrounding(t, pool, "mcp-servers", "vault_search", "agent_initiated",
		"2026-05-17T03:00:00Z", 5, `["a"]`)
	seedInteractionWithWeight(t, pool, geID, "a", telemetry.ClickCited, 0.8, "span-b1")

	mustRebuildAll(t, pool, []string{"retrieval_success_per_query"})

	success, hadResolved, maxW := retrievalSuccessRow(t, pool, geID)
	if maxW < 0.79 || maxW > 0.81 {
		t.Fatalf("max_click_weight = %v, want ~0.8", maxW)
	}
	if hadResolved != 0 {
		t.Errorf("had_resolved_from = %d, want 0 (this is the weight arm, not the rescue arm)", hadResolved)
	}
	if success != 1 {
		t.Errorf("success = %d, want 1 (max_click_weight 0.8 satisfies the `>= 0.8` boundary)", success)
	}
}

// B2: a mentioned-only event (weight 0.4) is in the band 0 < w < 0.8 with no
// resolved-from → neither arm fires → success 0. Pins the rejection band.
func TestCharacterization_RetrievalSuccess_BelowBoundaryNoResolvedFromFails(t *testing.T) {
	pool := testutil.NewTestDB(t)
	geID := seedOneGrounding(t, pool, "mcp-servers", "vault_search", "agent_initiated",
		"2026-05-17T03:00:00Z", 5, `["a"]`)
	seedInteractionWithWeight(t, pool, geID, "a", telemetry.ClickMentioned, 0.4, "span-b2")

	mustRebuildAll(t, pool, []string{"retrieval_success_per_query"})

	success, hadResolved, maxW := retrievalSuccessRow(t, pool, geID)
	if maxW < 0.39 || maxW > 0.41 {
		t.Fatalf("max_click_weight = %v, want ~0.4", maxW)
	}
	if hadResolved != 0 {
		t.Errorf("had_resolved_from = %d, want 0", hadResolved)
	}
	if success != 0 {
		t.Errorf("success = %d, want 0 (0.4 < 0.8 and no resolved-from → neither arm fires)", success)
	}
}

// readVolumeSuccess sums (query_count, success_count) across all
// proj_query_volume_by_source rows (these fixtures land a single bucket).
func readVolumeSuccess(t *testing.T, pool *db.Pool) (queryCount, successCount int) {
	t.Helper()
	if err := pool.DB().QueryRow(`
		SELECT COALESCE(SUM(query_count), 0), COALESCE(SUM(success_count), 0)
		  FROM proj_query_volume_by_source`).Scan(&queryCount, &successCount); err != nil {
		t.Fatalf("read volume success: %v", err)
	}
	return
}

// bug 954: proj_query_volume_by_source.success_count and
// proj_retrieval_success_per_query.success both back a /telemetry "success
// rate", so they must use the SAME definition (max_click_weight >= 0.8 OR
// resolved-from). The divergence point is a CITED-only event (weight 0.8): a
// success in retrieval_success, historically UNcounted by volume (cited is not
// in {followed, resolved-from}). After the fix the two agree.
func TestCharacterization_VolumeSuccess_AgreesWithRetrievalSuccessOnCited(t *testing.T) {
	pool := testutil.NewTestDB(t)
	geID := seedOneGrounding(t, pool, "mcp-servers", "vault_search", "agent_initiated",
		"2026-05-17T03:00:00Z", 5, `["a"]`)
	seedInteractionWithWeight(t, pool, geID, "a", telemetry.ClickCited, 0.8, "span-954-cited")

	mustRebuildAll(t, pool, []string{"retrieval_success_per_query", "query_volume_by_source"})

	rsSuccess, _, _ := retrievalSuccessRow(t, pool, geID)
	_, volSuccess := readVolumeSuccess(t, pool)
	if rsSuccess != 1 {
		t.Fatalf("retrieval_success.success = %d, want 1 (cited at the 0.8 boundary)", rsSuccess)
	}
	if volSuccess != rsSuccess {
		t.Errorf("query_volume.success_count = %d, want %d — the two /telemetry success surfaces must agree on a cited-only event (bug 954)",
			volSuccess, rsSuccess)
	}
}

// Guard: a mentioned-only event (weight 0.4) is below the boundary with no
// resolved-from → success 0 in BOTH surfaces. Pins that the alignment does not
// start counting the weakest tier.
func TestCharacterization_VolumeSuccess_MentionedOnlyCountsInNeither(t *testing.T) {
	pool := testutil.NewTestDB(t)
	geID := seedOneGrounding(t, pool, "mcp-servers", "vault_search", "agent_initiated",
		"2026-05-17T03:00:00Z", 5, `["a"]`)
	seedInteractionWithWeight(t, pool, geID, "a", telemetry.ClickMentioned, 0.4, "span-954-ment")

	mustRebuildAll(t, pool, []string{"retrieval_success_per_query", "query_volume_by_source"})

	rsSuccess, _, _ := retrievalSuccessRow(t, pool, geID)
	_, volSuccess := readVolumeSuccess(t, pool)
	if rsSuccess != 0 || volSuccess != 0 {
		t.Errorf("mentioned-only must be success=0 in both surfaces; got retrieval=%d volume=%d", rsSuccess, volSuccess)
	}
}

// Guard: a resolved-from interaction carried below 0.8 (0.5) must count as a
// success in BOTH surfaces via the resolved-from arm (volume counted
// resolved-from before and must continue to). Pins that the alignment keeps
// the rescue arm.
func TestCharacterization_VolumeSuccess_ResolvedFromLowWeightCountsInBoth(t *testing.T) {
	pool := testutil.NewTestDB(t)
	geID := seedOneGrounding(t, pool, "mcp-servers", "vault_search", "agent_initiated",
		"2026-05-17T03:00:00Z", 5, `["a"]`)
	seedInteractionWithWeight(t, pool, geID, "a", telemetry.ClickResolvedFrom, 0.5, "span-954-rf")

	mustRebuildAll(t, pool, []string{"retrieval_success_per_query", "query_volume_by_source"})

	rsSuccess, _, _ := retrievalSuccessRow(t, pool, geID)
	_, volSuccess := readVolumeSuccess(t, pool)
	if rsSuccess != 1 || volSuccess != 1 {
		t.Errorf("resolved-from@0.5 must be success=1 in both surfaces; got retrieval=%d volume=%d", rsSuccess, volSuccess)
	}
}

// B3: the resolved-from rescue arm in isolation. A resolved-from interaction
// carried at an EXPLICIT weight below 0.8 (0.5) leaves the weight arm unsatisfied,
// yet had_resolved_from = 1 makes success 1. This is the documented "inclusive
// of resolved-from even when no other tier fired" behavior — the OR's second
// branch carrying the result on its own.
func TestCharacterization_RetrievalSuccess_ResolvedFromRescuesLowWeight(t *testing.T) {
	pool := testutil.NewTestDB(t)
	geID := seedOneGrounding(t, pool, "mcp-servers", "vault_search", "agent_initiated",
		"2026-05-17T03:00:00Z", 5, `["a"]`)
	seedInteractionWithWeight(t, pool, geID, "a", telemetry.ClickResolvedFrom, 0.5, "span-b3")

	mustRebuildAll(t, pool, []string{"retrieval_success_per_query"})

	success, hadResolved, maxW := retrievalSuccessRow(t, pool, geID)
	if maxW >= 0.8 {
		t.Fatalf("max_click_weight = %v, want < 0.8 so the weight arm cannot be what fires", maxW)
	}
	if hadResolved != 1 {
		t.Errorf("had_resolved_from = %d, want 1", hadResolved)
	}
	if success != 1 {
		t.Errorf("success = %d, want 1 (resolved-from rescues a sub-0.8 event via the OR's second arm)", success)
	}
}
