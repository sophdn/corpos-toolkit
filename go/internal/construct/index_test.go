package construct_test

import (
	"context"
	"encoding/json"
	"testing"

	"toolkit/internal/construct"
	"toolkit/internal/db"
)

func readPointerFields(t *testing.T, pool *db.Pool, project, sourceType, sourceRef string) (question, descr, tags string, quality float64) {
	t.Helper()
	if err := pool.DB().QueryRow(
		`SELECT question, COALESCE(description,''), tags, COALESCE(quality_score,0)
		   FROM knowledge_pointers WHERE project_id = ? AND source_type = ? AND source_ref = ?`,
		project, sourceType, sourceRef,
	).Scan(&question, &descr, &tags, &quality); err != nil {
		t.Fatalf("read pointer %s/%s: %v", sourceType, sourceRef, err)
	}
	return
}

func pointerCount(t *testing.T, pool *db.Pool, sourceType string) int {
	t.Helper()
	var n int
	if err := pool.DB().QueryRow(
		`SELECT COUNT(*) FROM knowledge_pointers WHERE source_type = ?`, sourceType).Scan(&n); err != nil {
		t.Fatalf("count pointers %s: %v", sourceType, err)
	}
	return n
}

// TestCreateForgeIndexSyncParity proves B-F3 over the construct umbrella: an
// entity created via construct.Create lands a knowledge_pointer whose
// content-derived columns match forge(schema)+IndexUpsertNotifier — the
// umbrella runs SyncCreateIndex INTERNALLY for Indexed DB schemas (chain,
// task, bug). Suggestion + memory are not Indexed: their pointer counts stay
// at zero.
func TestCreateForgeIndexSyncParity(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	reg := loadForgeRegistry(t)
	// The "-forge" fixtures now route through the SAME construct create path
	// (construct.Create pre-syncs the indexed-schema pointer); the parity assert
	// below proves the HandleForgeCreate envelope + a direct construct.Create land
	// identical pointer content. (forge archived — chain 311 T7 Stage 6 P2-C.2.)
	forgeCreate := func(params map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(params)
		res, err := forgeCreateRaw(t, pool, "mcp-servers", raw)
		if err != nil || res.Error != "" {
			t.Fatalf("create(%v): err=%v res.Error=%q", params["schema_name"], err, res.Error)
		}
	}
	deps := construct.Deps{Pool: pool, Schemas: reg}

	comparePointer := func(sourceType, forgeRef, recordRef string) {
		t.Helper()
		fq, fd, ft, fqual := readPointerFields(t, pool, "mcp-servers", sourceType, forgeRef)
		rq, rd, rt, rqual := readPointerFields(t, pool, "mcp-servers", sourceType, recordRef)
		if fq != rq || fd != rd || ft != rt || fqual != rqual {
			t.Fatalf("%s pointer parity mismatch:\n  forge:     q=%q d=%q tags=%q qual=%v\n  construct: q=%q d=%q tags=%q qual=%v",
				sourceType, fq, fd, ft, fqual, rq, rd, rt, rqual)
		}
	}

	// chain
	forgeCreate(map[string]any{"schema_name": "chain", "slug": "idx-chain-forge",
		"output": "chain index output", "design_decisions": "dd", "completion_condition": "the cc"})
	if _, err := construct.Create(ctx, deps, "chain", "mcp-servers", construct.Input{
		Chain: &construct.ChainInput{Slug: "idx-chain-record", Output: "chain index output", DesignDecisions: "dd", CompletionCondition: "the cc"},
	}); err != nil {
		t.Fatalf("Create(chain): %v", err)
	}
	comparePointer("chain", "mcp-servers::idx-chain-forge", "mcp-servers::idx-chain-record")

	// bug
	forgeCreate(map[string]any{"schema_name": "bug", "slug": "idx-bug-forge",
		"title": "Index bug title", "problem_statement": "the bug problem"})
	if _, err := construct.Create(ctx, deps, "bug", "mcp-servers", construct.Input{
		Bug: &construct.BugInput{Slug: "idx-bug-record", Title: "Index bug title", ProblemStatement: "the bug problem"},
	}); err != nil {
		t.Fatalf("Create(bug): %v", err)
	}
	comparePointer("bug", "mcp-servers::idx-bug-forge", "mcp-servers::idx-bug-record")

	// task (each under its own fresh chain)
	forgeCreate(map[string]any{"schema_name": "chain", "slug": "idx-tf-chain",
		"output": "o", "design_decisions": "dd", "completion_condition": "cc"})
	forgeCreate(map[string]any{"schema_name": "task", "slug": "idx-task-forge", "chain_slug": "idx-tf-chain",
		"problem_statement": "the task problem"})
	if _, err := construct.Create(ctx, deps, "chain", "mcp-servers", construct.Input{
		Chain: &construct.ChainInput{Slug: "idx-tr-chain", Output: "o", DesignDecisions: "dd", CompletionCondition: "cc"},
	}); err != nil {
		t.Fatalf("Create(chain idx-tr): %v", err)
	}
	if _, err := construct.Create(ctx, deps, "task", "mcp-servers", construct.Input{
		Task: &construct.TaskInput{Slug: "idx-task-record", ChainSlug: "idx-tr-chain", ProblemStatement: "the task problem"},
	}); err != nil {
		t.Fatalf("Create(task): %v", err)
	}
	comparePointer("task", "mcp-servers::idx-tf-chain::idx-task-forge", "mcp-servers::idx-tr-chain::idx-task-record")

	// Negative: suggestion is non-Indexed — no pointer (the umbrella's
	// internal needsIndexSync returns false for suggestion).
	if _, err := construct.Create(ctx, deps, "suggestion", "mcp-servers", construct.Input{
		Suggestion: &construct.SuggestionInput{Slug: "idx-sug-record", Title: "Idx suggestion", ProblemStatement: "a suggestion"},
	}); err != nil {
		t.Fatalf("Create(suggestion): %v", err)
	}
	if n := pointerCount(t, pool, "suggestion"); n != 0 {
		t.Fatalf("suggestion is Indexed()=false — expected 0 knowledge_pointers, got %d", n)
	}
}

