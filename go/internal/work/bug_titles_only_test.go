package work_test

import (
	"context"
	"testing"

	"toolkit/internal/work"
)

func TestBugList_TitlesOnlyReturnsLightProjection(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "alpha", "open")
	seedBug(t, pool, "mcp-servers", "beta", "fixed")

	resp, _ := work.HandleBugList(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"titles_only": true,
	}))
	if !resp.TitlesOnly || len(resp.TitlesItems) != 2 {
		t.Fatalf("expected 2 titles-only rows, got %+v", resp)
	}
	for _, b := range resp.TitlesItems {
		if b.Slug == "" || b.Title == "" || b.Status == "" {
			t.Errorf("titles_only row missing required field: %+v", b)
		}
	}
}

func TestBugList_TitlesOnlyTakesPrecedenceOverVerbose(t *testing.T) {
	// Both flags passed → titles_only wins (smallest projection).
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "alpha", "open")

	resp, _ := work.HandleBugList(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"titles_only": true,
		"verbose":     true,
	}))
	if !resp.TitlesOnly {
		t.Errorf("expected TitlesOnly=true when both flags set, got %+v", resp)
	}
	if resp.Verbose {
		t.Errorf("verbose should be false when titles_only wins, got %+v", resp)
	}
}
