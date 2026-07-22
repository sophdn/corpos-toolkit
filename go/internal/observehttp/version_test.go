package observehttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Bug 1415: /version surfaces the ldflags-injected build identity so
// the dashboard can detect a daemon stale relative to its bundle.
// The handler must work without a Pool (drift detection needs to run
// even on degraded boots), must always 200, and must echo the AppState
// values verbatim — no fallback to a different sentinel that would
// hide a misconfigured ldflags injection.
func TestVersionEndpoint_ReturnsAppStateFields(t *testing.T) {
	state := AppState{
		GitSHA:      "abc1234",
		BuiltAtUnix: 1700000000,
		PackageVer:  "v0.1.0",
	}
	srv := httptest.NewServer(BuildRouter(state))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/version")
	if err != nil {
		t.Fatalf("GET /version: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	var body VersionResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.GitSHA != "abc1234" {
		t.Errorf("GitSHA: got %q, want abc1234", body.GitSHA)
	}
	if body.BuiltAtUnix != 1700000000 {
		t.Errorf("BuiltAtUnix: got %d, want 1700000000", body.BuiltAtUnix)
	}
	if body.PackageVersion != "v0.1.0" {
		t.Errorf("PackageVersion: got %q, want v0.1.0", body.PackageVersion)
	}
}

// Unversioned fallback: when ldflags aren't injected (go run, bare
// builds), /version returns the sentinel "unversioned" rather than an
// error envelope. The dashboard banner gate treats this as drift-
// unknown, not as a banner trigger.
func TestVersionEndpoint_UnversionedFallback(t *testing.T) {
	srv := httptest.NewServer(BuildRouter(AppState{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/version")
	if err != nil {
		t.Fatalf("GET /version: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	var body VersionResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.GitSHA != "" {
		t.Errorf("GitSHA: got %q, want empty (AppState zero value)", body.GitSHA)
	}
}
