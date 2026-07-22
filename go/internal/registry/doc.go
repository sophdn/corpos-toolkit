// Package registry is the local-side machinery for the canonical event
// registry — the CI-gated, immutable, git-backed source of truth that the
// forge-v2 `record` surface mirrors to (chain emit-surface-forge-v2 T2).
//
// ## Intended use
//
// **Workflow served:** the local toolkit.db is the HOT, fast, rebuildable
// draft; the Gitea registry (`sophdn/toolkit-event-registry`) is the REMOTE
// AUTHORITY — append-only, fast-forward-only, CI-validated, the published
// source of truth and disaster-recovery authority. This package owns the
// seam between them: export the local ledger to the registry's on-disk
// format, validate a registry checkout the way CI does, and prove the local
// draft is reconstructable from the registry.
//
// **Invocation pattern:** driven by the `event-registry` CLI
// (go/cmd/event-registry):
//
//	n, err := registry.ExportFromDB(ctx, pool, destDir)                    // db → registry files
//	rep, err := registry.Validate(ctx, dir, registry.ValidateOptions{...}) // the CI tier
//	err := registry.VerifyDR(ctx, srcPool, dir)                            // the DR proof
//
// ExportFromDB writes one JSON file per event, named by its UUIDv7
// event_id — the load-bearing storage choice: two machines appending
// different events never touch the same file, so git merges are trivially a
// union of files and never line-conflict. Validate is the CI validity-stamp
// tier (schema via the shared events.ValidateRecordJSON + causal +
// projection-coherence). VerifyDR reconstructs the ledger from a registry
// checkout and asserts projections rebuild byte-identically to the source.
//
// **Success shape:** ExportFromDB returns the event count written; Validate
// returns a Report whose OK() is true when no tier failed; VerifyDR returns
// nil when the registry is a faithful, lossless DR source.
//
// **Non-goals:** this package does NOT enforce fast-forward-only immutability
// (§7 invariant 1) — that is the Gitea repo's branch protection (force-push +
// deletion disabled); this package is the content validator that rides on top.
// It does not push/pull git (the CLI + git do that), does not own the local
// write path (that is the `record` surface, T3), and does not define event
// types (that is the shared go/internal/events enum it imports).
package registry
