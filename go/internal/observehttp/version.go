package observehttp

import "net/http"

// VersionResponse is the shape GET /version returns. Mirrors
// admin.ServerVersionResult so dashboards / scripts can use either
// surface interchangeably; the HTTP form is the dashboard's
// preferred entry point because it doesn't require an MCP session
// (bug 1415's daemon-staleness banner runs in the browser).
type VersionResponse struct {
	GitSHA         string `json:"git_sha"`
	BuiltAtUnix    int64  `json:"built_at_unix"`
	PackageVersion string `json:"package_version"`
}

// version returns the running binary's build identity. Used by the
// dashboard to detect drift from the bundle's vite-injected expected
// SHA (bug 1415). Always 200 — the response is informational, not a
// health gate.
func (s AppState) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, VersionResponse{
		GitSHA:         s.GitSHA,
		BuiltAtUnix:    s.BuiltAtUnix,
		PackageVersion: s.PackageVer,
	})
}
