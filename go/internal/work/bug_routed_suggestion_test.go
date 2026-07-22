package work_test

import (
	"context"
	"testing"

	"toolkit/internal/testutil"
	"toolkit/internal/work"
)

// TestBugRead_SurfacesRoutedSuggestionSlug pins the additive read-path
// shape after migration 054: bug_read must return routed_suggestion_slug
// as a JSON string (empty when unset, not null / not omitted).
func TestBugRead_SurfacesRoutedSuggestionSlug(t *testing.T) {
	pool := openTestPool(t)
	testutil.SeedBug(t, pool, "mcp-servers", "b1", "open", testutil.SeedBugOpts{
		Title:                "B1",
		Severity:             "low",
		RoutedSuggestionSlug: "sug-improve-frobnitz",
	})

	resp, err := work.HandleBugRead(context.Background(), pool, "", mustJSON(t, map[string]any{"slug": "b1"}))
	if err != nil {
		t.Fatalf("HandleBugRead: %v", err)
	}
	if resp.Bug == nil || resp.Bug.RoutedSuggestionSlug != "sug-improve-frobnitz" {
		t.Fatalf("bug_read should surface routed_suggestion_slug: %+v", resp.Bug)
	}

	// Default '' for unset rows — not null, not omitted.
	testutil.SeedBug(t, pool, "mcp-servers", "b2", "open", testutil.SeedBugOpts{
		Title:    "B2",
		Severity: "low",
	})
	resp2, _ := work.HandleBugRead(context.Background(), pool, "", mustJSON(t, map[string]any{"slug": "b2"}))
	if resp2.Bug == nil || resp2.Bug.RoutedSuggestionSlug != "" {
		t.Fatalf("default should be empty string, got: %+v", resp2.Bug)
	}
}

// TestBugList_SurfacesRoutedSuggestionSlug pins that the field lands on
// both compact and verbose projections from bug_list.
func TestBugList_SurfacesRoutedSuggestionSlug(t *testing.T) {
	pool := openTestPool(t)
	testutil.SeedBug(t, pool, "mcp-servers", "b1", "open", testutil.SeedBugOpts{
		Title:                "B1",
		Severity:             "low",
		RoutedSuggestionSlug: "sug-a",
	})
	testutil.SeedBug(t, pool, "mcp-servers", "b2", "open", testutil.SeedBugOpts{
		Title:    "B2",
		Severity: "low",
	})

	// Compact projection.
	resp, err := work.HandleBugList(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{}))
	if err != nil {
		t.Fatalf("HandleBugList: %v", err)
	}
	found := map[string]string{}
	for _, item := range resp.DefaultItems {
		found[item.Slug] = item.RoutedSuggestionSlug
	}
	if found["b1"] != "sug-a" || found["b2"] != "" {
		t.Errorf("compact projection routing field: %+v", found)
	}

	// Verbose projection.
	respV, err := work.HandleBugList(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"verbose": true}))
	if err != nil {
		t.Fatalf("HandleBugList verbose: %v", err)
	}
	foundV := map[string]string{}
	for _, item := range respV.VerboseItems {
		foundV[item.Slug] = item.RoutedSuggestionSlug
	}
	if foundV["b1"] != "sug-a" || foundV["b2"] != "" {
		t.Errorf("verbose projection routing field: %+v", foundV)
	}
}

// TestBugResolve_AcceptsRoutedSuggestionSlug pins that bug_resolve writes
// the new column when supplied. Mirrors the routed_chain_slug / routed_
// task_slug param shape.
func TestBugResolve_AcceptsRoutedSuggestionSlug(t *testing.T) {
	pool := openTestPool(t)
	testutil.SeedBug(t, pool, "mcp-servers", "rb", "open", testutil.SeedBugOpts{
		Title:    "RB",
		Severity: "medium",
	})

	resp, err := work.HandleBugResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":                   "rb",
		"resolution_kind":        "fixed",
		"commit_sha":             "abc1234",
		"routed_suggestion_slug": "sug-followup",
	}))
	if err != nil {
		t.Fatalf("HandleBugResolve: %v", err)
	}
	if !resp.OK {
		t.Fatalf("resolve: %+v", resp)
	}

	readResp, _ := work.HandleBugRead(context.Background(), pool, "", mustJSON(t, map[string]any{"slug": "rb"}))
	if readResp.Bug == nil || readResp.Bug.RoutedSuggestionSlug != "sug-followup" {
		t.Errorf("read after resolve: %+v", readResp.Bug)
	}
}
