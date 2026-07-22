// Package ml is the toolkit-server's trained-model serving layer.
//
// Wraps ONNX Runtime via github.com/yalue/onnxruntime_go to load
// trained models from disk and run inference in-process inside the
// canonical Go binary. Replaces the would-be Rust ml-serve crate per
// the chain `ml-capability-substrate` 2026-05-19 pivot (see
// docs/ML_CAPABILITY_SUBSTRATE.md §1.2 + §5).
//
// ## Intended use
//
// **Workflow served:** when an agent (or another internal handler)
// wants to invoke a trained model — source router, curation
// classifier, cross-encoder reranker, bug surface tagger, skill
// auto-loader — the call lands here. The MCP `inference` action
// (T5) and per-task convenience actions (`route_query`,
// `curation_score`, …) dispatch through Registry to load the
// currently-promoted model for a task and run inference. The A/B
// harness (T6) wraps this in baseline-vs-trained comparison.
//
// **Invocation pattern:** `reg.LoadByPromoted(ctx, project, task)
// (*Model, error)` resolves the live model for a task via the
// trained_models table (T4); `reg.LoadByID(ctx, id)` resolves a
// specific (task, version) row. Either returns a Model whose
// `Infer(ctx, Features{Data, Shape}) (Prediction, error)` runs the
// session. Models are cached LRU; Reload() drops the cache when
// promotion/retirement flips a row.
//
// **Success shape:** a `Prediction{Output []float32, OutputShape
// []int64, LatencyMs float64, ModelID int64, FeatHash string}`.
// FeatHash is the content key for the prediction telemetry row
// (T7) and drift detection. Target latency: sub-50ms for the five
// candidate models; budget 100ms end-to-end including load.
//
// **Non-goals:** the MCP action wrapper (T5), convenience actions
// per task (T5), A/B harness (T6), training-side anything (lives
// in ~/dev/ml-training/). This package is serving-only.
//
// Surface:
//
//   - Registry — loads a model by (project, task, status='promoted')
//     from the trained_models table (T4) or by trained_model.id.
//     Caches loaded Sessions with LRU eviction.
//   - Session — interface fulfilled by ortSession (real onnxruntime_go
//     wrapper) AND by fakeSession (test double). Exposes Run() over
//     the binding's DynamicAdvancedSession primitive.
//   - Model.Infer — validate features → run session → return Prediction
//     with output, latency, model id, feature hash.
//   - Prediction — typed output the MCP inference action wraps.
//
// Native library install path: onnxruntime_go uses dlopen at runtime
// via SetSharedLibraryPath. The package itself compiles + links without
// libonnxruntime.so present (the binding is cgo with deferred dlopen).
// To actually run inference, install libonnxruntime.so via
// scripts/setup-ml-deps.sh — that script downloads the official
// Microsoft ONNX Runtime binary to vendor/onnxruntime/lib/ and
// configures the ML_ONNXRUNTIME_LIB_PATH env var hint.
//
// Out of scope for T3:
//   - The MCP `inference` action wrapper (T5).
//   - Convenience actions per task (`route_query`, `curation_score`,
//     `forge_suggest_surfaces`, etc.) — also T5.
//   - The A/B harness (`go/internal/ml/abtest/`) — T6.
//
// Test split:
//   - inference_test.go — unit tests using fakeSession. Always runs.
//   - runtime_integration_test.go — actually loads a fixture ONNX via
//     onnxruntime_go. Skipped when libonnxruntime.so or the fixture
//     isn't present. The fixture is `testdata/example_dynamic_axes.onnx`
//     vendored from yalue/onnxruntime_go (MIT) — see testdata/README.md.
package ml
