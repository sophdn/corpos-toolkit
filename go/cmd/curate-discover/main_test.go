package main

import (
	"context"
	"io"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/knowledge/curation"
	"toolkit/internal/testutil"
)

// mockScorer mirrors the curate-rescore mockScorer — narrow Scorer impl.
type mockScorer struct {
	healthErr  error
	question   string
	invokeWhen string
	score      float64
}

func (m *mockScorer) Extract(_ context.Context, _, _, _ string) (curation.ExtractedMeta, error) {
	return curation.ExtractedMeta{
		Question:    m.question,
		InvokeWhen:  m.invokeWhen,
		Description: "diagnostic description",
	}, nil
}

func (m *mockScorer) Score(_ context.Context, _, _ string) (float64, error) {
	return m.score, nil
}

func (m *mockScorer) Health(_ context.Context) error { return m.healthErr }

func newDeps(t *testing.T, pool *db.Pool, scorer curation.Scorer) PassDeps {
	t.Helper()
	reg := curation.NewBuilderRegistry()
	// Discover doesn't need real builders for secondary-pass tests (it
	// inlines the source-material construction); primary-pass tests
	// would need a builder, but those go through helpers.
	return PassDeps{
		Pool:     pool,
		Scorer:   scorer,
		Registry: reg,
		Stdout:   io.Discard,
	}
}

// seedClosedTaskWithHandoff seeds (idempotently) the per-package
// fixture chain `test-chain` for project `test-proj` and inserts a
// closed task with the supplied problem_statement / handoff_output.
// The closed-task fixture-commit_sha is supplied by [testutil.SeedTask].
func seedClosedTaskWithHandoff(t *testing.T, pool *db.Pool, slug, handoff string) {
	t.Helper()
	testutil.SeedProject(t, pool, "test-proj")
	// Idempotent chain seed: tests share a single fixture chain across
	// multiple seedClosedTaskWithHandoff calls within the same DB.
	var chainID int64
	if err := pool.DB().QueryRow(
		`SELECT id FROM proj_chain_status WHERE project_id = ? AND slug = ?`,
		"test-proj", "test-chain",
	).Scan(&chainID); err != nil {
		chainID = testutil.SeedChain(t, pool, "test-proj", "test-chain", "open", testutil.SeedChainOpts{})
	}
	testutil.SeedTask(t, pool, chainID, slug, "closed", testutil.SeedTaskOpts{
		ProblemStatement: "Problem for " + slug,
		HandoffOutput:    handoff,
	})
}

func TestRunSecondary_HappyPath_AddsNewCandidate(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedClosedTaskWithHandoff(t, pool, "new-task",
		"This is the handoff body, longer than 50 chars to clear the SecondaryMinHandoff threshold.")

	deps := newDeps(t, pool, &mockScorer{question: "Q", invokeWhen: "W", score: 0.5})
	summary, err := RunSecondary(context.Background(), deps, PassOpts{Project: "test-proj", Limit: 10})
	if err != nil {
		t.Fatalf("RunSecondary: %v", err)
	}
	if summary.Added != 1 {
		t.Errorf("Added: want 1, got %d", summary.Added)
	}
	if summary.AutoPromoted != 0 {
		t.Errorf("AutoPromoted: want 0 (below threshold), got %d", summary.AutoPromoted)
	}

	// Candidate exists with expected shape.
	cands, _ := curation.ListPending(context.Background(), pool, curation.ListFilter{
		ProjectID: "test-proj",
	})
	if len(cands) != 1 {
		t.Fatalf("listed candidates: want 1, got %d", len(cands))
	}
	if cands[0].SourceRef != "test-proj::new-task" {
		t.Errorf("SourceRef: got %q", cands[0].SourceRef)
	}
	if cands[0].Origin != "task_handoff" {
		t.Errorf("Origin: got %q", cands[0].Origin)
	}
	if cands[0].QualityScore == nil || *cands[0].QualityScore != 0.5 {
		t.Errorf("QualityScore: got %v", cands[0].QualityScore)
	}
}

