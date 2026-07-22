# Curation Pipeline — Go Migration

> **Status:** **MIGRATION COMPLETE** (T12 landed). Originally T1 of chain `curation-go-migration`. The contracts below are now the live system; this doc is the historical reference for what the migration shipped.
>
> **Reading order:** §1 framing → §2 module layout → §3 Scorer interface → §4 SourceMaterialBuilder interface → §5 per-origin builders → §6 abort-on-unreachable → §7 MCP curation surface → §8 new-origin recipe → §9 migration order → §10 non-goals.
>
> **Source ported from (now deleted):** `benchmarks/src/bin/knowledge_curate.rs`, `benchmarks/src/bin/knowledge_seeder.rs`, `benchmarks/src/bin/inference_health.rs`. Rust source preserved in git history (deletion commit in T12).
>
> **Replaced by:** `go/cmd/curate-rescore/` (T7), `go/cmd/curate-discover/` (T8), `go/cmd/curate-seed/` (T10), `go/cmd/curate-health/` (T11). Shared logic in `go/internal/knowledge/curation/`.
>
> **Companion bug context:** five absorbed bugs (see chain `curation-go-migration` design_decisions §6) — this doc shows how each is solved structurally rather than patched. All five resolved by chain commits.

---

## 1. What this surface is and isn't

The Rust curation pipeline today (a) mines grounding events and closed-task handoffs into `curation_candidates`, (b) scores candidates via Qwen, (c) auto-promotes high-scoring candidates to `knowledge_pointers`, (d) leaves the rest pending for human review. Today's triage exposed that the pipeline is fragile in three ways: there's no path to score *existing* pending candidates, the silent-failure fallback inserts unscored rows when Qwen is unreachable, and a noisy seeder pass produces 60%+ junk that buries real signal. None of these are isolated bugs — they're structural consequences of inlining scoring + per-origin source-material assembly + DB writes into binaries with no shared contract.

This chain ports the pipeline to Go around two small interfaces: a `Scorer` that abstracts Qwen, and a `SourceMaterialBuilder` that abstracts "given a candidate row, produce the text that should be scored." Three concrete passes (rescore, primary-discovery, secondary-discovery) compose those interfaces; one replacement seeder ships only the paths that produced real signal; an MCP curation surface lets agents promote/reject through the same tool path as everything else.

**Scope:**

- Go ports of: scoring contract (prompts + parsers + qwen helpers); candidate DB layer (with the missing `UpdateScoring` and `Reject` paths); discovery passes; rescore pass; replacement seeder; inference-health diagnostic.
- MCP curation surface on `mcp__toolkit-server__knowledge`: `curation_list` / `curation_read` / `curation_promote` / `curation_reject` / `curation_bulk_action`.
- Deletion of the three Rust binaries (`knowledge_curate.rs`, `knowledge_seeder.rs`, `inference_health.rs`) once Go parity is verified.
- Dashboard-regression bracket: baseline-snapshot before any DB-layer change, verify-after-migration at the end.

**Explicit non-goals:**

| Out of scope | Why |
|---|---|
| Redesigning `knowledge_pointers` or `curation_candidates` schemas | Schema-compatible port; deferred schema work files as follow-on. |
| Porting `crates/knowledge-shared/` wholesale | Only the candidate-lifecycle functions move. FTS/pointer-link functions stay until a separate need drives them. |
| Reviving the deprecated session-mining paths (`session-deep-*`, `session-question-*`, `web-conv-*`) | T10 (replacement seeder) explicitly drops them; the bug filings (`knowledge-seeder-session-mining-captures-one-time-prompts-as-knowledge`) document why. |
| Replacing `go/internal/inference/llamacpp/` | Audited in T3; extended if missing pieces (`Health()`), not rewritten. |

---

## 2. Module layout

Follows the precedent set by the sibling `knowledge_grounding_processor` port (commit `f9dd8f6`): Go binaries at `go/cmd/<name>/`, shared logic under `go/internal/knowledge/`.

