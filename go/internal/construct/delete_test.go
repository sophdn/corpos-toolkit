package construct_test

import (
	"context"
	"strings"
	"testing"

	"toolkit/internal/construct"
)

// TestDeleteRejectsLifecycleSchemas covers B-DEL1: bug / chain / task /
// suggestion all reject construct.Delete, naming the lifecycle action that
// owns terminal state. Mirrors forge_delete's rejection envelope so any
// caller migrated through construct gets the same hint.
func TestDeleteRejectsLifecycleSchemas(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	deps := construct.Deps{Pool: pool, Schemas: loadForgeRegistry(t)}

	cases := []struct {
		schema, expectAction string
	}{
		{"bug", "bug_resolve"},
		{"task", "task_cancel"},
		{"chain", "chain_close"},
		{"suggestion", "suggestion_resolve"},
	}
	for _, c := range cases {
		t.Run(c.schema, func(t *testing.T) {
			_, err := construct.Delete(ctx, deps, c.schema, "mcp-servers", "any-slug")
			if err == nil {
				t.Fatalf("expected B-DEL1 rejection for %s, got nil", c.schema)
			}
			if !strings.Contains(err.Error(), c.expectAction) {
				t.Fatalf("%s rejection should name %q, got: %v", c.schema, c.expectAction, err)
			}
			if !strings.Contains(err.Error(), "does not support deletion") {
				t.Fatalf("%s rejection should say 'does not support deletion', got: %v", c.schema, err)
			}
		})
	}
}

// TestDeleteRejectsUnknownSchema: an unknown schema name returns a clear
// error before any DB work runs.
func TestDeleteRejectsUnknownSchema(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	deps := construct.Deps{Pool: pool, Schemas: loadForgeRegistry(t)}

	_, err := construct.Delete(ctx, deps, "no-such-schema", "mcp-servers", "x")
	if err == nil {
		t.Fatalf("expected unknown-schema rejection, got nil")
	}
	if !strings.Contains(err.Error(), "unknown schema") {
		t.Fatalf("expected unknown-schema phrasing: %v", err)
	}
}

// TestDeleteRejectsMissingArgs: empty slug / empty schema → clear error.
func TestDeleteRejectsMissingArgs(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	deps := construct.Deps{Pool: pool, Schemas: loadForgeRegistry(t)}

	if _, err := construct.Delete(ctx, deps, "", "mcp-servers", "x"); err == nil {
		t.Fatalf("empty schema must reject")
	}
	if _, err := construct.Delete(ctx, deps, "bug", "mcp-servers", ""); err == nil {
		t.Fatalf("empty slug must reject")
	}
}

// TestDeleteRejectsNilDeps: nil Pool / nil Schemas → clear error (parity
// with construct.Create / construct.Update defensive shape).
func TestDeleteRejectsNilDeps(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)

	if _, err := construct.Delete(ctx, construct.Deps{Schemas: loadForgeRegistry(t)}, "bug", "mcp-servers", "x"); err == nil {
		t.Fatalf("nil Pool must reject")
	}
	if _, err := construct.Delete(ctx, construct.Deps{Pool: pool}, "bug", "mcp-servers", "x"); err == nil {
		t.Fatalf("nil Schemas must reject")
	}
}
