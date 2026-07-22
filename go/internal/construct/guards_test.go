package construct_test

import (
	"context"
	"encoding/json"
	"testing"

	"toolkit/internal/construct"
)

// TestForgeDuplicateRejectParity proves B-D1 over the construct path: a
// second create on an existing slug must REJECT (not silently overwrite via
// the fold's ON CONFLICT DO UPDATE). The standalone construct.RejectDuplicateCreate
// guard is verified separately (used by Create internally but also exposed
// for callers that want to compose the orchestration differently).
func TestForgeDuplicateRejectParity(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	q := pool.DB()

	// create path baseline: a duplicate chain create rejects.
	mustForgeMap(t, pool, "mcp-servers", map[string]any{
		"schema_name": "chain", "slug": "dup-chain",
		"output": "o", "design_decisions": "dd", "completion_condition": "cc",
	})
	dupRaw, _ := json.Marshal(map[string]any{
		"schema_name": "chain", "slug": "dup-chain",
		"output": "o2", "design_decisions": "dd2", "completion_condition": "cc2",
	})
	res, err := forgeCreateRaw(t, pool, "mcp-servers", dupRaw)
	if err != nil {
		t.Fatalf("create dup chain: %v", err)
	}
	if res.Error == "" {
		t.Fatalf("create(chain) on existing slug should reject, got ok: %+v", res)
	}

	// The standalone construct guard rejects the SAME existing chain slug
	// and admits a free one.
	if err := construct.RejectDuplicateCreate(ctx, q, "chain", "mcp-servers", "", "dup-chain"); err == nil {
		t.Fatalf("RejectDuplicateCreate(chain, dup-chain) should reject (slug exists)")
	}
	if err := construct.RejectDuplicateCreate(ctx, q, "chain", "mcp-servers", "", "fresh-chain"); err != nil {
		t.Fatalf("RejectDuplicateCreate(chain, fresh-chain) should pass (free slug), got: %v", err)
	}

	// task arm: create a chain + task via the umbrella, then the standalone
	// guard rejects the existing (chain, slug) and admits a free task slug.
	deps := construct.Deps{Pool: pool, Schemas: loadForgeRegistry(t)}
	if _, err := construct.Create(ctx, deps, "chain", "mcp-servers", construct.Input{
		Chain: &construct.ChainInput{Slug: "dup-task-chain", Output: "o", DesignDecisions: "dd", CompletionCondition: "cc"},
	}); err != nil {
		t.Fatalf("Create(chain): %v", err)
	}
	if _, err := construct.Create(ctx, deps, "task", "mcp-servers", construct.Input{
		Task: &construct.TaskInput{Slug: "dup-task", ChainSlug: "dup-task-chain", ProblemStatement: "ps"},
	}); err != nil {
		t.Fatalf("Create(task): %v", err)
	}
	if err := construct.RejectDuplicateCreate(ctx, q, "task", "mcp-servers", "dup-task-chain", "dup-task"); err == nil {
		t.Fatalf("RejectDuplicateCreate(task, dup-task) should reject (exists in chain)")
	}
	if err := construct.RejectDuplicateCreate(ctx, q, "task", "mcp-servers", "dup-task-chain", "free-task"); err != nil {
		t.Fatalf("RejectDuplicateCreate(task, free-task) should pass, got: %v", err)
	}
}

// TestDoubleDatedSlugGuard proves B-G2 over the construct layer: a
// date-prefixed slug is rejected for a double-dating schema (vault-note's
// {date}_{slug}) and accepted for a non-double-dating schema. RejectDoubleDatedSlug
// stays exported as an orchestration primitive (Create doesn't currently
// dispatch to vault-note since vault-note has no event; future {date}_{slug}
// file schemas would route the guard through Create).
func TestDoubleDatedSlugGuard(t *testing.T) {
	reg := loadForgeRegistry(t)
	vn, ok := reg.Get("vault-note")
	if !ok {
		t.Fatal("vault-note schema not in registry")
	}
	mem, _ := reg.Get("memory")

	for _, slug := range []string{"2026-05-27_foo", "2026-05-27-foo"} {
		if err := construct.RejectDoubleDatedSlug(vn, "vault-note", slug); err == nil {
			t.Fatalf("RejectDoubleDatedSlug(vault-note, %q) should reject", slug)
		}
	}
	if err := construct.RejectDoubleDatedSlug(vn, "vault-note", "foo-bar"); err != nil {
		t.Fatalf("RejectDoubleDatedSlug(vault-note, clean) should pass, got: %v", err)
	}
	if err := construct.RejectDoubleDatedSlug(mem, "memory", "2026-05-27_foo"); err != nil {
		t.Fatalf("RejectDoubleDatedSlug(memory, date-prefixed) should pass (not double-dating), got: %v", err)
	}
}
