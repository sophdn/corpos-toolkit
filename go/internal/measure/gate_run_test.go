package measure_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/gate"
	"toolkit/internal/measure"
	"toolkit/internal/projections"
	"toolkit/internal/testutil"
)

// gate_run_test.go drives the gate_run + gate_trend actions with a STUBBED gate
// core (GateRunDeps.Runner) so the emit+persist+verdict path is exercised
// without running a real multi-minute suite. It proves: the verdict faithfully
// mirrors the core outcome (the identical-to-CLI guarantee at the mapping
// layer), the trend row persists via the fold, the DB-unavailable path still
// returns the verdict, and gate_trend reads the series back ordered.

// gatePool spins up a test DB with the projections fold hook installed and the
// project row seeded, so a gate_run emit populates proj_gate_runs in-tx.
func gatePool(t *testing.T, project string) *db.Pool {
	t.Helper()
	pool := testutil.NewTestDB(t)
	seedProjectRow(t, pool, project)
	events.SetFoldHook(func(ctx context.Context, tx *sql.Tx, evt events.RawEvent) error {
		return projections.FoldAll(ctx, tx, projections.RawEvent{
			EventID: evt.EventID, Ts: evt.Ts, ActorKind: evt.ActorKind, ActorID: evt.ActorID,
			Type: evt.Type, EntityKind: evt.EntityKind, EntitySlug: evt.EntitySlug,
			EntityProjectID: evt.EntityProjectID, Payload: evt.Payload, Rationale: evt.Rationale,
			CausedByEventID: evt.CausedByEventID, RelatedEntities: evt.RelatedEntities,
			SpanID: evt.SpanID, SchemaVersion: evt.SchemaVersion,
		})
	})
	t.Cleanup(func() { events.SetFoldHook(nil) })
	return pool
}

// stubRunner returns a GateRunner that always yields the given outcome/err,
// ignoring repoDir/tier — the core is not run.
func stubRunner(outcome measure.GateRunOutcome, err error) measure.GateRunner {
	return func(_ context.Context, _ string, _ gate.Tier) (measure.GateRunOutcome, error) {
		return outcome, err
	}
}

func sampleOutcome(ok bool) measure.GateRunOutcome {
	return measure.GateRunOutcome{
		OverallOK: ok,
		Results: []gate.Result{
			{Name: "format", Tier: gate.TierPreCommit, OK: true, Duration: 10 * time.Millisecond},
			{Name: "coverage", Tier: gate.TierPrePush, OK: ok, Duration: 900 * time.Millisecond,
				Output: "coverage 80.0% >= 66% floor"},
			{Name: "mutation", Tier: gate.TierCI, OK: true, Duration: 500 * time.Millisecond,
				Output: "mutation report (go-mutesting): The mutation score is 0.750000 (3 passed, 1 failed)"},
		},
	}
}

func callGateRun(t *testing.T, deps measure.GateRunDeps, project string, params map[string]any) measure.GateRunResult {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	res, err := measure.HandleGateRun(context.Background(), deps, project, raw)
	if err != nil {
		t.Fatalf("HandleGateRun returned hard error: %v", err)
	}
	return res
}

// TestGateRun_PersistsAndReturnsVerdict: the happy path — the verdict mirrors
// the core outcome AND a trend row is persisted through the fold.
func TestGateRun_PersistsAndReturnsVerdict(t *testing.T) {
	pool := gatePool(t, "corpos-toolkit")
	deps := measure.GateRunDeps{Pool: pool, Runner: stubRunner(sampleOutcome(true), nil)}

	res := callGateRun(t, deps, "", map[string]any{
		"repo_dir": "/tmp/does-not-matter", "tier": "ci",
		"project": "corpos-toolkit", "commit_sha": "abc123",
	})

	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !res.OverallOK {
		t.Errorf("overall_ok = false, want true")
	}
	if !res.Persisted {
		t.Errorf("persisted = false, want true (pool + project present)")
	}
	if res.CoveragePct != 80.0 {
		t.Errorf("coverage_pct = %v, want 80.0", res.CoveragePct)
	}
	if res.BranchPct != -1 {
		t.Errorf("branch_pct = %v, want -1 (Go = statement coverage only)", res.BranchPct)
	}
	if res.MutationScore != 0.75 {
		t.Errorf("mutation_score = %v, want 0.75", res.MutationScore)
	}
	if res.DurationMS != 1410 {
		t.Errorf("duration_ms = %d, want 1410 (sum of check durations)", res.DurationMS)
	}
	if len(res.Checks) != 3 {
		t.Fatalf("checks len = %d, want 3", len(res.Checks))
	}
	if res.Checks[0].Name != "format" || res.Checks[0].Tier != "pre-commit" {
		t.Errorf("first check mapped wrong: %+v", res.Checks[0])
	}

	// The trend row landed.
	var count int
	if err := pool.DB().QueryRow(
		`SELECT COUNT(*) FROM proj_gate_runs WHERE project_id = ? AND commit_sha = ?`,
		"corpos-toolkit", "abc123").Scan(&count); err != nil {
		t.Fatalf("query trend: %v", err)
	}
	if count != 1 {
		t.Errorf("proj_gate_runs rows = %d, want 1", count)
	}
	if got := tableCountM(t, pool, "proj_gate_check_results"); got != 3 {
		t.Errorf("proj_gate_check_results rows = %d, want 3", got)
	}
}

