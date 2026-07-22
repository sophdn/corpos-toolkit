package measure_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toolkit/internal/inference/llamacpp"
	"toolkit/internal/inference/router"
	"toolkit/internal/measure"
	"toolkit/internal/rubric"
	"toolkit/internal/testutil"
)

// rubricsDir is relative from internal/measure/ up to the repo root.
const rubricsDir = "../../../blueprints/rubrics"

func makeDeps(t *testing.T, llamaResp map[string]any) measure.ClassifyDeps {
	t.Helper()
	pool := testutil.NewTestDB(t)
	reg, err := rubric.NewRegistry(rubricsDir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	raws := make(map[string]json.RawMessage, len(llamaResp))
	for k, v := range llamaResp {
		raws[k] = testutil.JSON(t, v)
	}
	srv := testutil.MockLlamaCPP(t, raws)
	r := router.NewWithClients(llamacpp.New(srv.URL), nil, "qwen2.5-32b")
	return measure.ClassifyDeps{
		Pool:      pool,
		Router:    r,
		Rubrics:   reg,
		Project:   "mcp-servers",
		VaultRoot: t.TempDir(), // hermetic vault — never touch ~/.claude/vault from tests
	}
}

func labelOf(t *testing.T, result any) string {
	t.Helper()
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	label, _ := m["label"].(string)
	return label
}

func rawParams(t *testing.T, kv map[string]string) json.RawMessage {
	t.Helper()
	m := make(map[string]any, len(kv))
	for k, v := range kv {
		m[k] = v
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return data
}

// --- classify_chain_task_proportionality ---

func TestHandleClassifyChainTaskProportionality_ValidInput(t *testing.T) {
	deps := makeDeps(t, map[string]any{
		"/v1/chat/completions": map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "proportionate"}}},
		},
	})
	params := rawParams(t, map[string]string{"task_spec": "Port the rubric loader to Go."})
	result, err := measure.HandleClassifyChainTaskProportionality(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := labelOf(t, result); got != "proportionate" {
		t.Errorf("label: want proportionate, got %q", got)
	}
}

func TestHandleClassifyChainTaskProportionality_MissingFieldReturnsError(t *testing.T) {
	deps := makeDeps(t, nil)
	result, err := measure.HandleClassifyChainTaskProportionality(context.Background(), deps, rawParams(t, map[string]string{}))
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	data, _ := json.Marshal(result)
	var m map[string]string
	_ = json.Unmarshal(data, &m)
	if m["error"] == "" {
		t.Error("expected error field in result for missing task_spec")
	}
}

