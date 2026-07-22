package construct

import (
	"encoding/json"
	"testing"

	"toolkit/internal/forge/fieldvalue"
)

// TestParseChainTasks_ContextRequiredListCoerced is the bug 1086 regression: a
// forge(chain) embedded task with a LIST-shaped context_required must be accepted and
// coerced, exactly as standalone forge(task) accepts it — not rejected with "cannot
// unmarshal array into Go struct field .context_required of type string".
func TestParseChainTasks_ContextRequiredListCoerced(t *testing.T) {
	raw := json.RawMessage(`[{
		"slug": "t1",
		"problem_statement": "do the thing",
		"acceptance_criteria": ["a", "b"],
		"context_required": ["file_x.go", "section_y"],
		"constraints": "none",
		"rationale": "needed for the chain"
	}]`)
	entries, err := ParseChainTasks(raw)
	if err != nil {
		t.Fatalf("forge(chain) must accept list-shaped context_required (bug 1086): %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	// acceptance_criteria stays a list (its storage form).
	if len(e.AcceptanceCriteria) != 2 || e.AcceptanceCriteria[0] != "a" || e.AcceptanceCriteria[1] != "b" {
		t.Fatalf("acceptance_criteria = %v, want [a b]", e.AcceptanceCriteria)
	}
	// context_required is joined to the SAME form a standalone task row stores.
	if e.ContextRequired != "file_x.go\n- section_y" {
		t.Fatalf("context_required = %q, want the joined standalone form", e.ContextRequired)
	}
}

// TestParseChainTasks_ContextRequiredStringStillWorks: the single-string shape (which
// already worked) is unchanged.
func TestParseChainTasks_ContextRequiredStringStillWorks(t *testing.T) {
	raw := json.RawMessage(`[{"slug":"t1","problem_statement":"p","context_required":"just one","rationale":"r"}]`)
	entries, err := ParseChainTasks(raw)
	if err != nil {
		t.Fatalf("single-string context_required must still work: %v", err)
	}
	if entries[0].ContextRequired != "just one" {
		t.Fatalf("context_required = %q, want %q", entries[0].ContextRequired, "just one")
	}
}

// TestParseChainTasks_ContextRequiredParityWithStandaloneJoin pins the parity the chain
// `tasks` docs promise: the embedded join equals the standalone forge(task) join
// (fieldvalue.AsJoined) for the same list.
func TestParseChainTasks_ContextRequiredParityWithStandaloneJoin(t *testing.T) {
	raw := json.RawMessage(`[{"slug":"t","problem_statement":"p","context_required":["x","y","z"],"rationale":"r"}]`)
	entries, err := ParseChainTasks(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := fieldvalue.FieldValue{IsList: true, List: []string{"x", "y", "z"}}.AsJoined()
	if entries[0].ContextRequired != want {
		t.Fatalf("context_required = %q, want standalone-join %q", entries[0].ContextRequired, want)
	}
}

// TestParseChainTasks_ContextRequiredEmptyIsBlank: absent/null context_required yields
// the empty string, matching an absent standalone field.
func TestParseChainTasks_ContextRequiredEmptyIsBlank(t *testing.T) {
	raw := json.RawMessage(`[{"slug":"t","problem_statement":"p","rationale":"r"}]`)
	entries, err := ParseChainTasks(raw)
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].ContextRequired != "" {
		t.Fatalf("absent context_required = %q, want empty", entries[0].ContextRequired)
	}
}
