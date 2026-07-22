package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/knowledge/curation"
	"toolkit/internal/knowledge/curation/sources"
	"toolkit/internal/testutil"
)

type mockScorer struct {
	healthErr error
	question  string
	score     float64
}

func (m *mockScorer) Extract(_ context.Context, _, _, _ string) (curation.ExtractedMeta, error) {
	return curation.ExtractedMeta{
		Question:    m.question,
		InvokeWhen:  "When investigating this note.",
		Description: "Vault diagnostic body.",
	}, nil
}
func (m *mockScorer) Score(_ context.Context, _, _ string) (float64, error) { return m.score, nil }
func (m *mockScorer) Health(_ context.Context) error                        { return m.healthErr }

// seedVaultRoot creates a minimal vault tree for testing.
func seedVaultRoot(t *testing.T) (root string) {
	t.Helper()
	root = t.TempDir()
	for _, sub := range []string{"reference", "decisions", "learnings"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	return root
}

func writeNote(t *testing.T, root, subdir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, subdir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s/%s: %v", subdir, name, err)
	}
}

func newSeedDeps(t *testing.T, pool *db.Pool, root string, scorer curation.Scorer) RunDeps {
	t.Helper()
	return RunDeps{
		Pool:      pool,
		Scorer:    scorer,
		Builder:   sources.NewVaultNoteBuilder(root),
		Stdout:    io.Discard,
		VaultRoot: root,
	}
}

