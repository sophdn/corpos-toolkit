package ecosystem

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func learnCorposToolkit(t *testing.T, d Deps) {
	t.Helper()
	// The current canonical repo.
	if _, err := d.canonLearn(context.Background(), json.RawMessage(`{
		"slug":"corpos-toolkit","kind":"repo","canonical":"sophdn/corpos-toolkit","status":"current",
		"gitea_owner":"shared","local_path":"~/dev/corpos-toolkit",
		"aliases":["mcp-servers","sophdn/toolkit","sophdn-toolkit","~/dev/mcp-servers","~/dev/sophdn-toolkit"],
		"soft_ref":"memory/reference/corpos-rename-canonical-names-2026-06"}`)); err != nil {
		t.Fatalf("canon_learn repo: %v", err)
	}
	// The retired native port.
	if _, err := d.canonLearn(context.Background(), json.RawMessage(`{
		"slug":"toolkit-port-3000","kind":"port","canonical":"3000","status":"retired",
		"replacement":"3001","aliases":["3000",":3000"],
		"notes":"native :3000 daemon retired; canonical is the container :3001"}`)); err != nil {
		t.Fatalf("canon_learn retired port: %v", err)
	}
}

// The canonical case: a retired alias resolves to the current canonical form.
func TestCanonResolve_RetiredAliasToCanonical(t *testing.T) {
	d := mkDeps(t)
	learnCorposToolkit(t, d)

	for _, tok := range []string{"mcp-servers", "sophdn/toolkit", "~/dev/mcp-servers"} {
		rec, err := d.canonResolve(context.Background(), json.RawMessage(`{"token":"`+tok+`"}`))
		if err != nil {
			t.Fatalf("canon_resolve(%q): %v", tok, err)
		}
		if !rec.Resolved || rec.Slug != "corpos-toolkit" {
			t.Errorf("token %q resolved to %+v, want corpos-toolkit", tok, rec)
		}
		if rec.Canonical != "sophdn/corpos-toolkit" || rec.MatchedVia != "alias" {
			t.Errorf("token %q: canonical=%q via=%q", tok, rec.Canonical, rec.MatchedVia)
		}
		if rec.Status != "current" {
			t.Errorf("token %q: status=%q, want current", tok, rec.Status)
		}
	}
}

// A retired entry (:3000) answers RETIRED with its replacement.
func TestCanonResolve_RetiredEntryReportsReplacement(t *testing.T) {
	d := mkDeps(t)
	learnCorposToolkit(t, d)

	for _, tok := range []string{"3000", ":3000"} {
		rec, err := d.canonResolve(context.Background(), json.RawMessage(`{"token":"`+tok+`"}`))
		if err != nil {
			t.Fatalf("canon_resolve(%q): %v", tok, err)
		}
		if rec.Status != "retired" || rec.Replacement != "3001" {
			t.Errorf("token %q: status=%q replacement=%q, want retired/3001 (%+v)", tok, rec.Status, rec.Replacement, rec)
		}
		if !strings.Contains(strings.ToUpper(rec.Answer), "RETIRED") {
			t.Errorf("token %q: answer=%q, want it to say RETIRED", tok, rec.Answer)
		}
	}
}

// Direct hits on slug / canonical / local_path resolve without an alias row.
func TestCanonResolve_DirectHits(t *testing.T) {
	d := mkDeps(t)
	learnCorposToolkit(t, d)
	cases := map[string]string{
		"corpos-toolkit":        "slug",
		"sophdn/corpos-toolkit": "canonical",
		"~/dev/corpos-toolkit":  "canonical", // local_path is matched in the canonical branch
	}
	for tok, wantVia := range cases {
		rec, err := d.canonResolve(context.Background(), json.RawMessage(`{"token":"`+tok+`"}`))
		if err != nil {
			t.Fatalf("canon_resolve(%q): %v", tok, err)
		}
		if !rec.Resolved || rec.Slug != "corpos-toolkit" {
			t.Errorf("token %q did not resolve to corpos-toolkit: %+v", tok, rec)
		}
		if rec.MatchedVia != wantVia {
			t.Errorf("token %q: matched_via=%q, want %q", tok, rec.MatchedVia, wantVia)
		}
	}
}

// Unknown token is not a wrong canonical — resolved=false with an 'unknown' answer.
func TestCanonResolve_UnknownNotWrong(t *testing.T) {
	d := mkDeps(t)
	rec, err := d.canonResolve(context.Background(), json.RawMessage(`{"token":"never-heard-of-it"}`))
	if err != nil {
		t.Fatalf("canon_resolve: %v", err)
	}
	if rec.Resolved {
		t.Errorf("resolved=true for an un-learned token: %+v", rec)
	}
	if !strings.Contains(strings.ToLower(rec.Answer), "unknown") {
		t.Errorf("answer=%q, want unknown", rec.Answer)
	}
}

// canon_learn is idempotent-upsert and replaces the alias set; canon_list +
// CanonTokens surface entries and every token for the refresolve catalog.
func TestCanonLearn_UpsertAndListAndTokens(t *testing.T) {
	d := mkDeps(t)
	learnCorposToolkit(t, d)
	// Re-learn with a reduced alias set — replaces, not appends.
	if _, err := d.canonLearn(context.Background(), json.RawMessage(`{
		"slug":"corpos-toolkit","kind":"repo","canonical":"sophdn/corpos-toolkit","aliases":["mcp-servers"]}`)); err != nil {
		t.Fatalf("re-learn: %v", err)
	}
	// sophdn/toolkit alias should be gone now.
	rec, err := d.canonResolve(context.Background(), json.RawMessage(`{"token":"sophdn/toolkit"}`))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if rec.Resolved {
		t.Errorf("stale alias sophdn/toolkit still resolves after alias-set replace: %+v", rec)
	}

	lst, err := d.canonList(context.Background(), nil)
	if err != nil {
		t.Fatalf("canon_list: %v", err)
	}
	if lst.Count != 2 {
		t.Errorf("canon_list count=%d, want 2", lst.Count)
	}

	tokens, err := CanonTokens(context.Background(), d.Pool.DB())
	if err != nil {
		t.Fatalf("CanonTokens: %v", err)
	}
	want := map[string]bool{"mcp-servers": false, "corpos-toolkit": false, "sophdn/corpos-toolkit": false, "3000": false}
	for _, tok := range tokens {
		if _, ok := want[tok]; ok {
			want[tok] = true
		}
	}
	for tok, seen := range want {
		if !seen {
			t.Errorf("CanonTokens missing %q (got %v)", tok, tokens)
		}
	}
}
