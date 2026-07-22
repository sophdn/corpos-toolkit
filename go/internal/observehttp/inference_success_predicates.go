package observehttp

// Per-task success-predicate registry for the /inference/health-cards +
// /inference/sparklines endpoints.
//
// As of chain telemetry-success-model-unification (Chain 2), the success
// VALUE is no longer computed here. The outcome predicate SQL was materialized
// into the proj_inference_call_success projection (see
// projections/inference_call_success.go::inferenceOutcomeSuccessExpr); the
// handlers read the materialized column. What stays here is the read-side
// PRESENTATION concern: mapping a task_id to its human-readable success_rate
// BASIS label (HealthCard.success_rate_basis) and signalling the
// papercut-vs-structural-rework fallback (a task with no registered predicate
// falls back to the default + a logged TODO, rather than blocking sign-off —
// vault decision 2026-05-19_observability-papercut-vs-structural-rework-split).
//
// The task_id→predicate DISPATCH (exact-match → classify_ prefix → default)
// is mirrored in SQL by inferenceOutcomeKindExpr in the projection; the two
// are kept in agreement by the projection package's
// TestInferenceCallSuccess_DispatchMatchesRegistry.

// SuccessPredicate is the registry entry for a task's outcome-success notion.
// It now carries only the basis Description (the SQL moved to the projection,
// chain telemetry-success-model-unification).
type SuccessPredicate struct {
	// Free-text basis shown in /inference/health-cards.success_rate_basis.
	Description string
}

// defaultSuccessPredicate is the floor — applies when no per-task predicate is
// registered. Its materialized arm ("output_tokens IS NOT NULL AND latency_ms
// > 0") captures "the call produced output and took nonzero time", the minimum
// bar for "the inference path is alive."
var defaultSuccessPredicate = SuccessPredicate{
	Description: "default: row has non-null output_tokens AND non-zero latency",
}

// classifyPredicate fires for any task_id starting with `classify_`. Its
// materialized arm counts a call successful iff ANY benchmark_results row for
// the task scores > 0.5. (It is any-row, not "latest": the former read-time
// SQL's `ORDER BY run_at DESC LIMIT 1` inside EXISTS was inert — bug 948 — and
// is dropped in the materialized form; the Description now states any-row.)
var classifyPredicate = SuccessPredicate{
	Description: "classify: any benchmark_results.accuracy_score for the task > 0.5",
}

// vaultRerankRetrievePredicate fires for the vault_search dispatch. Its
// materialized arm marks a call successful iff a proximate action='vault_search'
// grounding_events row with results_count > 0 exists, within a latency-scaled
// window that accommodates the two-pass search shape (see the projection's
// inferenceOutcomeSuccessExpr for the window rationale).
var vaultRerankRetrievePredicate = SuccessPredicate{
	Description: "vault-rerank-retrieve: matching grounding_events row has results_count > 0 (proximity window scaled by qwen latency for two-pass search shape)",
}

// successPredicateRegistry maps task_id → predicate. Lookup is exact-match
// first; then falls back to prefix-matching for the classify_ family.
var successPredicateRegistry = map[string]SuccessPredicate{
	"vault-rerank-retrieve": vaultRerankRetrievePredicate,
}

// lookupSuccessPredicate returns the predicate (basis label) for a task_id,
// defaulting to defaultSuccessPredicate when no entry exists. Per the papercut-
// vs-structural-rework split: missing predicates do NOT block sign-off; the
// caller logs a TODO so the gap is surfaced.
//
// Returns (predicate, hadCustom). hadCustom=false means the default was
// returned and the caller should consider logging.
func lookupSuccessPredicate(taskID string) (SuccessPredicate, bool) {
	if p, ok := successPredicateRegistry[taskID]; ok {
		return p, true
	}
	if len(taskID) >= 9 && taskID[:9] == "classify_" {
		return classifyPredicate, true
	}
	return defaultSuccessPredicate, false
}
