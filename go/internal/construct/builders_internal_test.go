package construct

// Internal tests for the per-schema builders, exercising shape concerns that
// don't need the full Create-umbrella orchestration: slug derivation, payload
// shape (optional-field omission), and required-field rejection message
// shape. The end-to-end forge↔layer parity tests live in the external
// _test.go files calling construct.Create.

import (
	"strings"
	"testing"
)

// TestBuildBugSlugDerive proves the bug builder derives a slug from its
// title IDENTICALLY to forge (reuses SlugifyTitle — B-G3).
func TestBuildBugSlugDerive(t *testing.T) {
	const title = "A Title With CAPS, punctuation! and   spaces"
	ev, err := buildBug("mcp-servers", BugInput{Title: title, ProblemStatement: "ps"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if want := SlugifyTitle(title); ev.EntitySlug != want {
		t.Fatalf("bug slug-derive divergence: build=%q forge=%q", ev.EntitySlug, want)
	}
}

// TestBuildSuggestionSlugDerive proves the suggestion builder derives a slug
// IDENTICALLY to forge. (Originally missing from the carve-out; one of the 5
// audit-gap closures.)
func TestBuildSuggestionSlugDerive(t *testing.T) {
	const title = "A Suggestion Title With CAPS, punctuation! and   spaces"
	ev, err := buildSuggestion("mcp-servers", SuggestionInput{Title: title, ProblemStatement: "ps"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if want := SlugifyTitle(title); ev.EntitySlug != want {
		t.Fatalf("suggestion slug-derive divergence: build=%q forge=%q", ev.EntitySlug, want)
	}
}

// TestBuildChainSlugDerive proves the chain builder derives a slug from a
// title IDENTICALLY to forge — chain has no `title` field of its own, so the
// title is consumed for the slug only (B-G3).
func TestBuildChainSlugDerive(t *testing.T) {
	const title = "A Chain Title — With Punctuation! And   Spaces"
	ev, err := buildChain("mcp-servers", ChainInput{
		Title: title, Output: "o", DesignDecisions: "dd", CompletionCondition: "cc",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if want := SlugifyTitle(title); ev.EntitySlug != want {
		t.Fatalf("chain slug-derive divergence: build=%q forge=%q", ev.EntitySlug, want)
	}
}

// TestBuildChainWithTasksRejectsMissingRationale proves the builder rejects
// the WHOLE call when a full-object task entry omits its rationale (B-C3).
func TestBuildChainWithTasksRejectsMissingRationale(t *testing.T) {
	_, err := buildChainWithTasks("mcp-servers", ChainWithTasksInput{
		ChainInput: ChainInput{Slug: "no-rationale", Output: "o", DesignDecisions: "dd", CompletionCondition: "cc"},
		Tasks: []ChainTaskInput{
			{Slug: "t1", ProblemStatement: "ps", Rationale: "first"}, // valid
			{Slug: "t2", ProblemStatement: "ps"},                     // rationale omitted
		},
	})
	if err == nil {
		t.Fatalf("buildChainWithTasks with missing rationale should reject, got nil")
	}
	if !strings.Contains(err.Error(), "rationale is required") {
		t.Fatalf("expected 'rationale is required' in error, got: %v", err)
	}
}

// TestBuildTaskOptionalFieldsOmittedFromPayload proves the optionalStr /
// *string omitempty contract: a TaskInput with the 3 optional strings empty
// produces a TaskCreated payload whose JSON has NO context_required,
// constraints, or handoff_output keys at all — not present-as-empty.
func TestBuildTaskOptionalFieldsOmittedFromPayload(t *testing.T) {
	// Optionals all empty → keys absent.
	evOmit, err := buildTask("mcp-servers", TaskInput{
		Slug: "opt-omit", ChainSlug: "any-chain", ProblemStatement: "ps",
	})
	if err != nil {
		t.Fatalf("build task (omit): %v", err)
	}
	payloadOmit := string(evOmit.Payload)
	for _, key := range []string{"context_required", "constraints", "handoff_output"} {
		if strings.Contains(payloadOmit, `"`+key+`"`) {
			t.Fatalf("payload with omitted optionals must NOT contain %q, got: %s", key, payloadOmit)
		}
	}
	// Optionals all set → keys present.
	evSet, err := buildTask("mcp-servers", TaskInput{
		Slug: "opt-set", ChainSlug: "any-chain", ProblemStatement: "ps",
		ContextRequired: "ctx", Constraints: "cons", HandoffOutput: "ho",
	})
	if err != nil {
		t.Fatalf("build task (set): %v", err)
	}
	payloadSet := string(evSet.Payload)
	for _, key := range []string{"context_required", "constraints", "handoff_output"} {
		if !strings.Contains(payloadSet, `"`+key+`"`) {
			t.Fatalf("payload with set optionals must contain %q, got: %s", key, payloadSet)
		}
	}
}

// TestBuildRequiredFieldRejectionMessageShapes proves the required-field
// rejections name the schema, the field, and the word "required" so an
// agent reading the error knows what to fix without trial-and-error.
func TestBuildRequiredFieldRejectionMessageShapes(t *testing.T) {
	cases := []struct {
		name  string
		field string
		build func() error
	}{
		{"bug-missing-title", "title", func() error {
			_, err := buildBug("mcp-servers", BugInput{ProblemStatement: "ps"})
			return err
		}},
		{"bug-missing-ps", "problem_statement", func() error {
			_, err := buildBug("mcp-servers", BugInput{Title: "t"})
			return err
		}},
		{"suggestion-missing-title", "title", func() error {
			_, err := buildSuggestion("mcp-servers", SuggestionInput{ProblemStatement: "ps"})
			return err
		}},
		{"suggestion-missing-ps", "problem_statement", func() error {
			_, err := buildSuggestion("mcp-servers", SuggestionInput{Title: "t"})
			return err
		}},
		{"chain-missing-output", "output", func() error {
			_, err := buildChain("mcp-servers", ChainInput{Slug: "s", DesignDecisions: "dd", CompletionCondition: "cc"})
			return err
		}},
		{"chain-missing-design-decisions", "design_decisions", func() error {
			_, err := buildChain("mcp-servers", ChainInput{Slug: "s", Output: "o", CompletionCondition: "cc"})
			return err
		}},
		{"chain-missing-completion-condition", "completion_condition", func() error {
			_, err := buildChain("mcp-servers", ChainInput{Slug: "s", Output: "o", DesignDecisions: "dd"})
			return err
		}},
		{"task-missing-chain-slug", "chain_slug", func() error {
			_, err := buildTask("mcp-servers", TaskInput{Slug: "s", ProblemStatement: "ps"})
			return err
		}},
		{"task-missing-slug", "slug", func() error {
			_, err := buildTask("mcp-servers", TaskInput{ChainSlug: "c", ProblemStatement: "ps"})
			return err
		}},
		{"task-missing-ps", "problem_statement", func() error {
			_, err := buildTask("mcp-servers", TaskInput{Slug: "s", ChainSlug: "c"})
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.build()
			if err == nil {
				t.Fatalf("expected rejection, got nil")
			}
			msg := err.Error()
			if !strings.Contains(msg, c.field) {
				t.Fatalf("rejection message must name the field %q, got: %s", c.field, msg)
			}
			if !strings.Contains(msg, "required") {
				t.Fatalf("rejection message must say 'required' so the agent knows what to fix, got: %s", msg)
			}
		})
	}
}
