package construct_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"toolkit/internal/construct"
	"toolkit/internal/db"
)

// handle_test.go is the end-to-end characterization net for the agent-facing
// forge-shaped dispatch (construct.HandleForgeCreate/Edit/Delete) — the parse
// front → persistence → finalize tail path the work-table forge/forge_edit/
// forge_delete actions + the record-sugar surface route through. It replaces
// the former cmd/toolkit-server agent_forge_test.go (which compared the adapter
// against a now-archived forge.HandleForge baseline) with construct-only value
// assertions on the surviving orchestrators (chain 311 T7 Stage 6 P2-C.2).

func forgeDeleteRaw(t *testing.T, pool *db.Pool, project string, raw json.RawMessage) (construct.ForgeDeleteResult, error) {
	t.Helper()
	deps := construct.Deps{Pool: pool, Schemas: loadForgeRegistry(t)}
	return construct.HandleForgeDelete(context.Background(), deps, project, raw)
}

// TestHandleForgeCreate_CoveredSchemas: bug/suggestion/chain/task create through
// the orchestrator land Ok=true + the projection row.
func TestHandleForgeCreate_CoveredSchemas(t *testing.T) {
	pool := openTestPool(t)
	cases := []struct {
		name, raw, table, slug string
	}{
		{"bug", `{"schema_name":"bug","slug":"e2e-bug","title":"E2E bug","problem_statement":"ps"}`, "proj_current_bugs", "e2e-bug"},
		{"suggestion", `{"schema_name":"suggestion","slug":"e2e-sug","title":"E2E sug","problem_statement":"ps"}`, "proj_current_suggestions", "e2e-sug"},
		{"chain", `{"schema_name":"chain","slug":"e2e-chain","output":"o","design_decisions":"dd","completion_condition":"cc"}`, "proj_chain_status", "e2e-chain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := forgeCreateRaw(t, pool, "mcp-servers", json.RawMessage(tc.raw))
			if err != nil {
				t.Fatalf("HandleForgeCreate(%s): %v", tc.name, err)
			}
			if !res.Ok || res.Error != "" {
				t.Fatalf("HandleForgeCreate(%s) not ok: %+v", tc.name, res)
			}
			if res.Action != "created" {
				t.Errorf("HandleForgeCreate(%s) action=%q, want created", tc.name, res.Action)
			}
			var n int
			if err := pool.DB().QueryRow("SELECT COUNT(*) FROM "+tc.table+" WHERE project_id='mcp-servers' AND slug=?", tc.slug).Scan(&n); err != nil {
				t.Fatalf("count %s: %v", tc.table, err)
			}
			if n != 1 {
				t.Fatalf("%s row count=%d, want 1", tc.table, n)
			}
		})
	}
}

// TestHandleForgeCreate_DuplicateRejects: a second create on an existing
// once-only slug (chain) returns the duplicate envelope, not a silent overwrite.
func TestHandleForgeCreate_DuplicateRejects(t *testing.T) {
	pool := openTestPool(t)
	raw := json.RawMessage(`{"schema_name":"chain","slug":"dup-e2e","output":"o","design_decisions":"dd","completion_condition":"cc"}`)
	if res, err := forgeCreateRaw(t, pool, "mcp-servers", raw); err != nil || !res.Ok {
		t.Fatalf("first create: err=%v res=%+v", err, res)
	}
	res, err := forgeCreateRaw(t, pool, "mcp-servers", raw)
	if err != nil {
		t.Fatalf("dup create transport error: %v", err)
	}
	if res.Error == "" {
		t.Fatalf("dup create should reject, got ok: %+v", res)
	}
	if res.Hint == "" || !strings.Contains(res.Hint, "forge_edit") {
		t.Errorf("dup envelope should hint forge_edit, got hint=%q", res.Hint)
	}
}