func TestRunSecondary_AutoPromotesHighScore(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedClosedTaskWithHandoff(t, pool, "high-task",
		"This is the handoff body, longer than 50 chars to clear the threshold check.")

	deps := newDeps(t, pool, &mockScorer{question: "Q", invokeWhen: "W", score: 0.95})
	summary, err := RunSecondary(context.Background(), deps, PassOpts{Project: "test-proj", Limit: 10})
	if err != nil {
		t.Fatalf("RunSecondary: %v", err)
	}
	if summary.AutoPromoted != 1 {
		t.Errorf("AutoPromoted: want 1, got %d", summary.AutoPromoted)
	}

	// Pointer was created.
	var pointerCount int
	pool.DB().QueryRow(
		`SELECT COUNT(*) FROM knowledge_pointers WHERE source_ref='test-proj::high-task' AND status='active'`,
	).Scan(&pointerCount)
	if pointerCount != 1 {
		t.Errorf("pointer count: want 1, got %d", pointerCount)
	}
}

func TestRunSecondary_SkipsExistingPointer(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedClosedTaskWithHandoff(t, pool, "pre-pointer",
		"Handoff body longer than 50 chars to clear the threshold check.")

	// Pre-insert a knowledge_pointer for the same source_ref — secondary
	// should skip the task.
	_, err := pool.DB().Exec(
		`INSERT INTO knowledge_pointers
		    (project_id, source_type, source_ref, question, invoke_when, tags, status)
		 VALUES ('test-proj', 'task', 'test-proj::pre-pointer', 'Existing Q', 'when', '[]', 'active')`)
	if err != nil {
		t.Fatalf("seed pointer: %v", err)
	}

	deps := newDeps(t, pool, &mockScorer{question: "Q", invokeWhen: "W", score: 0.95})
	summary, err := RunSecondary(context.Background(), deps, PassOpts{Project: "test-proj", Limit: 10})
	if err != nil {
		t.Fatalf("RunSecondary: %v", err)
	}
	if summary.Added != 0 {
		t.Errorf("Added: want 0 (skipped due to existing pointer), got %d", summary.Added)
	}
	if summary.Skipped != 1 {
		t.Errorf("Skipped: want 1, got %d", summary.Skipped)
	}
}

func TestRunSecondary_SkipsExistingCandidate(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedClosedTaskWithHandoff(t, pool, "pre-candidate",
		"Handoff body longer than 50 chars to clear the threshold check.")

	// Pre-insert a curation_candidate for the same source_ref.
	if _, err := curation.AddCandidate(context.Background(), pool, curation.CandidateInsert{
		ProjectID:   "test-proj",
		SourceType:  "task",
		SourceRef:   "test-proj::pre-candidate",
		Question:    "Existing Q",
		InvokeWhen:  "Existing W",
		Description: "Existing D",
		Origin:      "task_handoff",
	}); err != nil {
		t.Fatalf("seed candidate: %v", err)
	}

	deps := newDeps(t, pool, &mockScorer{question: "Q", invokeWhen: "W", score: 0.95})
	summary, err := RunSecondary(context.Background(), deps, PassOpts{Project: "test-proj", Limit: 10})
	if err != nil {
		t.Fatalf("RunSecondary: %v", err)
	}
	if summary.Added != 0 {
		t.Errorf("Added: want 0 (skipped due to existing candidate), got %d", summary.Added)
	}
	if summary.Skipped != 1 {
		t.Errorf("Skipped: want 1, got %d", summary.Skipped)
	}
}

