package router_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"toolkit/internal/inference/anthropic"
	"toolkit/internal/inference/llamacpp"
	"toolkit/internal/inference/router"
	"toolkit/internal/qwenctx"
	"toolkit/internal/testutil"
)

func goodLlamaResponse() map[string]any {
	return map[string]any{
		"choices": []any{
			map[string]any{
				"message": map[string]any{"role": "assistant", "content": "low"},
			},
		},
		"usage": map[string]any{"prompt_tokens": 412, "completion_tokens": 4},
	}
}

// llamaResponseWithoutUsage exercises the older-llama.cpp path where the
// `usage` field is absent — Generate must still return successfully with
// nil token-count pointers.
func llamaResponseWithoutUsage() map[string]any {
	return map[string]any{
		"choices": []any{
			map[string]any{
				"message": map[string]any{"role": "assistant", "content": "low"},
			},
		},
	}
}

func goodAnthropicResponse() map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": "high"}},
		"usage":   map[string]any{"input_tokens": 10, "output_tokens": 2},
	}
}

func localRouter(t *testing.T, llamaResp map[string]json.RawMessage) *router.Router {
	t.Helper()
	srv := testutil.MockLlamaCPP(t, llamaResp)
	return router.NewWithClients(llamacpp.New(srv.URL), nil, "qwen2.5-32b")
}

func remoteRouter(t *testing.T, llamaResp, anthropicResp map[string]json.RawMessage) (*router.Router, error) {
	t.Helper()
	llamaSrv := testutil.MockLlamaCPP(t, llamaResp)
	anthropicSrv := testutil.MockAnthropic(t, anthropicResp)

	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	anClient, err := anthropic.NewWithBaseURL(anthropicSrv.URL)
	if err != nil {
		return nil, err
	}
	return router.NewWithClients(llamacpp.New(llamaSrv.URL), anClient, "qwen2.5-32b"), nil
}

func TestGenerate_LocalSuccess(t *testing.T) {
	r := localRouter(t, map[string]json.RawMessage{"/v1/chat/completions": testutil.JSON(t, goodLlamaResponse())})
	out, err := r.Generate(context.Background(), "classify this", "you classify")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if out.Text != "low" {
		t.Errorf("text: want %q, got %q", "low", out.Text)
	}
	if out.InputTokens == nil || *out.InputTokens != 412 {
		t.Errorf("InputTokens: want *412, got %v", out.InputTokens)
	}
	if out.OutputTokens == nil || *out.OutputTokens != 4 {
		t.Errorf("OutputTokens: want *4, got %v", out.OutputTokens)
	}
}

func TestGenerate_OmitsTokensWhenUsageAbsent(t *testing.T) {
	r := localRouter(t, map[string]json.RawMessage{"/v1/chat/completions": testutil.JSON(t, llamaResponseWithoutUsage())})
	out, err := r.Generate(context.Background(), "x", "y")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if out.Text != "low" {
		t.Errorf("text: want %q, got %q", "low", out.Text)
	}
	if out.InputTokens != nil || out.OutputTokens != nil {
		t.Errorf("expected nil token pointers when usage field absent; got in=%v out=%v",
			out.InputTokens, out.OutputTokens)
	}
}

func TestGenerate_LocalFailureNoFallback(t *testing.T) {
	// Mock server returns 500 — Generate must return an error without
	// attempting the remote path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"broken"}`))
	}))
	t.Cleanup(srv.Close)

	r := router.NewWithClients(llamacpp.New(srv.URL), nil, "qwen2.5-32b")
	_, err := r.Generate(context.Background(), "input", "system")
	if err == nil {
		t.Error("expected error when local server returns 500, got nil")
	}
}

func TestGenerateRemote_Success(t *testing.T) {
	r, err := remoteRouter(t,
		map[string]json.RawMessage{"/v1/chat/completions": testutil.JSON(t, goodLlamaResponse())},
		map[string]json.RawMessage{"/v1/messages": testutil.JSON(t, goodAnthropicResponse())},
	)
	if err != nil {
		t.Fatalf("remoteRouter: %v", err)
	}

	out, err := r.GenerateRemote(context.Background(), "classify this", "you classify")
	if err != nil {
		t.Fatalf("GenerateRemote: %v", err)
	}
	if out.Text != "high" {
		t.Errorf("text: want %q, got %q", "high", out.Text)
	}
	if out.InputTokens == nil || *out.InputTokens != 10 {
		t.Errorf("InputTokens: want *10, got %v", out.InputTokens)
	}
	if out.OutputTokens == nil || *out.OutputTokens != 2 {
		t.Errorf("OutputTokens: want *2, got %v", out.OutputTokens)
	}
}

