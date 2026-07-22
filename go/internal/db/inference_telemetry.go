package db

import (
	"context"
	"database/sql"
	"fmt"
)

// InferenceInvocation is the insert payload for one row in
// inference_invocations — the per-call inference telemetry table that
// supersedes qwen_invocations (chain per-tool-per-model-observability T11).
// It carries the two signals qwen_invocations lacked: a call-level Success
// bool and a closed-enum ErrorClass. InputTokens / OutputTokens stay
// pointers so a NULL lands when the upstream model omits usage info (older
// llama.cpp builds, some Anthropic streaming responses).
type InferenceInvocation struct {
	TaskID       string
	ModelName    string
	LatencyMS    int64
	InputTokens  *int64
	OutputTokens *int64
	// Success is the call-level outcome: true = no upstream error AND
	// non-empty output. Stored as 0/1 (CHECK-guarded in the schema).
	Success bool
	// ErrorClass is the closed enum {'', upstream_error, empty_response,
	// not_configured, timeout}; '' on success. CHECK-guarded in the schema,
	// so an out-of-set value fails the INSERT loudly rather than silently
	// widening the column.
	ErrorClass string
}

// RecordInferenceInvocation appends one row to inference_invocations.
// Failures bubble up to the caller; the router-level recorder closure
// logs-and-drops so a telemetry-write outage never blocks the inference
// response itself (same logs-and-drops shape RecordQwenInvocation uses).
func RecordInferenceInvocation(ctx context.Context, pool *Pool, inv InferenceInvocation) (int64, error) {
	successInt := 0
	if inv.Success {
		successInt = 1
	}
	var id int64
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx,
			`INSERT INTO inference_invocations
				(task_id, model_name, latency_ms, input_tokens, output_tokens, success, error_class)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 RETURNING id`,
			inv.TaskID, inv.ModelName, inv.LatencyMS, inv.InputTokens, inv.OutputTokens, successInt, inv.ErrorClass,
		)
		return row.Scan(&id)
	})
	if err != nil {
		return 0, fmt.Errorf("record inference invocation: %w", err)
	}
	return id, nil
}
