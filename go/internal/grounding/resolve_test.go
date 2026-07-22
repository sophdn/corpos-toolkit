package grounding

import (
	"testing"

	"toolkit/internal/telemetry"
)

func TestCollectTerminalEvents_BugResolve(t *testing.T) {
	entries := []jsonlEntry{
		mustEntry(t, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"u1","name":"mcp__toolkit-server__work","input":{"action":"bug_resolve","project":"mcp-servers","params":{"slug":"forge-bug-title-omitted","resolution_note":"Fixed per 2026-05-12_foo learning."}}}]}}`),
	}
	entries[0].PromptID = "prompt-A"

	got := collectTerminalEvents(entries, "")
	if len(got) != 1 {
		t.Fatalf("want 1 terminal event, got %d", len(got))
	}
	te := got[0]
	if te.EntityKind != "bug" || te.EntitySlug != "forge-bug-title-omitted" ||
		te.OutcomeKind != telemetry.OutcomeResolved || te.EventType != "BugResolved" {
		t.Errorf("envelope mismatch: %+v", te)
	}
	if te.EntityProjectID != "mcp-servers" {
		t.Errorf("project_id = %q, want mcp-servers", te.EntityProjectID)
	}
	if te.Rationale == "" || te.PromptID != "prompt-A" {
		t.Errorf("rationale/prompt mismatch: %+v", te)
	}
}

func TestCollectTerminalEvents_TaskComplete(t *testing.T) {
	entries := []jsonlEntry{
		mustEntry(t, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"u2","name":"mcp__toolkit-server__work","input":{"action":"task_complete","project":"mcp-servers","params":{"slug":"design-substrate","closure_summary":"Landed migration 037."}}}]}}`),
	}
	got := collectTerminalEvents(entries, "")
	if len(got) != 1 || got[0].EventType != "TaskCompleted" {
		t.Fatalf("want one TaskCompleted, got %+v", got)
	}
	if got[0].Rationale != "Landed migration 037." {
		t.Errorf("rationale wrong: %q", got[0].Rationale)
	}
}

func TestCollectTerminalEvents_FallsBackToDefaultProject(t *testing.T) {
	entries := []jsonlEntry{
		mustEntry(t, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"u","name":"mcp__toolkit-server__work","input":{"action":"chain_close","params":{"slug":"some-chain","closure_summary":"done"}}}]}}`),
	}
	got := collectTerminalEvents(entries, "mcp-servers")
	if len(got) != 1 || got[0].EntityProjectID != "mcp-servers" {
		t.Fatalf("default project not applied: %+v", got)
	}
}

func TestCollectTerminalEvents_IgnoresNonTerminal(t *testing.T) {
	entries := []jsonlEntry{
		mustEntry(t, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"u","name":"mcp__toolkit-server__work","input":{"action":"task_start","params":{"slug":"x"}}}]}}`),
	}
	got := collectTerminalEvents(entries, "")
	if len(got) != 0 {
		t.Fatalf("non-terminal action should not appear: %+v", got)
	}
}

func TestDetectResolvedFrom_SlugMatch(t *testing.T) {
	refs := []string{"learnings/general/2026-05-12_foo.md", "decisions/2026-05-09_bar.md"}
	rationale := "Fixed by consulting 2026-05-12_foo for the boundary pattern."
	got := detectResolvedFrom(refs, rationale)
	if len(got) != 1 {
		t.Fatalf("want one resolved-from, got %+v", got)
	}
	if got[0].SourceRef != refs[0] || got[0].ClickKind != telemetry.ClickResolvedFrom {
		t.Errorf("mismatch: %+v", got[0])
	}
}

func TestDetectResolvedFrom_SchemePrefixedChainByBareSlug(t *testing.T) {
	// bug 958: a chain named by its bare terminal slug in a terminal rationale
	// must fire resolved-from on the scheme-prefixed ref.
	refs := []string{"chain:mcp-servers::action-docs-corpus"}
	rationale := "Resolved by applying the action-docs-corpus findings."
	got := detectResolvedFrom(refs, rationale)
	if len(got) != 1 || got[0].SourceRef != refs[0] || got[0].ClickKind != telemetry.ClickResolvedFrom {
		t.Fatalf("want one resolved-from on the chain ref, got %+v", got)
	}
}

func TestDetectResolvedFrom_EmptyRationale(t *testing.T) {
	refs := []string{"learnings/general/2026-05-12_foo.md"}
	if got := detectResolvedFrom(refs, ""); len(got) != 0 {
		t.Fatalf("empty rationale should not fire resolved-from, got %+v", got)
	}
}

func TestDetectResolvedFrom_NoMatch(t *testing.T) {
	refs := []string{"learnings/general/2026-05-12_foo.md"}
	rationale := "fixed by reading entirely unrelated source"
	if got := detectResolvedFrom(refs, rationale); len(got) != 0 {
		t.Fatalf("unrelated rationale should not match, got %+v", got)
	}
}
