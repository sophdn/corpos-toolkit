package admin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toolkit/internal/actiondocs"
)

// makeCorpus assembles a tiny action-docs corpus inside t.TempDir() for
// the action_describe handler tests. The corpus carries:
//   - work/bug_resolve.toml (a realistic chunk with optional fields)
//   - work/_general.toml    (a surface-wide chunk)
//   - measure/benchmark_query.toml (a second surface for cross-surface tests)
//
// Returns the loaded *actiondocs.Registry; tests build a Deps around it.
func makeCorpus(t *testing.T) *actiondocs.Registry {
	t.Helper()
	dir := t.TempDir()
	writeChunk(t, dir, "work", "bug_resolve.toml", `surface = "work"
action = "bug_resolve"
purpose = "Close a bug with a resolution kind."

[[params]]
name = "resolution_kind"
type = "string"
required = true
description = "One of: fixed, wontfix, upstream, dup, routed."

[[param_aliases]]
from = "kind"
to = "resolution_kind"
`)
	writeChunk(t, dir, "work", "_general.toml", `surface = "work"
action = "_general"
purpose = "Surface-wide conventions for the work meta-tool."
`)
	writeChunk(t, dir, "measure", "benchmark_query.toml", `surface = "measure"
action = "benchmark_query"
purpose = "Return previously-recorded benchmark result rows."
`)
	r, err := actiondocs.Load(dir)
	if err != nil {
		t.Fatalf("actiondocs.Load: %v", err)
	}
	return r
}

func writeChunk(t *testing.T, root, surface, file, body string) {
	t.Helper()
	d := filepath.Join(root, surface)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", d, err)
	}
	if err := os.WriteFile(filepath.Join(d, file), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s/%s: %v", surface, file, err)
	}
}

func mustMarshal(t *testing.T, r ActionDescribeResult) map[string]any {
	t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal back: %v", err)
	}
	return got
}

func TestActionDescribe_HitReturnsDocFieldsAtTopLevel(t *testing.T) {
	deps := Deps{ActionDocs: makeCorpus(t)}
	params := json.RawMessage(`{"surface":"work","action":"bug_resolve"}`)

	res, err := HandleActionDescribe(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleActionDescribe: %v", err)
	}
	if res.Doc == nil {
		t.Fatalf("expected Doc populated; got error envelope %q", res.Error)
	}
	if res.Doc.Action != "bug_resolve" {
		t.Errorf("doc.action: want bug_resolve, got %q", res.Doc.Action)
	}
	if len(res.Doc.Params) != 1 {
		t.Errorf("doc.params: want 1, got %d", len(res.Doc.Params))
	}

	// Wire shape: success returns the doc fields at the top level, not
	// nested under `doc`. Mirrors ForgeSchemaResult's two-branch shape.
	got := mustMarshal(t, res)
	if got["action"] != "bug_resolve" {
		t.Errorf("wire shape: want action at top level, got keys %v", keys(got))
	}
	if _, hasError := got["error"]; hasError {
		t.Errorf("wire shape: success path emitted an error key: %v", got)
	}
}

func TestActionDescribe_GeneralChunkFindable(t *testing.T) {
	deps := Deps{ActionDocs: makeCorpus(t)}
	params := json.RawMessage(`{"surface":"work","action":"_general"}`)

	res, err := HandleActionDescribe(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleActionDescribe: %v", err)
	}
	if res.Doc == nil {
		t.Fatalf("expected _general findable via Get; got error %q", res.Error)
	}
	if res.Doc.Action != actiondocs.GeneralAction {
		t.Errorf("doc.action: want _general, got %q", res.Doc.Action)
	}
}

func TestActionDescribe_CrossSurfaceLookup(t *testing.T) {
	// The registry doesn't care which meta-tool surfaces the call
	// externally — admin.action_describe(surface="work", action="...")
	// is the canonical shape even though the caller is hitting the
	// admin meta-tool. Same for measure / knowledge.
	deps := Deps{ActionDocs: makeCorpus(t)}
	params := json.RawMessage(`{"surface":"measure","action":"benchmark_query"}`)

	res, err := HandleActionDescribe(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleActionDescribe: %v", err)
	}
	if res.Doc == nil {
		t.Fatalf("expected cross-surface hit; got error %q", res.Error)
	}
	if res.Doc.Surface != "measure" || res.Doc.Action != "benchmark_query" {
		t.Errorf("cross-surface lookup: want measure/benchmark_query, got %s/%s",
			res.Doc.Surface, res.Doc.Action)
	}
}

