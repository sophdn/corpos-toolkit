// Package router provides an inference router that dispatches to the local
// llama.cpp Qwen model and optionally to a remote Anthropic model.
package router

import (
	"context"
	"fmt"
	"os"
	"time"

	"toolkit/internal/inference/anthropic"
	"toolkit/internal/inference/llamacpp"
)

// InvocationRecord is the per-call telemetry payload the router emits
// after every local Generate. main.go wires a RecordInvocation closure
// that persists this into inference_invocations (the per-call telemetry
// table — bug 1328's universal shape, generalized model-agnostic in
// Chain 1; the qwen_invocations sink it replaced was dropped in Chain 5).
// Tests leave RecordInvocation nil — no record, no dependency on the db
// package.
type InvocationRecord struct {
	// TaskID is the qwenctx.TaskID stamp the caller set before invoking
	// Generate. Unstamped callers record as qwenctx.Unattributed so the
	// call still appears in the volume figure on /inference/stats.
	TaskID string

	// ModelName is the router's ModelName() — i.e. the local model
	// identifier ("qwen2.5-32b" today). Recorded per-row so future
	// per-model variants can be filtered downstream without a schema
	// change.
	ModelName string

	// LatencyMS is the round-trip wall-clock time for the inference
	// call. Recorded on both success and failure paths so the dashboard
	// reflects time spent on broken calls too.
	LatencyMS int64

	// InputTokens / OutputTokens mirror GenerateResult's fields — nil
	// when the upstream model omits usage info (older llama.cpp builds,
	// some streaming responses).
	InputTokens  *int64
	OutputTokens *int64

	// Success is the call-level outcome: true = no upstream error AND
	// non-empty output. Recorded on every emit (chain per-tool-per-model-
	// observability T11) so the inference_invocations row carries a
	// first-class success signal instead of leaving it to be re-derived.
	Success bool

	// ErrorClass is the closed-enum failure reason ('' on success). The
	// router maps it from the branch that emitted the record; see the
	// ErrorClass* constants.
	ErrorClass string
}

// Closed error_class enum mirrored from migration 077's CHECK set. The
// router maps each failure branch to one of these; success emits "".
const (
	ErrorClassNone          = ""
	ErrorClassUpstream      = "upstream_error"
	ErrorClassEmptyResponse = "empty_response"
	ErrorClassNotConfigured = "not_configured"
	ErrorClassTimeout       = "timeout"
)

// remoteModelName is the Anthropic model the remote path invokes. Recorded
// as model_name on remote inference_invocations rows so per-model telemetry
// distinguishes remote Claude calls from local Qwen.
const remoteModelName = "claude-sonnet-4-6"

// RecordInvocationFunc persists a single inference call's telemetry.
// Nil is the no-op default — tests and pre-T68 callers run without a
// recorder wired and the router skips the persistence step.
type RecordInvocationFunc func(ctx context.Context, rec InvocationRecord)

// ModelSelectorFunc returns the model the router should use for the task on
// ctx, plus ok. ok=false (cold-start / insufficient data / read error) means
// "no data-driven choice" and the router falls back to its static default
// model — so a nil selector reproduces today's static routing exactly (chain
// data-driven-model-routing). The selector reads telemetry (the read-side
// proj_inference_tool_model_performance projection); keeping it an injected
// closure (wired in main.go where the DB pool lives) keeps this package
// db-free, mirroring SetInvocationRecorder.
type ModelSelectorFunc func(ctx context.Context) (modelName string, ok bool)

const anthropicKeyEnv = "ANTHROPIC_API_KEY"

// Router holds a required local (llama.cpp) client and an optional remote
// (Anthropic) client. Local inference is the default; remote is explicit.
// There is no silent automatic fallback from local to remote — if the local
// model fails, Generate returns the error.
type Router struct {
	local     *llamacpp.Client
	remote    *anthropic.Client // nil when ANTHROPIC_API_KEY is absent
	modelName string
	// recordInvocation persists per-call telemetry. Set via
	// SetInvocationRecorder; nil-safe so tests run without a recorder
	// and the persistence step is skipped silently.
	recordInvocation RecordInvocationFunc
	// selectModel makes the data-driven model choice. Set via
	// SetModelSelector; nil-safe — a nil selector means the router uses
	// its static default model (today's behavior).
	selectModel ModelSelectorFunc
}

// SetInvocationRecorder wires the per-call telemetry persistence
// closure. main.go calls this once after constructing the Router; tests
// leave it unset and the router runs without recording.
func (r *Router) SetInvocationRecorder(fn RecordInvocationFunc) {
	r.recordInvocation = fn
}

// SetModelSelector wires the data-driven model-selection closure (chain
// data-driven-model-routing). main.go installs a selector backed by the
// proj_inference_tool_model_performance projection + a 60s cache; tests
// leave it unset (or inject a fake) and the router falls back to the
// static default — so cold-start routing is identical to today's.
func (r *Router) SetModelSelector(fn ModelSelectorFunc) {
	r.selectModel = fn
}

