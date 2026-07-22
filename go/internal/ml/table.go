// Package ml's table.go assembles the dispatch.Table for the ml meta-tool:
// the `inference` action plus a registry surface for per-task
// convenience actions (e.g. `route_query`, `curation_score`,
// `forge_suggest_surfaces`). Pairs with the parallel BuildTable
// functions in internal/admin, internal/work, internal/measure,
// internal/knowledge.
//
// Convenience-action pattern: each downstream ML chain (when a model
// is promoted) registers a per-task convenience action that wraps
// `inference` and post-processes the output into a task-shaped
// response. Registration uses RegisterConvenience; this keeps the
// substrate's plumbing decoupled from per-model code while making
// the agent's surface ergonomic.
package ml

import (
	"context"
	"encoding/json"

	"toolkit/internal/db"
	"toolkit/internal/dispatch"
)

// TableDeps bundles dependencies the ml dispatch table needs. Pool is
// required for telemetry writes; Registry resolves trained_model rows
// to loaded Sessions. Both nil → an empty table (degraded mode; the
// surface registers but every action returns an error envelope).
type TableDeps struct {
	Pool     *db.Pool
	Registry *Registry
}

// ConvenienceAction is a per-task wrapper around the inference action.
// Each downstream ML chain registers one when its model gets promoted
// — the registered handler typically resolves task → promoted-model,
// runs inference, and post-processes (e.g. argmax + threshold for
// curation classifier; sorted source-list for source router).
//
// Convenience actions register under their natural name on the ml
// surface (route_query, curation_score, etc.). They share the ml
// table so callers see them via ml.<action>.
type ConvenienceAction struct {
	// Name is the dispatch action name (e.g. "route_query").
	Name string
	// Handler is the dispatch handler — same shape as any other
	// dispatch.Handler. Implementations typically call HandleInference
	// internally to share the telemetry-write path.
	Handler dispatch.Handler
}

// BuildTable returns the ml surface's dispatch.Table. The `inference`
// action is always registered (degraded mode returns error envelopes
// when Pool/Registry are nil). Per-task convenience actions register
// via the optional convenience slice — passed by main.go from each
// downstream ML chain's startup wire-up.
func BuildTable(deps TableDeps, convenience ...ConvenienceAction) dispatch.Table {
	table := dispatch.Table{}

	// Always register inference; the handler itself surfaces a typed
	// degraded-mode envelope when deps are nil.
	handlerDeps := HandlerDeps{Pool: deps.Pool, Registry: deps.Registry}
	table["inference"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (InferenceResult, error) {
		if handlerDeps.Pool == nil || handlerDeps.Registry == nil {
			return InferenceResult{
				Error: "ml surface degraded — Pool or Registry unavailable at startup. Ensure ML_MODELS_ROOT + trained_models table are reachable from the toolkit-server config.",
			}, nil
		}
		return HandleInference(ctx, handlerDeps, project, params)
	})

	// Per-task convenience actions land alongside inference. Caller
	// (main.go) is responsible for not registering duplicates — first
	// write wins.
	for _, c := range convenience {
		if _, exists := table[c.Name]; exists {
			continue
		}
		table[c.Name] = c.Handler
	}

	return table
}
