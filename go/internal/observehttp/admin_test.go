package observehttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"toolkit/internal/testutil"
)

func writeDispatchPolicy(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "dispatch-policy.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDispatchPolicy_503WhenPathEmpty(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := httptest.NewServer(BuildRouter(AppState{Pool: pool, DispatchPolicyPath: ""}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/admin/dispatch-policy")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestDispatchPolicy_ReturnsLoadedFalseForMissingFile(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := httptest.NewServer(BuildRouter(AppState{
		Pool:               pool,
		DispatchPolicyPath: filepath.Join(t.TempDir(), "does-not-exist.toml"),
	}))
	t.Cleanup(srv.Close)

	var resp dispatchPolicyResponse
	if code := getJSON(t, srv, "/admin/dispatch-policy", &resp); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.Loaded {
		t.Errorf("loaded = true, want false for absent file")
	}
	if len(resp.Surfaces) != 0 {
		t.Errorf("surfaces non-empty for absent file: %v", resp.Surfaces)
	}
}

func TestDispatchPolicy_ReturnsSurfacesAndActions(t *testing.T) {
	pool := testutil.NewTestDB(t)
	path := writeDispatchPolicy(t, `
[work.bug_resolve]
requires_rationale = true

[work.task_complete]
requires_rationale = true

[work.bug_read]
requires_rationale = false

[knowledge.kiwix_search]
requires_rationale = false

[admin.schema_reload]
requires_rationale = true
`)
	srv := httptest.NewServer(BuildRouter(AppState{Pool: pool, DispatchPolicyPath: path}))
	t.Cleanup(srv.Close)

	var resp dispatchPolicyResponse
	getJSON(t, srv, "/admin/dispatch-policy", &resp)
	if !resp.Loaded {
		t.Fatal("loaded should be true after policy load")
	}
	if resp.Path != path {
		t.Errorf("path = %q, want %q", resp.Path, path)
	}

	// Three surfaces: work, knowledge, admin.
	if _, ok := resp.Surfaces["work"]; !ok {
		t.Errorf("missing work surface: %v", resp.Surfaces)
	}
	if _, ok := resp.Surfaces["knowledge"]; !ok {
		t.Errorf("missing knowledge surface: %v", resp.Surfaces)
	}
	if _, ok := resp.Surfaces["admin"]; !ok {
		t.Errorf("missing admin surface: %v", resp.Surfaces)
	}

	if !resp.Surfaces["work"]["bug_resolve"].RequiresRationale {
		t.Errorf("work.bug_resolve.requires_rationale = false, want true")
	}
	if resp.Surfaces["work"]["bug_read"].RequiresRationale {
		t.Errorf("work.bug_read.requires_rationale = true, want false")
	}
	if !resp.Surfaces["admin"]["schema_reload"].RequiresRationale {
		t.Errorf("admin.schema_reload.requires_rationale = false, want true")
	}
}

func TestDispatchPolicy_ReloadFromDiskShowsEdits(t *testing.T) {
	pool := testutil.NewTestDB(t)
	path := writeDispatchPolicy(t, `
[work.bug_resolve]
requires_rationale = true
`)
	srv := httptest.NewServer(BuildRouter(AppState{Pool: pool, DispatchPolicyPath: path}))
	t.Cleanup(srv.Close)

	var first dispatchPolicyResponse
	getJSON(t, srv, "/admin/dispatch-policy", &first)
	if len(first.Surfaces["work"]) != 1 {
		t.Fatalf("first load got %d work actions, want 1", len(first.Surfaces["work"]))
	}

	// Edit the TOML on disk; the next request should reflect the change.
	if err := os.WriteFile(path, []byte(`
[work.bug_resolve]
requires_rationale = true

[work.task_complete]
requires_rationale = true

[work.task_cancel]
requires_rationale = true
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var second dispatchPolicyResponse
	getJSON(t, srv, "/admin/dispatch-policy", &second)
	if len(second.Surfaces["work"]) != 3 {
		t.Fatalf("second load got %d work actions, want 3 (post-edit): %v",
			len(second.Surfaces["work"]), second.Surfaces["work"])
	}
}

func TestDispatchPolicy_NoCacheHeader(t *testing.T) {
	pool := testutil.NewTestDB(t)
	path := writeDispatchPolicy(t, `
[work.bug_resolve]
requires_rationale = true
`)
	srv := httptest.NewServer(BuildRouter(AppState{Pool: pool, DispatchPolicyPath: path}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/admin/dispatch-policy")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
}

func TestDispatchPolicy_JSONShape(t *testing.T) {
	pool := testutil.NewTestDB(t)
	path := writeDispatchPolicy(t, `
[work.bug_resolve]
requires_rationale = true
`)
	srv := httptest.NewServer(BuildRouter(AppState{Pool: pool, DispatchPolicyPath: path}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/admin/dispatch-policy")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["path"]; !ok {
		t.Error("response missing path field")
	}
	if _, ok := raw["loaded"]; !ok {
		t.Error("response missing loaded field")
	}
	surfaces, ok := raw["surfaces"].(map[string]any)
	if !ok {
		t.Fatalf("surfaces wrong type: %T", raw["surfaces"])
	}
	work, ok := surfaces["work"].(map[string]any)
	if !ok {
		t.Fatalf("work wrong type: %T", surfaces["work"])
	}
	bugResolve, ok := work["bug_resolve"].(map[string]any)
	if !ok {
		t.Fatalf("bug_resolve wrong type: %T", work["bug_resolve"])
	}
	if v := bugResolve["requires_rationale"]; v != true {
		t.Errorf("requires_rationale = %v, want true", v)
	}
}
