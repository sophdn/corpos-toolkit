package admin

import (
	"context"
	"encoding/json"

	"toolkit/internal/actiondocs"
)

// ActionDescribeParams is the typed input shape for admin.action_describe.
// Both fields are required at the top level of params; missing surface or
// action surfaces a structured error envelope rather than a generic 4xx.
type ActionDescribeParams struct {
	Surface string `json:"surface"`
	Action  string `json:"action"`
}

// ActionDescribeResult is the response shape for HandleActionDescribe.
// Two distinct top-level JSON shapes (success → ActionDoc; error →
// envelope) are unified via a custom MarshalJSON that picks the right
// inline struct per populated branch. Mirrors the ForgeSchemaResult
// pattern in internal/forge/types.go — same two-branch wire contract.
type ActionDescribeResult struct {
	// Success-path payload. Mutually exclusive with the error fields below.
	Doc *actiondocs.ActionDoc

	// Error-envelope fields. Populated when Error is non-empty.
	Error              string
	Surface            string
	Action             string
	RegisteredSurfaces []string
	RegisteredActions  []string
	Hint               string
}

// MarshalJSON emits the parsed ActionDoc directly when Error is empty and
// Doc is set; otherwise emits the error envelope. Keeps the wire shape
// consistent with ForgeSchemaResult: success returns the doc fields at
// the top level, not nested under `doc`.
func (r ActionDescribeResult) MarshalJSON() ([]byte, error) {
	if r.Error == "" && r.Doc != nil {
		return json.Marshal(r.Doc)
	}
	envelope := struct {
		Error              string   `json:"error,omitempty"`
		Surface            string   `json:"surface,omitempty"`
		Action             string   `json:"action,omitempty"`
		RegisteredSurfaces []string `json:"registered_surfaces,omitempty"`
		RegisteredActions  []string `json:"registered_actions,omitempty"`
		Hint               string   `json:"hint,omitempty"`
	}{
		Error:              r.Error,
		Surface:            r.Surface,
		Action:             r.Action,
		RegisteredSurfaces: r.RegisteredSurfaces,
		RegisteredActions:  r.RegisteredActions,
		Hint:               r.Hint,
	}
	return json.Marshal(envelope)
}

// HandleActionDescribe implements admin.action_describe. Returns one
// parsed ActionDoc per (surface, action) lookup. On miss, returns a
// structured envelope naming what IS registered so the caller can
// self-correct without a second round-trip.
//
// The reserved literal action name actiondocs.GeneralAction ("_general")
// is findable here — surface-wide cross-cutting prose (cross-project
// defaults, alias conventions that span actions, sentinel values) lives
// under <surface>/_general.toml. The miss path's Hint mentions this so
// agents don't need to read the corpus convention to discover it.
//
// Degraded mode: when Deps.ActionDocs is nil (binary started with
// --action-docs-dir="" or the corpus dir was missing at startup) every
// call returns a corpus-not-loaded envelope. The action stays registered
// in admin.BuildTable either way so dispatcher behavior doesn't depend
// on startup flags.
func HandleActionDescribe(_ context.Context, deps Deps, rawParams json.RawMessage) (ActionDescribeResult, error) {
	var p ActionDescribeParams
	if len(rawParams) > 0 {
		if err := json.Unmarshal(rawParams, &p); err != nil {
			return ActionDescribeResult{
				Error: "action_describe: invalid params payload (" + err.Error() + ")",
			}, nil
		}
	}
	if p.Surface == "" {
		return ActionDescribeResult{
			Error: "action_describe: surface is required (e.g. surface=\"work\")",
		}, nil
	}
	if p.Action == "" {
		return ActionDescribeResult{
			Error: "action_describe: action is required (e.g. action=\"bug_resolve\"; pass action=\"_general\" for surface-wide conventions)",
		}, nil
	}
	if deps.ActionDocs == nil {
		return ActionDescribeResult{
			Error: "action-docs corpus not loaded (no --action-docs-dir resolved); admin.action_describe is disabled in this binary instance",
		}, nil
	}
	doc, ok := deps.ActionDocs.Get(p.Surface, p.Action)
	if ok {
		return ActionDescribeResult{Doc: doc}, nil
	}
	// Miss path: distinguish unknown-surface from unknown-action so the
	// caller can correct the right half of the (surface, action) tuple.
	surfaces := deps.ActionDocs.Surfaces()
	if !containsString(surfaces, p.Surface) {
		return ActionDescribeResult{
			Error:              "surface_not_found",
			Surface:            p.Surface,
			RegisteredSurfaces: surfaces,
			Hint:               "Pass one of the registered surfaces. Cross-cutting prose for any surface is at action=\"_general\" under that surface.",
		}, nil
	}
	return ActionDescribeResult{
		Error:             "action_not_found",
		Surface:           p.Surface,
		Action:            p.Action,
		RegisteredActions: deps.ActionDocs.Names(p.Surface),
		Hint:              "Pass one of the registered actions under this surface, or action=\"_general\" for surface-wide conventions.",
	}, nil
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
