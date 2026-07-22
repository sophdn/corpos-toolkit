package sources_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toolkit/internal/knowledge/curation"
	"toolkit/internal/knowledge/curation/sources"
	"toolkit/internal/testutil"
)

// --- TaskHandoffBuilder ---

func TestTaskHandoffBuilder_BuildsFromTasksRow(t *testing.T) {
	pool := testutil.NewTestDB(t)
	// Seed a projection task row directly. (Full forge wiring not needed;
	// we only care about the columns the builder reads.) Post-T6 the CRUD
	// `tasks` table is retired; production reads proj_current_tasks.
	// chain_id=1 is a synthetic foreign-key target — the builder reads
	// the task row by slug, not by chain join, so the chain row's
	// absence doesn't matter.
	testutil.SeedTask(t, pool, 1, "port-knowledge-curate", "closed", testutil.SeedTaskOpts{
		Position:         1,
		ProblemStatement: "Port the Rust binary to Go.",
		HandoffOutput:    "Done at commit abc123.",
	})

	b := sources.NewTaskHandoffBuilder()
	if b.Origin() != "task_handoff" {
		t.Errorf("Origin: want task_handoff, got %q", b.Origin())
	}

	out, err := b.Build(context.Background(), pool, curation.Candidate{
		SourceRef: "mcp-servers::port-knowledge-curate",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(out, "Port the Rust binary to Go.") {
		t.Errorf("Build output missing problem_statement: %q", out)
	}
	if !strings.Contains(out, "Done at commit abc123.") {
		t.Errorf("Build output missing handoff_output: %q", out)
	}
	if !strings.Contains(out, "Handoff:") {
		t.Errorf("Build output missing Handoff: separator: %q", out)
	}
}

func TestTaskHandoffBuilder_RejectsMalformedSourceRef(t *testing.T) {
	pool := testutil.NewTestDB(t)
	b := sources.NewTaskHandoffBuilder()
	_, err := b.Build(context.Background(), pool, curation.Candidate{
		SourceRef: "no-separator-here",
	})
	if err == nil {
		t.Fatal("Build: want error on malformed source_ref, got nil")
	}
}

func TestTaskHandoffBuilder_ErrorsOnMissingTask(t *testing.T) {
	pool := testutil.NewTestDB(t)
	b := sources.NewTaskHandoffBuilder()
	_, err := b.Build(context.Background(), pool, curation.Candidate{
		SourceRef: "mcp-servers::no-such-task",
	})
	if err == nil {
		t.Fatal("Build: want error on missing task, got nil")
	}
}

// --- VaultNoteBuilder ---

func TestVaultNoteBuilder_ReadsRelativeVaultPath(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "reference"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	relPath := "reference/design.md"
	content := "# Design\n\nThis is the design.\n"
	if err := os.WriteFile(filepath.Join(root, relPath), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	b := sources.NewVaultNoteBuilder(root)
	if b.Origin() != "session_mining" {
		t.Errorf("Origin: want session_mining, got %q", b.Origin())
	}

	out, err := b.Build(context.Background(), nil, curation.Candidate{
		SourceRef: ".claude/vault/reference/design.md",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if out != content {
		t.Errorf("Build content mismatch:\n  want: %q\n  got:  %q", content, out)
	}
}

func TestVaultNoteBuilder_RejectsPathTraversal(t *testing.T) {
	root := t.TempDir()
	b := sources.NewVaultNoteBuilder(root)
	_, err := b.Build(context.Background(), nil, curation.Candidate{
		SourceRef: ".claude/vault/../etc/passwd",
	})
	if err == nil {
		t.Fatal("Build: want error on path traversal, got nil")
	}
}

func TestVaultNoteBuilder_ErrorsOnMissingFile(t *testing.T) {
	root := t.TempDir()
	b := sources.NewVaultNoteBuilder(root)
	_, err := b.Build(context.Background(), nil, curation.Candidate{
		SourceRef: ".claude/vault/reference/missing.md",
	})
	if err == nil {
		t.Fatal("Build: want error on missing file, got nil")
	}
}

// --- ZeroResultGapBuilder ---

func TestZeroResultGapBuilder_ReconstructsQueryFromJSONL(t *testing.T) {
	pool := testutil.NewTestDB(t)

	// Insert grounding_events row.
	_, err := pool.DB().Exec(
		`INSERT INTO grounding_events (project_id, session_id, call_id, action, results_count, next_turn_has_output)
		 VALUES (?, ?, ?, ?, 0, 1)`,
		"mcp-servers", "sess-abc", "call-xyz", "knowledge_search",
	)
	if err != nil {
		t.Fatalf("insert grounding_event: %v", err)
	}
	var eventID int64
	if err := pool.DB().QueryRow(`SELECT last_insert_rowid()`).Scan(&eventID); err != nil {
		t.Fatalf("rowid: %v", err)
	}

	// Build a fake projects/ tree with a JSONL containing the tool_use.
	projectsRoot := t.TempDir()
	projDir := filepath.Join(projectsRoot, "-home-sophi-dev-mcp-servers")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	jsonl := `{"type":"system","message":{}}
{"type":"assistant","message":{"content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"call-xyz","input":{"params":{"query":"how do spans work"}}}]}}
{"type":"user","message":{}}
`
	if err := os.WriteFile(filepath.Join(projDir, "sess-abc.jsonl"), []byte(jsonl), 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}

	b := sources.NewZeroResultGapBuilder(projectsRoot)
	if b.Origin() != "zero_result_gap" {
		t.Errorf("Origin: want zero_result_gap, got %q", b.Origin())
	}

	originRef := "1" // first grounding_event in the test DB
	out, err := b.Build(context.Background(), pool, curation.Candidate{
		ID: 100, ProjectID: "mcp-servers", Origin: "zero_result_gap",
		OriginRef: &originRef,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(out, "action=knowledge_search") {
		t.Errorf("Build output missing action: %q", out)
	}
	if !strings.Contains(out, "query=how do spans work") {
		t.Errorf("Build output missing reconstructed query: %q", out)
	}
}

func TestZeroResultGapBuilder_HandlesParamsAsJSONString(t *testing.T) {
	// Variant: input.params is a JSON-encoded string (the MCP harness
	// shape mentioned in the Rust impl).
	pool := testutil.NewTestDB(t)
	_, err := pool.DB().Exec(
		`INSERT INTO grounding_events (project_id, session_id, call_id, action, results_count, next_turn_has_output)
		 VALUES (?, ?, ?, ?, 0, 1)`,
		"mcp-servers", "sess-1", "call-1", "knowledge_search",
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	projectsRoot := t.TempDir()
	projDir := filepath.Join(projectsRoot, "-home-sophi-dev-mcp-servers")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	jsonl := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call-1","input":{"params":"{\"query\":\"encoded string query\"}"}}]}}
`
	if err := os.WriteFile(filepath.Join(projDir, "sess-1.jsonl"), []byte(jsonl), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	b := sources.NewZeroResultGapBuilder(projectsRoot)
	originRef := "1"
	out, err := b.Build(context.Background(), pool, curation.Candidate{
		ID: 1, ProjectID: "mcp-servers", OriginRef: &originRef,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(out, "encoded string query") {
		t.Errorf("Build output missing encoded query: %q", out)
	}
}

func TestZeroResultGapBuilder_ErrorsOnMissingOriginRef(t *testing.T) {
	pool := testutil.NewTestDB(t)
	b := sources.NewZeroResultGapBuilder(t.TempDir())
	_, err := b.Build(context.Background(), pool, curation.Candidate{
		ID: 1, ProjectID: "mcp-servers", OriginRef: nil,
	})
	if err == nil {
		t.Fatal("Build: want error on missing origin_ref, got nil")
	}
}

func TestZeroResultGapBuilder_ErrorsOnMissingJSONL(t *testing.T) {
	pool := testutil.NewTestDB(t)
	_, err := pool.DB().Exec(
		`INSERT INTO grounding_events (project_id, session_id, call_id, action, results_count, next_turn_has_output)
		 VALUES (?, ?, ?, ?, 0, 1)`,
		"mcp-servers", "sess-missing", "call-missing", "knowledge_search",
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	b := sources.NewZeroResultGapBuilder(t.TempDir())
	originRef := "1"
	_, err = b.Build(context.Background(), pool, curation.Candidate{
		ID: 1, ProjectID: "mcp-servers", OriginRef: &originRef,
	})
	if err == nil {
		t.Fatal("Build: want error on missing JSONL, got nil")
	}
}

// --- DefaultBuilders wiring ---

func TestDefaultBuilders_ReturnsThreeKnownOrigins(t *testing.T) {
	builders := sources.DefaultBuilders("", "")
	if len(builders) != 3 {
		t.Fatalf("DefaultBuilders: want 3, got %d", len(builders))
	}
	seen := map[string]bool{}
	for _, b := range builders {
		seen[b.Origin()] = true
	}
	for _, want := range []string{"task_handoff", "session_mining", "zero_result_gap"} {
		if !seen[want] {
			t.Errorf("DefaultBuilders missing origin %q", want)
		}
	}
}

func TestDefaultBuilders_RegisterWithRegistry(t *testing.T) {
	reg := curation.NewBuilderRegistry()
	for _, b := range sources.DefaultBuilders("", "") {
		reg.Register(b)
	}
	for _, origin := range []string{"task_handoff", "session_mining", "zero_result_gap"} {
		if _, err := reg.ForOrigin(origin); err != nil {
			t.Errorf("ForOrigin(%q) after DefaultBuilders: %v", origin, err)
		}
	}
}
