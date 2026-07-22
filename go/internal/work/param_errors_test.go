package work_test

import (
	"context"
	"strings"
	"testing"

	"toolkit/internal/work"
)

// Chain quiet-and-instrument-operator-surface T4: param-shape rejections
// self-describe from the actions_discovery catalog. These pin (a) the
// renderer sources params + example from the catalog, (b) the identifier
// error names the {"id":<int>} form (the recurring fumble), and (c) the
// rejection envelope actually carries the shape end-to-end through a
// handler.

func TestCallShape_SourcesParamsAndExampleFromCatalog(t *testing.T) {
	// forge_schema has one required param (schema_name) + an example —
	// exercises the required-marker and the example tail.
	got := work.CallShape("forge_schema")
	for _, want := range []string{"schema_name (string)*", "[*=required]", `Example: {"schema_name":"task"}`} {
		if !strings.Contains(got, want) {
			t.Errorf("CallShape(forge_schema) = %q, missing %q", got, want)
		}
	}
}

func TestCallShape_NoParamsAction(t *testing.T) {
	// roadmap_diff takes no params but has an example.
	got := work.CallShape("roadmap_diff")
	if !strings.Contains(got, "Takes no params") {
		t.Errorf("CallShape(roadmap_diff) = %q, want a no-params phrasing", got)
	}
}

func TestCallShape_UnknownActionEmpty(t *testing.T) {
	if got := work.CallShape("does_not_exist"); got != "" {
		t.Errorf("CallShape(unknown) = %q, want empty", got)
	}
}

func TestIdentifierRequiredError_NamesIntIdFormAndShape(t *testing.T) {
	got := work.IdentifierRequiredError("task_read")
	// The id-as-integer guidance + concrete form is the load-bearing part:
	// it targets {"id":"6326"} / {"slug":"6326"} / {"task":6326} fumbles.
	for _, want := range []string{
		"task_read", "integer", `{"id":6326}`, "`slug`", "chain_slug", "Accepted params",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("IdentifierRequiredError(task_read) = %q, missing %q", got, want)
		}
	}
}

func TestIdentifierRequiredError_NonChainScopedOmitsChainSlug(t *testing.T) {
	// bug_read identifies by id/slug only — no chain_slug to mention.
	got := work.IdentifierRequiredError("bug_read")
	if strings.Contains(got, "with `chain_slug`") {
		t.Errorf("bug_read error should not mention chain_slug: %q", got)
	}
	if !strings.Contains(got, `{"id":6326}`) {
		t.Errorf("bug_read error should still name the {id:int} form: %q", got)
	}
}

func TestIdentifierRequiredError_UnknownActionFallsBack(t *testing.T) {
	got := work.IdentifierRequiredError("ghost_action")
	if !strings.Contains(got, "ghost_action") || !strings.Contains(got, "id") {
		t.Errorf("unknown-action fallback should still name the action + id: %q", got)
	}
}

// End-to-end: the rejection envelope a real handler returns carries the
// shape + example (the AC's "error envelope carries the shape"). task_read
// is the action whose {"id":N} vs {"slug":"N"} trap motivated the task.
func TestHandleTaskRead_EmptyParamsReturnsSelfDescribingError(t *testing.T) {
	pool := openTaskTestPool(t)
	resp, _ := work.HandleTaskRead(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{}))
	if resp.Err == nil {
		t.Fatalf("task_read with no identifier should reject")
	}
	for _, want := range []string{`{"id":6326}`, "chain_slug", "Accepted params", "Example:"} {
		if !strings.Contains(resp.Err.Error, want) {
			t.Errorf("task_read rejection %q missing %q", resp.Err.Error, want)
		}
	}
}