func TestGenerateRemote_NilClientReturnsError(t *testing.T) {
	// Router constructed without Anthropic client.
	srv := testutil.MockLlamaCPP(t, map[string]json.RawMessage{"/v1/chat/completions": testutil.JSON(t, goodLlamaResponse())})
	r := router.NewWithClients(llamacpp.New(srv.URL), nil, "qwen2.5-32b")

	_, err := r.GenerateRemote(context.Background(), "input", "system")
	if err == nil {
		t.Error("expected error when Anthropic client is nil, got nil")
	}
}

// Bug 1328 regression: GenerateWithOpts must hand the recorder a row
// carrying the qwenctx-stamped task_id, the model name, and the
// observed latency on the success path. The recorder runs even when
// tokens are nil (older llama.cpp builds).
func TestGenerate_InvokesInvocationRecorderOnSuccess(t *testing.T) {
	r := localRouter(t, map[string]json.RawMessage{"/v1/chat/completions": testutil.JSON(t, goodLlamaResponse())})

	var recorded []router.InvocationRecord
	r.SetInvocationRecorder(func(ctx context.Context, rec router.InvocationRecord) {
		recorded = append(recorded, rec)
	})

	ctx := qwenctx.WithTaskID(context.Background(), "vault-rerank-retrieve")
	if _, err := r.Generate(ctx, "input", "system"); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if len(recorded) != 1 {
		t.Fatalf("recorder called %d times, want 1", len(recorded))
	}
	got := recorded[0]
	if got.TaskID != "vault-rerank-retrieve" {
		t.Errorf("TaskID: want %q, got %q", "vault-rerank-retrieve", got.TaskID)
	}
	if got.ModelName != "qwen2.5-32b" {
		t.Errorf("ModelName: want %q, got %q", "qwen2.5-32b", got.ModelName)
	}
	if got.InputTokens == nil || *got.InputTokens != 412 {
		t.Errorf("InputTokens: want *412, got %v", got.InputTokens)
	}
}

