package observehttp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"toolkit/internal/dispatch"
	"toolkit/internal/dispatch/policy"
)

// dispatcherRegistry holds the per-surface dispatch.Table values plus
// the shared policy registry and default project resolution. Populated
// at server startup AFTER each surface's BuildTable returns;
// thread-safe so handlers can read at request time.
//
// Why a package-level registry instead of an AppState field: AppState
// is constructed before the dispatch tables in main, and reordering
// risks breaking other startup sequences. A package-level setter lets
// main register each surface as its table is built without touching
// the HTTP-startup order. Same shape as events.SetFoldHook.
type dispatcherRegistry struct {
	mu             sync.RWMutex
	tables         map[string]dispatch.Table
	policy         *policy.Registry
	defaultProject string
	projectPaths   []dispatch.ProjectPath
}

var dispRegistry = &dispatcherRegistry{
	tables: map[string]dispatch.Table{},
}

// RegisterDispatchTable installs a per-surface dispatch.Table for
// HTTP-side dispatch via POST /mcp/{surface}. Call once per surface
// (work / admin / measure / knowledge / ml) after the surface's
// BuildTable returns. Idempotent: subsequent calls with the same
// surface name replace the prior table — useful for tests.
//
// Policy, defaultProject, and projectPaths are package-global because they
// don't vary per surface; the last caller's values win. Pass the same
// values across all surfaces. projectPaths lets the HTTP path resolve the
// effective project from a caller's Cwd, matching the native stdio path
// (previously only the stdio path was Cwd-aware).
func RegisterDispatchTable(surface string, table dispatch.Table, pol *policy.Registry, defaultProject string, projectPaths []dispatch.ProjectPath) {
	dispRegistry.mu.Lock()
	defer dispRegistry.mu.Unlock()
	dispRegistry.tables[surface] = table
	dispRegistry.policy = pol
	dispRegistry.defaultProject = defaultProject
	dispRegistry.projectPaths = projectPaths
}

// ResetDispatchRegistry clears every registered table. Test-only.
func ResetDispatchRegistry() {
	dispRegistry.mu.Lock()
	defer dispRegistry.mu.Unlock()
	dispRegistry.tables = map[string]dispatch.Table{}
	dispRegistry.policy = nil
	dispRegistry.defaultProject = ""
	dispRegistry.projectPaths = nil
}