func TestRunSecondary_DryRunNoWrites(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedClosedTaskWithHandoff(t, pool, "dry",
		"Handoff body longer than 50 chars to clear the threshold check.")

	deps := newDeps(t, pool, &mockScorer{question: "Q", invokeWhen: "W", score: 0.95})
	summary, err := RunSecondary(context.Background(), deps, PassOpts{
		Project: "test-proj", Limit: 10, DryRun: true,
	})
	if err != nil {
		t.Fatalf("RunSecondary: %v", err)
	}
	if summary.Added != 1 || summary.AutoPromoted != 1 {
		t.Errorf("dry-run counters should still increment: added=%d promoted=%d",
			summary.Added, summary.AutoPromoted)
	}

	// No candidate / pointer written.
	cands, _ := curation.ListPending(context.Background(), pool, curation.ListFilter{
		ProjectID: "test-proj",
	})
	if len(cands) != 0 {
		t.Errorf("dry-run: expected no candidates, got %d", len(cands))
	}
}

func TestRunSecondary_RespectsLimit(t *testing.T) {
	pool := testutil.NewTestDB(t)
	for i := 0; i < 5; i++ {
		seedClosedTaskWithHandoff(t, pool, "limit-task-"+itoa(i),
			"Handoff body longer than 50 chars to clear the threshold check.")
	}

	deps := newDeps(t, pool, &mockScorer{question: "Q", invokeWhen: "W", score: 0.5})
	summary, err := RunSecondary(context.Background(), deps, PassOpts{Project: "test-proj", Limit: 2})
	if err != nil {
		t.Fatalf("RunSecondary: %v", err)
	}
	if summary.Added != 2 {
		t.Errorf("Added: want 2 (limited), got %d", summary.Added)
	}
}

// itoa avoids importing strconv for a single call site in tests.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}

func TestRunPrimary_NoEventsNoOp(t *testing.T) {
	pool := testutil.NewTestDB(t)
	deps := newDeps(t, pool, &mockScorer{question: "Q", invokeWhen: "W", score: 0.3})
	deps.Registry.Register(&stubBuilder{origin: "zero_result_gap"})
	summary, err := RunPrimary(context.Background(), deps, PassOpts{})
	if err != nil {
		t.Fatalf("RunPrimary: %v", err)
	}
	if summary.Added != 0 {
		t.Errorf("Added: want 0 (no events), got %d", summary.Added)
	}
}

func TestRunPrimary_SkipsAlreadyCandidatedEvent(t *testing.T) {
	pool := testutil.NewTestDB(t)
	// Insert a grounding_event.
	_, err := pool.DB().Exec(
		`INSERT INTO grounding_events
		    (project_id, session_id, call_id, action, results_count, next_turn_has_output)
		 VALUES ('test-proj', 'sess-1', 'call-1', 'knowledge_search', 0, 1)`)
	if err != nil {
		t.Fatalf("seed event: %v", err)
	}
	// Insert a curation_candidate already targeting this event.
	originRef := "1"
	if _, err := curation.AddCandidate(context.Background(), pool, curation.CandidateInsert{
		ProjectID:   "test-proj",
		SourceType:  "vault",
		SourceRef:   "vault://existing",
		Question:    "Existing Q",
		InvokeWhen:  "Existing W",
		Description: "D",
		Origin:      "zero_result_gap",
		OriginRef:   &originRef,
	}); err != nil {
		t.Fatalf("seed candidate: %v", err)
	}

	// Need to register the zero_result_gap builder; even though the
	// event is skipped, the loop initializer fetches the builder before
	// the loop runs.
	deps := newDeps(t, pool, &mockScorer{question: "Q", invokeWhen: "W", score: 0.3})
	// Manually register a stub so ForOrigin doesn't fail.
	deps.Registry.Register(&stubBuilder{origin: "zero_result_gap"})

	summary, err := RunPrimary(context.Background(), deps, PassOpts{})
	if err != nil {
		t.Fatalf("RunPrimary: %v", err)
	}
	if summary.Skipped != 1 {
		t.Errorf("Skipped: want 1, got %d", summary.Skipped)
	}
	if summary.Added != 0 {
		t.Errorf("Added: want 0, got %d", summary.Added)
	}
}

type stubBuilder struct{ origin string }

func (s *stubBuilder) Origin() string { return s.origin }
func (s *stubBuilder) Build(_ context.Context, _ *db.Pool, _ curation.Candidate) (string, error) {
	return "stub gap material", nil
}
