package arcreview

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"toolkit/internal/events"
)

// seedBugReported emits one BugReported event for the project. The
// recent-filings query reads from this event type to assemble the
// in-arc dedupe block; tests use the helper to populate the events
// table without going through the forge boundary.
func seedBugReported(t *testing.T, p Deps, project, slug, title string) {
	t.Helper()
	_, _ = p.Pool.DB().Exec(`INSERT INTO projects (id, name) VALUES (?, ?) ON CONFLICT DO NOTHING`, project, project)
	err := p.Pool.WithWrite(context.Background(), func(tx *sql.Tx) error {
		_, emitErr := events.Emit(context.Background(), tx, events.EmitArgs{
			Entity: events.NewEntityRef("bug", slug, project),
			Payload: events.BugReportedPayload{
				Title:            title,
				ProblemStatement: "test problem statement",
			},
		})
		return emitErr
	})
	if err != nil {
		t.Fatalf("seed BugReported %s: %v", slug, err)
	}
}

func TestRecentFilingsInArc_EmptyEvents(t *testing.T) {
	pool := openTestPool(t)
	out, err := recentFilingsInArc(context.Background(), pool, "mcp-servers", time.Now().Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected 0 filings on empty events; got %d", len(out))
	}
}

func TestRecentFilingsInArc_ReturnsRecentBugsTitleAndSlug(t *testing.T) {
	pool := openTestPool(t)
	deps := Deps{Pool: pool}
	seedBugReported(t, deps, "mcp-servers", "first-recent-bug", "First Recent Bug Title")
	seedBugReported(t, deps, "mcp-servers", "second-recent-bug", "Second Recent Bug Title")

	out, err := recentFilingsInArc(context.Background(), pool, "mcp-servers", time.Now().Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 filings; got %d (%+v)", len(out), out)
	}
	// Returned newest-first per the ORDER BY ts DESC.
	if out[0].Slug != "second-recent-bug" {
		t.Errorf("expected newest first (second-recent-bug); got %q", out[0].Slug)
	}
	if out[0].Title != "Second Recent Bug Title" {
		t.Errorf("title extraction missed: got %q", out[0].Title)
	}
	if out[1].Slug != "first-recent-bug" || out[1].Title != "First Recent Bug Title" {
		t.Errorf("second row mismatch: %+v", out[1])
	}
	for _, r := range out {
		if r.Kind != "bug" {
			t.Errorf("Kind must be 'bug' for BugReported source; got %q", r.Kind)
		}
	}
}

func TestRecentFilingsInArc_RespectsProjectScope(t *testing.T) {
	pool := openTestPool(t)
	deps := Deps{Pool: pool}
	seedBugReported(t, deps, "mcp-servers", "scoped-bug", "Scoped Bug")
	seedBugReported(t, deps, "other-project", "other-project-bug", "Other Project Bug")

	out, err := recentFilingsInArc(context.Background(), pool, "mcp-servers", time.Now().Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("project scope should isolate; got %d (%+v)", len(out), out)
	}
	if out[0].Slug != "scoped-bug" {
		t.Errorf("project scope leaked: %q", out[0].Slug)
	}
}

func TestRecentFilingsInArc_RespectsTimeWindow(t *testing.T) {
	pool := openTestPool(t)
	deps := Deps{Pool: pool}
	seedBugReported(t, deps, "mcp-servers", "recent-bug", "Recent Bug")
	// since=future timestamp → no rows.
	out, err := recentFilingsInArc(context.Background(), pool, "mcp-servers", time.Now().Add(1*time.Hour))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("future since should filter all rows; got %d", len(out))
	}
}