// mcpDispatch implements POST /mcp/{surface}. Reads the request body
// as dispatch.Args, looks up the registered table by surface, and
// invokes dispatch.DispatchWith. Returns the typed handler result as
// JSON on success (HTTP 200), a structured error envelope for
// transport-level rejections (404 for unknown surface, 400 for
// malformed body, 503 when no surfaces registered yet).
//
// Application-level outcomes (policy reject, action not implemented,
// handler returns an error, handler returns a typed error envelope)
// all surface as HTTP 200 with the discriminating shape inside the
// response body — matching the stdio MCP semantics where every tool
// call lands a CallToolResult.
//
// CALLERS: shell hooks (e.g. arc-close-filing-review-hook.sh) that
// need to invoke MCP write actions without the stdio transport. The
// route mirrors the stdio dispatcher's policy gate so rationale-
// required actions still reject empty rationale, etc.
func (s AppState) mcpDispatch(w http.ResponseWriter, r *http.Request) {
	surface := r.PathValue("surface")
	dispRegistry.mu.RLock()
	table, ok := dispRegistry.tables[surface]
	pol := dispRegistry.policy
	defaultProj := dispRegistry.defaultProject
	projectPaths := dispRegistry.projectPaths
	dispRegistry.mu.RUnlock()

	if len(dispRegistry.tables) == 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error":   "dispatcher_not_ready",
			"message": "no surfaces registered yet (server still in startup)",
		})
		return
	}
	if !ok || table == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "unknown_surface",
			"message": fmt.Sprintf("no dispatch table registered for surface %q", surface),
		})
		return
	}

	var args dispatch.Args
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_body",
			"message": fmt.Sprintf("decode body: %s", err.Error()),
		})
		return
	}
	if args.Action == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "action_required",
			"message": "action field is required in the request body",
		})
		return
	}

	// Per-session default project: the toolkit-proxy forwards the calling
	// session's --default-project as X-MCP-Default-Project, so an unscoped
	// WRITE defaults to that session's project rather than one static
	// server-global value. Header-less direct callers (shell hooks) fall back
	// to the server-global default. The resolver also matches args.Cwd against
	// the project registry (parity with the native stdio path). Reads ignore
	// all of this — the dispatcher treats them as cross-project (see
	// dispatch.IsCrossProjectRead).
	sessionDefault := defaultProj
	if h := strings.TrimSpace(r.Header.Get("X-MCP-Default-Project")); h != "" {
		sessionDefault = h
	}
	resolver := dispatch.NewCwdProjectResolver(projectPaths, sessionDefault)
	var spanID string
	opts := dispatch.Options{Policy: pol, Surface: surface, CaptureSpanID: &spanID}
	callResult, _, err := dispatch.DispatchWithOptions(r.Context(), resolver, table, args, opts)
	if err != nil {
		// True transport-level dispatch error (rare; most handler
		// failures are typed in the result). Surface as 500 with the
		// error string in a stable envelope.
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error":   "dispatch_error",
			"message": err.Error(),
		})
		return
	}

	// dispatch.jsonResult bundles the typed handler return into a
	// stringified-JSON TextContent slot on the CallToolResult; the
	// middle `any` return value is nil. Unwrap the TextContent and
	// write its bytes directly — the caller sees the action-specific
	// shape (e.g. ReviewArcForFilingResult, BugListResult) at the
	// top level of the response body, or the {error: rationale_required,
	// ...} envelope for policy-rejected / action-not-implemented paths.
	bodyText := extractCallToolText(callResult)
	if bodyText == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error":   "encode_error",
			"message": "dispatcher returned no text content",
		})
		return
	}
	// Echo the request span_id on the response so a client can link follow-up
	// telemetry (e.g. query_interactions off a search's grounding_event) back to
	// this call. Spliced as an extra top-level key on the JSON-object body — purely
	// additive, existing fields untouched. (String splice rather than decode-to-map
	// to stay clear of the bare-`any` ban in this package; the dispatcher already
	// produced valid JSON.)
	bodyText = injectSpanID(bodyText, spanID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(bodyText))
}

// injectSpanID splices a top-level "span_id" key into a JSON-object body. Bodies
// that aren't a JSON object, an empty spanID, or a body that already carries a
// span_id are returned unchanged. The value is JSON-encoded so quoting/escaping
// is correct.
func injectSpanID(body, spanID string) string {
	if spanID == "" {
		return body
	}
	trimmed := strings.TrimLeft(body, " \t\r\n")
	if !strings.HasPrefix(trimmed, "{") {
		return body // not a JSON object (array/scalar/error text) — leave as-is
	}
	if strings.Contains(body, `"span_id"`) {
		return body // handler already set one; don't clobber
	}
	enc, err := json.Marshal(spanID)
	if err != nil {
		return body
	}
	field := `"span_id":` + string(enc)
	rest := trimmed[1:] // everything after the opening '{'
	if strings.HasPrefix(strings.TrimLeft(rest, " \t\r\n"), "}") {
		return "{" + field + rest // empty object: {"span_id":"x"}
	}
	return "{" + field + "," + rest
}

// extractCallToolText pulls the JSON string out of a CallToolResult's
// first TextContent slot. Returns "" when the result is nil or has no
// text content — both should be unreachable in practice (every dispatch
// path goes through jsonResult).
func extractCallToolText(r *mcp.CallToolResult) string {
	if r == nil || len(r.Content) == 0 {
		return ""
	}
	tc, ok := r.Content[0].(*mcp.TextContent)
	if !ok || tc == nil {
		return ""
	}
	return tc.Text
}
