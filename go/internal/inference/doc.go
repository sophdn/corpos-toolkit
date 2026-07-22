// Package inference routes generate calls to the configured model
// backends (local llama.cpp Qwen, remote Anthropic Claude, mock).
//
// ## Intended use
//
// **Workflow served:** handlers that need a model call (classify a
// rubric, rerank retrieval candidates, summarize) hit the inference
// router; the router picks the backend by model name and forwards
// through the right client without exposing backend-specific options
// to the caller.
//
// **Invocation pattern:** `resp, err := inference.Generate(ctx, req)`
// where `req` carries model, messages, temperature, etc. Backend
// selection: `qwen*` → llamacpp, `claude*` → anthropic, `mock*` → mock;
// callers can also stamp a task_id via internal/qwenctx for the
// `/inference/stats` attribution path.
//
// **Success shape:** `GenerateResponse{Text, PromptTokens,
// CompletionTokens, FinishReason}`; backend errors wrap as
// `dispatch.Error` so the MCP boundary sees a typed envelope, and every
// qwen call lands one row in `inference_invocations` for telemetry.
//
// **Non-goals:** not a prompt-template engine (callers shape prompts;
// see internal/qwenretrieve for one shared shape), not a token counter
// for backends that don't return usage, not a streaming proxy — handlers
// that want SSE must wire their own forwarder above this layer.
package inference