func TestHandleClassifyChainTaskProportionality_InferenceFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"broken"}`))
	}))
	t.Cleanup(srv.Close)

	pool := testutil.NewTestDB(t)
	reg, err := rubric.NewRegistry(rubricsDir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	r := router.NewWithClients(llamacpp.New(srv.URL), nil, "qwen2.5-32b")
	deps := measure.ClassifyDeps{Pool: pool, Router: r, Rubrics: reg, Project: "mcp-servers", VaultRoot: t.TempDir()}

	params := rawParams(t, map[string]string{"task_spec": "some task"})
	_, inferErr := measure.HandleClassifyChainTaskProportionality(context.Background(), deps, params)
	err = inferErr
	if err == nil {
		t.Error("expected error on inference failure, got nil")
	}
}

// --- classify_retirement_observation ---

func TestHandleClassifyRetirementObservation_ValidInput(t *testing.T) {
	deps := makeDeps(t, map[string]any{
		"/v1/chat/completions": map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "tool-retirement"}}},
		},
	})
	params := rawParams(t, map[string]string{"observation_text": "Zero invocations of mcp__signal_table_export in 200 sessions."})
	result, err := measure.HandleClassifyRetirementObservation(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := labelOf(t, result); got != "tool-retirement" {
		t.Errorf("label: want tool-retirement, got %q", got)
	}
}

func TestHandleClassifyRetirementObservation_MissingField(t *testing.T) {
	deps := makeDeps(t, nil)
	result, err := measure.HandleClassifyRetirementObservation(context.Background(), deps, rawParams(t, map[string]string{}))
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	data, _ := json.Marshal(result)
	var m map[string]string
	_ = json.Unmarshal(data, &m)
	if m["error"] == "" {
		t.Error("expected error field for missing observation_text")
	}
}

// --- classify_artifact_tier ---

func TestHandleClassifyArtifactTier_ValidInput(t *testing.T) {
	deps := makeDeps(t, map[string]any{
		"/v1/chat/completions": map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "tier-zero"}}},
		},
	})
	params := rawParams(t, map[string]string{"artifact_descriptor": "CLAUDE.md at the project root."})
	result, err := measure.HandleClassifyArtifactTier(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := labelOf(t, result); got != "tier-zero" {
		t.Errorf("label: want tier-zero, got %q", got)
	}
}

func TestHandleClassifyArtifactTier_MissingField(t *testing.T) {
	deps := makeDeps(t, nil)
	result, err := measure.HandleClassifyArtifactTier(context.Background(), deps, rawParams(t, map[string]string{}))
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	data, _ := json.Marshal(result)
	var m map[string]string
	_ = json.Unmarshal(data, &m)
	if m["error"] == "" {
		t.Error("expected error field for missing artifact_descriptor")
	}
}

// --- classify_audit_finding_severity ---

func TestHandleClassifyAuditFindingSeverity_ValidInput(t *testing.T) {
	deps := makeDeps(t, map[string]any{
		"/v1/chat/completions": map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "critical"}}},
		},
	})
	params := rawParams(t, map[string]string{"finding_prose": "Dispatch silently drops unknown keys."})
	result, err := measure.HandleClassifyAuditFindingSeverity(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := labelOf(t, result); got != "critical" {
		t.Errorf("label: want critical, got %q", got)
	}
}

func TestHandleClassifyAuditFindingSeverity_MissingField(t *testing.T) {
	deps := makeDeps(t, nil)
	result, err := measure.HandleClassifyAuditFindingSeverity(context.Background(), deps, rawParams(t, map[string]string{}))
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	data, _ := json.Marshal(result)
	var m map[string]string
	_ = json.Unmarshal(data, &m)
	if m["error"] == "" {
		t.Error("expected error field for missing finding_prose")
	}
}

// --- classify_artifact_review_criterion ---

func TestHandleClassifyArtifactReviewCriterion_ValidInput(t *testing.T) {
	deps := makeDeps(t, map[string]any{
		"/v1/chat/completions": map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "fail"}}},
		},
	})
	params := rawParams(t, map[string]string{
		"artifact_excerpt": "DB_PASSWORD=\"hunter2\"",
		"purpose":          "safety",
		"criterion":        "No hardcoded secrets",
	})
	result, err := measure.HandleClassifyArtifactReviewCriterion(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := labelOf(t, result); got != "fail" {
		t.Errorf("label: want fail, got %q", got)
	}
}

func TestHandleClassifyArtifactReviewCriterion_MissingField(t *testing.T) {
	deps := makeDeps(t, nil)
	tests := []struct {
		name   string
		params map[string]string
		want   string
	}{
		{"no excerpt", map[string]string{"purpose": "safety", "criterion": "c"}, "params.artifact_excerpt is required"},
		{"no purpose", map[string]string{"artifact_excerpt": "x", "criterion": "c"}, "params.purpose is required"},
		{"no criterion", map[string]string{"artifact_excerpt": "x", "purpose": "safety"}, "params.criterion is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := measure.HandleClassifyArtifactReviewCriterion(context.Background(), deps, rawParams(t, tc.params))
			if err != nil {
				t.Fatalf("unexpected hard error: %v", err)
			}
			data, _ := json.Marshal(result)
			var m map[string]string
			_ = json.Unmarshal(data, &m)
			if m["error"] == "" {
				t.Errorf("expected error field, got none — result: %s", data)
			}
		})
	}
}

// --- Fix 1: chain-assessment includes team_context_prose in the response ---

func TestHandleClassifyChainTaskProportionality_IncludesTeamContextProse(t *testing.T) {
	deps := makeDeps(t, map[string]any{
		"/v1/chat/completions": map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "proportionate"}}},
		},
	})
	params := rawParams(t, map[string]string{"task_spec": "Port the rubric registry to Go."})
	result, err := measure.HandleClassifyChainTaskProportionality(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	prose := result.TeamContextProse
	if prose == "" {
		t.Fatalf("expected non-empty team_context_prose, got %+v", result)
	}
	if !strings.Contains(prose, "team_bandwidth:") || !strings.Contains(prose, "prior_signal_strength:") {
		t.Errorf("team_context_prose must carry both bandwidth + prior_signal lines: %q", prose)
	}
}

func TestHandleClassifyChainTaskProportionality_HonorsTeamContextOverride(t *testing.T) {
	deps := makeDeps(t, map[string]any{
		"/v1/chat/completions": map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "disproportionate"}}},
		},
	})
	override := "team_bandwidth: thin — solo dev.\nprior_signal_strength: strong — settled stack."
	params := rawParams(t, map[string]string{
		"task_spec":             "Spike a 2-week framework comparison.",
		"team_context_override": override,
	})
	result, err := measure.HandleClassifyChainTaskProportionality(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.TeamContextProse != override {
		t.Errorf("override not echoed back: want %q, got %q", override, result.TeamContextProse)
	}
}

// --- Fix 4: NoMatch / Multiple labels return errors ---

func TestClassify_NoMatchReturnsError(t *testing.T) {
	deps := makeDeps(t, map[string]any{
		"/v1/chat/completions": map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "xenon"}}},
		},
	})
	params := rawParams(t, map[string]string{"observation_text": "x"})
	_, err := measure.HandleClassifyRetirementObservation(context.Background(), deps, params)
	if err == nil {
		t.Fatal("expected error when model returns a label not in the allowed set")
	}
	if !strings.Contains(err.Error(), "did not match") {
		t.Errorf("error should mention no-match: %v", err)
	}
	// Rust parity: NoMatch carries the raw response so logs / dashboard show
	// what the model actually said.
	if !strings.Contains(err.Error(), "xenon") {
		t.Errorf("error should include raw response: %v", err)
	}
}

func TestClassify_MultipleLabelsReturnsError(t *testing.T) {
	// Mock returns two valid labels on separate lines under SingleClass mode.
	deps := makeDeps(t, map[string]any{
		"/v1/chat/completions": map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "low\nhigh"}}},
		},
	})
	params := rawParams(t, map[string]string{"finding_prose": "x"})
	_, err := measure.HandleClassifyAuditFindingSeverity(context.Background(), deps, params)
	if err == nil {
		t.Fatal("expected error when model returns multiple labels under SingleClass")
	}
	if !strings.Contains(err.Error(), "multiple labels") {
		t.Errorf("error should mention multiple labels: %v", err)
	}
}

// --- Token-count propagation (bug: go-classify-benchmark-results-rows-have-null-input-tokens-output-tokens) ---

func TestClassify_TokenCountsLandInBenchmarkRow(t *testing.T) {
	deps := makeDeps(t, map[string]any{
		"/v1/chat/completions": map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "tool-retirement"}}},
			"usage":   map[string]any{"prompt_tokens": 537, "completion_tokens": 6},
		},
	})
	params := rawParams(t, map[string]string{"observation_text": "zero invocations across 200 sessions"})
	_, err := measure.HandleClassifyRetirementObservation(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("classify failed: %v", err)
	}

	var inTok, outTok sql.NullInt64
	row := deps.Pool.DB().QueryRow(
		"SELECT input_tokens, output_tokens FROM proj_benchmark_results WHERE task_id = 'retirement-signal' ORDER BY run_at DESC LIMIT 1")
	if err := row.Scan(&inTok, &outTok); err != nil {
		t.Fatalf("query benchmark_results: %v", err)
	}
	if !inTok.Valid || inTok.Int64 != 537 {
		t.Errorf("input_tokens: want 537 (non-NULL), got valid=%v val=%d", inTok.Valid, inTok.Int64)
	}
	if !outTok.Valid || outTok.Int64 != 6 {
		t.Errorf("output_tokens: want 6 (non-NULL), got valid=%v val=%d", outTok.Valid, outTok.Int64)
	}
}

func TestClassify_TokenCountsNULLWhenUsageAbsent(t *testing.T) {
	// Older llama.cpp builds omit `usage`. Behavior must be: row written
	// successfully, token columns NULL — not a crash.
	deps := makeDeps(t, map[string]any{
		"/v1/chat/completions": map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "tool-retirement"}}},
			// no "usage" field
		},
	})
	params := rawParams(t, map[string]string{"observation_text": "x"})
	_, err := measure.HandleClassifyRetirementObservation(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("classify failed: %v", err)
	}

	var inTok, outTok sql.NullInt64
	row := deps.Pool.DB().QueryRow(
		"SELECT input_tokens, output_tokens FROM proj_benchmark_results WHERE task_id = 'retirement-signal' ORDER BY run_at DESC LIMIT 1")
	if err := row.Scan(&inTok, &outTok); err != nil {
		t.Fatalf("query benchmark_results: %v", err)
	}
	if inTok.Valid || outTok.Valid {
		t.Errorf("expected NULL token columns when usage absent; got in=(valid=%v,val=%d) out=(valid=%v,val=%d)",
			inTok.Valid, inTok.Int64, outTok.Valid, outTok.Int64)
	}
}

// --- Fix 3: session-routing writes structured notes ---

func TestHandleClassifySessionRoutingTrigger_WritesStructuredNotes(t *testing.T) {
	deps := makeDeps(t, map[string]any{
		"/v1/chat/completions": map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "chain-execution"}}},
		},
	})
	params := rawParams(t, map[string]string{"user_input": "continue the agent-os-go-migration chain"})
	_, err := measure.HandleClassifySessionRoutingTrigger(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Inspect the row that was written.
	var notes string
	row := deps.Pool.DB().QueryRow(
		"SELECT notes FROM proj_benchmark_results WHERE task_id = 'session-routing' ORDER BY run_at DESC LIMIT 1")
	if err := row.Scan(&notes); err != nil {
		t.Fatalf("query benchmark_results: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(notes), &payload); err != nil {
		t.Fatalf("notes is not JSON: %v (raw: %s)", err, notes)
	}
	// fallback_used is gone with the role system (2026-07-22): the remote
	// escalation existed only to rescue role invocations Qwen missed. Assert
	// its ABSENCE so the field can't quietly return.
	if _, present := payload["fallback_used"]; present {
		t.Errorf("fallback_used should no longer be emitted, got %v", payload["fallback_used"])
	}
	if payload["final_label"] != "chain-execution" {
		t.Errorf("final_label: want chain-execution, got %v", payload["final_label"])
	}
	if payload["raw"] != "chain-execution" {
		t.Errorf("raw: want raw qwen response, got %v", payload["raw"])
	}
}

// --- classify_bug_severity ---

func TestHandleClassifyBugSeverity_ValidInputReturnsLabel(t *testing.T) {
	deps := makeDeps(t, map[string]any{
		"/v1/chat/completions": map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "high"}}},
		},
	})
	params := rawParams(t, map[string]string{
		"bug_report": "Every Claude session opens with zero MCP tools registered.",
	})
	result, err := measure.HandleClassifyBugSeverity(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := labelOf(t, result); got != "high" {
		t.Errorf("label: want high, got %q", got)
	}
}

func TestHandleClassifyBugSeverity_MissingFieldReturnsErrorEnvelope(t *testing.T) {
	deps := makeDeps(t, nil)
	result, err := measure.HandleClassifyBugSeverity(context.Background(), deps, rawParams(t, map[string]string{}))
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	data, _ := json.Marshal(result)
	var m map[string]string
	_ = json.Unmarshal(data, &m)
	if m["error"] == "" {
		t.Error("expected error field for missing bug_report")
	}
}

func TestHandleClassifyBugSeverity_UnclearEnumValueResolves(t *testing.T) {
	// "unclear" is in bug-severity's output_enum, so Qwen returning it
	// must resolve as the label directly (not via the unclassifiable
	// fallback). Pins the contract that bug-severity's escape hatch is
	// a real enum value the caller can branch on.
	deps := makeDeps(t, map[string]any{
		"/v1/chat/completions": map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "unclear"}}},
		},
	})
	params := rawParams(t, map[string]string{
		"bug_report": "Vague report with no scope or surface info.",
	})
	result, err := measure.HandleClassifyBugSeverity(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := labelOf(t, result); got != "unclear" {
		t.Errorf("label: want unclear, got %q", got)
	}
}
