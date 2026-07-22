package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/knowledge/curation"
	"toolkit/internal/testutil"
)

// mockScorer mirrors the one in curate-rescore tests — minimal Scorer
// impl with canned values. Kept separate from the rescore mockScorer
// because main_test.go can't import another main package.
type mockScorer struct {
	healthErr  error
	question   string
	invokeWhen string
	score      float64
	extractErr error
	scoreErr   error
}

func (m *mockScorer) Extract(_ context.Context, _, _, _ string) (curation.ExtractedMeta, error) {
	if m.extractErr != nil {
		return curation.ExtractedMeta{}, m.extractErr
	}
	return curation.ExtractedMeta{
		Question:    m.question,
		InvokeWhen:  m.invokeWhen,
		Description: "diagnostic description",
	}, nil
}

func (m *mockScorer) Score(_ context.Context, _, _ string) (float64, error) {
	if m.scoreErr != nil {
		return 0, m.scoreErr
	}
	return m.score, nil
}

func (m *mockScorer) Health(_ context.Context) error { return m.healthErr }

type stubBuilder struct{ origin string }

func (s *stubBuilder) Origin() string { return s.origin }
func (s *stubBuilder) Build(_ context.Context, _ *db.Pool, _ curation.Candidate) (string, error) {
	return "stub material for " + s.origin, nil
}

func TestRunStage2_NoCandidatesNoOp(t *testing.T) {
	pool := testutil.NewTestDB(t)
	registry := curation.NewBuilderRegistry()
	registry.Register(&stubBuilder{origin: "task_handoff"})

	var buf bytes.Buffer
	mock := &mockScorer{question: "Q", score: 0.5}
	if err := runStage2(context.Background(), &buf, pool, mock, registry, "no-such-proj", 3); err != nil {
		t.Fatalf("runStage2: %v", err)
	}
	if !strings.Contains(buf.String(), "nothing to probe") {
		t.Errorf("expected 'nothing to probe' message, got: %q", buf.String())
	}
}

func TestRunStage2_ProbesCandidatesPrintsScoresWithoutWriting(t *testing.T) {
	pool := testutil.NewTestDB(t)
	// Seed two unscored task_handoff candidates.
	ins := curation.CandidateInsert{
		ProjectID: "test-proj", SourceType: "task",
		SourceRef:   "test-proj::task-a",
		Question:    "Initial Q",
		InvokeWhen:  "Initial W",
		Description: "Initial D",
		Origin:      "task_handoff",
	}
	idA, err := curation.AddCandidate(context.Background(), pool, ins)
	if err != nil {
		t.Fatalf("seed A: %v", err)
	}
	ins.SourceRef = "test-proj::task-b"
	idB, err := curation.AddCandidate(context.Background(), pool, ins)
	if err != nil {
		t.Fatalf("seed B: %v", err)
	}

	registry := curation.NewBuilderRegistry()
	registry.Register(&stubBuilder{origin: "task_handoff"})

	var buf bytes.Buffer
	mock := &mockScorer{question: "Diagnostic Q", invokeWhen: "Diagnostic W", score: 0.92}
	if err := runStage2(context.Background(), &buf, pool, mock, registry, "test-proj", 10); err != nil {
		t.Fatalf("runStage2: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Diagnostic Q") {
		t.Errorf("expected extracted question in output: %q", out)
	}
	if !strings.Contains(out, "score: 0.92") {
		t.Errorf("expected score in output: %q", out)
	}
	if !strings.Contains(out, "would auto-promote") {
		t.Errorf("expected auto-promote verdict for 0.92: %q", out)
	}

	// Critical: candidates remain pending + unscored. No DB writes.
	for _, id := range []int64{idA, idB} {
		got, _ := curation.ReadCandidate(context.Background(), pool, id)
		if got.Status != "pending" {
			t.Errorf("candidate %d Status: want pending, got %q", id, got.Status)
		}
		if got.QualityScore != nil {
			t.Errorf("candidate %d QualityScore: want nil (no writes), got %v", id, *got.QualityScore)
		}
		if got.Question != "Initial Q" {
			t.Errorf("candidate %d Question: want Initial Q (no writes), got %q", id, got.Question)
		}
	}
}

func TestRunStage2_BuilderMissContinues(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ins := curation.CandidateInsert{
		ProjectID: "test-proj", SourceType: "task",
		SourceRef: "test-proj::orphan", Question: "Q", InvokeWhen: "W",
		Description: "D", Origin: "task_handoff",
	}
	if _, err := curation.AddCandidate(context.Background(), pool, ins); err != nil {
		t.Fatalf("seed: %v", err)
	}

	registry := curation.NewBuilderRegistry()
	// Intentionally don't register task_handoff — should hit builder-miss.
	registry.Register(&stubBuilder{origin: "zero_result_gap"})

	var buf bytes.Buffer
	mock := &mockScorer{question: "Q", score: 0.5}
	if err := runStage2(context.Background(), &buf, pool, mock, registry, "test-proj", 10); err != nil {
		t.Fatalf("runStage2: %v", err)
	}
	if !strings.Contains(buf.String(), "builder-miss") {
		t.Errorf("expected builder-miss message: %q", buf.String())
	}
}

func TestRunStage2_ExtractErrorContinues(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ins := curation.CandidateInsert{
		ProjectID: "test-proj", SourceType: "task",
		SourceRef: "test-proj::extract-fail", Question: "Q", InvokeWhen: "W",
		Description: "D", Origin: "task_handoff",
	}
	if _, err := curation.AddCandidate(context.Background(), pool, ins); err != nil {
		t.Fatalf("seed: %v", err)
	}

	registry := curation.NewBuilderRegistry()
	registry.Register(&stubBuilder{origin: "task_handoff"})

	var buf bytes.Buffer
	mock := &mockScorer{extractErr: errors.New("qwen timeout")}
	if err := runStage2(context.Background(), &buf, pool, mock, registry, "test-proj", 10); err != nil {
		t.Fatalf("runStage2: %v", err)
	}
	if !strings.Contains(buf.String(), "extract:") {
		t.Errorf("expected extract error in output: %q", buf.String())
	}
}