func TestRecentFilingsInArc_CapsAtTwentyRows(t *testing.T) {
	pool := openTestPool(t)
	deps := Deps{Pool: pool}
	for i := 0; i < 25; i++ {
		seedBugReported(t, deps, "mcp-servers", "bulk-bug-"+string(rune('a'+i)), "Bulk Bug")
	}
	out, err := recentFilingsInArc(context.Background(), pool, "mcp-servers", time.Now().Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(out) != recentFilingsCap {
		t.Fatalf("expected cap=%d on the result; got %d", recentFilingsCap, len(out))
	}
}

func TestRecentFilingsInArc_NilPoolErrs(t *testing.T) {
	_, err := recentFilingsInArc(context.Background(), nil, "mcp-servers", time.Now())
	if err == nil {
		t.Fatalf("expected error on nil pool")
	}
}

func TestExtractInArcFilingsFromSnapshot_DetectsForgeVaultNote(t *testing.T) {
	snap := Snapshot{
		Messages: []Message{
			{
				Role: "assistant",
				Content: `Let me file this.  [tool_use: mcp__toolkit-server__work] {"action":"forge","params":{"schema_name":"vault-note","note_kind":"learning","title":"Orphan Pointer Cleanup Process","body":"...","tags":"vault,orphan"}, "project":"mcp-servers"}  ` +
					`After that:  [tool_use: mcp__toolkit-server__work] {"action":"forge","params":{"schema_name":"bug","title":"Some Bug Title","problem_statement":"foo"}, "project":"mcp-servers"}`,
			},
		},
	}
	got := extractInArcFilingsFromSnapshot(snap)
	if len(got) != 2 {
		t.Fatalf("expected 2 in-arc filings (vault_note + bug); got %d (%+v)", len(got), got)
	}
	if got[0].Kind != "vault_note" || got[0].Title != "Orphan Pointer Cleanup Process" {
		t.Errorf("first row: expected vault_note kind with the orphan-pointer title; got %+v", got[0])
	}
	if got[1].Kind != "bug" || got[1].Title != "Some Bug Title" {
		t.Errorf("second row: expected bug kind with the bug title; got %+v", got[1])
	}
}

func TestExtractInArcFilingsFromSnapshot_DetectsWriteToVaultPath(t *testing.T) {
	snap := Snapshot{
		Messages: []Message{
			{
				Role: "assistant",
				Content: `Filing via Write.  [tool_use: Write] {"file_path":"/home/user/.claude/vault/decisions/2026-05-20_some-decision.md","content":"..."}  ` +
					`Editing too.  [tool_use: Edit] {"file_path":"/home/user/.claude/vault/learnings/2026-05-20_some-learning.md","old_string":"a","new_string":"b"}`,
			},
		},
	}
	got := extractInArcFilingsFromSnapshot(snap)
	if len(got) != 2 {
		t.Fatalf("expected 2 filings from Write+Edit; got %d (%+v)", len(got), got)
	}
	for _, g := range got {
		if g.Kind != "vault_note" {
			t.Errorf("Write/Edit against /vault/ must classify as vault_note; got %q", g.Kind)
		}
	}
}

func TestExtractInArcFilingsFromSnapshot_SkipsUserTurnsAndNonFilingToolUses(t *testing.T) {
	snap := Snapshot{
		Messages: []Message{
			{
				// User turns sometimes contain rendered tool_use blocks
				// (echoed back); ignore them — only the agent's own
				// filings count.
				Role:    "user",
				Content: `[tool_use: mcp__toolkit-server__work] {"action":"forge","params":{"schema_name":"vault-note","title":"Should Not Count"}}`,
			},
			{
				Role:    "assistant",
				Content: `[tool_use: Read] {"file_path":"/home/user/dev/something.go"} [tool_use: Bash] {"command":"ls"}`,
			},
		},
	}
	got := extractInArcFilingsFromSnapshot(snap)
	if len(got) != 0 {
		t.Fatalf("expected 0 filings; got %d (%+v)", len(got), got)
	}
}

func TestMergeRecentFilings_DedupesByKindAndSlug(t *testing.T) {
	primary := []RecentFiling{
		{Kind: "bug", Slug: "alpha", Title: "Alpha From Events"},
		{Kind: "vault_note", Slug: "beta", Title: "Beta"},
	}
	secondary := []RecentFiling{
		{Kind: "bug", Slug: "alpha", Title: "Alpha From Snapshot"}, // dup
		{Kind: "vault_note", Slug: "gamma", Title: "Gamma"},        // new
		{Kind: "", Slug: "", Title: "skip empty"},                  // skipped
	}
	out := mergeRecentFilings(primary, secondary)
	if len(out) != 3 {
		t.Fatalf("expected 3 merged rows; got %d (%+v)", len(out), out)
	}
	// Primary order preserved; new entries appended.
	if out[0].Title != "Alpha From Events" {
		t.Errorf("primary should win on collision; got title=%q", out[0].Title)
	}
	if out[2].Slug != "gamma" {
		t.Errorf("new secondary entry should append; got %+v", out[2])
	}
}

func TestTitleToSlugApprox_NormalizesCommonCases(t *testing.T) {
	cases := map[string]string{
		"Orphan Pointer Cleanup Process": "orphan-pointer-cleanup-process",
		"  Leading And Trailing  ":       "leading-and-trailing",
		"underscore_and-hyphen mix":      "underscore-and-hyphen-mix",
		"Mixed!@# Punctuation_2026":      "mixed-punctuation-2026",
		"":                               "",
	}
	for in, want := range cases {
		if got := titleToSlugApprox(in); got != want {
			t.Errorf("titleToSlugApprox(%q) = %q; want %q", in, got, want)
		}
	}
}