func TestRun_AddsNewVaultCandidates(t *testing.T) {
	pool := testutil.NewTestDB(t)
	root := seedVaultRoot(t)
	writeNote(t, root, "reference", "design_v1.md", "# Design v1\nContent here.")
	writeNote(t, root, "decisions", "2026-05-17_test.md", "# Decision\nBody.")
	writeNote(t, root, "learnings", "lesson.md", "# Lesson\nFinding body.")

	deps := newSeedDeps(t, pool, root, &mockScorer{question: "Q", score: 0.5})
	summary, err := Run(context.Background(), deps, RunOpts{Project: "test-proj"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.Added != 3 {
		t.Errorf("Added: want 3, got %d", summary.Added)
	}

	// Each candidate exists with origin='session_mining' and expires_at set.
	var count int
	pool.DB().QueryRow(
		`SELECT COUNT(*) FROM curation_candidates
		  WHERE source_type='vault' AND origin='session_mining'
		    AND expires_at IS NOT NULL`,
	).Scan(&count)
	if count != 3 {
		t.Errorf("session_mining vault candidates with expires_at: want 3, got %d", count)
	}
}

func TestRun_SkipsWebConvFiles(t *testing.T) {
	pool := testutil.NewTestDB(t)
	root := seedVaultRoot(t)
	// Two valid + one web-conv.
	writeNote(t, root, "reference", "design_v1.md", "# Design v1")
	writeNote(t, root, "reference", "web-conv-abc123.md", "# Archived chat")
	writeNote(t, root, "learnings", "lesson.md", "# Lesson")

	deps := newSeedDeps(t, pool, root, &mockScorer{question: "Q", score: 0.5})
	summary, _ := Run(context.Background(), deps, RunOpts{Project: "test-proj"})

	if summary.Added != 2 {
		t.Errorf("Added: want 2 (web-conv skipped), got %d", summary.Added)
	}
	if summary.Skipped < 1 {
		t.Errorf("Skipped: want >=1 (web-conv), got %d", summary.Skipped)
	}
}

func TestRun_SkipsExistingPointer(t *testing.T) {
	pool := testutil.NewTestDB(t)
	root := seedVaultRoot(t)
	writeNote(t, root, "reference", "indexed.md", "# Already indexed")

	// Pre-insert a pointer for this source_ref.
	_, err := pool.DB().Exec(
		`INSERT INTO knowledge_pointers
		    (project_id, source_type, source_ref, question, invoke_when, tags, status)
		 VALUES ('test-proj', 'vault', '.claude/vault/reference/indexed.md',
		         'Existing', 'when', '[]', 'active')`)
	if err != nil {
		t.Fatalf("seed pointer: %v", err)
	}

	deps := newSeedDeps(t, pool, root, &mockScorer{question: "Q", score: 0.5})
	summary, _ := Run(context.Background(), deps, RunOpts{Project: "test-proj"})
	if summary.Added != 0 {
		t.Errorf("Added: want 0 (pointer exists), got %d", summary.Added)
	}
	if summary.Skipped != 1 {
		t.Errorf("Skipped: want 1, got %d", summary.Skipped)
	}
}

func TestRun_SkipsRejectedCandidates(t *testing.T) {
	pool := testutil.NewTestDB(t)
	root := seedVaultRoot(t)
	writeNote(t, root, "reference", "noise.md", "# Noise")

	// Pre-insert a rejected candidate (e.g. from a past bulk-reject sweep).
	id, err := curation.AddCandidate(context.Background(), pool, curation.CandidateInsert{
		ProjectID:   "test-proj",
		SourceType:  "vault",
		SourceRef:   ".claude/vault/reference/noise.md",
		Question:    "Old",
		InvokeWhen:  "Old",
		Description: "Old",
		Origin:      "session_mining",
	})
	if err != nil {
		t.Fatalf("seed candidate: %v", err)
	}
	if _, err := pool.DB().Exec(`UPDATE curation_candidates SET status='rejected' WHERE id=?`, id); err != nil {
		t.Fatalf("flip status: %v", err)
	}

	deps := newSeedDeps(t, pool, root, &mockScorer{question: "Q", score: 0.5})
	summary, _ := Run(context.Background(), deps, RunOpts{Project: "test-proj"})
	if summary.Added != 0 {
		t.Errorf("Added: want 0 (rejected candidate exists), got %d", summary.Added)
	}
	if summary.Skipped != 1 {
		t.Errorf("Skipped: want 1, got %d", summary.Skipped)
	}
}

func TestRun_AutoPromotesHighScore(t *testing.T) {
	pool := testutil.NewTestDB(t)
	root := seedVaultRoot(t)
	writeNote(t, root, "reference", "stellar.md", "# Stellar content")

	deps := newSeedDeps(t, pool, root, &mockScorer{question: "Q", score: 0.95})
	summary, _ := Run(context.Background(), deps, RunOpts{Project: "test-proj"})

	if summary.AutoPromoted != 1 {
		t.Errorf("AutoPromoted: want 1, got %d", summary.AutoPromoted)
	}

	var pointerCount int
	pool.DB().QueryRow(
		`SELECT COUNT(*) FROM knowledge_pointers
		  WHERE source_ref='.claude/vault/reference/stellar.md' AND status='active'`,
	).Scan(&pointerCount)
	if pointerCount != 1 {
		t.Errorf("pointer count: want 1, got %d", pointerCount)
	}
}

func TestRun_DryRunNoWrites(t *testing.T) {
	pool := testutil.NewTestDB(t)
	root := seedVaultRoot(t)
	writeNote(t, root, "reference", "dry.md", "# Dry")

	deps := newSeedDeps(t, pool, root, &mockScorer{question: "Q", score: 0.95})
	summary, _ := Run(context.Background(), deps, RunOpts{Project: "test-proj", DryRun: true})

	if summary.Added != 1 {
		t.Errorf("dry-run Added counter should still increment: %d", summary.Added)
	}

	var count int
	pool.DB().QueryRow(`SELECT COUNT(*) FROM curation_candidates`).Scan(&count)
	if count != 0 {
		t.Errorf("dry-run should not write: got %d candidates", count)
	}
}

func TestRun_RespectsLimit(t *testing.T) {
	pool := testutil.NewTestDB(t)
	root := seedVaultRoot(t)
	writeNote(t, root, "reference", "a.md", "# a")
	writeNote(t, root, "reference", "b.md", "# b")
	writeNote(t, root, "reference", "c.md", "# c")

	deps := newSeedDeps(t, pool, root, &mockScorer{question: "Q", score: 0.5})
	summary, _ := Run(context.Background(), deps, RunOpts{Project: "test-proj", Limit: 2})

	if summary.Added != 2 {
		t.Errorf("Added: want 2 (limited), got %d", summary.Added)
	}
}
