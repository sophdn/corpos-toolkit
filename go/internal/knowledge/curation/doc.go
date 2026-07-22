// Package curation implements the candidate-lifecycle pipeline for the
// knowledge index: scoring candidates via Qwen, building per-origin
// source material, and reading/writing curation_candidates rows.
//
// Design contract: docs/CURATION_GO_MIGRATION.md. The chain
// curation-go-migration (chain_slug) ports this surface from the Rust
// binaries at benchmarks/src/bin/knowledge_curate.rs and
// benchmarks/src/bin/knowledge_seeder.rs.
//
// Layout:
//   - scoring.go       — Qwen prompts (extraction + adversarial scoring),
//     parsers, and async helpers.
//   - scorer.go        — Scorer interface + QwenScorer impl. (T5)
//   - builder.go       — SourceMaterialBuilder interface + registry. (T5)
//   - candidates.go    — DB layer for curation_candidates. (T6)
//   - sources/*        — per-origin SourceMaterialBuilder impls. (T5)
package curation