// TestGateRun_VerdictIdenticalToCore: every field of the verdict is derived
// from the core outcome, not re-decided — the mapping half of the
// identical-to-`corpos-gate run` guarantee (the core itself is the same
// gate.Run the CLI calls; here we pin that the handler does not distort it).
func TestGateRun_VerdictIdenticalToCore(t *testing.T) {
	pool := gatePool(t, "corpos-toolkit")
	outcome := sampleOutcome(false) // a failing run
	deps := measure.GateRunDeps{Pool: pool, Runner: stubRunner(outcome, nil)}

	res := callGateRun(t, deps, "corpos-toolkit", map[string]any{"repo_dir": "/x"})

	if res.OverallOK != outcome.OverallOK {
		t.Errorf("overall_ok = %v, core said %v", res.OverallOK, outcome.OverallOK)
	}
	if len(res.Checks) != len(outcome.Results) {
		t.Fatalf("checks len = %d, core had %d", len(res.Checks), len(outcome.Results))
	}
	for i, c := range res.Checks {
		src := outcome.Results[i]
		if c.Name != src.Name || c.OK != src.OK || c.Skipped != src.Skipped || c.Tier != src.Tier.String() {
			t.Errorf("check %d distorted: got %+v from %+v", i, c, src)
		}
	}
}

// TestGateRun_DBUnavailableStillReturnsVerdict: with no pool, the verdict is
// still returned (persisted=false + a note). A gate run must work with storage
// unavailable (CI).
func TestGateRun_DBUnavailableStillReturnsVerdict(t *testing.T) {
	deps := measure.GateRunDeps{Pool: nil, Runner: stubRunner(sampleOutcome(true), nil)}

	res := callGateRun(t, deps, "corpos-toolkit", map[string]any{"repo_dir": "/x", "project": "corpos-toolkit"})

	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !res.OverallOK {
		t.Errorf("verdict lost: overall_ok = false")
	}
	if res.Persisted {
		t.Errorf("persisted = true with nil pool")
	}
	if res.PersistNote == "" {
		t.Errorf("persist_note empty; want an explanation")
	}
	if res.CoveragePct != 80.0 {
		t.Errorf("verdict metrics lost: coverage = %v", res.CoveragePct)
	}
}

// TestGateRun_NoProjectSkipsPersist: pool present but no project → verdict
// returned, persistence skipped.
func TestGateRun_NoProjectSkipsPersist(t *testing.T) {
	pool := gatePool(t, "corpos-toolkit")
	deps := measure.GateRunDeps{Pool: pool, Runner: stubRunner(sampleOutcome(true), nil)}

	res := callGateRun(t, deps, "", map[string]any{"repo_dir": "/x"}) // no project anywhere

	if !res.OverallOK || res.Persisted {
		t.Errorf("want verdict + not persisted, got ok=%v persisted=%v", res.OverallOK, res.Persisted)
	}
	if res.PersistNote == "" {
		t.Errorf("expected a persist_note about the missing project")
	}
}

// TestGateRun_CoreInfraFailure: a gate-core infra failure (e.g. missing
// gate.yml) surfaces as Error, no trend row is written, but any partial checks
// are still mapped.
func TestGateRun_CoreInfraFailure(t *testing.T) {
	pool := gatePool(t, "corpos-toolkit")
	deps := measure.GateRunDeps{
		Pool:   pool,
		Runner: stubRunner(measure.GateRunOutcome{Results: []gate.Result{{Name: "build", Tier: gate.TierPreCommit}}}, context.DeadlineExceeded),
	}

	res := callGateRun(t, deps, "corpos-toolkit", map[string]any{"repo_dir": "/x"})

	if res.Error == "" {
		t.Errorf("expected Error on core infra failure")
	}
	if res.Persisted {
		t.Errorf("must not persist on infra failure")
	}
	if got := tableCountM(t, pool, "proj_gate_runs"); got != 0 {
		t.Errorf("proj_gate_runs rows = %d, want 0 on infra failure", got)
	}
}

func TestGateRun_ParamErrors(t *testing.T) {
	deps := measure.GateRunDeps{Runner: stubRunner(sampleOutcome(true), nil)}

	if res := callGateRun(t, deps, "", map[string]any{}); res.Error == "" {
		t.Errorf("missing repo_dir should error")
	}
	if res := callGateRun(t, deps, "", map[string]any{"repo_dir": "/x", "tier": "bogus"}); res.Error == "" {
		t.Errorf("invalid tier should error")
	}
}

