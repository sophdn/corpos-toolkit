package db_test

import (
	"context"
	"strings"
	"testing"

	"toolkit/internal/db"
)

func TestRecordInferenceInvocation_RoundTrip(t *testing.T) {
	pool := freshPool(t)

	input := int64(120)
	output := int64(8)
	id, err := db.RecordInferenceInvocation(context.Background(), pool, db.InferenceInvocation{
		TaskID:       "vault-rerank-retrieve",
		ModelName:    "qwen2.5-32b",
		LatencyMS:    234,
		InputTokens:  &input,
		OutputTokens: &output,
		Success:      true,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero row id")
	}

	var taskID, model, errorClass string
	var latency, success int64
	var in, out *int64
	if err := pool.DB().QueryRow(
		`SELECT task_id, model_name, latency_ms, input_tokens, output_tokens, success, error_class
		 FROM inference_invocations WHERE id = ?`, id,
	).Scan(&taskID, &model, &latency, &in, &out, &success, &errorClass); err != nil {
		t.Fatalf("readback: %v", err)
	}
	if taskID != "vault-rerank-retrieve" || model != "qwen2.5-32b" || latency != 234 {
		t.Errorf("row mismatch: task=%q model=%q latency=%d", taskID, model, latency)
	}
	if in == nil || *in != 120 || out == nil || *out != 8 {
		t.Errorf("tokens mismatch: in=%v out=%v", in, out)
	}
	if success != 1 {
		t.Errorf("success: want 1, got %d", success)
	}
	if errorClass != "" {
		t.Errorf("error_class: want empty on success, got %q", errorClass)
	}
}

func TestRecordInferenceInvocation_FailureRowWithErrorClass(t *testing.T) {
	// A remote-model failure: no tokens, success=false, a closed-enum
	// error_class. This is the coverage qwen_invocations never had.
	pool := freshPool(t)

	id, err := db.RecordInferenceInvocation(context.Background(), pool, db.InferenceInvocation{
		TaskID:     "classify_rubric",
		ModelName:  "claude-sonnet-4-6",
		LatencyMS:  91,
		Success:    false,
		ErrorClass: "upstream_error",
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	var success int64
	var errorClass, model string
	var in, out *int64
	if err := pool.DB().QueryRow(
		`SELECT success, error_class, model_name, input_tokens, output_tokens
		 FROM inference_invocations WHERE id = ?`, id,
	).Scan(&success, &errorClass, &model, &in, &out); err != nil {
		t.Fatalf("readback: %v", err)
	}
	if success != 0 || errorClass != "upstream_error" {
		t.Errorf("failure row: success=%d error_class=%q", success, errorClass)
	}
	if model != "claude-sonnet-4-6" {
		t.Errorf("remote model_name not recorded: %q", model)
	}
	if in != nil || out != nil {
		t.Errorf("expected NULL tokens on failure, got in=%v out=%v", in, out)
	}
}

func TestRecordInferenceInvocation_NullableTokens(t *testing.T) {
	pool := freshPool(t)
	id, err := db.RecordInferenceInvocation(context.Background(), pool, db.InferenceInvocation{
		TaskID:    "unattributed",
		ModelName: "qwen2.5-32b",
		LatencyMS: 50,
		Success:   true,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	var in, out *int64
	if err := pool.DB().QueryRow(
		`SELECT input_tokens, output_tokens FROM inference_invocations WHERE id = ?`, id,
	).Scan(&in, &out); err != nil {
		t.Fatalf("readback: %v", err)
	}
	if in != nil || out != nil {
		t.Errorf("expected NULL tokens, got in=%v out=%v", in, out)
	}
}

// The error_class CHECK is the closed-enum guard: an out-of-set value must
// fail the INSERT loudly rather than silently widening the column.
func TestRecordInferenceInvocation_RejectsUnknownErrorClass(t *testing.T) {
	pool := freshPool(t)
	_, err := db.RecordInferenceInvocation(context.Background(), pool, db.InferenceInvocation{
		TaskID:     "classify_rubric",
		ModelName:  "qwen2.5-32b",
		LatencyMS:  10,
		Success:    false,
		ErrorClass: "wat_is_this",
	})
	if err == nil {
		t.Fatal("expected CHECK violation for an out-of-enum error_class, got nil")
	}
	if !strings.Contains(err.Error(), "CHECK") && !strings.Contains(err.Error(), "constraint") {
		t.Errorf("expected a constraint error, got: %v", err)
	}
}
