package projections_test

import (
	"context"
	"database/sql"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/testutil"
)

// gate_runs_test.go exercises the gate-run projection: the emit→fold→read path
// (which also proves the literal-filter contract — the writer emits the exact
// entity_kind='gate_run' the fold filters on) and the rebuild-from-empty
// byte-identical parity.

// emitGateRun emits one GateRunCompleted event through the production fold hook
// (installed by installProjectionsFoldHook) so the projection tables populate
// in the same tx. Returns the generated event id — the parent projection's PK.
func emitGateRun(t *testing.T, pool *db.Pool, p events.GateRunCompletedPayload) string {
	t.Helper()
	var eventID string
	if err := pool.WithWrite(context.Background(), func(tx *sql.Tx) error {
		id, err := events.Emit(context.Background(), tx, events.EmitArgs{
			Entity:  events.NewCrossCuttingEntityRef("gate_run", "slug-"+p.CommitSHA),
			Payload: p,
		})
		eventID = id
		return err
	}); err != nil {
		t.Fatalf("emit gate run: %v", err)
	}
	return eventID
}

func sampleGateRun(project, commit string, ok bool) events.GateRunCompletedPayload {
	return events.GateRunCompletedPayload{
		Project:       project,
		CommitSHA:     commit,
		Tier:          "pre-push",
		OverallOK:     ok,
		CoveragePct:   72.4,
		BranchPct:     -1,
		MutationScore: 0.9,
		DurationMS:    1234,
		Checks: []events.GateCheckResult{
			{Name: "format", Tier: "pre-commit", OK: true, Skipped: false, DurationMS: 10, Note: ""},
			{Name: "coverage", Tier: "pre-push", OK: ok, Skipped: false, DurationMS: 900, Note: "coverage 72.4% >= 66% floor"},
			{Name: "vuln", Tier: "pre-push", OK: true, Skipped: true, DurationMS: 0, Note: "skipped"},
		},
	}
}

// TestGateRuns_EmitFoldRead is the literal-filter proof: a writer emits a
// GateRunCompleted event on entity_kind='gate_run', and the fold populates
// proj_gate_runs (parent) + proj_gate_check_results (child) with the exact
// literal the projection filters on.
func TestGateRuns_EmitFoldRead(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "corpos-toolkit")
	installProjectionsFoldHook(t)

	id := emitGateRun(t, pool, sampleGateRun("corpos-toolkit", "abc123", true))

	if got := tableCount(t, pool, "proj_gate_runs"); got != 1 {
		t.Fatalf("proj_gate_runs rows = %d, want 1", got)
	}
	if got := tableCount(t, pool, "proj_gate_check_results"); got != 3 {
		t.Fatalf("proj_gate_check_results rows = %d, want 3", got)
	}

	var tier, commit, ranAt string
	var overallOK int
	var coverage, branch, mutation float64
	var durationMS int
	if err := pool.DB().QueryRow(
		`SELECT tier, commit_sha, overall_ok, coverage_pct, branch_pct, mutation_score, duration_ms, ran_at
		 FROM proj_gate_runs WHERE id = ?`, id).
		Scan(&tier, &commit, &overallOK, &coverage, &branch, &mutation, &durationMS, &ranAt); err != nil {
		t.Fatalf("read parent: %v", err)
	}
	if tier != "pre-push" || commit != "abc123" || overallOK != 1 {
		t.Errorf("parent fields wrong: tier=%q commit=%q ok=%d", tier, commit, overallOK)
	}
	if coverage != 72.4 || branch != -1 || mutation != 0.9 || durationMS != 1234 {
		t.Errorf("metrics wrong: cov=%v branch=%v mut=%v dur=%d", coverage, branch, mutation, durationMS)
	}
	if ranAt == "" {
		t.Errorf("ran_at not populated from event ts")
	}

	// Child rows are joined by run_id == the parent id, ordered by run_seq.
	var skippedCount int
	if err := pool.DB().QueryRow(
		`SELECT SUM(skipped) FROM proj_gate_check_results WHERE run_id = ? AND project_id = ?`,
		id, "corpos-toolkit").Scan(&skippedCount); err != nil {
		t.Fatalf("read checks: %v", err)
	}
	if skippedCount != 1 {
		t.Errorf("skipped check count = %d, want 1", skippedCount)
	}
	var firstName string
	if err := pool.DB().QueryRow(
		`SELECT name FROM proj_gate_check_results WHERE run_id = ? ORDER BY run_seq ASC LIMIT 1`, id).
		Scan(&firstName); err != nil {
		t.Fatalf("read first check: %v", err)
	}
	if firstName != "format" {
		t.Errorf("run_seq ordering wrong: first check = %q, want format", firstName)
	}
}

// TestGateRuns_FoldIsIdempotent proves re-folding the same event id upserts the
// parent and replaces the child grid rather than duplicating.
func TestGateRuns_FoldIsIdempotent(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "corpos-toolkit")
	installProjectionsFoldHook(t)

	id := emitGateRun(t, pool, sampleGateRun("corpos-toolkit", "abc123", true))

	// Re-fold the SAME event (a rebuild of the single projection) — the parent
	// upserts and the child grid is replaced, not duplicated.
	mustRebuild(t, pool, []string{"gate_runs"})

	if got := tableCount(t, pool, "proj_gate_runs"); got != 1 {
		t.Fatalf("proj_gate_runs rows = %d, want 1 (upsert)", got)
	}
	if got := tableCount(t, pool, "proj_gate_check_results"); got != 3 {
		t.Fatalf("proj_gate_check_results rows = %d, want 3 (grid replaced)", got)
	}
	// id is stable across rebuild (== event id).
	var count int
	if err := pool.DB().QueryRow(`SELECT COUNT(*) FROM proj_gate_runs WHERE id = ?`, id).Scan(&count); err != nil {
		t.Fatalf("read: %v", err)
	}
	if count != 1 {
		t.Errorf("parent id not stable across rebuild")
	}
}

// TestGateRuns_RebuildFromEmpty seeds two runs, checksums the projection pair,
// TRUNCATEs + RebuildFromEmpty, and asserts byte-identical convergence
// (excluding the volatile watermark columns, per tableChecksum).
func TestGateRuns_RebuildFromEmpty(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "corpos-toolkit")
	installProjectionsFoldHook(t)

	emitGateRun(t, pool, sampleGateRun("corpos-toolkit", "abc123", true))
	emitGateRun(t, pool, sampleGateRun("corpos-toolkit", "def456", false))

	referenceParent := tableChecksum(t, pool, "proj_gate_runs")
	referenceChecks := tableChecksum(t, pool, "proj_gate_check_results")

	mustRebuild(t, pool, []string{"gate_runs"})
	afterParent := tableChecksum(t, pool, "proj_gate_runs")
	afterChecks := tableChecksum(t, pool, "proj_gate_check_results")

	if referenceParent != afterParent {
		t.Errorf("proj_gate_runs checksum drift: ref=%s after=%s", referenceParent, afterParent)
	}
	if referenceChecks != afterChecks {
		t.Errorf("proj_gate_check_results checksum drift: ref=%s after=%s", referenceChecks, afterChecks)
	}
	if got := tableCount(t, pool, "proj_gate_runs"); got != 2 {
		t.Errorf("proj_gate_runs rows = %d, want 2 after rebuild", got)
	}
	if got := tableCount(t, pool, "proj_gate_check_results"); got != 6 {
		t.Errorf("proj_gate_check_results rows = %d, want 6 after rebuild", got)
	}
}
