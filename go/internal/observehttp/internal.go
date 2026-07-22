package observehttp

import (
	"log/slog"
	"net/http"
	"strconv"

	"toolkit/internal/obs"
)

// projectFilter reads the shared project-scope query parameter. Accepts
// either `project=` or `project_id=` for symmetry with the work
// meta-tool's `project` param and the rows' actual column name.
func projectFilter(r *http.Request) string {
	if p := r.URL.Query().Get("project"); p != "" {
		return p
	}
	return r.URL.Query().Get("project_id")
}

// dbErr writes a 500 JSON error envelope mirroring the Rust db_err
// helper. Callers log + return; we log here so each handler does not
// repeat the same logging boilerplate.
//
// Observability: observe-HTTP requests do not run under the MCP dispatch
// span (they enter through net/http, not the meta-tool seam), so the
// emitted log line uses the bare package logger without span attrs.
// Adding HTTP-side spans is a follow-on if observe-HTTP grows enough
// surface area to justify it; the current handlers are read-only
// projection queries and the span tree's primary value is on the
// mutating MCP path.
func dbErr(w http.ResponseWriter, err error) {
	obs.L().Error("observehttp: db error", slog.String("err", err.Error()))
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

// optSince reads the shared `since` query parameter (Unix epoch seconds)
// used by every benchmark/event time-range handler. Returns (value, true)
// when the parameter parses cleanly; (0, false) when absent or malformed
// — handlers treat the false branch as "no since filter".
func optSince(r *http.Request) (int64, bool) {
	v := r.URL.Query().Get("since")
	if v == "" {
		return 0, false
	}
	if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
		return parsed, true
	}
	return 0, false
}

// boolParam parses a "true"/"false" (or "1"/"0") query parameter,
// returning fallback when the value is absent or unparseable.
//
// Distinct from mcpparam.Int64Opt et al because the input is an
// *http.Request query (HTTP-side) not a json.RawMessage (MCP-dispatch
// side). The two surfaces don't share a helper because their inputs
// don't compose.
func boolParam(r *http.Request, name string, fallback bool) bool {
	v := r.URL.Query().Get(name)
	switch v {
	case "true", "1":
		return true
	case "false", "0":
		return false
	default:
		return fallback
	}
}
