package refresolve_test

import (
	"context"
	"testing"

	"toolkit/internal/refresolve"
	"toolkit/internal/testutil"
)

// T5 acceptance: calling resolve_references emits one grounding_events
// row per detected reference, with query_source='reference_resolution'.
func TestT5_EmitsOneGroundingEventPerReference(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedChainProj(t, pool, "mcp-servers", "tel-chain", "open")
	seedBugProj(t, pool, "mcp-servers", "tel-bug", "tiny bug", "observed", "open", "medium", "", "", "", "")

	registry := refresolve.BuildProductionRegistry(refresolve.ProductionDeps{
		Pool:    pool,
		Project: "mcp-servers",
	})
	deps := refresolve.HandlerDeps{Pool: pool, Project: "mcp-servers", Registry: registry}
	params := mustMarshalParams(t, resolveRefsParams{
		MessageText: "look at tel-chain and tel-bug",
	})
	result, err := refresolve.HandleResolveReferences(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleResolveReferences: %v", err)
	}
	if len(result.References) != 2 {
		t.Fatalf("want 2 references, got %d: %+v", len(result.References), result.References)
	}

	// Each ResolvedReference should carry a non-zero GroundingEventID.
	for _, r := range result.References {
		if r.GroundingEventID == 0 {
			t.Errorf("ref %s: GroundingEventID is 0 (expected populated)", r.Token)
		}
	}

	// Verify the rows exist in the DB with query_source='reference_resolution'.
	var rows int
	if err := pool.DB().QueryRow(
		"SELECT COUNT(*) FROM grounding_events WHERE query_source = ?",
		"reference_resolution",
	).Scan(&rows); err != nil {
		t.Fatalf("count grounding_events: %v", err)
	}
	if rows != 2 {
		t.Errorf("want 2 grounding_events rows with query_source='reference_resolution', got %d", rows)
	}

	// RF2 acceptance: every grounding_events row from refresolve has a
	// matching reference_resolution_emits side-table row in the same tx.
	var sideRows int
	if err := pool.DB().QueryRow(`
		SELECT COUNT(*) FROM reference_resolution_emits rre
		JOIN grounding_events ge ON ge.id = rre.grounding_event_id
		WHERE ge.query_source = 'reference_resolution'`,
	).Scan(&sideRows); err != nil {
		t.Fatalf("count reference_resolution_emits: %v", err)
	}
	if sideRows != 2 {
		t.Errorf("want 2 reference_resolution_emits rows, got %d", sideRows)
	}

	// And the side-table carries the per-resolution detail the Context
	// Pull Inspector reads (RF3): shape and resolver_name are populated
	// non-empty for every row.
	srows, err := pool.DB().Query(`
		SELECT rre.shape, rre.resolver_name, rre.confidence_tier
		FROM reference_resolution_emits rre`,
	)
	if err != nil {
		t.Fatalf("read reference_resolution_emits: %v", err)
	}
	defer srows.Close()
	for srows.Next() {
		var shape, resolver, tier string
		if err := srows.Scan(&shape, &resolver, &tier); err != nil {
			t.Fatalf("scan side-table row: %v", err)
		}
		if shape == "" || resolver == "" || tier == "" {
			t.Errorf("side-table row missing fields: shape=%q resolver_name=%q tier=%q",
				shape, resolver, tier)
		}
	}
}

