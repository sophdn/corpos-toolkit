// Package abtest is the A/B hot-swap harness for the ml-capability
// substrate. It lets a code path that currently calls a baseline
// implementation (Qwen-rubric reranker, in-LLM scorer, etc.) gain a
// trained-model alternative on every call without conditional spaghetti
// in the call site.
//
// ## Intended use
//
// **Workflow served:** when a trained_model row is in status='ab_testing'
// (the explicit "we've trained it, now we want to compare against
// baseline before promoting" phase), call sites wrap their existing
// baseline invocation in abtest.Dispatch. The harness fires both
// baseline and trained, records the comparison, and returns the
// Policy-selected output. When the row flips to 'promoted', the
// harness short-circuits to trained-only; when it's 'evaluating' or
// 'retired', it short-circuits to baseline-only.
//
// **Invocation pattern:** `abtest.Dispatch(ctx, deps, Config{
// Baseline: <func>, Model: <*ml.Model>, Features: <ml.Features>,
// Policy: <PreferBaseline | PreferTrained | Alternate>}) (Result,
// error)`. The Baseline parameter is a func that takes ctx + Features
// and returns ([]float32, []int64, error) — matching the Model.Infer
// output shape so callers receive the same []float32 regardless of
// path. Each call writes one row to ab_comparisons (migration 045).
//
// **Success shape:** Result{Output []float32, OutputShape []int64,
// UsedPath ("baseline"|"trained"), BaselineLatencyMs, TrainedLatencyMs,
// ComparisonRowID}. UsedPath records which output the caller is
// actually receiving; downstream telemetry joins on this to score
// outcomes.
//
// **Non-goals:** the promotion gate itself (lives in
// internal/ml/abtest's promotion_gate.go as a read helper, not in
// Dispatch); training-side code (ml-training); MCP action wrappers
// (ml.inference + the convenience-action layer in T5).
package abtest
