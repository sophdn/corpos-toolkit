package observehttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toolkit/internal/dispatch"
	"toolkit/internal/observehttp"
)

// withCleanRegistry resets the package-level dispatcher registry so
// tests don't bleed state into each other.
func withCleanRegistry(t *testing.T) {
	t.Helper()
	observehttp.ResetDispatchRegistry()
	t.Cleanup(observehttp.ResetDispatchRegistry)
}

// stubTable returns a dispatch.Table with one action that echoes the
// raw params back as the result. Used to verify the POST route's
// wiring without involving any DB.
func stubTable() dispatch.Table {
	return dispatch.Table{
		"echo": dispatch.Adapt(func(_ context.Context, project string, params json.RawMessage) (map[string]string, error) {
			return map[string]string{
				"echoed_action":  "echo",
				"echoed_project": project,
				"echoed_params":  string(params),
			}, nil
		}),
	}
}

func TestMCPDispatch_UnknownSurfaceReturns404(t *testing.T) {
	withCleanRegistry(t)
	observehttp.RegisterDispatchTable("work", stubTable(), nil, "test-project", nil)

	router := observehttp.BuildRouter(observehttp.AppState{})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/mcp/nonexistent", "application/json", strings.NewReader(`{"action":"echo"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown surface, got %d", resp.StatusCode)
	}
}

func TestMCPDispatch_NoSurfacesRegisteredReturns503(t *testing.T) {
	withCleanRegistry(t)
	// Intentionally do not register any tables.

	router := observehttp.BuildRouter(observehttp.AppState{})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/mcp/work", "application/json", strings.NewReader(`{"action":"echo"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when no tables registered, got %d", resp.StatusCode)
	}
}

func TestMCPDispatch_MalformedBodyReturns400(t *testing.T) {
	withCleanRegistry(t)
	observehttp.RegisterDispatchTable("work", stubTable(), nil, "test-project", nil)

	router := observehttp.BuildRouter(observehttp.AppState{})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/mcp/work", "application/json", strings.NewReader(`not json`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed body, got %d", resp.StatusCode)
	}
}

func TestMCPDispatch_MissingActionReturns400(t *testing.T) {
	withCleanRegistry(t)
	observehttp.RegisterDispatchTable("work", stubTable(), nil, "test-project", nil)

	router := observehttp.BuildRouter(observehttp.AppState{})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/mcp/work", "application/json", strings.NewReader(`{"project":"x"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing action, got %d", resp.StatusCode)
	}
}

func TestMCPDispatch_HappyPathReturnsTypedResult(t *testing.T) {
	withCleanRegistry(t)
	observehttp.RegisterDispatchTable("work", stubTable(), nil, "default-proj", nil)

	router := observehttp.BuildRouter(observehttp.AppState{})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	body := `{"action":"echo","project":"override-proj","params":{"hello":"world"}}`
	resp, err := http.Post(srv.URL+"/mcp/work", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var decoded map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if decoded["echoed_action"] != "echo" {
		t.Fatalf("expected echoed_action=echo, got %q", decoded["echoed_action"])
	}
	if decoded["echoed_project"] != "override-proj" {
		t.Fatalf("expected echoed_project=override-proj, got %q", decoded["echoed_project"])
	}
	if !strings.Contains(decoded["echoed_params"], `"hello"`) {
		t.Fatalf("expected echoed_params to contain hello, got %q", decoded["echoed_params"])
	}
	// The dispatch echoes the request span_id on the response (additive), so a
	// client can link follow-up telemetry to this call's grounding_events.
	if decoded["span_id"] == "" {
		t.Fatalf("expected a non-empty span_id on the response, got %q", decoded["span_id"])
	}
}

func TestMCPDispatch_UsesDefaultProjectWhenAbsent(t *testing.T) {
	withCleanRegistry(t)
	observehttp.RegisterDispatchTable("work", stubTable(), nil, "default-proj", nil)

	router := observehttp.BuildRouter(observehttp.AppState{})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	// No project field in body → resolver falls back to registered default.
	body := `{"action":"echo"}`
	resp, err := http.Post(srv.URL+"/mcp/work", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var decoded map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	if decoded["echoed_project"] != "default-proj" {
		t.Fatalf("expected echoed_project=default-proj (resolver fallback), got %q", decoded["echoed_project"])
	}
}

// TestMCPDispatch_HonorsPerSessionDefaultProjectHeader is the regression for
// bug http-dispatch-ignores-per-session-default-project-header-and-cwd-uses-server-global:
// an unscoped write must default to the SESSION's project (forwarded by the
// proxy as X-MCP-Default-Project), not the one static server-global default.
func TestMCPDispatch_HonorsPerSessionDefaultProjectHeader(t *testing.T) {
	withCleanRegistry(t)
	observehttp.RegisterDispatchTable("work", stubTable(), nil, "server-global-default", nil)

	router := observehttp.BuildRouter(observehttp.AppState{})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/mcp/work", bytes.NewReader([]byte(`{"action":"echo"}`)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-MCP-Default-Project", "session-proj")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var decoded map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	if decoded["echoed_project"] != "session-proj" {
		t.Fatalf("expected echoed_project=session-proj (per-session header), got %q", decoded["echoed_project"])
	}
}

// TestMCPDispatch_ResolvesProjectFromCwd pins CWD parity with the native stdio
// path: an unscoped call whose Cwd sits under a registered project path
// resolves to that project.
func TestMCPDispatch_ResolvesProjectFromCwd(t *testing.T) {
	withCleanRegistry(t)
	paths := []dispatch.ProjectPath{{ID: "corpos-toolkit", Path: "/home/user/dev/corpos-toolkit"}}
	observehttp.RegisterDispatchTable("work", stubTable(), nil, "server-global-default", paths)

	router := observehttp.BuildRouter(observehttp.AppState{})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	body := `{"action":"echo","cwd":"/home/user/dev/corpos-toolkit/go"}`
	resp, err := http.Post(srv.URL+"/mcp/work", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var decoded map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	if decoded["echoed_project"] != "corpos-toolkit" {
		t.Fatalf("expected echoed_project=corpos-toolkit (CWD match), got %q", decoded["echoed_project"])
	}
}
