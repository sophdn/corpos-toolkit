package admin

import (
	"context"
	"encoding/json"
	"time"

	"toolkit/internal/actiondocs"
	"toolkit/internal/db"
	"toolkit/internal/dispatch"
	"toolkit/internal/forge/registry"
)

// Deps bundles everything the admin dispatch table needs. Constructed
// once at server startup; handlers borrow what they need.
type Deps struct {
	Pool        *db.Pool
	Schemas     *registry.Registry
	ActionDocs  *actiondocs.Registry
	StartedAt   time.Time
	GitSHA      string
	BuiltAtUnix int64
	PackageVer  string
}

// BuildTable returns a dispatch.Table populated with every admin action.
// Each entry adapts a typed-return handler into dispatch.Handler — the
// only place where the typed result widens to `any` for JSON marshaling.
func BuildTable(deps Deps) dispatch.Table {
	t := dispatch.Table{}

	healthHandler := dispatch.AdaptNoParams(deps.serverHealth)
	t["health"] = healthHandler
	t["server_health"] = healthHandler

	t["server_version"] = dispatch.AdaptNoParams(deps.serverVersion)
	t["schema_version"] = dispatch.AdaptNoParams(deps.schemaVersion)
	t["schema_reload"] = dispatch.AdaptNoParams(deps.schemaReload)

	t["project_register"] = dispatch.AdaptParamsOnly(deps.projectRegister)
	t["project_list"] = dispatch.AdaptNoParams(deps.projectList)

	t["host_register"] = dispatch.AdaptParamsOnly(deps.hostRegister)
	t["host_list"] = dispatch.AdaptParamsOnly(deps.hostList)
	t["host_remove"] = dispatch.AdaptParamsOnly(deps.hostRemove)

	t["vault_search_metrics"] = dispatch.AdaptParamsOnly(deps.vaultSearchMetrics)

	t["vault_orphan_list"] = dispatch.AdaptNoParams(deps.vaultOrphanList)
	t["vault_integrity_sweep"] = dispatch.AdaptNoParams(deps.vaultIntegritySweep)

	// action_describe is always registered so dispatch behavior doesn't
	// depend on the binary's --action-docs-dir resolution; the handler
	// itself surfaces a corpus-not-loaded envelope when ActionDocs is nil.
	t["action_describe"] = dispatch.AdaptParamsOnly(func(ctx context.Context, params json.RawMessage) (ActionDescribeResult, error) {
		return HandleActionDescribe(ctx, deps, params)
	})

	t["remote_exec"] = dispatch.AdaptParamsOnly(deps.remoteExec)

	// apply_recipe + step_probe land as explicit deferred stubs so a
	// caller hitting them gets a structured error envelope, not a
	// silent action-not-implemented. The full port lands when a
	// concrete need (host re-setup) drives it.
	t["apply_recipe"] = dispatch.AdaptParamsOnly(func(_ context.Context, _ json.RawMessage) (DeferredStubResult, error) {
		return deferredRecipeStub("apply_recipe"), nil
	})
	t["step_probe"] = dispatch.AdaptParamsOnly(func(_ context.Context, _ json.RawMessage) (DeferredStubResult, error) {
		return deferredRecipeStub("step_probe"), nil
	})

	// Orchestrator-tier escalation contract (chain orchestrator-tier-
	// escalation-contract T2). escalation_propose emits EscalationProposed
	// through the write-side ledger; the two threshold actions read/write
	// the escalation_thresholds config table. See docs/ORCHESTRATOR_ESCALATION.md.
	t["escalation_threshold_list"] = dispatch.AdaptParamsOnly(deps.escalationThresholdList)
	t["escalation_threshold_set"] = dispatch.AdaptParamsOnly(deps.escalationThresholdSet)
	t["escalation_propose"] = dispatch.AdaptParamsOnly(deps.escalationPropose)

	return t
}