// TestGateRun_DefaultsToPrePush: omitting tier uses pre-push.
func TestGateRun_DefaultsToPrePush(t *testing.T) {
	deps := measure.GateRunDeps{Runner: stubRunner(sampleOutcome(true), nil)}
	res := callGateRun(t, deps, "", map[string]any{"repo_dir": "/x"})
	if res.Tier != "pre-push" {
		t.Errorf("default tier = %q, want pre-push", res.Tier)
	}
}

// ── gate_trend ────────────────────────────────────────────────────────────

func callGateTrend(t *testing.T, deps measure.GateRunDeps, project string, params map[string]any) measure.GateTrendResult {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	res, err := measure.HandleGateTrend(context.Background(), deps, project, raw)
	if err != nil {
		t.Fatalf("HandleGateTrend hard error: %v", err)
	}
	return res
}

// TestGateTrend_OrderedRows: three runs persist, and gate_trend returns them
// most-recent first (ran_at DESC, id DESC tiebreak).
func TestGateTrend_OrderedRows(t *testing.T) {
	pool := gatePool(t, "corpos-toolkit")
	deps := measure.GateRunDeps{Pool: pool, Runner: stubRunner(sampleOutcome(true), nil)}

	for _, sha := range []string{"c1", "c2", "c3"} {
		res := callGateRun(t, deps, "corpos-toolkit", map[string]any{"repo_dir": "/x", "commit_sha": sha})
		if !res.Persisted {
			t.Fatalf("run %s not persisted: %s", sha, res.PersistNote)
		}
	}

	trend := callGateTrend(t, deps, "corpos-toolkit", map[string]any{})
	if len(trend.Points) != 3 {
		t.Fatalf("points = %d, want 3", len(trend.Points))
	}
	if trend.Points[0].CommitSHA != "c3" || trend.Points[2].CommitSHA != "c1" {
		t.Errorf("ordering wrong: %s ... %s (want c3 ... c1, most-recent first)",
			trend.Points[0].CommitSHA, trend.Points[2].CommitSHA)
	}
	if trend.Points[0].CoveragePct != 80.0 || !trend.Points[0].OverallOK {
		t.Errorf("point fields wrong: %+v", trend.Points[0])
	}

	// limit caps the window.
	limited := callGateTrend(t, deps, "corpos-toolkit", map[string]any{"limit": 2})
	if len(limited.Points) != 2 {
		t.Errorf("limited points = %d, want 2", len(limited.Points))
	}
}

// TestGateTrend_MetricFilter: metric=coverage returns only runs carrying a
// coverage metric.
func TestGateTrend_MetricFilter(t *testing.T) {
	pool := gatePool(t, "corpos-toolkit")
	withCov := measure.GateRunDeps{Pool: pool, Runner: stubRunner(sampleOutcome(true), nil)}
	// A run whose core produced NO coverage/mutation (all metrics -1).
	noCov := measure.GateRunDeps{Pool: pool, Runner: stubRunner(measure.GateRunOutcome{
		OverallOK: true,
		Results:   []gate.Result{{Name: "build", Tier: gate.TierPreCommit, OK: true, Duration: time.Millisecond}},
	}, nil)}

	callGateRun(t, withCov, "corpos-toolkit", map[string]any{"repo_dir": "/x", "commit_sha": "hascov"})
	callGateRun(t, noCov, "corpos-toolkit", map[string]any{"repo_dir": "/x", "commit_sha": "nocov"})

	cov := callGateTrend(t, withCov, "corpos-toolkit", map[string]any{"metric": "coverage"})
	if len(cov.Points) != 1 || cov.Points[0].CommitSHA != "hascov" {
		t.Errorf("coverage filter wrong: %+v", cov.Points)
	}
	all := callGateTrend(t, withCov, "corpos-toolkit", map[string]any{"metric": "verdict"})
	if len(all.Points) != 2 {
		t.Errorf("verdict (full) series = %d, want 2", len(all.Points))
	}
}

func TestGateTrend_Errors(t *testing.T) {
	pool := gatePool(t, "corpos-toolkit")
	deps := measure.GateRunDeps{Pool: pool}

	if res := callGateTrend(t, deps, "", map[string]any{}); res.Error == "" {
		t.Errorf("missing project should error")
	}
	if res := callGateTrend(t, deps, "corpos-toolkit", map[string]any{"metric": "bogus"}); res.Error == "" {
		t.Errorf("invalid metric should error")
	}
}

// TestGateTrend_DBUnavailable: nil pool → empty series, no error.
func TestGateTrend_DBUnavailable(t *testing.T) {
	deps := measure.GateRunDeps{Pool: nil}
	res := callGateTrend(t, deps, "corpos-toolkit", map[string]any{})
	if res.Error != "" {
		t.Errorf("unexpected error: %s", res.Error)
	}
	if len(res.Points) != 0 {
		t.Errorf("points = %d, want 0 with nil pool", len(res.Points))
	}
}

// tableCountM is a small measure_test-local row counter (the projections test
// helper of the same shape lives in another package).
func tableCountM(t *testing.T, pool *db.Pool, table string) int64 {
	t.Helper()
	var n int64
	if err := pool.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}