```
go/internal/knowledge/curation/
  candidates.go        # DB layer: List, Read, Add, UpdateScoring, Promote, Reject
  scoring.go           # Prompts (constants), parsers, qwen_extract/qwen_score
  scorer.go            # Scorer interface + QwenScorer impl
  builder.go           # SourceMaterialBuilder interface + registry
  sources/
    task_handoff.go    # TaskHandoffBuilder
    zero_result_gap.go # ZeroResultGapBuilder (session JSONL reconstruction)
    vault_note.go      # VaultNoteBuilder (the only session_mining path we keep)
  curation_test.go     # table-driven tests against testutil.NewTestDB
go/internal/knowledge/
  curation_handler.go  # MCP HandleCuration{List,Read,Promote,Reject,BulkAction}
go/cmd/
  curate-rescore/      # T7 — drains pending unscored
  curate-discover/     # T8 — primary + secondary, replaces knowledge_curate.rs
  curate-seed/         # T10 — narrow vault-note seeder
  curate-health/       # T11 — replaces inference_health.rs
```

**Rationale:** keep curation co-located with the existing Go knowledge surface (`go/internal/knowledge/pointers/` already lives there) rather than spinning a top-level `go/internal/curation/`. The MCP handler co-locates with `vault_search` / `library_*` handlers it joins on the `knowledge` meta-tool.

---

## 3. Scorer interface (§T5)

```go
package curation

type ExtractedMeta struct {
    Question    string
    InvokeWhen  string
    Description string
}

// Scorer abstracts the Qwen-backed extraction + scoring pipeline so passes
// can be tested against a mock and live against llamacpp through the same
// surface. All methods take ctx for cancellation propagation per
// go-conventions.
type Scorer interface {
    // Extract generates retrieval metadata from source material. Returns
    // an error if generate fails or the response is unparseable.
    Extract(ctx context.Context, sourceType, sourceRef, sourceMaterial string) (ExtractedMeta, error)

    // Score returns an adversarial relevance score in [0.0, 1.0]. The
    // description is intentionally not passed — it would inflate scores
    // because the scorer would grade what it itself wrote.
    Score(ctx context.Context, question, sourceMaterial string) (float64, error)

    // Health returns nil iff the underlying inference endpoint is
    // reachable AND responding. Passes call this once before any
    // candidate work to honor the abort-on-unreachable contract (§6).
    // Returns a typed error naming the URL + cause so operators can fix
    // without code-reading.
    Health(ctx context.Context) error
}
```

**Implementations (this chain):**

- `QwenScorer` — wraps `*llamacpp.Client` (extended by T3 to add `Health()`). Reads `local_base_url` from `TOOLKIT_LOCAL_URL` env, defaults to `http://localhost:8081`. Constructor: `NewQwenScorer(client *llamacpp.Client) *QwenScorer`.
- `mockScorer` — test-internal in `curation_test.go`, returns canned values. Not exported.

Per go-conventions: callers accept `Scorer` interface; constructors return concrete `*QwenScorer`.

---

## 4. SourceMaterialBuilder interface (§T5)

```go
// Candidate is the curation_candidates row shape this package operates on.
// Mirror of crates/knowledge-shared CandidateRow.
type Candidate struct {
    ID            int64
    ProjectID     string
    SourceType    string // 'task' | 'vault' | etc.
    SourceRef     string // <project>::<slug> for tasks, path for vault
    Origin        string // 'task_handoff' | 'zero_result_gap' | 'session_mining'
    Question      string
    QualityScore  *float64
    // ... other fields per pointers.rs CandidateRow
}

// SourceMaterialBuilder reconstructs the text payload that should be
// scored, given a candidate row. One impl per origin.
type SourceMaterialBuilder interface {
    Origin() string
    Build(ctx context.Context, pool *db.Pool, cand Candidate) (string, error)
}
```