// resolveModel returns the model name to dispatch this call to: the
// selector's choice when it has one (ok && non-empty), else the static
// default. A nil selector always yields the static default — the
// cold-start parity guarantee (TestStaticRouting_ColdStartModelSelectionIsStaticDefault).
func (r *Router) resolveModel(ctx context.Context) string {
	if r.selectModel != nil {
		if name, ok := r.selectModel(ctx); ok && name != "" {
			return name
		}
	}
	return r.modelName
}

// New constructs a Router using the given llama.cpp base URL and the Anthropic
// API key from ANTHROPIC_API_KEY. If the env var is absent the remote client
// is nil; GenerateRemote will return an error.
//
// llamaURL may be empty to use the default (localhost:8081).
func New(llamaURL string) (*Router, error) {
	local := llamacpp.New(llamaURL)

	var remote *anthropic.Client
	if key := os.Getenv(anthropicKeyEnv); key != "" {
		var err error
		remote, err = anthropic.New()
		if err != nil {
			return nil, fmt.Errorf("router: construct anthropic client: %w", err)
		}
	}

	return &Router{
		local:     local,
		remote:    remote,
		modelName: "qwen2.5-32b",
	}, nil
}

// NewWithClients constructs a Router from pre-built clients. remote may be nil
// when remote inference is unavailable. Used in tests and when callers manage
// client construction themselves.
func NewWithClients(local *llamacpp.Client, remote *anthropic.Client, modelName string) *Router {
	return &Router{local: local, remote: remote, modelName: modelName}
}

// ModelName returns the identifier of the local model in use.
func (r *Router) ModelName() string {
	return r.modelName
}

// GenerateResult carries the text response plus per-call token telemetry.
// InputTokens / OutputTokens are *int64 so the caller can store NULL in the
// benchmark_results columns when the upstream model omits usage info (older
// llama.cpp builds and some Anthropic streaming responses).
type GenerateResult struct {
	Text         string
	InputTokens  *int64
	OutputTokens *int64
	// LatencyMS is the round-trip wall-clock time for the inference call.
	// Populated for both Generate and GenerateRemote; zero on early errors
	// where the HTTP call was never attempted.
	LatencyMS int64
}

// GenerateOpts controls per-call parameters. Zero MaxTokens uses the classify
// default (64); retrieve passes higher caps for multi-line output.
type GenerateOpts struct {
	MaxTokens int
}

const defaultMaxTokens = 64

func ptrI64(n int) *int64 {
	v := int64(n)
	return &v
}

// Generate classifies input using the local Qwen model with the default
// max_tokens cap (64). See GenerateWithOpts for retrieve-shape callers that
// need a higher cap.
func (r *Router) Generate(ctx context.Context, prompt, system string) (GenerateResult, error) {
	return r.GenerateWithOpts(ctx, prompt, system, GenerateOpts{})
}

// GenerateWithOpts is the explicit-options variant. MaxTokens defaults to 64
// when zero. Latency, token counts, and text shape match Generate.
//
// Model selection is data-driven (chain data-driven-model-routing): the
// injected selector chooses the model for this task; when it names the remote
// model and a remote client is configured the call routes remote, otherwise it
// routes to the local default. With no selector, no data, or a read error the
// local default handles the call — identical to today's static routing. Both
// paths record one row of per-call telemetry (success / empty-response / error)
// best-effort; a nil/erroring recorder never blocks the inference response.
func (r *Router) GenerateWithOpts(ctx context.Context, prompt, system string, opts GenerateOpts) (GenerateResult, error) {
	if r.resolveModel(ctx) == remoteModelName && r.remote != nil {
		return r.generateRemote(ctx, prompt, system, opts)
	}
	return r.generateLocal(ctx, prompt, system, opts)
}

// generateLocal dispatches to the local llama.cpp model. This is the body the
// pre-data-driven-routing GenerateWithOpts ran unconditionally; extracted so
// the routed default path and any future caller share one local-dispatch path.
func (r *Router) generateLocal(ctx context.Context, prompt, system string, opts GenerateOpts) (GenerateResult, error) {
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	req := llamacpp.CompletionRequest{
		Model: r.modelName,
		Messages: []llamacpp.Message{
			{Role: "system", Content: system},
			{Role: "user", Content: prompt},
		},
		MaxTokens: maxTokens,
	}
	start := time.Now()
	resp, err := r.local.Complete(ctx, req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		r.emitInvocation(ctx, InvocationRecord{
			ModelName: r.modelName, LatencyMS: latency,
			Success: false, ErrorClass: ErrorClassUpstream,
		})
		return GenerateResult{LatencyMS: latency}, fmt.Errorf("router generate: %w", err)
	}
	if len(resp.Choices) == 0 {
		r.emitInvocation(ctx, InvocationRecord{
			ModelName: r.modelName, LatencyMS: latency,
			Success: false, ErrorClass: ErrorClassEmptyResponse,
		})
		return GenerateResult{LatencyMS: latency}, fmt.Errorf("router generate: empty choices in response")
	}
	out := GenerateResult{
		Text:      resp.Choices[0].Message.Content,
		LatencyMS: latency,
	}
	if resp.Usage != nil {
		out.InputTokens = ptrI64(resp.Usage.PromptTokens)
		out.OutputTokens = ptrI64(resp.Usage.CompletionTokens)
	}
	// Call-level success = non-empty output (the choices guard above only
	// rules out a missing choice, not an empty content string).
	rec := InvocationRecord{
		ModelName:    r.modelName,
		LatencyMS:    latency,
		InputTokens:  out.InputTokens,
		OutputTokens: out.OutputTokens,
		Success:      out.Text != "",
	}
	if out.Text == "" {
		rec.ErrorClass = ErrorClassEmptyResponse
	}
	r.emitInvocation(ctx, rec)
	return out, nil
}

