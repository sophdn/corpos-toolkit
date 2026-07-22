package arcreview

import (
	"context"
	"database/sql"
	"testing"

	"toolkit/internal/db"
)

// telemetry_test.go — T7 of chain arc-close-decision-authoring-split.
// Verifies the author-vs-fallback instrument: the sweep emits an
// ArcCloseAuthoringResolved event, and arc_review_audit surfaces the rate
// from pending_decisions.authoring_state (warmup-gated).

// TestTelemetry_SweepEmitsAuthoringResolved: a disengagement sweep emits an
// ArcCloseAuthoringResolved event carrying the fallback count.
func TestTelemetry_SweepEmitsAuthoringResolved(t *testing.T) {
	pool := openTestPool(t)
	insertStagedRow(t, pool, "sess-tel", "2026-05-26 00:00:00",
		[]FilingDecision{stagedVaultDecision("telemetry split note", "draft")})

	cf := &capturingForge{}
	deps := Deps{Pool: pool, ForgeFn: cf.fn()}
	if _, err := SweepUnauthoredStaged(context.Background(), deps, "mcp-servers", "sess-tel"); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	var authored, fallback int
	err := pool.DB().QueryRowContext(context.Background(), `
		SELECT
			COALESCE(SUM(json_extract(payload,'$.authored_count')),0),
			COALESCE(SUM(json_extract(payload,'$.fallback_forged_count')),0)
		FROM events WHERE type = 'ArcCloseAuthoringResolved'`).Scan(&authored, &fallback)
	if err != nil {
		t.Fatalf("query resolved events: %v", err)
	}
	if fallback != 1 || authored != 0 {
		t.Fatalf("expected ArcCloseAuthoringResolved authored=0 fallback=1, got authored=%d fallback=%d", authored, fallback)
	}
}

// TestTelemetry_AuthorVsFallbackRate: the audit summary computes the rate
// from authoring_state, and warmup-gates it below the sample floor.
func TestTelemetry_AuthorVsFallbackRate(t *testing.T) {
	pool := openTestPool(t)
	seed := func(state string, n int) {
		for i := 0; i < n; i++ {
			mustExec(t, pool, `
				INSERT INTO pending_decisions
					(event_id, project_id, target_session_id, decisions_json, triggers_json, created_at, authoring_state)
				VALUES ('e','mcp-servers','s','[]','[]','2026-05-26 00:00:00', ?)`, state)
		}
	}
	// Below warmup (3 resolved): rate suppressed.
	seed("authored", 2)
	seed("fallback_forged", 1)
	seed("staged", 1)
	res, err := HandleArcReviewAudit(context.Background(), Deps{Pool: pool}, "mcp-servers",
		mustParams(t, map[string]any{"since": "2020-01-01T00:00:00Z"}))
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if res.AuthorVsFallback == nil {
		t.Fatal("expected AuthorVsFallback summary")
	}
	if res.AuthorVsFallback.Authored != 2 || res.AuthorVsFallback.FallbackForged != 1 || res.AuthorVsFallback.Staged != 1 {
		t.Fatalf("unexpected counts: %+v", res.AuthorVsFallback)
	}
	if res.AuthorVsFallback.Rate != nil {
		t.Fatalf("rate should be warmup-suppressed below %d resolved, got %v", authorVsFallbackMinSample, *res.AuthorVsFallback.Rate)
	}

	// Cross warmup: add enough authored to reach the sample floor.
	seed("authored", 5) // now 7 authored, 1 fallback = 8 resolved >= 5
	res, err = HandleArcReviewAudit(context.Background(), Deps{Pool: pool}, "mcp-servers",
		mustParams(t, map[string]any{"since": "2020-01-01T00:00:00Z"}))
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if res.AuthorVsFallback.Rate == nil {
		t.Fatal("rate should be present past warmup")
	}
	// 7 authored / 8 resolved = 0.875
	if *res.AuthorVsFallback.Rate < 0.87 || *res.AuthorVsFallback.Rate > 0.88 {
		t.Fatalf("expected rate ~0.875, got %v", *res.AuthorVsFallback.Rate)
	}
}

func mustExec(t *testing.T, pool *db.Pool, query, arg string) {
	t.Helper()
	if err := pool.WithWrite(context.Background(), func(tx *sql.Tx) error {
		_, e := tx.ExecContext(context.Background(), query, arg)
		return e
	}); err != nil {
		t.Fatalf("exec: %v", err)
	}
}