**Registry** (single wiring point — see §8 for the new-origin recipe):

```go
type BuilderRegistry struct {
    byOrigin map[string]SourceMaterialBuilder
}

func NewBuilderRegistry() *BuilderRegistry { ... }

// Register panics on duplicate origin. Called once at package init or
// from main(); not safe for concurrent registration after passes start.
func (r *BuilderRegistry) Register(b SourceMaterialBuilder)

// ForOrigin returns the builder or ErrUnknownOrigin.
func (r *BuilderRegistry) ForOrigin(origin string) (SourceMaterialBuilder, error)
```

Each pass takes a `*BuilderRegistry` at construction. No global state per go-conventions.

---

## 5. Per-origin builders (§T5)

| Origin | Builder | Source material shape |
|---|---|---|
| `task_handoff` | `TaskHandoffBuilder` | `tasks.problem_statement + "\n\nHandoff:\n" + tasks.handoff_output[:EXCERPT_CHARS]`. Mirror of `secondary_pass` in Rust. |
| `zero_result_gap` | `ZeroResultGapBuilder` | Reconstructs query from `~/.claude/projects/<project>/<session>.jsonl` by locating the `tool_use` block whose id matches the grounding event's `call_id`, then assembles the failed-query text + session context. Mirror of `primary_pass`. |
| `session_mining` (vault notes only) | `VaultNoteBuilder` | Reads file content at `~/.claude/vault/<source_ref>` via existing `go/internal/knowledge/vault/`. The deprecated session-deep/question/web-conv paths are NOT implemented — see T10. |

`EXCERPT_CHARS = 2000` (matches Rust `EXCERPT_CHARS`).

---

## 6. Abort-on-unreachable semantics (§T7 absorbing bug `knowledge-curate-secondary-pass-cannot-score-existing-candidates-and-silently-adds-unscored-on-qwen-failure`)

Every pass that calls `Scorer.Extract` or `Scorer.Score` must:

1. Call `Scorer.Health(ctx)` exactly once, before any candidate work.
2. If Health returns an error, print the typed error message (URL + cause) to stderr and exit non-zero. **Zero DB modifications** on this path.
3. If Health succeeds, the pass proceeds. Per-candidate extract/score failures are logged and the candidate is skipped — they do NOT silently fall back to templated metadata.

There is no `qwen_ok` boolean, no `task_fallback` function, no "graceful degradation" mode. The Rust pipeline had both and they hid the silent failure that triggered this chain.

Mechanically:
- `QwenScorer.Health()` issues `GET <local_base_url>/health`, returns `nil` on 2xx and a typed `*UnreachableError{URL, Cause}` otherwise (timeout, connection refused, non-2xx).
- Passes log a single banner line on Health success: `scorer.health ok url=http://localhost:8081`.

---

## 7. MCP curation surface (§T9 absorbing bug `no-mcp-curation-surface-cannot-promote-or-reject-candidates-via-tool-calls`)

Five actions on `mcp__toolkit-server__knowledge`, following the existing `HandleVaultSearch` / `HandleLibraryGet` pattern in `go/internal/knowledge/handler.go`:

| Action | Params | Result | Notes |
|---|---|---|---|
| `curation_list` | `{project?, origin?, status?='pending', scored? (bool), limit?=50}` | `{candidates: [CandidateSummary]}` | Compact projection mirroring `bug_list`. |
| `curation_read` | `{id}` | `Candidate` | Full row. |
| `curation_promote` | `{id, override_question?, override_invoke_when?, override_description?}` | `{pointer_id, source_ref}` | Wraps `Candidates.Promote`. Overrides let caller refine metadata at promotion time. |
| `curation_reject` | `{id, reason}` | `{ok: true}` | `reason` is **required** (rejected if empty). |
| `curation_bulk_action` | `{filter, action: 'promote'|'reject', reason?, dry_run?=false}` | `{counts, sample}` | Filter shape mirrors `curation_list`. `action='reject'` requires reason. `filter` must be non-empty (no accidental table-nukes). |

