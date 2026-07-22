// Package measure serves the measure meta-tool — benchmark runs,
// benchmark replay, and rubric classification.
//
// ## Intended use
//
// **Workflow served:** the user runs a benchmark scenario against a
// model to capture timing, token counts, accuracy / honesty /
// ranking-quality scores, and provenance for replay; the classify path
// uses a TOML-declared rubric to label arbitrary model outputs against
// a fixed label set.
//
// **Invocation pattern:** `benchmark_run` with
// `{scenario_id, model_name}`; `benchmark_replay` with `{result_id}`
// (T6); `classify` with `{rubric_id, input}` returning a `ParseResult`.
//
// **Success shape:** a `BenchmarkResult` row in `benchmark_results`
// carrying provenance (model_id, model_version, prompt_template_hash,
// corpus_hash, retriever_version, seed, env_hash); replay produces a
// byte-identical row; classify returns a typed `rubric.ParseResult`.
//
// **Non-goals:** not a regression gate — `invocation_ok = false` is
// signal, not a CI fail; not a load-testing harness; not a corpus
// generator — scenarios live under `benchmarks/scenarios/` and are
// authored by hand.
package measure
