package ecosystem

import (
	"toolkit/internal/db"
	"toolkit/internal/dispatch"
)

// Deps is the ecosystem surface's dependency set. Mirrors admin.Deps' minimal
// shape — the surface is a thin deterministic reader/writer over the shared
// ledger pool (the reused `hosts` table + the ecosystem_* tables).
type Deps struct {
	Pool *db.Pool
}

// BuildTable returns the ecosystem surface's dispatch.Table.
//
//   - Reads (ungated): access_check (the deterministic "do I have access to X"
//     query), describe (a target's full record), list (enumerate the learned
//     ecosystem).
//   - Writes (rationale-gated in action-manifests/dispatch-policy.toml):
//     host_learn, service_learn, access_learn — idempotent upserts that populate
//     the tenant's ecosystem as data, never code.
//
// Pairs with the BuildTable functions in internal/sys, internal/admin, etc.
func BuildTable(deps Deps) dispatch.Table {
	return dispatch.Table{
		"access_check":  dispatch.AdaptParamsOnly(deps.accessCheck),
		"describe":      dispatch.AdaptParamsOnly(deps.describe),
		"list":          dispatch.AdaptParamsOnly(deps.list),
		"host_learn":    dispatch.AdaptParamsOnly(deps.hostLearn),
		"service_learn": dispatch.AdaptParamsOnly(deps.serviceLearn),
		"access_learn":  dispatch.AdaptParamsOnly(deps.accessLearn),
		// canonical-names / artifact-identity map (canon_resolve extraction).
		"canon_resolve": dispatch.AdaptParamsOnly(deps.canonResolve),
		"canon_learn":   dispatch.AdaptParamsOnly(deps.canonLearn),
		"canon_list":    dispatch.AdaptParamsOnly(deps.canonList),
	}
}
