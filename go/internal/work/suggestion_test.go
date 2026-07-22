package work_test

import (
	"context"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/work"
)

func seedSuggestion(t *testing.T, pool *db.Pool, project, slug, status string) {
	t.Helper()
	// Direct write to the projection table (post-T6 CRUD retirement).
	// proj_current_suggestions has no AUTOINCREMENT — derive next id
	// from COALESCE(MAX(id), 0) + 1.
	if _, err := pool.DB().Exec(
		`INSERT INTO proj_current_suggestions
		 (id, slug, project_id, title, problem_statement, status, filed_at, updated_at)
		 VALUES ((SELECT COALESCE(MAX(id), 0) + 1 FROM proj_current_suggestions),
		         ?, ?, ?, 'detail', ?, datetime('now'), datetime('now'))`,
		slug, project, "T:"+slug, status); err != nil {
		t.Fatalf("seed suggestion %q: %v", slug, err)
	}
}

func TestSuggestionList_EmptyScopeReturnsCrossProject(t *testing.T) {
	pool := openTestPool(t)
	if _, err := pool.DB().Exec(
		`INSERT INTO projects (id, name) VALUES ('seed-packet', 'seed-packet')`); err != nil {
		t.Fatalf("seed second project: %v", err)
	}
	seedSuggestion(t, pool, "mcp-servers", "a", "open")
	seedSuggestion(t, pool, "seed-packet", "b", "open")

	resp, err := work.HandleSuggestionList(context.Background(), pool, "", mustJSON(t, map[string]any{}))
	if err != nil {
		t.Fatalf("HandleSuggestionList: %v", err)
	}
	if resp.Err != nil {
		t.Fatalf("expected cross-project list, got error envelope: %+v", resp.Err)
	}
	if len(resp.DefaultItems) != 2 {
		t.Errorf("expected 2 items across both projects, got %+v", resp.DefaultItems)
	}
}

func TestSuggestionList_StatusFilter(t *testing.T) {
	pool := openTestPool(t)
	seedSuggestion(t, pool, "mcp-servers", "a", "open")
	seedSuggestion(t, pool, "mcp-servers", "b", "adopted")

	resp, err := work.HandleSuggestionList(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"status": "open"}))
	if err != nil {
		t.Fatalf("HandleSuggestionList: %v", err)
	}
	if len(resp.DefaultItems) != 1 || resp.DefaultItems[0].Slug != "a" {
		t.Errorf("status filter wrong: %+v", resp.DefaultItems)
	}
}

func TestSuggestionRead_BySlugAndID(t *testing.T) {
	pool := openTestPool(t)
	seedSuggestion(t, pool, "mcp-servers", "my-sug", "open")

	resp, err := work.HandleSuggestionRead(context.Background(), pool, "", mustJSON(t, map[string]any{"slug": "my-sug"}))
	if err != nil {
		t.Fatalf("HandleSuggestionRead by slug: %v", err)
	}
	if resp.Err != nil || resp.Suggestion == nil || resp.Suggestion.Slug != "my-sug" {
		t.Fatalf("read by slug: %+v err=%+v", resp.Suggestion, resp.Err)
	}

	id := resp.Suggestion.ID
	resp2, err := work.HandleSuggestionRead(context.Background(), pool, "", mustJSON(t, map[string]any{"id": id}))
	if err != nil {
		t.Fatalf("HandleSuggestionRead by id: %v", err)
	}
	if resp2.Err != nil || resp2.Suggestion == nil || resp2.Suggestion.ID != id {
		t.Fatalf("read by id: %+v err=%+v", resp2.Suggestion, resp2.Err)
	}
}