// T5 acceptance: the CHECK widening admits 'reference_resolution'
// AND still rejects misspellings.
func TestT5_CheckConstraintAdmitsNewValueRejectsMisspellings(t *testing.T) {
	pool := testutil.NewTestDB(t)

	// Insert with the new value: must succeed.
	if _, err := pool.DB().Exec(`
        INSERT INTO grounding_events
            (project_id, session_id, call_id, action, query_source)
        VALUES ('mcp-servers', 'sess', 'call-1', 'resolve_references', 'reference_resolution')
    `); err != nil {
		t.Fatalf("INSERT with reference_resolution rejected: %v", err)
	}

	// Insert with a misspelling: must fail.
	if _, err := pool.DB().Exec(`
        INSERT INTO grounding_events
            (project_id, session_id, call_id, action, query_source)
        VALUES ('mcp-servers', 'sess', 'call-2', 'resolve_references', 'reference_resolutions')
    `); err == nil {
		t.Errorf("INSERT with misspelled query_source should have failed; CHECK constraint not enforced")
	}

	// Insert with all pre-existing values still works.
	for _, qs := range []string{"agent_initiated", "proactive_hook", "dashboard_user", "other"} {
		if _, err := pool.DB().Exec(`
            INSERT INTO grounding_events
                (project_id, session_id, call_id, action, query_source)
            VALUES (?, ?, ?, ?, ?)
        `, "mcp-servers", "sess-"+qs, "call-"+qs, "vault_search", qs); err != nil {
			t.Errorf("INSERT with %q rejected: %v", qs, err)
		}
	}
}

// T5 acceptance: span_id and session_id propagate from ctx to the
// emitted grounding_events row.
func TestT5_SpanIDPropagation(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedChainProj(t, pool, "mcp-servers", "span-test-chain", "open")

	registry := refresolve.BuildProductionRegistry(refresolve.ProductionDeps{
		Pool:    pool,
		Project: "mcp-servers",
	})
	deps := refresolve.HandlerDeps{Pool: pool, Project: "mcp-servers", Registry: registry}
	params := mustMarshalParams(t, resolveRefsParams{
		MessageText: "look at span-test-chain",
	})
	// No explicit span on ctx → events.SpanIDFromContext mints a
	// deterministic fallback. Either way, the inserted row must have
	// a non-empty span_id.
	if _, err := refresolve.HandleResolveReferences(context.Background(), deps, params); err != nil {
		t.Fatalf("HandleResolveReferences: %v", err)
	}
	var spanID string
	if err := pool.DB().QueryRow(
		`SELECT span_id FROM grounding_events WHERE query_source = 'reference_resolution' LIMIT 1`,
	).Scan(&spanID); err != nil {
		t.Fatalf("read span_id: %v", err)
	}
	if spanID == "" {
		t.Errorf("emitted row has empty span_id")
	}
}

// T5 acceptance: was_injected stays 0 on query_interactions rows
// from reference-resolution emits (the design doc commits this in §6.2).
//
// This test verifies that emission of grounding_events rows from
// reference resolution doesn't accidentally set was_injected via
// some shared path — we INSERT a query_interactions row tied to a
// resolved-from emit and check the default.
func TestT5_WasInjectedDefaultsZero(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedChainProj(t, pool, "mcp-servers", "inj-chain", "open")

	registry := refresolve.BuildProductionRegistry(refresolve.ProductionDeps{
		Pool:    pool,
		Project: "mcp-servers",
	})
	deps := refresolve.HandlerDeps{Pool: pool, Project: "mcp-servers", Registry: registry}
	params := mustMarshalParams(t, resolveRefsParams{
		MessageText: "look at inj-chain",
	})
	result, err := refresolve.HandleResolveReferences(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleResolveReferences: %v", err)
	}
	if len(result.References) == 0 {
		t.Fatalf("no references emitted")
	}

	// Emit a query_interactions row manually pointing at the first
	// grounding_event_id. The schema default for was_injected is 0.
	geID := result.References[0].GroundingEventID
	if _, err := pool.DB().Exec(`
        INSERT INTO query_interactions
            (grounding_event_id, source_ref, click_kind, click_weight,
             span_id, session_id, detected_at)
        VALUES (?, ?, 'cited', 0.8, 'span-x', 'sess-x', datetime('now'))
    `, geID, "chain:inj-chain"); err != nil {
		t.Fatalf("INSERT query_interactions: %v", err)
	}
	var wasInjected int
	if err := pool.DB().QueryRow(
		`SELECT was_injected FROM query_interactions WHERE grounding_event_id = ?`,
		geID,
	).Scan(&wasInjected); err != nil {
		t.Fatalf("read was_injected: %v", err)
	}
	if wasInjected != 0 {
		t.Errorf("was_injected should default 0 for reference-resolution emits, got %d", wasInjected)
	}
}
