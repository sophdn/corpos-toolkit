package main

import (
	"bytes"
	"context"
	"errors"
	stdio "io"
	"strings"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/knowledge/curation"
	"toolkit/internal/testutil"
)

// mockScorer is a Scorer impl that the tests inject. Each method
// returns canned values; healthErr controls whether Health succeeds.
type mockScorer struct {
	healthErr   error
	question    string
	invokeWhen  string
	description string
	score       float64
	extractErr  error
	scoreErr    error
	url         string
}

func (m *mockScorer) Extract(_ context.Context, _, _, _ string) (curation.ExtractedMeta, error) {
	if m.extractErr != nil {
		return curation.ExtractedMeta{}, m.extractErr
	}
	return curation.ExtractedMeta{
		Question:    m.question,
		InvokeWhen:  m.invokeWhen,
		Description: m.description,
	}, nil
}

func (m *mockScorer) Score(_ context.Context, _, _ string) (float64, error) {
	if m.scoreErr != nil {
		return 0, m.scoreErr
	}
	return m.score, nil
}

func (m *mockScorer) Health(_ context.Context) error { return m.healthErr }

func (m *mockScorer) BaseURL() string { return m.url }

// stubBuilder echoes a canned source material — passes don't care about
// the actual content for these tests.
type stubBuilder struct {
	origin string
}

func (s *stubBuilder) Origin() string { return s.origin }
func (s *stubBuilder) Build(_ context.Context, _ *db.Pool, _ curation.Candidate) (string, error) {
	return "stub material for " + s.origin, nil
}

func seedPendingCandidate(t *testing.T, pool *db.Pool, sourceRef string, scoreSet bool, score float64) int64 {
	t.Helper()
	ins := curation.CandidateInsert{
		ProjectID:   "test-proj",
		SourceType:  "task",
		SourceRef:   sourceRef,
		Question:    "Initial question",
		InvokeWhen:  "Initial invoke_when",
		Description: "Initial description",
		Tags:        []string{},
		Origin:      "task_handoff",
	}
	if scoreSet {
		ins.QualityScore = &score
	}
	id, err := curation.AddCandidate(context.Background(), pool, ins)
	if err != nil {
		t.Fatalf("seed candidate: %v", err)
	}
	return id
}

func registryWithBuilders(origins ...string) *curation.BuilderRegistry {
	reg := curation.NewBuilderRegistry()
	for _, o := range origins {
		reg.Register(&stubBuilder{origin: o})
	}
	return reg
}

// TestRun_HealthFailureAbortsWithZeroWrites is the load-bearing
// regression test for bug knowledge-curate-secondary-pass-cannot-score-
// existing-candidates-and-silently-adds-unscored-on-qwen-failure. The
// rescore pass must NOT touch the DB when Health() returns error.
func TestRun_HealthFailureAbortsWithZeroWrites(t *testing.T) {
	pool := testutil.NewTestDB(t)
	candID := seedPendingCandidate(t, pool, "test-proj::task-1", false, 0)

	mock := &mockScorer{
		healthErr: errors.New("connection refused"),
		url:       "http://localhost:8081",
	}
	deps := RunDeps{
		Pool:     pool,
		Scorer:   mock,
		Registry: registryWithBuilders("task_handoff"),
		Stdout:   stdio.Discard,
	}

	summary, err := Run(context.Background(), deps, RunOpts{
		Project: "test-proj",
		Limit:   10,
	})
	if err == nil {
		t.Fatal("Run: want error when Health() fails, got nil")
	}
	if summary.Processed != 0 {
		t.Errorf("Processed: want 0 on health failure, got %d", summary.Processed)
	}
	if summary.AutoPromoted != 0 || summary.LeftPending != 0 {
		t.Errorf("write counts: want 0, got auto=%d left=%d",
			summary.AutoPromoted, summary.LeftPending)
	}

	// Critical: the candidate row must not have changed.
	got, err := curation.ReadCandidate(context.Background(), pool, candID)
	if err != nil {
		t.Fatalf("ReadCandidate: %v", err)
	}
	if got.QualityScore != nil {
		t.Errorf("QualityScore: want nil (unchanged) after health failure, got %v",
			*got.QualityScore)
	}
	if got.Question != "Initial question" {
		t.Errorf("Question changed despite health failure: %q", got.Question)
	}
}

