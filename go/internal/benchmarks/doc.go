// Package benchmarks holds scenario types, fixtures, and (in later T5
// phases) the runner for offline rubric-dispatch benchmarks.
//
// ## Intended use
//
// **Workflow served:** an operator (or scheduled job) runs a benchmark
// binary to exercise the rubric-dispatch + retrieval surfaces against
// fixed gold-set scenarios, capturing accuracy/honesty/ranking-quality
// scores and provenance into `benchmark_results`. Distinct from the
// MCP measure surface — benchmarks is a *producer* of benchmark rows,
// the measure handlers expose individual classify_* actions for live
// agent calls.
//
// **Invocation pattern:** scenarios are constructed in code
// (`ExtractScenario{...}`, `ClassifyScenario{Slug, Text, RubricSlug,
// Gold}`, `RetrieveScenario{...}`), then passed to a per-binary runner
// (Phase 2+) that resolves rubric definitions via `rubric.Registry`,
// composes prompts via `rubric.ComposeClassify` /
// `qwenretrieve.DispatchTwoPassRetrieve`, dispatches via the
// `inference/router`, scores against the carried Gold, and writes a
// row to `benchmark_results`. Fixtures (e.g. `SeedSnapshot`) provide
// static project-context for Layer 4 prompt injection.
//
// **Success shape:** scenario types deserialize cleanly, render to the
// expected prompt-injection text (byte-identical to the Rust
// `benchmarks/` crate's render output during the parity-audit window),
// and round-trip the gold answer types so the runner can compute
// per-scenario verdicts.
//
// **Non-goals:** not a live agent surface — no MCP actions are
// registered from this package; not a corpus generator — scenarios are
// authored by hand in the scenarios/ subpackages (later phases); not a
// general-purpose scenario framework — the types are shaped specifically
// for the rubric-dispatch + retrieve + extract flows used today.
//
// ## Migration provenance
//
// Ported from the Rust `benchmarks/` crate per chain
// `rust-retirement-and-db-hardening` T5 (Option A). The Rust crate is
// the source of truth during the per-phase parity audit window; once
// Phase 5 lands, `benchmarks/` archives to
// `tools/archive/benchmarks-2026-05-XX/`. See
// `docs/BENCHMARKS_PORT_PLAN_2026-05.md` for the full phase plan and
// `docs/RUST_RETIREMENT_PARITY_2026-05.md` for the T3 parity audit.
//
// Translate-behavior-not-syntax decisions encoded in `types.go` are
// documented inline at the relevant struct definitions.
package benchmarks