// TestHandleForgeCreate_PipeChainRejects: a pipe-delimited chain task is the
// deprecated shape and must reject with a migration message.
func TestHandleForgeCreate_PipeChainRejects(t *testing.T) {
	pool := openTestPool(t)
	raw := json.RawMessage(`{"schema_name":"chain","slug":"pipe-e2e","output":"o","design_decisions":"dd","completion_condition":"cc","tasks":["t1|some scope|pending"]}`)
	res, err := forgeCreateRaw(t, pool, "mcp-servers", raw)
	if err != nil {
		t.Fatalf("pipe chain transport error: %v", err)
	}
	if res.Error == "" || !strings.Contains(res.Error, "pipe-mode") {
		t.Fatalf("pipe-mode chain task should reject with a pipe-mode message, got: %+v", res)
	}
}

// TestHandleForgeCreate_VaultNote: vault-note create writes the file + upserts
// the knowledge_pointer via the full notifier (action "created"); a same-slug
// re-forge updates it (action "updated", policy A), proving the survivor arm +
// full-finalize wiring.
func TestHandleForgeCreate_VaultNote(t *testing.T) {
	pool := openTestPool(t)
	t.Setenv("FORGE_MARKDOWN_ROOT", t.TempDir())
	raw := json.RawMessage(`{"schema_name":"vault-note","slug":"e2e-vault","note_kind":"reference","title":"E2E vault note","body":"Body content."}`)

	res, err := forgeCreateRaw(t, pool, "mcp-servers", raw)
	if err != nil || res.Error != "" {
		t.Fatalf("vault-note create: err=%v res=%+v", err, res)
	}
	if res.Action != "created" {
		t.Errorf("vault-note first create action=%q, want created", res.Action)
	}
	if res.ArtifactPath == "" {
		t.Errorf("vault-note create ArtifactPath empty")
	}
	var n int
	if err := pool.DB().QueryRow("SELECT COUNT(*) FROM knowledge_pointers WHERE source_type='vault'").Scan(&n); err != nil {
		t.Fatalf("count vault pointers: %v", err)
	}
	if n != 1 {
		t.Fatalf("vault pointer count=%d, want 1", n)
	}

	// Re-forge the same slug → auto-update (policy A), action "updated".
	res2, err := forgeCreateRaw(t, pool, "mcp-servers", raw)
	if err != nil || res2.Error != "" {
		t.Fatalf("vault-note re-forge: err=%v res=%+v", err, res2)
	}
	if res2.Action != "updated" {
		t.Errorf("vault-note re-forge action=%q, want updated", res2.Action)
	}
}

// TestHandleForgeCreate_Bench: bench create lands a bench_harnesses row through
// the orchestrator (the P2-A event-sourced arm).
func TestHandleForgeCreate_Bench(t *testing.T) {
	pool := openTestPool(t)
	raw := json.RawMessage(`{"schema_name":"bench","slug":"e2e-bench","binary_path":"go/bin/b","flag_set":"--f v","baseline_json_path":"x/b.json"}`)
	res, err := forgeCreateRaw(t, pool, "mcp-servers", raw)
	if err != nil || res.Error != "" {
		t.Fatalf("bench create: err=%v res=%+v", err, res)
	}
	var n int
	if err := pool.DB().QueryRow("SELECT COUNT(*) FROM bench_harnesses WHERE project_id='mcp-servers' AND slug='e2e-bench'").Scan(&n); err != nil {
		t.Fatalf("count bench: %v", err)
	}
	if n != 1 {
		t.Fatalf("bench row count=%d, want 1", n)
	}
}