// Telemetry must persist even when the local model fails, otherwise the
// dashboard "Calls" column under-counts broken invocations. The recorder
// runs in both the empty-choices and HTTP-error branches.
func TestGenerate_InvokesInvocationRecorderOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"broken"}`))
	}))
	t.Cleanup(srv.Close)
	r := router.NewWithClients(llamacpp.New(srv.URL), nil, "qwen2.5-32b")

	var recorded []router.InvocationRecord
	r.SetInvocationRecorder(func(ctx context.Context, rec router.InvocationRecord) {
		recorded = append(recorded, rec)
	})

	ctx := qwenctx.WithTaskID(context.Background(), "classify-pre-commit-failure")
	if _, err := r.Generate(ctx, "input", "system"); err == nil {
		t.Fatal("expected error on 500")
	}
	if len(recorded) != 1 {
		t.Fatalf("recorder called %d times, want 1 even on failure", len(recorded))
	}
	if recorded[0].TaskID != "classify-pre-commit-failure" {
		t.Errorf("TaskID: want %q, got %q", "classify-pre-commit-failure", recorded[0].TaskID)
	}
}

// Unstamped callers must still produce a row so the universal counter
// stays accurate; the row records under qwenctx.Unattributed.
func TestGenerate_UnstampedCtxRecordsAsUnattributed(t *testing.T) {
	r := localRouter(t, map[string]json.RawMessage{"/v1/chat/completions": testutil.JSON(t, goodLlamaResponse())})

	var recorded []router.InvocationRecord
	r.SetInvocationRecorder(func(ctx context.Context, rec router.InvocationRecord) {
		recorded = append(recorded, rec)
	})
	if _, err := r.Generate(context.Background(), "x", "y"); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(recorded) != 1 || recorded[0].TaskID != qwenctx.Unattributed {
		t.Fatalf("expected one unattributed record; got %+v", recorded)
	}
}

func TestModelName(t *testing.T) {
	srv := testutil.MockLlamaCPP(t, nil)
	r := router.NewWithClients(llamacpp.New(srv.URL), nil, "qwen2.5-32b")
	if r.ModelName() != "qwen2.5-32b" {
		t.Errorf("ModelName: want qwen2.5-32b, got %q", r.ModelName())
	}
}

// TestStaticRouting_ColdStartModelSelectionIsStaticDefault is the Chain-3
// (data-driven-model-routing) characterization-net oracle: TODAY the router's
// model selection is STATIC and DATA-BLIND — the production constructor New()
// yields the constant local default "qwen2.5-32b" with no performance data
// consulted, and the identifier does not vary across calls. The chain adds a
// data-backed SelectModel(task) with a 60s cache; its COLD-START branch (no
// rows in proj_inference_tool_model_performance) must reproduce exactly this
// default, so this test must stay green after the refactor (the data-PRESENT
// branch is new behavior with its own tests in step 7). TestModelName only
// pins an INJECTED name via NewWithClients; this pins the real New() default.
func TestStaticRouting_ColdStartModelSelectionIsStaticDefault(t *testing.T) {
	// New("") builds real clients without any HTTP call or DB/telemetry
	// source — there is no seam through which performance data could enter
	// the selection today. ANTHROPIC_API_KEY absent → remote stays nil.
	t.Setenv("ANTHROPIC_API_KEY", "")
	r, err := router.New("")
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	if got := r.ModelName(); got != "qwen2.5-32b" {
		t.Errorf("cold-start default model = %q, want qwen2.5-32b (the static routing baseline)", got)
	}
	// Stable across calls — the choice is a constant, not a per-call computation.
	if r.ModelName() != r.ModelName() {
		t.Error("ModelName must be deterministic (data-blind static selection)")
	}
}

// captureRecorder wires a recorder that appends to a slice and returns it.
func captureRecorder(r *router.Router) *[]router.InvocationRecord {
	rec := &[]router.InvocationRecord{}
	r.SetInvocationRecorder(func(_ context.Context, in router.InvocationRecord) {
		*rec = append(*rec, in)
	})
	return rec
}

// Chain per-tool-per-model-observability T11: the local success path
// stamps Success=true / ErrorClass="" on the emitted record.
func TestGenerate_RecordsCallLevelSuccess(t *testing.T) {
	r := localRouter(t, map[string]json.RawMessage{"/v1/chat/completions": testutil.JSON(t, goodLlamaResponse())})
	recorded := captureRecorder(r)
	if _, err := r.Generate(context.Background(), "x", "y"); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(*recorded) != 1 {
		t.Fatalf("want 1 record, got %d", len(*recorded))
	}
	got := (*recorded)[0]
	if !got.Success || got.ErrorClass != router.ErrorClassNone {
		t.Errorf("success path: Success=%v ErrorClass=%q, want true/\"\"", got.Success, got.ErrorClass)
	}
}

// The local HTTP-error branch stamps Success=false / ErrorClass=upstream_error.
func TestGenerate_RecordsUpstreamErrorClass(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"broken"}`))
	}))
	t.Cleanup(srv.Close)
	r := router.NewWithClients(llamacpp.New(srv.URL), nil, "qwen2.5-32b")
	recorded := captureRecorder(r)
	if _, err := r.Generate(context.Background(), "x", "y"); err == nil {
		t.Fatal("expected error on 500")
	}
	if len(*recorded) != 1 {
		t.Fatalf("want 1 record, got %d", len(*recorded))
	}
	got := (*recorded)[0]
	if got.Success || got.ErrorClass != router.ErrorClassUpstream {
		t.Errorf("error path: Success=%v ErrorClass=%q, want false/upstream_error", got.Success, got.ErrorClass)
	}
}

// The remote-coverage gap this chain closes: GenerateRemote must now emit a
// record on success, stamped with the remote model name and call-level
// success — qwen_invocations never saw remote Claude calls.
func TestGenerateRemote_RecordsRemoteModelOnSuccess(t *testing.T) {
	r, err := remoteRouter(t,
		map[string]json.RawMessage{"/v1/chat/completions": testutil.JSON(t, goodLlamaResponse())},
		map[string]json.RawMessage{"/v1/messages": testutil.JSON(t, goodAnthropicResponse())},
	)
	if err != nil {
		t.Fatalf("remoteRouter: %v", err)
	}
	recorded := captureRecorder(r)
	ctx := qwenctx.WithTaskID(context.Background(), "classify_rubric")
	if _, err := r.GenerateRemote(ctx, "classify this", "you classify"); err != nil {
		t.Fatalf("GenerateRemote: %v", err)
	}
	if len(*recorded) != 1 {
		t.Fatalf("want 1 remote record, got %d", len(*recorded))
	}
	got := (*recorded)[0]
	if got.ModelName != "claude-sonnet-4-6" {
		t.Errorf("remote ModelName: want claude-sonnet-4-6, got %q", got.ModelName)
	}
	if got.TaskID != "classify_rubric" {
		t.Errorf("TaskID: want classify_rubric, got %q", got.TaskID)
	}
	if !got.Success || got.ErrorClass != router.ErrorClassNone {
		t.Errorf("remote success: Success=%v ErrorClass=%q, want true/\"\"", got.Success, got.ErrorClass)
	}
	if got.InputTokens == nil || *got.InputTokens != 10 || got.OutputTokens == nil || *got.OutputTokens != 2 {
		t.Errorf("remote tokens: in=%v out=%v, want *10/*2", got.InputTokens, got.OutputTokens)
	}
}

