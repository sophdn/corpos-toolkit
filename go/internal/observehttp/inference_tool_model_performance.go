package observehttp

import (
	"net/http"
)

// Per-tool-per-model performance endpoint (chain per-tool-per-model-
// observability T12). Reads proj_inference_tool_model_performance — the
// read-side projection over inference_invocations — and surfaces the
// per-(tool, model) ranking the chain exists to provide (and the Chain-3
// router will consume). Rates and averages are computed on read from the
// projection's stored totals.

// ToolModelStat is one (tool, model) ranking row.
type ToolModelStat struct {
	TaskID    string `json:"task_id"`
	ModelName string `json:"model_name"`
	CallCount int64  `json:"call_count"`
	// SuccessRate is the CALL-LEVEL rate (success_count / call_count) — the
	// "did the call return cleanly" liveness layer.
	SuccessRate float64 `json:"success_rate"`
	// OutcomeSuccessRate is the OUTCOME-level rate (outcome_success_count /
	// call_count) — the materialized Layer-2 predicate (classify→benchmark,
	// vault→grounding, else liveness floor) rolled up per (tool, model).
	// Added by chain telemetry-success-model-unification (the both-layers
	// model); the Chain-3 data-driven router reads this alongside SuccessRate.
	OutcomeSuccessRate float64  `json:"outcome_success_rate"`
	AvgLatencyMS       int64    `json:"avg_latency_ms"`
	MaxLatencyMS       int64    `json:"max_latency_ms"`
	AvgTokens          *float64 `json:"avg_tokens"` // (in+out)/calls_with_tokens; nil when no usage was recorded
	LastInvokedAt      string   `json:"last_invoked_at"`
}

// inferenceToolModelPerformance returns the per-(tool, model) ranking rows,
// ordered by tool then most-used model first (call_count desc) so the
// dashboard's per-tool grouping reads top-down.
func (s AppState) inferenceToolModelPerformance(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Pool.DB().QueryContext(r.Context(),
		`SELECT task_id, model_name, call_count, success_count, outcome_success_count,
		        total_latency_ms, max_latency_ms,
		        total_input_tokens, total_output_tokens, calls_with_tokens, last_invoked_at
		 FROM proj_inference_tool_model_performance
		 ORDER BY task_id, call_count DESC, model_name`)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()

	out := make([]ToolModelStat, 0)
	for rows.Next() {
		var (
			taskID, model, lastInvoked                                                                string
			callCount, successCount, outcomeSuccessCount, totalLat, maxLat, totIn, totOut, withTokens int64
		)
		if err := rows.Scan(&taskID, &model, &callCount, &successCount, &outcomeSuccessCount, &totalLat, &maxLat,
			&totIn, &totOut, &withTokens, &lastInvoked); err != nil {
			dbErr(w, err)
			return
		}
		stat := ToolModelStat{
			TaskID:        taskID,
			ModelName:     model,
			CallCount:     callCount,
			MaxLatencyMS:  maxLat,
			LastInvokedAt: lastInvoked,
		}
		if callCount > 0 {
			stat.SuccessRate = float64(successCount) / float64(callCount)
			stat.OutcomeSuccessRate = float64(outcomeSuccessCount) / float64(callCount)
			stat.AvgLatencyMS = totalLat / callCount
		}
		if withTokens > 0 {
			avg := float64(totIn+totOut) / float64(withTokens)
			stat.AvgTokens = &avg
		}
		out = append(out, stat)
	}
	if err := rows.Err(); err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
