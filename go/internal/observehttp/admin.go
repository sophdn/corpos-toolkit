package observehttp

import (
	"net/http"
	"sort"

	"toolkit/internal/dispatch/policy"
)

// agent-substrate-frontend F5: dispatch-policy peek surface.
//
// The admin endpoint surfaces the loaded dispatch policy so operators
// can answer "which actions enforce rationale?" without grepping the
// TOML or the policy package. The file is the source of truth; this
// endpoint reads it fresh per request so an edit + admin.schema_reload
// reflects in the dashboard without a server restart (per the F1 §8.3
// design contract — "loaded on disk on demand").
//
// See docs/SUBSTRATE_FRONTEND.md §8.3 for the design.

// dispatchPolicyResponse mirrors the dashboard's expected shape.
// Surfaces is a sorted-by-name map-of-maps: surfaces["work"]["bug_resolve"]
// = { requires_rationale: true }. Sorting at the wire level gives the
// dashboard a stable rendering without re-sorting client-side.
type dispatchPolicyResponse struct {
	Path     string                          `json:"path"`
	Loaded   bool                            `json:"loaded"`
	Surfaces map[string]map[string]policyRow `json:"surfaces"`
}

type policyRow struct {
	RequiresRationale bool `json:"requires_rationale"`
}

// dispatchPolicy is the GET /admin/dispatch-policy handler. Returns the
// current on-disk policy. Reads fresh; never cached.
//
// Failure modes:
//   - state.DispatchPolicyPath is empty → 503 with "policy disabled".
//   - file missing → 200 with loaded=false, empty surfaces (the policy
//     package treats this as the no-policy-default; the dashboard can
//     surface "policy file absent — no rationale enforcement").
//   - file unparseable → 500 (the policy package's Load returns an error).
func (s AppState) dispatchPolicy(w http.ResponseWriter, r *http.Request) {
	if s.DispatchPolicyPath == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "dispatch policy disabled — server was started without --dispatch-policy",
		})
		return
	}

	reg, err := policy.Load(s.DispatchPolicyPath)
	if err != nil {
		dbErr(w, err)
		return
	}

	resp := dispatchPolicyResponse{
		Path:     s.DispatchPolicyPath,
		Loaded:   reg.Len() > 0,
		Surfaces: map[string]map[string]policyRow{},
	}
	// Walk the registry by enumerating actions; split each "surface.action"
	// back into its components and populate the nested map. Sort key sets
	// for stable JSON output.
	actions := reg.Actions()
	sort.Strings(actions)
	for _, key := range actions {
		surface, action, ok := splitSurfaceAction(key)
		if !ok {
			continue
		}
		if _, present := resp.Surfaces[surface]; !present {
			resp.Surfaces[surface] = map[string]policyRow{}
		}
		resp.Surfaces[surface][action] = policyRow{
			RequiresRationale: reg.Gates(surface, action).RequiresRationale,
		}
	}

	// Don't cache — the operator's reload-from-disk button depends on
	// always seeing the current file contents.
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, resp)
}

// splitSurfaceAction splits a "surface.action" key from policy.Registry
// into its (surface, action) parts. Returns ok=false on malformed keys
// (no dot), which would be a bug in the policy registry.
func splitSurfaceAction(key string) (string, string, bool) {
	for i := 0; i < len(key); i++ {
		if key[i] == '.' {
			return key[:i], key[i+1:], true
		}
	}
	return "", "", false
}
