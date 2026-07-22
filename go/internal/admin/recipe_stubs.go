package admin

// DeferredStubResult is the response shape for apply_recipe / step_probe
// while those actions remain unimplemented in the Go port.
type DeferredStubResult struct {
	Error  string `json:"error"`
	Action string `json:"action"`
	Detail string `json:"detail"`
}

// deferredRecipeStub returns the structured error envelope for
// apply_recipe and step_probe — both depend on the recipe walker +
// parameter resolution + SSH transport pipeline that lives in the
// Rust workspace under crates/toolkit-server/src/dispatch/admin.rs
// (apply_recipe_with_transport and step_probe_with_transport) and
// transport-lib. The Go port covers ad-hoc remote_exec; the recipe
// walker has not been ported because no caller has driven it post-
// migration. When a concrete need arises (host re-setup), promote
// this stub to a real implementation; until then the stub keeps the
// MCP surface complete so dispatch never silently drops the action.
func deferredRecipeStub(action string) DeferredStubResult {
	return DeferredStubResult{
		Error:  "action_deferred",
		Action: action,
		Detail: "apply_recipe / step_probe are not yet ported to Go; the Rust archive's recipe walker is the reference. Drive the port if you have a concrete recipe to run; until then call the Rust admin server (--rust-admin-port if running both) or run the recipe manually via remote_exec.",
	}
}