func TestActionDescribe_MissingSurfaceListsRegisteredSurfaces(t *testing.T) {
	deps := Deps{ActionDocs: makeCorpus(t)}
	params := json.RawMessage(`{"surface":"nonexistent","action":"foo"}`)

	res, err := HandleActionDescribe(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleActionDescribe: %v", err)
	}
	if res.Doc != nil {
		t.Errorf("expected miss path; got Doc populated")
	}
	if res.Error != "surface_not_found" {
		t.Errorf("Error: want surface_not_found, got %q", res.Error)
	}
	if res.Surface != "nonexistent" {
		t.Errorf("envelope.Surface: want nonexistent, got %q", res.Surface)
	}
	want := []string{"measure", "work"}
	if !equalStringSlice(res.RegisteredSurfaces, want) {
		t.Errorf("RegisteredSurfaces: want %v, got %v", want, res.RegisteredSurfaces)
	}

	got := mustMarshal(t, res)
	if got["error"] != "surface_not_found" {
		t.Errorf("wire shape: missing or wrong error key: %v", got)
	}
	if _, ok := got["registered_surfaces"]; !ok {
		t.Errorf("wire shape: missing registered_surfaces key: %v", got)
	}
}

func TestActionDescribe_MissingActionUnderKnownSurfaceListsRegisteredActions(t *testing.T) {
	deps := Deps{ActionDocs: makeCorpus(t)}
	params := json.RawMessage(`{"surface":"work","action":"no_such_action"}`)

	res, err := HandleActionDescribe(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleActionDescribe: %v", err)
	}
	if res.Doc != nil {
		t.Errorf("expected miss path; got Doc populated")
	}
	if res.Error != "action_not_found" {
		t.Errorf("Error: want action_not_found, got %q", res.Error)
	}
	// RegisteredActions uses Names which excludes _general. The corpus
	// has bug_resolve + _general under work; the miss envelope lists
	// bug_resolve only. The Hint mentions _general as a separate path.
	if !equalStringSlice(res.RegisteredActions, []string{"bug_resolve"}) {
		t.Errorf("RegisteredActions: want [bug_resolve], got %v", res.RegisteredActions)
	}
	if !strings.Contains(res.Hint, "_general") {
		t.Errorf("Hint should mention _general as a surface-wide-conventions path; got %q", res.Hint)
	}
}

func TestActionDescribe_MissingParamsReturnEnvelope(t *testing.T) {
	deps := Deps{ActionDocs: makeCorpus(t)}

	t.Run("missing surface", func(t *testing.T) {
		params := json.RawMessage(`{"action":"bug_resolve"}`)
		res, err := HandleActionDescribe(context.Background(), deps, params)
		if err != nil {
			t.Fatalf("HandleActionDescribe: %v", err)
		}
		if !strings.Contains(res.Error, "surface is required") {
			t.Errorf("want 'surface is required' in Error; got %q", res.Error)
		}
	})

	t.Run("missing action", func(t *testing.T) {
		params := json.RawMessage(`{"surface":"work"}`)
		res, err := HandleActionDescribe(context.Background(), deps, params)
		if err != nil {
			t.Fatalf("HandleActionDescribe: %v", err)
		}
		if !strings.Contains(res.Error, "action is required") {
			t.Errorf("want 'action is required' in Error; got %q", res.Error)
		}
		// The error hint nudges callers toward _general for surface conventions.
		if !strings.Contains(res.Error, "_general") {
			t.Errorf("missing-action error should mention _general; got %q", res.Error)
		}
	})

	t.Run("empty params payload", func(t *testing.T) {
		res, err := HandleActionDescribe(context.Background(), deps, nil)
		if err != nil {
			t.Fatalf("HandleActionDescribe: %v", err)
		}
		// Empty payload means no surface — same as the missing-surface case.
		if !strings.Contains(res.Error, "surface is required") {
			t.Errorf("want 'surface is required' for empty payload; got %q", res.Error)
		}
	})
}

func TestActionDescribe_CorpusNotLoaded(t *testing.T) {
	deps := Deps{ActionDocs: nil}
	params := json.RawMessage(`{"surface":"work","action":"bug_resolve"}`)

	res, err := HandleActionDescribe(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleActionDescribe: %v", err)
	}
	if res.Doc != nil {
		t.Errorf("expected corpus-not-loaded envelope; got Doc populated")
	}
	if !strings.Contains(res.Error, "action-docs corpus not loaded") {
		t.Errorf("Error: want 'action-docs corpus not loaded' substring; got %q", res.Error)
	}
}

func TestActionDescribe_InvalidParamsPayload(t *testing.T) {
	deps := Deps{ActionDocs: makeCorpus(t)}
	// Send malformed JSON (not a typed-param failure — a real parse failure).
	params := json.RawMessage(`{"surface":`)

	res, err := HandleActionDescribe(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleActionDescribe: %v", err)
	}
	if !strings.Contains(res.Error, "invalid params payload") {
		t.Errorf("want 'invalid params payload' substring; got %q", res.Error)
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