func TestRun_AutoPromotesHighScore(t *testing.T) {
	pool := testutil.NewTestDB(t)
	candID := seedPendingCandidate(t, pool, "test-proj::task-high", false, 0)

	mock := &mockScorer{
		question:    "Refined question",
		invokeWhen:  "Refined invoke_when",
		description: "Refined description",
		score:       0.92,
	}
	deps := RunDeps{
		Pool:     pool,
		Scorer:   mock,
		Registry: registryWithBuilders("task_handoff"),
		Stdout:   stdio.Discard,
	}

	summary, err := Run(context.Background(), deps, RunOpts{
		Project: "test-proj",
		Limit:   10,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.AutoPromoted != 1 {
		t.Errorf("AutoPromoted: want 1, got %d", summary.AutoPromoted)
	}
	if summary.LeftPending != 0 {
		t.Errorf("LeftPending: want 0, got %d", summary.LeftPending)
	}

	// Candidate is now status='promoted' with the refined metadata.
	got, _ := curation.ReadCandidate(context.Background(), pool, candID)
	if got.Status != "promoted" {
		t.Errorf("Status: want promoted, got %q", got.Status)
	}
	if !got.PromotedAutomatically {
		t.Errorf("PromotedAutomatically: want true, got false")
	}
	if got.Question != "Refined question" {
		t.Errorf("Question: want refined, got %q", got.Question)
	}
	if got.QualityScore == nil || *got.QualityScore != 0.92 {
		t.Errorf("QualityScore: want 0.92, got %v", got.QualityScore)
	}
}

func TestRun_LowScoreLeavesPendingWithScore(t *testing.T) {
	pool := testutil.NewTestDB(t)
	candID := seedPendingCandidate(t, pool, "test-proj::task-low", false, 0)

	mock := &mockScorer{
		question:   "Q",
		invokeWhen: "W",
		score:      0.42, // below threshold
	}
	deps := RunDeps{
		Pool:     pool,
		Scorer:   mock,
		Registry: registryWithBuilders("task_handoff"),
		Stdout:   stdio.Discard,
	}

	summary, _ := Run(context.Background(), deps, RunOpts{Project: "test-proj", Limit: 10})
	if summary.LeftPending != 1 {
		t.Errorf("LeftPending: want 1, got %d", summary.LeftPending)
	}
	if summary.AutoPromoted != 0 {
		t.Errorf("AutoPromoted: want 0 below threshold, got %d", summary.AutoPromoted)
	}

	got, _ := curation.ReadCandidate(context.Background(), pool, candID)
	if got.Status != "pending" {
		t.Errorf("Status: want pending, got %q", got.Status)
	}
	if got.QualityScore == nil || *got.QualityScore != 0.42 {
		t.Errorf("QualityScore: want 0.42, got %v", got.QualityScore)
	}
}

func TestRun_DryRunDoesNotWrite(t *testing.T) {
	pool := testutil.NewTestDB(t)
	candID := seedPendingCandidate(t, pool, "test-proj::task-dry", false, 0)

	mock := &mockScorer{
		question:   "Q",
		invokeWhen: "W",
		score:      0.95, // would auto-promote
	}
	var buf bytes.Buffer
	deps := RunDeps{
		Pool:     pool,
		Scorer:   mock,
		Registry: registryWithBuilders("task_handoff"),
		Stdout:   &buf,
	}

	summary, _ := Run(context.Background(), deps, RunOpts{
		Project: "test-proj", Limit: 10, DryRun: true,
	})
	if summary.AutoPromoted != 1 {
		t.Errorf("DryRun: AutoPromoted count should still increment, got %d",
			summary.AutoPromoted)
	}

	// But the DB must be unchanged.
	got, _ := curation.ReadCandidate(context.Background(), pool, candID)
	if got.Status != "pending" {
		t.Errorf("DryRun: Status should remain pending, got %q", got.Status)
	}
	if got.QualityScore != nil {
		t.Errorf("DryRun: QualityScore should remain nil, got %v", *got.QualityScore)
	}
	if !strings.Contains(buf.String(), "would auto-promote") {
		t.Errorf("DryRun: stdout should mention 'would auto-promote', got: %q", buf.String())
	}
}

func TestRun_SkipsAlreadyScoredCandidates(t *testing.T) {
	pool := testutil.NewTestDB(t)
	// Seed one unscored and one already-scored — only the unscored should
	// be processed by the rescore pass (UnscoredOnly filter).
	seedPendingCandidate(t, pool, "test-proj::task-already-scored", true, 0.5)
	unscoredID := seedPendingCandidate(t, pool, "test-proj::task-needs-scoring", false, 0)

	mock := &mockScorer{
		question:   "Q",
		invokeWhen: "W",
		score:      0.7,
	}
	deps := RunDeps{
		Pool:     pool,
		Scorer:   mock,
		Registry: registryWithBuilders("task_handoff"),
		Stdout:   stdio.Discard,
	}

	summary, _ := Run(context.Background(), deps, RunOpts{Project: "test-proj", Limit: 10})
	if summary.Processed != 1 {
		t.Errorf("Processed: want 1 (only the unscored), got %d", summary.Processed)
	}

	// The already-scored row should be unchanged.
	already, _ := curation.ListPending(context.Background(), pool, curation.ListFilter{
		ProjectID: "test-proj",
	})
	var foundAlready *curation.Candidate
	for i, c := range already {
		if c.SourceRef == "test-proj::task-already-scored" {
			foundAlready = &already[i]
		}
	}
	if foundAlready == nil {
		t.Fatal("already-scored candidate disappeared")
	}
	if foundAlready.QualityScore == nil || *foundAlready.QualityScore != 0.5 {
		t.Errorf("already-scored QualityScore changed: got %v",
			foundAlready.QualityScore)
	}

	// And the unscored one is now scored.
	updated, _ := curation.ReadCandidate(context.Background(), pool, unscoredID)
	if updated.QualityScore == nil || *updated.QualityScore != 0.7 {
		t.Errorf("unscored row: QualityScore: want 0.7, got %v", updated.QualityScore)
	}
}

func TestRun_BuilderMissCountedAndCandidateSkipped(t *testing.T) {
	pool := testutil.NewTestDB(t)
	// Insert a candidate with an origin the registry doesn't know.
	ins := curation.CandidateInsert{
		ProjectID:   "test-proj",
		SourceType:  "task",
		SourceRef:   "test-proj::task-unknown-origin",
		Question:    "Q",
		InvokeWhen:  "W",
		Description: "D",
		Origin:      "task_handoff", // valid origin per enum
	}
	candID, err := curation.AddCandidate(context.Background(), pool, ins)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	mock := &mockScorer{question: "Q", invokeWhen: "W", score: 0.5}
	deps := RunDeps{
		Pool:     pool,
		Scorer:   mock,
		Registry: registryWithBuilders("zero_result_gap"), // no task_handoff builder
		Stdout:   stdio.Discard,
	}

	summary, _ := Run(context.Background(), deps, RunOpts{Project: "test-proj", Limit: 10})
	if summary.BuilderMisses != 1 {
		t.Errorf("BuilderMisses: want 1, got %d", summary.BuilderMisses)
	}

	got, _ := curation.ReadCandidate(context.Background(), pool, candID)
	if got.QualityScore != nil {
		t.Errorf("QualityScore: should remain nil on builder-miss, got %v", *got.QualityScore)
	}
}