func TestSuggestionResolve_RejectsBugSideKind(t *testing.T) {
	pool := openTestPool(t)
	seedSuggestion(t, pool, "mcp-servers", "my-sug", "open")

	resp, err := work.HandleSuggestionResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "my-sug", "kind": "fixed",
	}))
	if err != nil {
		t.Fatalf("HandleSuggestionResolve: %v", err)
	}
	if resp.Error == "" {
		t.Fatalf("expected error envelope for bug-side kind, got success: %+v", resp)
	}
	if !contains(resp.Error, "adopted") || !contains(resp.Error, "deferred") || !contains(resp.Error, "rejected") {
		t.Errorf("error should name suggestion-side vocabulary, got: %q", resp.Error)
	}
}

func TestSuggestionResolve_HappyPathAdopted(t *testing.T) {
	pool := openTestPool(t)
	seedSuggestion(t, pool, "mcp-servers", "my-sug", "open")

	resp, err := work.HandleSuggestionResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":              "my-sug",
		"kind":              "adopted",
		"routed_chain_slug": "fts5-on-roadmap-list",
	}))
	if err != nil {
		t.Fatalf("HandleSuggestionResolve: %v", err)
	}
	if !resp.OK || resp.Status != "adopted" {
		t.Fatalf("resolve adopted: %+v", resp)
	}

	// Round-trip via suggestion_read confirms the row landed.
	readResp, _ := work.HandleSuggestionRead(context.Background(), pool, "", mustJSON(t, map[string]any{"slug": "my-sug"}))
	if readResp.Suggestion == nil || readResp.Suggestion.Status != "adopted" || readResp.Suggestion.RoutedChainSlug != "fts5-on-roadmap-list" {
		t.Errorf("read after resolve: %+v", readResp.Suggestion)
	}
}

func TestSuggestionResolve_VerbAliasAdopt(t *testing.T) {
	pool := openTestPool(t)
	seedSuggestion(t, pool, "mcp-servers", "my-sug", "open")

	resp, err := work.HandleSuggestionResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "my-sug", "kind": "adopt",
	}))
	if err != nil {
		t.Fatalf("HandleSuggestionResolve: %v", err)
	}
	if !resp.OK || resp.Status != "adopted" {
		t.Fatalf("verb alias `adopt` should normalise to `adopted`: %+v", resp)
	}
}

// TestSuggestionResolve_AcceptsResolutionNoteAliases — sibling table-
// driven contract pin to TestBugResolve_AcceptsResolutionNoteAliases.
// Same alias culture, same fix shape across both resolve actions.
// Bug 549 (notes) + bug 858 (resolution_summary, summary).
func TestSuggestionResolve_AcceptsResolutionNoteAliases(t *testing.T) {
	aliases := []struct {
		key, slug string
	}{
		{"notes", "sug-alias-notes"},
		{"resolution_summary", "sug-alias-resolution-summary"},
		{"summary", "sug-alias-summary"},
	}
	for _, c := range aliases {
		t.Run(c.key, func(t *testing.T) {
			pool := openTestPool(t)
			seedSuggestion(t, pool, "mcp-servers", c.slug, "open")
			value := "adopted via " + c.key + " alias"
			resp, err := work.HandleSuggestionResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
				"slug": c.slug, "kind": "adopted",
				c.key: value,
			}))
			if err != nil {
				t.Fatalf("HandleSuggestionResolve: %v", err)
			}
			if !resp.OK {
				t.Fatalf("resolve rejected: %+v", resp)
			}
			var note string
			if err := pool.DB().QueryRow(
				`SELECT json_extract(payload, '$.resolution_note') FROM events WHERE type='SuggestionResolved' AND entity_slug=?`,
				c.slug,
			).Scan(&note); err != nil {
				t.Fatalf("read event payload: %v", err)
			}
			if note != value {
				t.Errorf("alias %q did not persist to resolution_note: got %q, want %q", c.key, note, value)
			}
		})
	}
}

func TestSuggestionResolve_DefaultsToAdoptedWhenSHAProvided(t *testing.T) {
	pool := openTestPool(t)
	seedSuggestion(t, pool, "mcp-servers", "my-sug", "open")

	resp, err := work.HandleSuggestionResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "my-sug", "commit_sha": "abc1234",
	}))
	if err != nil {
		t.Fatalf("HandleSuggestionResolve: %v", err)
	}
	if !resp.OK || resp.Status != "adopted" {
		t.Fatalf("commit_sha without kind should default kind=adopted: %+v", resp)
	}
}

