package refresolve_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"toolkit/internal/refresolve"
	"toolkit/internal/testutil"
)

// reference-resolution-migration T10: parsing MEMORY.md into a
// MemoryIndex maps the hyphenated identifiers in titles +
// descriptions to entries. Tests against a synthetic MEMORY.md so
// the test doesn't depend on the user's live auto-memory.
func TestLoadMemoryIndex_ParsesEntriesAndBuildsTokenMap(t *testing.T) {
	dir := t.TempDir()
	body := `- [ml-capability-substrate framing](reference_ml-capability-substrate-framing.md) — build the ML capability once
- [atomic-tasks vs atomic-agents](project_atomic-tasks-vs-atomic-agents.md) — name-collision prior art
- [Plain title](file.md) — no hyphenated tokens here at all
`
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	idx, err := refresolve.LoadMemoryIndex(dir)
	if err != nil {
		t.Fatalf("LoadMemoryIndex: %v", err)
	}
	if idx == nil || len(idx.Entries) != 3 {
		t.Fatalf("expected 3 entries; got %+v", idx)
	}
	got := idx.Lookup("ml-capability-substrate")
	if len(got) != 1 || got[0].Slug != "reference_ml-capability-substrate-framing.md" {
		t.Errorf("ml-capability-substrate lookup: %+v", got)
	}
	got = idx.Lookup("atomic-tasks")
	if len(got) != 1 || got[0].Slug != "project_atomic-tasks-vs-atomic-agents.md" {
		t.Errorf("atomic-tasks lookup: %+v", got)
	}
	if len(idx.Lookup("not-in-memory")) != 0 {
		t.Errorf("non-matching token returned hits")
	}
}

// own-memory-read-then-disable-harness-auto-memory T7: a materialized
// memory row's slug embeds its title (`- [linguistic-tics](linguistic-
// tics.md)`), so the same hyphenated token matches in BOTH title and
// slug. Without per-entry token dedup the entry index lands in byToken
// twice and Lookup returns it twice; and a second index line pointing
// at the same slug (a title-case duplicate the legacy harness writer
// left behind) adds a third. All must collapse to exactly one Lookup
// hit — duplicate candidates forced a spurious ask_user_to_disambiguate
// that cascaded into a reranker-projection rebuild abort.
func TestLoadMemoryIndex_DedupsSelfMatchingTokenAndSameSlugLines(t *testing.T) {
	dir := t.TempDir()
	body := `- [linguistic-tics](linguistic-tics.md) — Sophi noticed the agent appending -o to words
- [Linguistic tics](linguistic-tics.md) — title-case duplicate index line for the same file
- [worktree-agents-pattern](worktree-agents-pattern.md) — parallel worktree agents
`
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	idx, err := refresolve.LoadMemoryIndex(dir)
	if err != nil {
		t.Fatalf("LoadMemoryIndex: %v", err)
	}
	// Self-matching token (title) + a same-slug duplicate line: 1 hit.
	if got := idx.Lookup("linguistic-tics"); len(got) != 1 {
		t.Errorf("linguistic-tics: expected 1 dedup'd hit; got %d: %+v", len(got), got)
	}
	// Single line whose slug embeds its title: the self-match must not
	// double it.
	if got := idx.Lookup("worktree-agents-pattern"); len(got) != 1 {
		t.Errorf("worktree-agents-pattern: expected 1 hit; got %d: %+v", len(got), got)
	}
}

// End-to-end T10 acceptance: a message containing "ml-capability-
// substrate" produces a parse_context hit including the
// reference_ml-capability-substrate-framing.md entry as a Candidate.
func TestHandleParseContext_MemoryEntryFromHyphenatedToken(t *testing.T) {
	dir := t.TempDir()
	body := `- [ml-capability-substrate framing](reference_ml-capability-substrate-framing.md) — build the ML capability once; each model small
`
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	idx, err := refresolve.LoadMemoryIndex(dir)
	if err != nil {
		t.Fatalf("LoadMemoryIndex: %v", err)
	}
	pool := testutil.NewTestDB(t)
	registry := refresolve.NewRegistry()
	registry.Register(refresolve.NewMemoryEntryResolver(idx))
	deps := refresolve.HandlerDeps{
		Pool:      pool,
		Project:   "mcp-servers",
		Registry:  registry,
		MemoryDir: dir,
	}
	params, _ := json.Marshal(struct {
		MessageText string `json:"message_text"`
	}{MessageText: "what does the ml-capability-substrate framing say about model size?"})
	result, err := refresolve.HandleParseContext(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleParseContext: %v", err)
	}
	hasMemoryHit := false
	for _, ref := range result.References {
		if ref.Shape != refresolve.ShapeMemoryEntry {
			continue
		}
		for _, c := range ref.TopCandidates {
			if c.ID == "reference_ml-capability-substrate-framing.md" {
				hasMemoryHit = true
			}
		}
	}
	if !hasMemoryHit {
		t.Errorf("expected memory_entry candidate for reference_ml-capability-substrate-framing.md; got %+v",
			result.References)
	}
}