// TestCreateChainWithTasksSyncsEachTaskPointer proves the chain+tasks fan-out
// path of construct.Create runs SyncCreateIndex for each TaskCreated event,
// not just the chain head. Without this, callers driving fan-outs would have
// orphan tasks (no pointer) — the original audit gap that the umbrella
// closes by handling the orchestration internally.
func TestCreateChainWithTasksSyncsEachTaskPointer(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	reg := loadForgeRegistry(t)

	deps := construct.Deps{Pool: pool, Schemas: reg}
	if _, err := construct.Create(ctx, deps, "chain", "mcp-servers", construct.Input{
		ChainWithTasks: &construct.ChainWithTasksInput{
			ChainInput: construct.ChainInput{
				Slug: "fan-idx-chain", Output: "o", DesignDecisions: "dd", CompletionCondition: "cc",
			},
			Tasks: []construct.ChainTaskInput{
				{Slug: "fan-idx-t1", ProblemStatement: "ps1", Rationale: "first"},
				{Slug: "fan-idx-t2", ProblemStatement: "ps2", Rationale: "second"},
			},
		},
	}); err != nil {
		t.Fatalf("Create(chain+tasks): %v", err)
	}

	// Chain pointer + 2 task pointers — three rows total.
	if n := pointerCount(t, pool, "chain"); n != 1 {
		t.Fatalf("expected 1 chain pointer after fan-out, got %d", n)
	}
	if n := pointerCount(t, pool, "task"); n != 2 {
		t.Fatalf("expected 2 task pointers after fan-out (one per TaskCreated), got %d", n)
	}
}
