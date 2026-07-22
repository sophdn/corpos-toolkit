package observehttp

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"toolkit/internal/actiondocs"
	"toolkit/internal/testutil"
)

// writeActionDocsCorpus stages a small two-surface corpus on disk:
// work/{bug_resolve, bug_read} and admin/health, plus a work/_general
// chunk and a knowledge surface dir to test sort ordering. Returns
// the corpus root.
func writeActionDocsCorpus(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	mkdir := func(p string) {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	mkdir(filepath.Join(root, "work"))
	mkdir(filepath.Join(root, "admin"))

	writeFile(filepath.Join(root, "work", "bug_resolve.toml"), `
surface = "work"
action = "bug_resolve"
purpose = "Resolve a bug."
`)
	writeFile(filepath.Join(root, "work", "bug_read.toml"), `
surface = "work"
action = "bug_read"
purpose = "Read one bug."
`)
	writeFile(filepath.Join(root, "work", "_general.toml"), `
surface = "work"
action = "_general"
purpose = "Work surface conventions."
`)
	writeFile(filepath.Join(root, "admin", "health.toml"), `
surface = "admin"
action = "health"
purpose = "Liveness probe."
`)

	return root
}

func writeDispatchPolicyWithBugResolve(t *testing.T) string {
	t.Helper()
	return writeDispatchPolicy(t, `
[work.bug_resolve]
requires_rationale = true
`)
}

func TestActionDocs_EmptyRegistryReturnsZeroValueResponse(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := httptest.NewServer(BuildRouter(AppState{Pool: pool}))
	t.Cleanup(srv.Close)

	var resp actionDocsResponse
	if code := getJSON(t, srv, "/admin/action-docs", &resp); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.Count != 0 {
		t.Errorf("count = %d, want 0", resp.Count)
	}
	if len(resp.Surfaces) != 0 {
		t.Errorf("surfaces non-empty: %v", resp.Surfaces)
	}
	if len(resp.Actions) != 0 {
		t.Errorf("actions non-empty: %v", resp.Actions)
	}
}

func TestActionDocs_FullCorpusReturnsAllChunks(t *testing.T) {
	root := writeActionDocsCorpus(t)
	reg, err := actiondocs.Load(root)
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}

	pool := testutil.NewTestDB(t)
	srv := httptest.NewServer(BuildRouter(AppState{
		Pool:          pool,
		ActionDocs:    reg,
		ActionDocsDir: root,
	}))
	t.Cleanup(srv.Close)

	var resp actionDocsResponse
	if code := getJSON(t, srv, "/admin/action-docs", &resp); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.Count != 4 {
		t.Errorf("count = %d, want 4", resp.Count)
	}
	if len(resp.Surfaces) != 2 || resp.Surfaces[0] != "admin" || resp.Surfaces[1] != "work" {
		t.Errorf("surfaces = %v, want [admin work]", resp.Surfaces)
	}
	if resp.CorpusPath != root {
		t.Errorf("corpus_path = %q, want %q", resp.CorpusPath, root)
	}
	if _, ok := resp.Actions["work"]["_general"]; !ok {
		t.Errorf("work/_general missing from response")
	}
	if _, ok := resp.Actions["work"]["bug_resolve"]; !ok {
		t.Errorf("work/bug_resolve missing from response")
	}
}

func TestActionDocs_SurfaceFilterReturnsOneSurface(t *testing.T) {
	root := writeActionDocsCorpus(t)
	reg, _ := actiondocs.Load(root)
	pool := testutil.NewTestDB(t)
	srv := httptest.NewServer(BuildRouter(AppState{Pool: pool, ActionDocs: reg, ActionDocsDir: root}))
	t.Cleanup(srv.Close)

	var resp actionDocsResponse
	if code := getJSON(t, srv, "/admin/action-docs?surface=work", &resp); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(resp.Surfaces) != 1 || resp.Surfaces[0] != "work" {
		t.Errorf("surfaces = %v, want [work]", resp.Surfaces)
	}
	if _, ok := resp.Actions["admin"]; ok {
		t.Errorf("admin surface leaked into surface=work response")
	}
}