All five emit substrate events (`CurationCandidatePromoted`, `CurationCandidateRejected`) through the existing event ledger so AuditLedger surfaces curation activity. Event types added to `blueprints/events/`.

`status='rejected'` becomes a first-class state. Today the column is unconstrained `TEXT` (we used `'rejected'` informally during the manual bulk-reject); this chain formalizes it as a recognized value but does NOT add a `CHECK` constraint (would block the historical NULL-expires_at rows already in the table — see bug `session-mining-candidates-have-no-expires-at-never-auto-expire`'s constraint notes).

---

## 8. New-origin extension recipe

To add a hypothetical new origin (say, `library_entry_mining`):

1. Create `go/internal/knowledge/curation/sources/library_entry.go` with a struct that implements `SourceMaterialBuilder`. ~30 lines including imports.
2. In the package wiring point (or in `main.go` of each pass binary), call `registry.Register(&LibraryEntryBuilder{...})`.
3. Add `"library_entry_mining"` to the valid-origins enum check in `candidates.go` `Add()` (currently `{"zero_result_gap", "task_handoff", "session_mining"}` per `pointers.rs:540`).

Zero changes elsewhere — passes iterate registered builders by origin, the DB layer is origin-agnostic, the scorer is origin-agnostic. **This is the chain's claim to non-prematurity:** three concrete builders justify the abstraction at rule-of-three; a fourth costs one new file.

---

## 9. Migration order

| # | Task slug | Depends on | Notes |
|---|---|---|---|
| T1 | `design-doc-module-and-interface-shapes` | — | This doc. |
| T2 | `knowledge-dashboard-baseline-behavior-snapshot` | T1 | Gates T6 — locks dashboard contract before DB-layer change. |
| T3 | `audit-go-llamacpp-parity-vs-rust-inference-clients` | T1 | Identifies/fills gaps (notably `Health()`). |
| T4 | `port-scoring-contract-prompts-parsers-and-qwen-helpers` | T3 | Pure-port; identical prompts; unit tests for parsers. |
| T5 | `define-scorer-and-source-material-builder-interfaces` | T4 | Interfaces + QwenScorer + three builders. |
| T6 | `port-curation-candidate-db-layer` | T2, T5 | Includes the new `UpdateScoring` and `Reject`. |
| T7 | `rescore-pass-with-qwen-unreachable-abort` | T5, T6 | **First production pass.** Validates the architecture against the live 391-pending backlog. |
| T8 | `port-discovery-passes-primary-and-secondary` | T7 | After rescore proves the contract. |
| T9 | `mcp-curation-surface-list-read-promote-reject-bulk` | T6 | New MCP actions. |
| T10 | `replacement-seeder-named-vault-only-with-expires-at` | T5, T6 | Skip-and-replace, not port. |
| T11 | `port-inference-health-diagnostic-to-go` | T3 | Go replacement for today's Rust diagnostic. |
| T12 | `retire-rust-curation-binaries` | T7, T8, T9, T10, T11 | `git rm`; `cargo tree -p benchmarks \| grep knowledge` returns empty. |
| T13 | `end-to-end-backlog-drain-and-triage` | T7, T9 | Data validation: every pending row has `quality_score IS NOT NULL`. |
| T14 | `verify-knowledge-dashboard-post-migration` | T12, T13 | Surface validation against T2's locked baseline. |

---

## 10. Non-goals (recap)

- Not a schema redesign. Tables unchanged.
- Not a rewrite of `crates/knowledge-shared/`; only candidate-lifecycle functions move.
- Not reviving the deprecated session-mining paths.
- Not bundling orthogonal already-fixed bugs (precommit-advisor make-build, forge envelope error, dashboard CSS sweep — see chain design_decisions §10).