// TestHandleForgeEdit_BugAndNotFound: a bug edit updates the row; an edit of a
// missing slug returns the not_found envelope.
func TestHandleForgeEdit_BugAndNotFound(t *testing.T) {
	pool := openTestPool(t)
	if res, err := forgeCreateRaw(t, pool, "mcp-servers", json.RawMessage(
		`{"schema_name":"bug","slug":"edit-e2e","title":"Orig","problem_statement":"ps"}`)); err != nil || !res.Ok {
		t.Fatalf("seed bug: err=%v res=%+v", err, res)
	}
	res, err := forgeEditRaw(t, pool, "mcp-servers", json.RawMessage(
		`{"schema_name":"bug","slug":"edit-e2e","fields":{"title":"Renamed"}}`))
	if err != nil || res.Error != "" {
		t.Fatalf("bug edit: err=%v res=%+v", err, res)
	}
	var title string
	if err := pool.DB().QueryRow("SELECT title FROM proj_current_bugs WHERE project_id='mcp-servers' AND slug='edit-e2e'").Scan(&title); err != nil {
		t.Fatalf("read bug: %v", err)
	}
	if title != "Renamed" {
		t.Fatalf("bug title=%q, want Renamed", title)
	}

	nf, err := forgeEditRaw(t, pool, "mcp-servers", json.RawMessage(
		`{"schema_name":"bug","slug":"no-such-bug","fields":{"title":"x"}}`))
	if err != nil {
		t.Fatalf("not-found edit transport error: %v", err)
	}
	if nf.Error != "not_found" {
		t.Fatalf("edit of missing slug should return not_found, got: %+v", nf)
	}
}

// TestHandleForgeEdit_BenchGeneric: a bench edit goes through the generic
// (project_id, slug) UPDATE survivor arm (no event).
func TestHandleForgeEdit_BenchGeneric(t *testing.T) {
	pool := openTestPool(t)
	if res, err := forgeCreateRaw(t, pool, "mcp-servers", json.RawMessage(
		`{"schema_name":"bench","slug":"bench-edit-e2e","binary_path":"go/bin/b","flag_set":"--f v","baseline_json_path":"x/b.json"}`)); err != nil || res.Error != "" {
		t.Fatalf("seed bench: err=%v res=%+v", err, res)
	}
	res, err := forgeEditRaw(t, pool, "mcp-servers", json.RawMessage(
		`{"schema_name":"bench","slug":"bench-edit-e2e","fields":{"binary_path":"go/bin/b2"}}`))
	if err != nil || res.Error != "" {
		t.Fatalf("bench edit: err=%v res=%+v", err, res)
	}
	var bp string
	if err := pool.DB().QueryRow("SELECT binary_path FROM bench_harnesses WHERE project_id='mcp-servers' AND slug='bench-edit-e2e'").Scan(&bp); err != nil {
		t.Fatalf("read bench: %v", err)
	}
	if bp != "go/bin/b2" {
		t.Fatalf("bench binary_path=%q, want go/bin/b2", bp)
	}
}

// TestHandleForgeDelete_RejectsLifecycleSchema: forge_delete on a bug rejects
// (no hard delete — bug_resolve owns terminal state), naming the lifecycle action.
func TestHandleForgeDelete_RejectsLifecycleSchema(t *testing.T) {
	pool := openTestPool(t)
	res, err := forgeDeleteRaw(t, pool, "mcp-servers", json.RawMessage(`{"schema_name":"bug","slug":"x"}`))
	if err != nil {
		t.Fatalf("delete transport error: %v", err)
	}
	if res.Error == "" {
		t.Fatalf("forge_delete(bug) should reject (lifecycle-owned), got ok: %+v", res)
	}
}

// TestHandleForgeSchemas: the introspection surface lists the loaded schemas.
func TestHandleForgeSchemas(t *testing.T) {
	pool := openTestPool(t)
	deps := construct.Deps{Pool: pool, Schemas: loadForgeRegistry(t)}
	res, err := construct.HandleForgeSchemas(context.Background(), deps, "mcp-servers", nil)
	if err != nil {
		t.Fatalf("HandleForgeSchemas: %v", err)
	}
	found := false
	for _, s := range res {
		if s.Name == "bug" {
			found = true
		}
	}
	if !found {
		t.Fatalf("forge_schemas should list the bug schema, got %d entries", len(res))
	}
}