func TestActionDocs_SurfaceAndActionFilterReturnsOneChunk(t *testing.T) {
	root := writeActionDocsCorpus(t)
	reg, _ := actiondocs.Load(root)
	pool := testutil.NewTestDB(t)
	srv := httptest.NewServer(BuildRouter(AppState{Pool: pool, ActionDocs: reg, ActionDocsDir: root}))
	t.Cleanup(srv.Close)

	var resp actionDocsResponse
	if code := getJSON(t, srv, "/admin/action-docs?surface=work&action=bug_resolve", &resp); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.Count != 1 {
		t.Errorf("count = %d, want 1", resp.Count)
	}
	if _, ok := resp.Actions["work"]["bug_resolve"]; !ok {
		t.Errorf("bug_resolve missing")
	}
	if _, ok := resp.Actions["work"]["bug_read"]; ok {
		t.Errorf("bug_read leaked under action=bug_resolve filter")
	}
}

func TestActionDocs_ActionWithoutSurfaceReturns400(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := httptest.NewServer(BuildRouter(AppState{Pool: pool}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/admin/action-docs?action=bug_resolve")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestActionDocs_ReloadFreshLoadsFromDisk(t *testing.T) {
	root := writeActionDocsCorpus(t)
	reg, _ := actiondocs.Load(root)

	pool := testutil.NewTestDB(t)
	srv := httptest.NewServer(BuildRouter(AppState{Pool: pool, ActionDocs: reg, ActionDocsDir: root}))
	t.Cleanup(srv.Close)

	// Add a new chunk after the registry was loaded.
	newChunk := filepath.Join(root, "admin", "host_list.toml")
	if err := os.WriteFile(newChunk, []byte(`
surface = "admin"
action = "host_list"
purpose = "List registered hosts."
`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Default GET still sees the stale registry — host_list absent.
	var stale actionDocsResponse
	if code := getJSON(t, srv, "/admin/action-docs", &stale); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if _, ok := stale.Actions["admin"]["host_list"]; ok {
		t.Errorf("host_list visible without ?reload=1 (registry not stale)")
	}

	// ?reload=1 picks it up.
	var fresh actionDocsResponse
	if code := getJSON(t, srv, "/admin/action-docs?reload=1", &fresh); code != http.StatusOK {
		t.Fatalf("reload status = %d, want 200", code)
	}
	if _, ok := fresh.Actions["admin"]["host_list"]; !ok {
		t.Errorf("host_list missing after ?reload=1: %v", fresh.Actions["admin"])
	}
}

func TestActionDocs_WriteActionsReflectsDispatchPolicy(t *testing.T) {
	root := writeActionDocsCorpus(t)
	reg, _ := actiondocs.Load(root)
	policyPath := writeDispatchPolicyWithBugResolve(t)

	pool := testutil.NewTestDB(t)
	srv := httptest.NewServer(BuildRouter(AppState{
		Pool:               pool,
		ActionDocs:         reg,
		ActionDocsDir:      root,
		DispatchPolicyPath: policyPath,
	}))
	t.Cleanup(srv.Close)

	var resp actionDocsResponse
	if code := getJSON(t, srv, "/admin/action-docs", &resp); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !resp.WriteActions["work.bug_resolve"] {
		t.Errorf("work.bug_resolve missing from write_actions: %v", resp.WriteActions)
	}
	if resp.WriteActions["work.bug_read"] {
		t.Errorf("work.bug_read incorrectly classified as write: %v", resp.WriteActions)
	}
}

func TestActionDocs_WriteActionsEmptyWhenPolicyAbsent(t *testing.T) {
	root := writeActionDocsCorpus(t)
	reg, _ := actiondocs.Load(root)

	pool := testutil.NewTestDB(t)
	srv := httptest.NewServer(BuildRouter(AppState{
		Pool:          pool,
		ActionDocs:    reg,
		ActionDocsDir: root,
	}))
	t.Cleanup(srv.Close)

	var resp actionDocsResponse
	if code := getJSON(t, srv, "/admin/action-docs", &resp); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(resp.WriteActions) != 0 {
		t.Errorf("write_actions non-empty without policy path: %v", resp.WriteActions)
	}
}

func TestActionDocs_ParseErrorsPropagate(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Chunk with action name mismatch — registry surfaces this in ParseErrors.
	if err := os.WriteFile(filepath.Join(root, "work", "bug_resolve.toml"), []byte(`
surface = "work"
action = "WRONG_NAME"
purpose = "stub"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, _ := actiondocs.Load(root)

	pool := testutil.NewTestDB(t)
	srv := httptest.NewServer(BuildRouter(AppState{Pool: pool, ActionDocs: reg, ActionDocsDir: root}))
	t.Cleanup(srv.Close)

	var resp actionDocsResponse
	if code := getJSON(t, srv, "/admin/action-docs", &resp); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(resp.ParseErrors) == 0 {
		t.Errorf("parse_errors empty; expected one entry for action mismatch")
	}
}