// A remote upstream failure emits a failure record (not_configured is
// reserved for the nil-client path; an HTTP error is upstream_error).
func TestGenerateRemote_RecordsUpstreamErrorOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"type":"overloaded"}}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	anClient, err := anthropic.NewWithBaseURL(srv.URL)
	if err != nil {
		t.Fatalf("anthropic client: %v", err)
	}
	r := router.NewWithClients(llamacpp.New("http://unused"), anClient, "qwen2.5-32b")
	recorded := captureRecorder(r)
	if _, err := r.GenerateRemote(context.Background(), "x", "y"); err == nil {
		t.Fatal("expected error on remote 500")
	}
	if len(*recorded) != 1 {
		t.Fatalf("want 1 record, got %d", len(*recorded))
	}
	got := (*recorded)[0]
	if got.Success || got.ErrorClass != router.ErrorClassUpstream || got.ModelName != "claude-sonnet-4-6" {
		t.Errorf("remote failure: Success=%v ErrorClass=%q Model=%q, want false/upstream_error/claude-sonnet-4-6",
			got.Success, got.ErrorClass, got.ModelName)
	}
}

// A remote 200 with an empty content array records empty_response — the
// new branch the remote-coverage change introduced (densification, T13).
func TestGenerateRemote_RecordsEmptyResponseOnEmptyContent(t *testing.T) {
	r, err := remoteRouter(t,
		map[string]json.RawMessage{"/v1/chat/completions": testutil.JSON(t, goodLlamaResponse())},
		map[string]json.RawMessage{"/v1/messages": testutil.JSON(t, map[string]any{"content": []any{}})},
	)
	if err != nil {
		t.Fatalf("remoteRouter: %v", err)
	}
	recorded := captureRecorder(r)
	if _, err := r.GenerateRemote(context.Background(), "x", "y"); err == nil {
		t.Fatal("expected error on empty remote content")
	}
	if len(*recorded) != 1 {
		t.Fatalf("want 1 record, got %d", len(*recorded))
	}
	got := (*recorded)[0]
	if got.Success || got.ErrorClass != router.ErrorClassEmptyResponse {
		t.Errorf("empty content: Success=%v ErrorClass=%q, want false/empty_response", got.Success, got.ErrorClass)
	}
}

// A local 200 whose single choice has an empty content string is a
// successful HTTP call with no usable output → call-level success=false,
// error_class=empty_response (densification, T13). Generate returns no error
// (the choice exists), so this is distinct from the HTTP-error branch.
func TestGenerate_RecordsEmptyResponseOnEmptyText(t *testing.T) {
	resp := map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": ""}}},
	}
	r := localRouter(t, map[string]json.RawMessage{"/v1/chat/completions": testutil.JSON(t, resp)})
	recorded := captureRecorder(r)
	out, err := r.Generate(context.Background(), "x", "y")
	if err != nil {
		t.Fatalf("Generate: %v (empty text is not an error path)", err)
	}
	if out.Text != "" {
		t.Errorf("text = %q, want empty", out.Text)
	}
	if len(*recorded) != 1 {
		t.Fatalf("want 1 record, got %d", len(*recorded))
	}
	got := (*recorded)[0]
	if got.Success || got.ErrorClass != router.ErrorClassEmptyResponse {
		t.Errorf("empty text: Success=%v ErrorClass=%q, want false/empty_response", got.Success, got.ErrorClass)
	}
}

