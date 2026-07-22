package work_test

import (
	"context"
	"strings"
	"testing"

	"toolkit/internal/work"
)

// These pin the compact-list strict-param hardening: handlers REJECT params
// they don't recognise rather than silently dropping them (which returned
// unfiltered, authoritative-looking results). The hardening coexists with the
// dispatcher's bug-1070 nested-project repair.

// An unrecognised filter (bug_list has no pattern/query) must error and name
// the accepted filters, instead of silently returning the unfiltered list.
func TestBugList_RejectsUnknownParam(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "a", "open")

	_, err := work.HandleBugList(context.Background(), pool, "mcp-servers",
		mustJSON(t, map[string]any{"pattern": "a"}))
	if err == nil {
		t.Fatal("expected an error for an unknown param, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "pattern") || !strings.Contains(msg, "accepted") {
		t.Errorf("error should name the bad key and list accepted filters, got: %q", msg)
	}
}

// A misplaced `cwd` (an envelope-level field) errors with a placement hint.
func TestBugList_RejectsCwdNestedInParams(t *testing.T) {
	pool := openTestPool(t)
	_, err := work.HandleBugList(context.Background(), pool, "",
		mustJSON(t, map[string]any{"cwd": "/home/x"}))
	if err == nil || !strings.Contains(err.Error(), "envelope") {
		t.Fatalf("expected envelope-level hint for cwd, got: %v", err)
	}
}

// Coexistence with bug 1070: a `project` nested in params must NOT be rejected
// (the dispatcher promotes it to the envelope and the handler tolerates the
// leftover key). The call decodes cleanly and scopes by the resolved project.
func TestBugList_ToleratesNestedProjectFromBug1070(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "a", "open")
	seedBug(t, pool, "seed-packet", "b", "open")

	// Dispatch would promote params.project → the project arg; here we pass the
	// already-resolved project and the leftover params.project, and assert no
	// strict-decode rejection + correct scoping.
	resp, err := work.HandleBugList(context.Background(), pool, "mcp-servers",
		mustJSON(t, map[string]any{"project": "mcp-servers", "status": "open"}))
	if err != nil {
		t.Fatalf("nested project must be tolerated, got: %v", err)
	}
	if len(resp.DefaultItems) != 1 || resp.DefaultItems[0].Slug != "a" {
		t.Errorf("expected only mcp-servers bug 'a', got %+v", resp.DefaultItems)
	}
}

// Recognised filters still decode and scope.
func TestBugList_ValidFilterStillWorks(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "a", "open")
	seedBug(t, pool, "mcp-servers", "b", "fixed")

	resp, err := work.HandleBugList(context.Background(), pool, "mcp-servers",
		mustJSON(t, map[string]any{"status": "open"}))
	if err != nil {
		t.Fatalf("HandleBugList: %v", err)
	}
	if len(resp.DefaultItems) != 1 || resp.DefaultItems[0].Slug != "a" {
		t.Errorf("expected only open bug 'a', got %+v", resp.DefaultItems)
	}
}

// suggestion_list shares the helper: unknown filter rejected, nested project
// tolerated.
func TestSuggestionList_RejectsUnknownParam(t *testing.T) {
	pool := openTestPool(t)
	_, err := work.HandleSuggestionList(context.Background(), pool, "mcp-servers",
		mustJSON(t, map[string]any{"pattern": "x"}))
	if err == nil || !strings.Contains(err.Error(), "accepted") {
		t.Fatalf("expected unknown-param error listing accepted filters, got: %v", err)
	}
}

func TestSuggestionList_ToleratesNestedProject(t *testing.T) {
	pool := openTestPool(t)
	_, err := work.HandleSuggestionList(context.Background(), pool, "mcp-servers",
		mustJSON(t, map[string]any{"project": "mcp-servers", "status": "open"}))
	if err != nil {
		t.Fatalf("nested project must be tolerated, got: %v", err)
	}
}