// emitInvocation stamps the task_id from ctx and dispatches to the
// recorder closure. The task_id is populated from qwenctx via the
// helper in invocation_stamp.go so this package doesn't pick up a
// direct dependency on qwenctx (allowing test code to set arbitrary
// recorders without dragging the ctx-key types in).
func (r *Router) emitInvocation(ctx context.Context, rec InvocationRecord) {
	if r.recordInvocation == nil {
		return
	}
	rec.TaskID = stampTaskID(ctx)
	r.recordInvocation(ctx, rec)
}

// GenerateRemote classifies input using the remote Anthropic model. Returns
// an error if the Anthropic client is nil (ANTHROPIC_API_KEY was not set at
// startup). Token counts are returned from the Anthropic Messages API usage
// block. This is the EXPLICIT-remote entry point (e.g. the session-routing
// escalation); the data-driven default path reaches the same dispatch via
// generateRemote when the selector names the remote model.
func (r *Router) GenerateRemote(ctx context.Context, prompt, system string) (GenerateResult, error) {
	if r.remote == nil {
		// Record the attempt so a remote-not-configured run is visible in
		// telemetry rather than silently absent (the remote-coverage gap
		// this chain closes). Latency is 0 — no HTTP call was made.
		r.emitInvocation(ctx, InvocationRecord{
			ModelName: remoteModelName, LatencyMS: 0,
			Success: false, ErrorClass: ErrorClassNotConfigured,
		})
		return GenerateResult{}, fmt.Errorf("router generate remote: Anthropic client not configured (ANTHROPIC_API_KEY not set)")
	}
	return r.generateRemote(ctx, prompt, system, GenerateOpts{})
}

// generateRemote dispatches to the remote Anthropic model. The caller
// guarantees r.remote != nil (GenerateRemote does the nil-check; the routed
// path in GenerateWithOpts guards on it). MaxTokens defaults to 64 — the value
// the explicit GenerateRemote always sent — so that path stays behavior-
// identical; the routed default path forwards the caller's opts (e.g. the
// retrieve cap) instead of silently truncating.
func (r *Router) generateRemote(ctx context.Context, prompt, system string, opts GenerateOpts) (GenerateResult, error) {
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	// CacheSystem engages Anthropic's prompt-caching surface for the
	// system prompt. The classify_* path reuses the same per-rubric
	// system prompt across many input classifications within a session;
	// any pair of calls within the 5m cache TTL hits the cache and pays
	// ~10% of the cached portion's input-token cost. See the
	// CacheSystem doc on anthropic.MessagesRequest for the cost-model
	// detail.
	req := anthropic.MessagesRequest{
		Model:       remoteModelName,
		MaxTokens:   maxTokens,
		System:      system,
		Messages:    []anthropic.Message{{Role: "user", Content: prompt}},
		CacheSystem: true,
	}
	start := time.Now()
	resp, err := r.remote.Complete(ctx, req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		r.emitInvocation(ctx, InvocationRecord{
			ModelName: remoteModelName, LatencyMS: latency,
			Success: false, ErrorClass: ErrorClassUpstream,
		})
		return GenerateResult{LatencyMS: latency}, fmt.Errorf("router generate remote: %w", err)
	}
	if len(resp.Content) == 0 {
		r.emitInvocation(ctx, InvocationRecord{
			ModelName: remoteModelName, LatencyMS: latency,
			Success: false, ErrorClass: ErrorClassEmptyResponse,
		})
		return GenerateResult{LatencyMS: latency}, fmt.Errorf("router generate remote: empty content in response")
	}
	out := GenerateResult{
		Text:         resp.Content[0].Text,
		InputTokens:  ptrI64(resp.Usage.InputTokens),
		OutputTokens: ptrI64(resp.Usage.OutputTokens),
		LatencyMS:    latency,
	}
	rec := InvocationRecord{
		ModelName:    remoteModelName,
		LatencyMS:    latency,
		InputTokens:  out.InputTokens,
		OutputTokens: out.OutputTokens,
		Success:      out.Text != "",
	}
	if out.Text == "" {
		rec.ErrorClass = ErrorClassEmptyResponse
	}
	r.emitInvocation(ctx, rec)
	return out, nil
}