// The nil-client path now records a not_configured attempt instead of
// vanishing silently.
func TestGenerateRemote_RecordsNotConfiguredWhenNilClient(t *testing.T) {
	srv := testutil.MockLlamaCPP(t, map[string]json.RawMessage{"/v1/chat/completions": testutil.JSON(t, goodLlamaResponse())})
	r := router.NewWithClients(llamacpp.New(srv.URL), nil, "qwen2.5-32b")
	recorded := captureRecorder(r)
	if _, err := r.GenerateRemote(context.Background(), "x", "y"); err == nil {
		t.Fatal("expected error when Anthropic client is nil")
	}
	if len(*recorded) != 1 {
		t.Fatalf("want 1 record on the nil-client path, got %d", len(*recorded))
	}
	got := (*recorded)[0]
	if got.Success || got.ErrorClass != router.ErrorClassNotConfigured {
		t.Errorf("nil-client: Success=%v ErrorClass=%q, want false/not_configured", got.Success, got.ErrorClass)
	}
}

// ── Chain 3 (data-driven-model-routing) selector seam ─────────────────────
// The mock servers return "low" for local (llama) and "high" for remote
// (anthropic), so the response text discriminates which path a Generate call
// took. These pin the seam db-free (no projection needed — the selector is
// injected directly).

// Nil selector ⇒ local dispatch — the cold-start parity guarantee at the
// dispatch level. (TestStaticRouting_ColdStartModelSelectionIsStaticDefault
// pins ModelName(); this pins that Generate actually routes local.)
func TestRouting_NilSelectorDispatchesLocal(t *testing.T) {
	r := localRouter(t, map[string]json.RawMessage{"/v1/chat/completions": testutil.JSON(t, goodLlamaResponse())})
	recorded := captureRecorder(r)
	out, err := r.Generate(context.Background(), "x", "y")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if out.Text != "low" {
		t.Errorf("text = %q, want \"low\" (local path) — nil selector must route local", out.Text)
	}
	if len(*recorded) != 1 || (*recorded)[0].ModelName != "qwen2.5-32b" {
		t.Errorf("recorded model = %+v, want local qwen2.5-32b", *recorded)
	}
}

// A selector naming the remote model (with a remote client configured) routes
// Generate to the remote path — the data-driven switch.
func TestRouting_SelectorRoutesRemote(t *testing.T) {
	r, err := remoteRouter(t,
		map[string]json.RawMessage{"/v1/chat/completions": testutil.JSON(t, goodLlamaResponse())},
		map[string]json.RawMessage{"/v1/messages": testutil.JSON(t, goodAnthropicResponse())},
	)
	if err != nil {
		t.Fatalf("remoteRouter: %v", err)
	}
	r.SetModelSelector(func(context.Context) (string, bool) { return "claude-sonnet-4-6", true })
	recorded := captureRecorder(r)
	out, err := r.Generate(context.Background(), "x", "y")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if out.Text != "high" {
		t.Errorf("text = %q, want \"high\" (remote path) — selector must route remote", out.Text)
	}
	if len(*recorded) != 1 || (*recorded)[0].ModelName != "claude-sonnet-4-6" {
		t.Errorf("recorded model = %+v, want remote claude-sonnet-4-6", *recorded)
	}
}

// A selector naming remote but with NO remote client falls back to local (the
// `r.remote != nil` guard) — never crashes, never misroutes to a nil client.
func TestRouting_SelectorRemoteButNilClientFallsLocal(t *testing.T) {
	r := localRouter(t, map[string]json.RawMessage{"/v1/chat/completions": testutil.JSON(t, goodLlamaResponse())})
	r.SetModelSelector(func(context.Context) (string, bool) { return "claude-sonnet-4-6", true })
	out, err := r.Generate(context.Background(), "x", "y")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if out.Text != "low" {
		t.Errorf("text = %q, want \"low\" — remote unavailable must fall back to local", out.Text)
	}
}

// A selector returning ok=false (cold start / no data) uses the static default
// (local) even though it named a model — ok gates the choice.
func TestRouting_SelectorNotOkUsesLocalDefault(t *testing.T) {
	r, err := remoteRouter(t,
		map[string]json.RawMessage{"/v1/chat/completions": testutil.JSON(t, goodLlamaResponse())},
		map[string]json.RawMessage{"/v1/messages": testutil.JSON(t, goodAnthropicResponse())},
	)
	if err != nil {
		t.Fatalf("remoteRouter: %v", err)
	}
	r.SetModelSelector(func(context.Context) (string, bool) { return "claude-sonnet-4-6", false })
	out, err := r.Generate(context.Background(), "x", "y")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if out.Text != "low" {
		t.Errorf("text = %q, want \"low\" — ok=false must use the local static default", out.Text)
	}
}