func TestSuggestionReopen_FromAdopted(t *testing.T) {
	pool := openTestPool(t)
	seedSuggestion(t, pool, "mcp-servers", "my-sug", "open")

	if _, err := work.HandleSuggestionResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "my-sug", "kind": "adopted",
	})); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	resp, err := work.HandleSuggestionReopen(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "my-sug",
	}))
	if err != nil {
		t.Fatalf("HandleSuggestionReopen: %v", err)
	}
	if !resp.OK || resp.Status != "open" {
		t.Fatalf("reopen: %+v", resp)
	}
}

func TestSuggestionStampSHA_AcceptsUnversioned(t *testing.T) {
	pool := openTestPool(t)
	seedSuggestion(t, pool, "mcp-servers", "my-sug", "open")
	if _, err := work.HandleSuggestionResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "my-sug", "kind": "adopted",
	})); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	resp, err := work.HandleSuggestionStampSHA(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "my-sug", "sha": "unversioned",
	}))
	if err != nil {
		t.Fatalf("HandleSuggestionStampSHA: %v", err)
	}
	if !resp.OK || resp.ResolvedCommitSHA != "unversioned" {
		t.Fatalf("stamp unversioned: %+v", resp)
	}
}

// TestSuggestionFTS_PersistsAcrossResolve pins that the suggestions_fts
// shadow row survives a suggestion_resolve transition — the lifecycle
// handler does not touch title or problem_statement, so the FTS5 row
// should remain matchable after the parent row's status flips.
func TestSuggestionFTS_PersistsAcrossResolve(t *testing.T) {
	pool := openTestPool(t)
	if _, err := pool.DB().Exec(
		`INSERT INTO proj_current_suggestions
		 (id, slug, project_id, title, problem_statement, status, filed_at, updated_at)
		 VALUES ((SELECT COALESCE(MAX(id), 0) + 1 FROM proj_current_suggestions),
		         'fts-sug', 'mcp-servers', 'Fast retry on benchmark replay', 'detail', 'open',
		         datetime('now'), datetime('now'))`,
	); err != nil {
		t.Fatalf("seed suggestion: %v", err)
	}
	var id int64
	if err := pool.DB().QueryRow(`SELECT id FROM proj_current_suggestions WHERE slug = 'fts-sug'`).Scan(&id); err != nil {
		t.Fatalf("id lookup: %v", err)
	}
	if _, err := pool.DB().Exec(
		`INSERT INTO suggestions_fts (rowid, title, problem_statement)
		 VALUES (?, 'Fast retry on benchmark replay', 'detail')`, id); err != nil {
		t.Fatalf("seed fts row: %v", err)
	}

	if _, err := work.HandleSuggestionResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "fts-sug", "kind": "adopted",
	})); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	var hit int64
	if err := pool.DB().QueryRow(
		`SELECT rowid FROM suggestions_fts WHERE suggestions_fts MATCH ? LIMIT 1`,
		"benchmark",
	).Scan(&hit); err != nil {
		t.Fatalf("fts5 MATCH after resolve: %v", err)
	}
	if hit != id {
		t.Errorf("fts5 hit id=%d, want %d (parent row id)", hit, id)
	}
}

func TestSuggestionStampSHA_RefusesOpenSuggestion(t *testing.T) {
	pool := openTestPool(t)
	seedSuggestion(t, pool, "mcp-servers", "my-sug", "open")

	resp, err := work.HandleSuggestionStampSHA(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "my-sug", "sha": "abc1234",
	}))
	if err != nil {
		t.Fatalf("HandleSuggestionStampSHA: %v", err)
	}
	if resp.OK || resp.Error == "" {
		t.Fatalf("expected error for stamping open suggestion: %+v", resp)
	}
}
