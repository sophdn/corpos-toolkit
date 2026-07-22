# Reference Resolution Substrate — Design

> **Status:** Draft for review. Produced by chain `reference-resolution-substrate` T1 (`design-reference-resolution-architecture`). Decisions here are durable; downstream tasks T2–T9 bind to them. Amendments after this doc lands require a chain-level decision, not a unilateral task edit.
>
> **Reading order:** §1 framing → §2 shape taxonomy + detection rules → §3 resolver registry contract → §4 confidence tiers + presentation → §5 orchestration + the `resolve_references` MCP action → §6 telemetry integration → §7 hook supersession → §8 harness-reminder interception (T9) → §9 v1-skill evolution → §10 future-ML boundary → §11 the four proactive-injection decisions, answered → §12 worked example → §13 cross-substrate seam → §14 open questions.
>
> **Companion docs:** `docs/EVENT_SUBSTRATE.md` (write-side ledger; this chain inherits the `span_id` contract from §6 and the rationale-enforcement posture from §5). `docs/TELEMETRY_SUBSTRATE.md` (read-side telemetry; this chain consumes the `query_source` discriminator from §8 and the `grounding_events` / `query_interactions` row shapes from §3). `skills/reference-resolution.md` (v1 skill — the discipline this substrate codifies as a mechanism).
>
> **Cross-chain dependencies:** `agent-first-substrate` T1 + T5 (event envelope + `span_id`, closed 2026-05-17). `query-telemetry-substrate` TT1 + TT2 (telemetry shape + migration 037, closed 2026-05-17). Forward-coordinates with the (not-yet-forged) ML capability chain: §10 specifies the integration boundary so T7 doesn't have to re-derive it.

---

## 1. What this substrate is and isn't

The v1 skill `reference-resolution` (`skills/reference-resolution.md`, drafted 2026-05-16) names the discipline: when the user mentions a token the agent can't bind to current context — a chain slug, a path, a skill name, a forge schema, a domain term — resolve the reference *first*, then respond. The skill is the agent-side teaching surface; it works today without any code from this chain.

This chain ships the **code-backed mechanism**: automated detection, dispatch, telemetry, and supersession of always-on reminder hooks. The skill stays valid post-chain; it gets sharper, not deprecated. Concretely, this substrate adds:

- A reference detector (`go/internal/refresolve/`) that scans a message and tags reference-shape tokens.
- A pluggable resolver registry (one resolver per shape category) wrapping existing tools.
- A `knowledge(action='resolve_references', ...)` MCP action that ties them together and returns formatted resolution context.
- Telemetry integration with `query-telemetry-substrate` (every detection + resolution emits a `grounding_events` row with `query_source='reference_resolution'`; citations of resolved bindings flow through the existing four-tier click-kind detector).
- Supersession of the `friction-filing-reminder.sh` Stop hook via a `friction_shape` reference category.
- ~~A UserPromptSubmit interception hook (`hooks/intercept-task-tools-reminder.sh`) that strips the harness's over-firing "task tools haven't been used recently" reminder when the relevance gate fires negative.~~ **[REMOVED 2026-05-25 — superseded by a `permissions.deny` fix; see the §8 banner.]**

### 1.1 Third leg of the substrate trilogy

This is the third and final substrate in the agent-first stack:

| Chain | Surface | Records | Status |
|---|---|---|---|
| `agent-first-substrate` | Write-side audit ledger (`events` table + `_envelope.json` + rationale enforcement) | One row per agent mutation. | Closed 2026-05-17. |
| `query-telemetry-substrate` | Read-side telemetry (`grounding_events` + `query_interactions` + `query_resolutions`) | One row per search call + per detected click signal + per terminal resolution. | Closed 2026-05-17. |
| `reference-resolution-substrate` (this chain) | Reference-detection + resolver layer + supersession of always-on reminders | Triggered per user message; emits into the read-side telemetry surface. | In progress. |

After all three close, the toolkit-server has the full agent-first stack: write-side audit, read-side telemetry, and the reference-resolution layer that makes the unified knowledge surface **ergonomic to consume on every turn**. The ML capability chain (not yet forged) is the first time a trained model ships through this infrastructure — domain-term classifier and cross-encoder reranker (§10).

### 1.2 Scope of this chain

- Reference detector — rule-based for deterministic shapes; Qwen rubric (then trained classifier in §10) for domain-term shape.
- Resolver registry — pluggable Go interface, one implementation per shape category.
- The `resolve_references` MCP action — the public surface; agents call it to get formatted resolution context.
- Telemetry emit path — `grounding_events.query_source = 'reference_resolution'` (Path A enum extension, see §6.1); citation detection flows through the existing four-tier mechanism.
- Friction-filing-reminder supersession — `friction_shape` reference category + contextual filing suggestion replaces the Stop hook.
- ~~Harness task-tools-reminder interception — UserPromptSubmit hook strips the upstream reminder when the relevance gate fires negative; fails open on text drift.~~ **[REMOVED 2026-05-25 — see §8 banner.]**
- Updated v1 skill body that delegates to the MCP action while preserving its trigger discipline.

### 1.3 Out of scope

| Out of scope | Why |
|---|---|
| Full proactive-injection layer (system-fires on every message) | Different feature. See `~/.claude/vault/learnings/general/2026-05-15_proactive-injection-feature-design.md` for the framing if/when it becomes the next chain. The reference resolver is **agent-invoked**, not system-fired. The hook in §8 is a thin interception, not a parallel proactive surface. |
| Training new ML models | Qwen rubric is the cold-start classifier for domain-term shape; trained models replace it via T7 once the ML capability chain ships. Pure substrate work this chain. |
| Retiring `grounding-events-processor.sh` | Belongs to `query-telemetry-substrate`'s scope (its TT2 live-emit path supersedes batched processing). Boundary documented in §7.3. |
| Fixing the upstream harness reminder | Claude Code internal. T9's scope is strictly local interception of text on the way IN to the agent; we cannot retire the upstream firing, only intercept it. |
| Auto-firing the resolver on every message | Auto-firing is proactive-injection territory and stays separate. The resolver is an MCP tool the agent calls per the skill's discipline. |
| Other harness reminders (parallel-Bash hard-cancel, MCP-reconnect notices) | Scope is one known case (task-tools reminder) with a generalization path documented in T9 only if the pattern proves out. |

---

## 2. Shape taxonomy and detection rules

The detection contract is **shape-based, not uncertainty-based**. LLMs are unreliable at "do I know X?" but reliable at "is this token a slug/path/skill/project/tool/schema-shaped reference I don't have loaded?" The detector trades on the second framing throughout.

### 2.1 Eleven shape categories

| Shape category | Detection method | Confidence inputs |
|---|---|---|
| `chain_slug` | Rule-based: kebab-case regex `^[a-z][a-z0-9]*(-[a-z0-9]+)+$` AND list-match against `chains.slug` in toolkit.db. | Exact match in chains table = high; regex-only = low (may be unknown slug). |
| `task_slug` | Rule-based: kebab-case regex AND list-match against `tasks.slug` (typically scoped under a chain mentioned earlier in the message or in the conversation). | Exact match in tasks = high; regex-only with chain context = medium. |
| `bug_slug` | Rule-based: kebab-case regex AND list-match against `bugs.slug` in toolkit.db. | Exact match in bugs = high; regex-only = low. |
| `path` | Rule-based: contains `/` AND ends in a recognized extension (`.md`, `.go`, `.rs`, `.toml`, `.json`, `.py`, `.sh`, `.sql`, …) OR begins with `/`, `~`, `./`. | Filesystem stat decides on resolve; detection is shape-only. |
| `skill_name` | Rule-based: exact match (case-sensitive) against the basename (sans `.toml`) of any file in `skills/`. | Exact filename match in skills/ = high. |
| `project_name` | Rule-based: exact match against the closed list of known projects: `mcp-servers`, `seed-packet`, `self-compile`, `dm-toolkit`, `dashboard`. List is closed; new projects extend `go/internal/refresolve/projects.go`. | List-match = high; no fuzzy form for projects. |
| `tool_name` | Rule-based: exact match against the basename (sans `.toml`) of any file in `action-manifests/` OR a registered action name from `dispatch-policy.toml` (`work.bug_resolve`, `knowledge.vault_search`, etc.). | Exact catalog match = high. |
| `forge_schema` | Rule-based: exact match against the basename (sans `.toml`) of any file in `blueprints/forge-schemas/` (`bug`, `task`, `chain`). | Exact schema match = high. |
| `library_entry` | Rule-based: exact match against `library_entries.title` OR `library_entries.slug`. | Exact entry match = high; title fuzzy-match = low (left to library_find). |
| `domain_term` | Rubric-based: Qwen classifier (cold-start) reads candidate noun phrases and returns is-domain-term + confidence. Trained sklearn classifier replaces Qwen via §10. | Rubric returns a float in [0, 1]; threshold for "treat as domain term" is 0.6. |
| `external_technical` | Heuristic: capitalized multi-word concept not matched by any above category; falls through to kiwix on resolve. | Always low confidence at detection time; resolver's hit-or-miss is the real signal. |

Additional category, added by T6 (hook supersession, §7):

| Shape category | Detection method | Confidence inputs |
|---|---|---|
| `friction_shape` | Rule-based pattern match for observation-of-friction phrases: `that's weird`, `annoying that`, `why does X always Y`, `this should just work`, tool-failure echoes, multi-attempt-with-different-result. Qwen rubric refines ambiguous cases. | Pattern-match = high; rubric refinement for borderline cases. |

`friction_shape` is structurally a reference shape: the user is naming an observed-but-unfiled friction; resolving it means *suggesting filing it as a bug* (no other resolver does this). See §7 for the hook-supersession story.

### 2.2 Detection priority

When multiple shapes could apply to one token, the detector emits one `Reference` per shape with the dispatcher (§3) choosing priority order on resolve:

```
slug-shaped (chain → task → bug)
  → path-shaped
    → skill-shaped → project-shaped → tool-shaped → schema-shaped → library-shaped
      → domain-term-shaped
        → external-technical-shaped
          → friction-shape (whole-message-level, not token-level)
```

Higher-precision lookups go first because they're cheap (O(10ms) SQLite query) and authoritative (a slug match in the chains table is the answer; no further dispatch needed). Domain-term and external-technical resolvers are expensive (Qwen/Kiwix call, 600ms–2s) and right-tailed in confidence; they run only when the cheap resolvers miss or when the shape itself is non-slug-shaped.

### 2.3 What is NOT a reference (when NOT to fire)

The detector emits **zero** references for:

- **Trivial messages.** "yes", "go on", "make the change", "thanks" — no tokens of the recognized shapes.
- **Generic English.** "tickets", "tasks", "the project" — category terms, not references. The detector's regex/list-match scope excludes these by construction.
- **Inline-bound references.** "Work on `ableton-wine-setup` — that's the chain for getting Ableton running under Wine." When the user binds the reference inline, the detector still emits it (the resolver call is cheap, and confirming-the-binding is a valid outcome). Empirically: ~5ms cost, large debug-value win.
- **References already in current context.** The detector does NOT track conversation state. De-duplication is the *agent*'s discipline (per the v1 skill: "If the user mentions agent-first-substrate and that chain is in current context, the reference is already bound — don't re-lookup"). The MCP action does not refuse calls for already-bound references; it returns the resolution and the agent decides whether to surface it again.

### 2.4 Detection performance budget

- Rule-based detectors run pure Go (no LLM call); target ≤5ms for messages up to 1000 chars.
- Domain-term and friction-shape rubric runs route through `go/internal/measure/` and target ≤500ms (existing rubric latency baseline).
- Total `Detect(ctx, text)` call caps at 600ms with parallel rubric calls (the two rubrics run concurrently).

### 2.5 Deterministic detection

The rule-based detectors are deterministic for the same input. The rubric path inherits whatever determinism `go/internal/measure/` offers (fixed seed when available); for the user-facing flow, two calls with the same message produce the same `Reference` list up to rubric variance, which is in practice low for the binary "is-domain-term" classification.

### 2.6 T2 implementation notes

**Landed in commit (T2 close):** the detector ships at `go/internal/refresolve/` with one file per shape group:

- `doc.go` — package documentation with the four-field block.
- `types.go` — `Reference`, `ShapeCategory`, `ConfidenceTier`, `Candidate`, `HitSet`, `ResolverCostHint`, `Resolver` interface. T3 consumes these.
- `detect.go` — the `Detector` struct, `NewDetector`, `Detect(ctx, text)` entrypoint, priority-sort, dedupe.
- `detect_slugs.go` — `chain_slug` / `task_slug` / `bug_slug` rule-based detectors via the shared `detectSlugAgainstCatalog` helper.
- `detect_filesystem.go` — `path` / `skill_name` / `project_name` / `tool_name` / `forge_schema` / `library_entry` detectors. Library titles use the only case-insensitive path; everything else is case-sensitive exact match.
- `detect_heuristic.go` — `external_technical` heuristic via the title-cased phrase regex + the domain-term entry point.
- `catalogs.go` — `LoadCatalogs(ctx, repoRoot, pool)` reads filesystem catalogs (skills/, action-manifests/, blueprints/forge-schemas/) and DB-backed slug / library catalogs.
- `domain_term_classifier.go` — `DomainTermRubricClassifier` implements the `DomainTermClassifier` interface by wrapping the rubric registry + inference router; T7 hot-swaps a trained classifier behind the same interface.
- `detect_test.go` — covers acceptance scenarios (a)–(e), path detection, tool/schema/project detection, library title case-insensitive match, catalog gating (kebab-not-in-catalog is skipped), latency budget (rule-based median ≤5ms on 1000-char message across 5 runs), empty-message, position-sort, and determinism.

**One deviation from the design's §2.2 phrasing:** the heuristic `external_technical` step runs AFTER the rubric-based `domain_term` step (the design listed both at the end of the priority order; T2 fixed the orchestration order so domain_term preempts external_technical when both regex-match the same phrase, matching the priority intent). The dispatcher's short-circuit logic (T3) will further prune cross-shape duplicates per `single_exact` hits.

**Rubric blueprint at `blueprints/rubrics/reference-domain-term-detector.toml`** ships with `is_deployed = true`, twelve worked examples, and a three-label output enum (`domain-term` / `not-domain-term` / `unclear`). Confidence mapping inside the classifier wrapper: `domain-term` → 0.8, `unclear` → 0.5, `not-domain-term` → 0.0. The detector threshold is 0.6, so `domain-term` passes and `unclear` does not — matching the rubric's "bias toward not-domain-term when generic" guidance.

**Per-call timeout** in the rubric path: `context.WithTimeout(ctx, 800ms)` inside `DomainTermRubricClassifier.IsDomainTerm`, giving headroom over the design doc's 500ms target on slow Qwen days. Failure → permissive fallback (Detect continues without domain-term refs) per the design constraint.

### 3.7 T3 implementation notes

**Landed in commit (T3 close):** the resolver registry, dispatch core, and eleven resolvers ship at `go/internal/refresolve/`:

- `registry.go` — `Registry` struct with `Register` / `Get` / `All` / `Shapes` methods. Per-instance, not package-global.
- `dispatch.go` — `Dispatch(ctx, registry, refs, opts) (map[Reference]HitSet, error)` with priority-ordered, best-effort, budget-bounded execution. `DispatchOptions` is the tuning struct (zero values give design defaults: 2s total budget, 4× per-resolver multiplier, 10 candidates per resolver).
- `tier_classify.go` — `classifyTier(candidates, shape)` is the single source of truth for tier classification. Tested directly via `TestDispatch_TierClassification`. Domain-term and external-technical have shape-specific thresholds (0.5 for weak/strong split on domain-term; 0.8 for single-exact promotion on external-technical).
- `resolvers_work.go` — `chainResolver`, `taskResolver`, `bugResolver`. Each is a thin SELECT against the toolkit DB returning one Candidate per matching row.
- `resolvers_filesystem.go` — `pathResolver` (os.Stat), `skillResolver`, `toolResolver`, `schemaResolver`, `projectResolver` (closed list match).
- `resolvers_knowledge.go` — `libraryResolver` (direct DB lookup by dewey), `domainTermResolver` (wraps `knowledge.HandleKnowledgeSearch`), `externalTechnicalResolver` (wraps `knowledge.HandleKiwixSearch`).
- `build_registry.go` — `BuildProductionRegistry(deps ProductionDeps) *Registry` wires every resolver against supplied dependencies. Partial deps yield partial registries; missing-shape Dispatch calls return `TierNoHit + "no resolver registered" error` without aborting.
- `dispatch_test.go` — covers acceptance scenarios (a)–(d): resolver-shape, priority short-circuit, resolver-failure-best-effort, latency cap; plus total-budget-exceeded, candidate-limit, missing-resolver, tier classification table, registry priority ordering, empty-refs, nil-registry.
- `resolvers_test.go` — integration tests for chain / task / bug / path / project resolvers using `testutil.NewTestDB`.

**Two deviations from the design:**

1. **Registry is per-instance, not package-global** (design doc §3.2 described an init()-time pattern mirroring projections). Reason: resolvers carry concrete dependencies (DB pool, knowledge.Deps, inference router) that can't be installed at init time — they need the server's startup wiring. Per-instance `Registry` with a `BuildProductionRegistry` builder gives the same extensibility (new shape = new resolver file + one Register call inside the builder) without the global-state cost. Tests construct empty Registries and register mock resolvers cleanly.

2. **`library_entry` shape uses `dewey` instead of `slug`** (design doc §2.1 named `library_entries.slug`). Reason: the `library_entries` table has no `slug` column — dewey IS the identifier. The catalog loader populates `Catalogs.LibrarySlugs` from `dewey`; `Catalogs.LibraryTitles` is left empty because the table has no clean title column (citation is verbose prose; establishes is descriptive). Title-style fuzzy matching against library entries is left to T7's trained reranker.

**Tier classification thresholds** (design open question #2 — per-resolver `tier_thresholds` block in `dispatch-policy.toml`): not yet wired through dispatch-policy. Current behavior: thresholds are hardcoded in `tier_classify.go` (chain/task/bug/path/skill/tool/schema/project: ≥0.95 single-exact; domain-term: ≥0.8 single-exact, <0.5 weak; external-technical: ≥0.8 single-exact, else weak). T5 or a follow-on threshold-tuning task lands the config-knob form.

**Short-circuit semantics:** the dispatcher short-circuits LOWER-priority resolvers for the **same token** when a higher-priority resolver returns `TierSingleExact`. Different tokens have independent short-circuit state. Verified by `TestDispatch_PriorityShortCircuit`.

**Best-effort error semantics:** resolver failures (Go-error return OR HitSet.Err set) DO NOT abort other resolvers. The dispatcher records the error on the per-Reference HitSet and continues. Verified by `TestDispatch_ResolverFailureBestEffort`.

**Latency budget enforcement:** per-resolver budget = `Cost().TypicalMs × DispatchOptions.PerResolverMultiplier`. Budget exhaustion returns `ErrResolverBudgetExceeded` on the HitSet; total-dispatcher-budget exhaustion returns `ErrTotalBudgetExceeded` on subsequent un-reached resolvers. Both verified.

### 6.6 T5 implementation notes

**Landed in commit (T5 close):**

- `crates/shared-db/migrations/040_grounding_events_reference_resolution_source.sql` — Path A CHECK widen via the SQLite table-rebuild idiom (CREATE new table with wider CHECK, INSERT SELECT, DROP old, RENAME new, reinstall indexes from 019/034/037). FKs from `query_interactions` and the telemetry projections (`migration 038`) reference `grounding_events(id)` by name; the RENAME preserves them. The new CHECK admits `'reference_resolution'` alongside the existing four values; misspellings still reject.
- `go/internal/refresolve/handler.go` — `emitGroundingEvents` helper writes one row per detected reference inside one write tx. Synthesizes `call_id = "<span>#r<i>"` per-reference so the `(session_id, call_id)` UNIQUE constraint admits N rows from one `tools/call`. `query_source = 'reference_resolution'`; `span_id` / `session_id` inherited from `obs.SpanFromContext(ctx)` (falls back to `events.SpanIDFromContext` for test drivers). `user_message_id` and `query_text` populated from ctx + the action's `message_text` param. Emit failure is logged at WARN and the per-reference `GroundingEventID` field is left at 0; the resolution result still ships.
- `ResolvedReference.GroundingEventID` is now populated so downstream consumers (T6 friction-shape verifier, dashboards, training-data extraction) can join back to `grounding_events`.
- `go/internal/refresolve/telemetry_test.go` — covers four T5 acceptance scenarios: (a) per-reference grounding row emit + `query_source='reference_resolution'`; (b) CHECK widening admits new value AND rejects misspellings AND preserves pre-existing values; (c) span_id propagates from ctx; (d) `was_injected` defaults 0 on downstream query_interactions rows.

**One implementation note:** the unique constraint `(session_id, call_id)` on grounding_events was designed for one-row-per-`tools/call`; resolve_references emits N rows from one call. Synthesizing `call_id = "<span>#r<i>"` preserves the constraint without schema change. T6 (friction-shape extension) follows the same pattern.

**Citation propagation is unchanged.** The existing four-tier `click_kind` detector (`docs/TELEMETRY_SUBSTRATE.md` §5) runs over `grounding_events` rows without distinguishing by `query_source`; resolve_references-sourced rows flow through the same path. T5 verifies this via `TestT5_WasInjectedDefaultsZero` (downstream query_interactions row default is 0 — agent-initiated, not injected).

### 7.6 T6 implementation notes — code half landed; settings.json edit pending user confirmation

**Code half (landed in commit at T6 close):**

- `go/internal/refresolve/detect_friction.go` — `frictionPatterns` list (8 hook-derived patterns + 8 broader patterns) and `detectFrictionShape(message)` whole-message-level matcher. Emits at most one `ShapeFrictionShape` reference per message. Pattern list is closed; expansion requires a chain-level decision (the verification doc's coverage promise depends on it).
- `go/internal/refresolve/resolver_friction.go` — `frictionResolver` returns a filing-suggestion HitSet (TierSingleExact, single Candidate). Uniform-contract path per design open question #3 — no separate handler dispatch for friction.
- `go/internal/refresolve/build_registry.go` — registers `frictionResolver{}` unconditionally (no dependencies).
- `go/internal/refresolve/handler.go` — `presentSingleExact` formatter adds a `ShapeFrictionShape` arm producing the filing-suggestion prose.
- `go/internal/refresolve/detect_friction_test.go` — canonical `frictionTestCases` table (5 positive + 2 negative inputs, 100% coverage) plus `TestHandleResolveReferences_FrictionSuggestion` confirming PresentedAs includes "consider filing" + "forge".
- `process-docs/adhoc/reference-resolution-friction-supersession-verification.md` — verification document with coverage table, hook-trigger-phrase cross-reference, hook-retirement steps, and rollback procedure.

**Settings.json edit (pending user confirmation):** removing the `friction-filing-reminder.sh` entry from `~/.claude/settings.json`'s `Stop.hooks` block. The T6 commit documents the change; the edit lands after user confirmation per CLAUDE.md's "executing actions with care" framing. The script file at `~/.claude/hooks/friction-filing-reminder.sh` stays in place as a rollback artifact.

**Rollback:** re-add the hook entry to settings.json's Stop block; the script file is unchanged. One-line edit; takes effect the next session.

**Boundary with grounding-events-processor.sh:** `grounding-events-processor.sh` belongs to `query-telemetry-substrate`'s scope, not this chain's. Its retirement is owned by that chain's TT4 retrospective (when the live-emit path supersedes batched processing). This chain's T6 explicitly does NOT touch it.

---

## 3. Resolver registry contract

### 3.1 The Go interface

```go
// go/internal/refresolve/resolver.go (T2 + T3 land this)
package refresolve

import "context"

type ShapeCategory string

const (
    ShapeChainSlug         ShapeCategory = "chain_slug"
    ShapeTaskSlug          ShapeCategory = "task_slug"
    ShapeBugSlug           ShapeCategory = "bug_slug"
    ShapePath              ShapeCategory = "path"
    ShapeSkillName         ShapeCategory = "skill_name"
    ShapeProjectName       ShapeCategory = "project_name"
    ShapeToolName          ShapeCategory = "tool_name"
    ShapeForgeSchema       ShapeCategory = "forge_schema"
    ShapeLibraryEntry      ShapeCategory = "library_entry"
    ShapeDomainTerm        ShapeCategory = "domain_term"
    ShapeExternalTechnical ShapeCategory = "external_technical"
    ShapeFrictionShape     ShapeCategory = "friction_shape"
)

type Reference struct {
    Token           string         // the substring extracted from the message
    Shape           ShapeCategory  // detector's classification
    Confidence      float64        // detection confidence in [0, 1]; rubric output for domain terms, 1.0 for exact-list-match
    DetectionMethod string         // "regex+list_match" / "rubric" / "filename_match" / etc.
    StartPos, EndPos int           // byte offsets in the source message
}

type ConfidenceTier string

const (
    TierSingleExact ConfidenceTier = "single_exact"   // exactly one matching candidate; use it
    TierFuzzyMulti  ConfidenceTier = "fuzzy_multi"    // 2-5 candidates; ask user to disambiguate
    TierWeakDomain  ConfidenceTier = "weak_domain"    // domain-term hit with low relevance; surface as possibly-relevant
    TierNoHit       ConfidenceTier = "no_hit"         // resolver found nothing
)

type Candidate struct {
    ID         string  // resolver-specific identifier (chain slug, file path, etc.)
    Title      string  // human-readable label
    Score      float64 // resolver-specific score; not comparable across resolvers
    SourceRef  string  // canonical pointer (the value that lands in grounding_events.source_refs)
    DebugNotes string  // optional; "rank 1 in chain_find" / "fts5 score 0.83"
}

type HitSet struct {
    ResolverName    string
    ConfidenceTier  ConfidenceTier
    Candidates      []Candidate
    RetrievalCostMs int64
    Err             error  // non-nil if the underlying tool failed; tier=no_hit in that case
}

type ResolverCostHint struct {
    TypicalMs int64  // expected latency; used by dispatcher for ordering and budget
}

type Resolver interface {
    Shape() ShapeCategory
    Resolve(ctx context.Context, ref Reference) (HitSet, error)
    Cost() ResolverCostHint
}

// Package-level registry (mirrors the projections package pattern).
func Register(r Resolver)
func All() []Resolver
func Get(shape ShapeCategory) (Resolver, bool)
```

### 3.2 Registration pattern

Each resolver lives in its own file (`chain_resolver.go`, `task_resolver.go`, …) and registers in `init()`:

```go
// go/internal/refresolve/chain_resolver.go
func init() { Register(chainResolver{}) }

type chainResolver struct{}

func (chainResolver) Shape() ShapeCategory   { return ShapeChainSlug }
func (chainResolver) Cost() ResolverCostHint { return ResolverCostHint{TypicalMs: 10} }

func (chainResolver) Resolve(ctx context.Context, ref Reference) (HitSet, error) {
    // Thin wrapper around the existing work.chain_find handler.
    // Returns one Candidate per matching chain.
}
```

Adding a new shape category is one file + one `init()` call. **No core changes** to the dispatcher, the detector, or the MCP action. This is the load-bearing extensibility property the design commits to.

### 3.3 Error semantics

- **Resolver returns `(HitSet{Err: nil, Tier: no_hit}, nil)`** when the underlying tool succeeded but found nothing. Normal.
- **Resolver returns `(HitSet{Err: theError, Tier: no_hit}, nil)`** when the underlying tool errored (DB unavailable, FTS5 syntax error, Kiwix timeout, …). The dispatcher continues with other resolvers; the error surfaces in debug output and in the action's response envelope's `partial_failures` field.
- **Resolver returns `(_, err)`** ONLY for programmer errors (nil context, malformed `Reference`). The dispatcher logs and aborts the single resolver but continues with others.

The dispatcher is **best-effort across shapes**: a failure of one resolver does not abort others. This matches the v1 skill's spirit (if `chain_find` fails, fall back to `vault_search` for context).

### 3.4 Confidence-tier classification

Tier classification happens in the **dispatcher**, not per-resolver. Single source of truth. Rules:

| Tier | Condition | Presentation |
|---|---|---|
| `single_exact` | Exactly one candidate AND `score >= 0.95` (or resolver-specific exact-match indicator) | Name what you found. Proceed. |
| `fuzzy_multi` | 2-5 candidates, OR one candidate with `score < 0.95` | List candidates; ask user to disambiguate. |
| `weak_domain` | Domain-term hit with `score < 0.5` (Qwen rerank threshold) | Surface as possibly-relevant; don't assume answer. |
| `no_hit` | Zero candidates returned | "I don't have a binding for `X`. Could you point me at it, or did you mean a different reference?" Do NOT proceed by guessing. |

For `external_technical` shape (kiwix-backed), the tier rules track the kiwix score distribution: `single_exact` if one hit at score ≥ 0.8, `weak_domain` otherwise. The dispatcher consults a per-resolver `tier_thresholds` block in `dispatch-policy.toml` (loaded once at startup); thresholds are documented per-shape, not hardcoded.

### 3.5 Cross-resolver short-circuits

The dispatcher executes resolvers in priority order (§2.2). If a higher-priority resolver returns `single_exact`, lower-priority resolvers for the **same token** are skipped:

- "ableton-wine-setup" → `chain_resolver` returns `single_exact` → skip `task_resolver` and `bug_resolver` for that token.
- "ableton" → `chain_resolver` returns `fuzzy_multi` → continue to `task_resolver` and `bug_resolver` (the token might be a partial slug for either).

Different tokens in the same message run their own dispatcher passes; short-circuit is per-token.

### 3.6 Dispatcher budget

Total dispatcher time per `Dispatch(ctx, refs)` call caps at 2 seconds. Resolvers exceeding their `Cost().TypicalMs * 4` budget are short-circuited and contribute a `HitSet{Tier: no_hit, RetrievalCostMs: budgetExceeded}` row. The action handler (§5) gets a partial result with a `truncated_for_latency` marker rather than waiting indefinitely.

---

## 4. Confidence tiers and presentation

### 4.1 Tier-specific presentation rules

The presentation contract is **verbose and explicit**. The agent NAMES what it found. Silent context injection is the territory of the deferred proactive-injection feature, **not this chain**.

| Tier | `PresentedAs` template | `RecommendedAction` |
|---|---|---|
| `single_exact` | `` `<token>` → <kind> in <project>, <metadata>`` (e.g., `` `ableton-wine-setup` → chain in mcp-servers, 6 tasks (4 pending, 2 blocked), open since 2026-05-10``) | `use_directly` |
| `fuzzy_multi` | `` `<token>` matched <n> candidates: 1) <title> (<context>); 2) <title> (<context>); …`` | `ask_user_to_disambiguate` |
| `weak_domain` | `` `<token>` may refer to: <closest-match> (rank 1, weak)`` | `mention_as_possibly_relevant` |
| `no_hit` | `` `<token>` did not resolve in any <shape> index.`` | `acknowledge_no_hit_and_ask` |

The presentation strings are produced by the dispatcher and returned in the `ResolvedReference.PresentedAs` field. Agents consume them verbatim or paraphrase per their flow; the contract is that the **source attribution** ("chain in mcp-servers", "vault note at <path>") is always present.

### 4.2 Three purposes the named-what-you-found pattern serves

(From the v1 skill, preserved here as the discipline T4's handler implements):

1. **Disambiguation** — if the user meant a different slug, they correct before the agent starts work in the wrong direction.
2. **Debuggability** — the user sees what context the agent acquired and why it shaped the response. Silent injection has the opposite property.
3. **Confidence calibration** — strong-hit phrasing ("found X") distinguishes from weak-hit phrasing ("the closest match was Y, but the score was low — is that what you meant?").

### 4.3 Cross-category hits

A token matching multiple shapes (e.g., `agent-first-substrate` matches both `chain_slug` and `forge_schema` if a `forge-schemas/agent-first-substrate.toml` exists) produces one `ResolvedReference` per shape, each with its own tier and `PresentedAs`. The dispatcher does NOT merge them; the agent decides which to surface. Empirically rare in current data; the substrate prefers explicitness over auto-merging.

---

## 5. Orchestration and the `resolve_references` MCP action

T4 ships the public surface: a new action under the `knowledge` meta-tool that agents call to resolve a message's references in one shot.

### 5.1 Action signature

```
knowledge(action='resolve_references', params={
    message_text: string,          // the user message to scan
    top_k_per_shape?: int,         // default 5; per-shape candidate limit
    include_no_hits?: bool,        // default false; whether to return references that returned no_hit
})
```

Returns `ResolveReferencesResult`:

```go
type ResolveReferencesResult struct {
    References        []ResolvedReference
    ResolutionTimeMs  int64
    ResolverCallsMade int
    NoHitTokens       []string
    PartialFailures   []string  // resolver errors that didn't abort the call
    TruncatedByBudget bool      // true if dispatcher hit the 2.5s cap
}

type ResolvedReference struct {
    Token              string
    Shape              ShapeCategory
    ConfidenceTier     ConfidenceTier
    PresentedAs        string                    // human-readable summary; agent-consumable
    TopCandidates      []Candidate
    RecommendedAction  PresentationRecommendation
    GroundingEventID   int64                     // FK back to grounding_events; debuggable (T5)
}

type PresentationRecommendation string

const (
    PresentUseDirectly             PresentationRecommendation = "use_directly"
    PresentAskUserToDisambiguate   PresentationRecommendation = "ask_user_to_disambiguate"
    PresentMentionAsPossiblyRelevant PresentationRecommendation = "mention_as_possibly_relevant"
    PresentAcknowledgeNoHitAndAsk    PresentationRecommendation = "acknowledge_no_hit_and_ask"
)
```

### 5.2 Handler location

`go/internal/knowledge/resolve_references_handler.go` registers under the `knowledge` meta-tool. The handler is a thin pass-through: detect → dispatch → format → emit telemetry → return.

```go
// Pseudocode
func (h *Handler) ResolveReferences(ctx context.Context, params Params) (ResolveReferencesResult, error) {
    refs, err := refresolve.Detect(ctx, params.MessageText)
    if err != nil { return zero, err }

    hits, err := refresolve.Dispatch(ctx, refs)
    if err != nil { return zero, err }

    var out ResolveReferencesResult
    for _, ref := range refs {
        hs := hits[ref]
        ge, _ := telemetry.EmitGroundingEvent(ctx, telemetry.EmitArgs{
            Action:        "resolve_references",
            QuerySource:   "reference_resolution",   // §6.1 Path A
            QueryText:     ref.Token,
            SourceRefs:    candidateSourceRefs(hs.Candidates),
            ResultsCount:  len(hs.Candidates),
            SpanID:        spanFromCtx(ctx),
            UserMessageID: userMessageIDFromCtx(ctx),
        })
        out.References = append(out.References, formatResolved(ref, hs, ge.ID))
    }
    return out, nil
}
```

### 5.3 Manifest

The action manifest lands at `action-manifests/resolve-references.toml` (skeleton at `action-manifests/resolve-references.toml.skeleton` — T1 artifact; T4 promotes). The companion instruction file at `action-manifests/instructions/resolve-references.md` documents the call shape, the result shape, and the agent-side discipline (named-what-you-found, never silent).

### 5.4 Dispatcher rationale-requirement

`resolve_references` is a **read-only** action. Dispatch policy: `requires_rationale = false`. Rationale enforcement (§5 of `EVENT_SUBSTRATE.md`) is opt-in per-action via `dispatch-policy.toml`; read-only resolvers do not require it.

### 5.5 Latency cap

Total handler call (including detect + dispatch + format + telemetry emit) caps at 2.5 seconds. If exceeded, return the partial result with `TruncatedByBudget = true`. Agents inspect this flag and either proceed with partial bindings or surface the truncation to the user.

---

## 6. Telemetry integration

### 6.1 `query_source` enum extension — **DECISION: Path A**

`query-telemetry-substrate` TT2 (migration 037, shipped 2026-05-17) landed the `query_source` CHECK constraint at:

```sql
CHECK (query_source IN ('agent_initiated', 'proactive_hook', 'dashboard_user', 'other'))
```

with `'other'` as the open-fallback. This chain introduces two new query sources — `reference_resolution` (T5) and `harness_reminder_interception` (T9). The choice:

**Path A (recommended, adopted)** — Ship CHECK-widening migrations to add `reference_resolution` and `harness_reminder_interception` as first-class enum values. Downstream `training_data_for_reranker` pivots include them by name.

**Path B (rejected)** — Use `'other'` for both, with a secondary discriminator field (e.g., a new `query_subsource TEXT` column on `grounding_events`) to distinguish.

**Rationale for Path A:**

- Both use cases are **stable, named, well-bounded** with known lifetimes. They are not speculative exploration sources; they are first-class telemetry citizens for the duration of this substrate.
- The CHECK-widening cost is **one ALTER per chain task** — `T5` and `T9` each ship a one-line `DROP CHECK + ADD CHECK` migration. SQLite doesn't support `ALTER TABLE … DROP CONSTRAINT` directly; the idiom is to rebuild the column via a transaction (documented in `crates/shared-db/migrations/036_telemetry_substrate.sql.skeleton`).
- `training_data_for_reranker` (TT3 projection) **already pivots by `query_source` name** in its sample SELECT (`docs/TELEMETRY_SUBSTRATE.md` §6.3). Path B would require a JOIN to a separate `query_subsource` column for every consumer — load-bearing complexity for marginal flexibility benefit.
- Reserve Path B (`query_subsource` column) for **genuinely speculative or short-lived sources** that don't yet justify a CHECK widen. None apply in this chain.

The doc commits T5 + T9 to ship the migrations as part of their scope. After both migrations land, the constraint reads:

```sql
CHECK (query_source IN (
    'agent_initiated', 'proactive_hook', 'dashboard_user',
    'reference_resolution', 'harness_reminder_interception',
    'other'
))
```

**Migration sequencing:** T5 lands first (chain ordering: T5 is gated on T4, T9 is gated on T4 — but T5 also gates T6 and is on the chain's critical path). T9's migration extends the constraint produced by T5's migration. Both migrations are synced to the two Go-embed mirror dirs per `scripts/sync-migrations.sh`.

### 6.2 What gets emitted per call

Every `resolve_references` call emits **one `grounding_events` row per detected reference** (not one per call). Multiple references in one message produce multiple rows, all sharing `span_id` (the per-`tools/call` ID — `resolve_references` is one `tools/call`, but each Reference is logically its own retrieval pass against the registry).

| `grounding_events` column | Value |
|---|---|
| `action` | `resolve_references` (new action name; recorded as-is in `action_name` column). For per-resolver granularity, the source_ref candidates encode which resolver they came from (`chain:<slug>`, `vault:<path>`, etc.). |
| `query_source` | `reference_resolution` |
| `query_text` | The `Reference.Token` (the substring detected, not the whole message). The whole message is preserved via the `user_message_id` FK to the transcript JSONL record. |
| `source_refs` | JSON array of `Candidate.SourceRef` values returned by the dispatcher. |
| `results_count` | `len(Candidates)`. |
| `session_id`, `span_id`, `prompt_id`, `parent_span_id` | Inherited from ctx (per `agent-first-substrate` T5 contract; `prompt_id` may be NULL at live-emit and stamped post-session by the Stop hook). |
| `user_message_id` | The transcript JSONL UUID of the user message that the `resolve_references` call is scanning. |
| `was_injected` (on `query_interactions`) | `0`. Reference resolution is agent-initiated, **not injected**. The proactive-injection chain's future hook will be the only emitter that sets this to `1`. |

### 6.3 What counts as 'cited' for a resolved reference

The existing four-tier `click_kind` detector (`docs/TELEMETRY_SUBSTRATE.md` §5) runs unchanged on `grounding_events` rows with `query_source='reference_resolution'`. Concretely:

- `followed` — agent subsequently calls `chain_status` / `task_read` / `bug_read` / `vault_read` / `Read` on the resolved `source_ref` within the same `span_id` chain. The reference was deliberately followed.
- `cited` — the resolved candidate's `source_ref` appears in the next assistant turn as either a substring quote (≥40 chars) or a markdown link / `file:line` reference. The agent surfaced the binding to the user.
- `mentioned` — the `source_ref` string itself appears in subsequent assistant text within the same `prompt_id`. The weakest tier.
- `resolved-from` — a terminal write-side event (`BugResolved`, `TaskCompleted`, `ChainClosed`) within the same `prompt_id` references the `source_ref` in its rationale. The strongest tier.

No new emit logic in this chain. T5 verifies the standard path picks up `resolve_references`-originated grounding events the same as agent-initiated ones (the verification is one of T5's acceptance tests).

### 6.4 Training-data shape

`training_data_for_reranker` (TT3 projection) automatically picks up `reference_resolution`-sourced rows. Its `query_source` column distinguishes them from `agent_initiated`. Two model-training cuts:

```sql
-- Agent-initiated retrievals only (the default reranker training cut).
SELECT * FROM proj_training_data_for_reranker
 WHERE query_source = 'agent_initiated';

-- Reference-resolution retrievals only (a separate training cut for tuning
-- the resolver's confidence thresholds and presentation choices).
SELECT * FROM proj_training_data_for_reranker
 WHERE query_source = 'reference_resolution';
```

Mixing the two cuts is a downstream consumer choice, not a substrate enforcement. The two query sources have **different agent-behavior dynamics** (agent-initiated retrievals reflect what the agent thought it needed; reference-resolution retrievals reflect what the agent missed binding), so training a single reranker on both conflates the signals.

### 6.5 Sample telemetry queries

```sql
-- Reference-resolution success rate, last 7 days.
SELECT
  COUNT(*) AS total_references,
  SUM(CASE WHEN results_count > 0 THEN 1 ELSE 0 END) AS resolved,
  SUM(CASE WHEN results_count = 0 THEN 1 ELSE 0 END) AS no_hit
FROM grounding_events
WHERE query_source = 'reference_resolution'
  AND created_at >= datetime('now', '-7 days');

-- Top no-hit tokens (candidates for index gaps or detector refinement).
SELECT query_text AS token, COUNT(*) AS occurrences
FROM grounding_events
WHERE query_source = 'reference_resolution' AND results_count = 0
GROUP BY query_text
ORDER BY occurrences DESC LIMIT 20;

-- Resolved references that got cited in the next assistant turn.
SELECT ge.query_text, ge.source_refs, qi.click_kind, qi.click_weight
FROM grounding_events ge
JOIN query_interactions qi ON qi.grounding_event_id = ge.id
WHERE ge.query_source = 'reference_resolution'
  AND qi.click_kind IN ('cited', 'resolved-from')
ORDER BY ge.created_at DESC LIMIT 50;
```

---

## 7. Hook supersession

This chain retires **one** Stop hook (`friction-filing-reminder.sh`) via a contextual friction-shape detector. Other always-on reminder hooks are explicitly out of scope (§7.3); they belong to sibling chains or remain as-is.

### 7.1 Hooks in scope

| Hook | Triggering surface | What it does today | Supersession path | Owner |
|---|---|---|---|---|
| `~/.claude/hooks/friction-filing-reminder.sh` | Stop | Scans assistant text for "also noted", "could file", etc. trigger phrases; cross-references against bug-file count in unified DB; blocks Stop with reminder when phrase-hits > filings. | `friction_shape` reference category (T6) — the resolver returns a HitSet with confidence tier `friction_observed` and a presentation recommendation `'suggest filing as bug via work(action=forge, schema_name=bug, …)'` populated with the relevant template. Agent sees the suggestion at the natural point in its flow (post-detection, pre-response), not at session end. | This chain (T6). |
| `~/.claude/hooks/grounding-events-processor.sh` | Stop | Walks transcript JSONL post-session; emits `grounding_events` + `query_interactions` rows from detected click signals. | TT2 of `query-telemetry-substrate` ships a live-emit path that supersedes batched session-end processing. | Sibling chain (`query-telemetry-substrate` TT4 retrospective handles retirement). |

### 7.2 Replacement contractual sequence

The replacement ships **before** the hook retires. Concretely, T6's acceptance criteria enforce:

1. `friction_shape` category lands in the detector (T2 scope, extended in T6).
2. `friction_shape` resolver lands in the registry (T3 scope, extended in T6). Resolves to a "filing suggestion" rather than a binding.
3. The `resolve_references` handler (T4) returns the suggestion in `ResolvedReference.PresentedAs` when friction-shape is detected.
4. **Verification:** a test agent session with ≥3 representative friction-shape inputs (sample list documented in `process-docs/adhoc/reference-resolution-friction-supersession-verification.md` at T6 close) triggers the contextual suggestion correctly. The verification compares the contextual fire's coverage against a transcript-replay of historical hook fires; the new path must catch ≥90% of the hook's true-positive fires (the 10% margin allows for trigger-phrase variance the contextual detector intentionally widens beyond the hook's literal list).
5. **ONLY after verification:** edit `~/.claude/settings.json` to remove the `friction-filing-reminder.sh` entry from the `Stop` hooks. **Do not delete the script file** — leave it in `~/.claude/hooks/` as a rollback artifact and a historical record.
6. Smoke session post-edit confirms the hook no longer fires and the contextual detector picks up the same signal.

### 7.3 Rollback procedure

If a regression is detected after retirement (a friction signal the contextual detector misses but the hook would have caught):

1. Re-add the `friction-filing-reminder.sh` entry to `~/.claude/settings.json`'s `Stop.hooks` block (the script file is still present; no re-install needed).
2. File a follow-on task against `reference-resolution-substrate` to widen the friction-shape detector to cover the missed pattern.
3. Re-run the verification step from §7.2 once the detector is updated.
4. Remove the hook entry again only after the verification passes.

The rollback is **fast** (one-line settings edit) and **safe** (no state loss; the hook script is untouched).

### 7.4 Boundary with `grounding-events-processor.sh`

This chain does **NOT** touch `grounding-events-processor.sh`. The hook is shared between this chain's read-side telemetry consumption and `query-telemetry-substrate`'s emit path. Its retirement is owned by `query-telemetry-substrate` TT4 retrospective (when the live-emit path supersedes batched processing). This chain's T6 retrospective explicitly references that scope to prevent confusion.

### 7.5 No new always-on reminder hooks

Supersession is **one-way**: from hook-shaped to contextual-shaped. This chain does NOT add new always-on reminder hooks. Future contextual reminders (e.g., "you committed a change with no tests in the diff") land as additional shape categories under the resolver registry, not as Stop/PreToolUse hooks.

---

## 8. Harness task-tools reminder interception (T9)

> **⚠️ REMOVED 2026-05-25 — this subsystem is no longer live.** The over-firing "task tools haven't been used recently" reminder was resolved structurally by a bare-name `permissions.deny` on `TaskUpdate`/`TaskCreate`/`TodoWrite` in `~/.claude/settings.json`: the deny strips those tools from the session, tripping the harness reminder's own tool-presence gate so it can never fire. The T9 UserPromptSubmit hook (`intercept-task-tools-reminder.sh`) + its `GET /admin/harness-reminder-relevance` Go relevance-gate endpoint never reliably worked anyway (the reminder is an internal attachment, not in the UserPromptSubmit payload — see §8.9) and were removed in commit `5b037b41`. See bug `task-tools-reminder-overfires-when-mcp-work-tools-are-active` (resolved fixed). The rest of §8 is retained as the design record of what was built. NOTE: the `harness_reminder_interception` `query_source` enum value + its CHECK-widening migration were **not** reverted — historical `grounding_events` rows stay valid; the value is now producer-less.

T9 ships a **UserPromptSubmit hook**, not a Stop hook. The semantics are different: UserPromptSubmit fires on every incoming user prompt before the agent sees it, and the hook can rewrite the prompt content. T9 uses this to **strip** the upstream Claude Code "task tools haven't been used recently" reminder when the relevance gate fires negative.

### 8.1 What the harness reminder looks like

The upstream Claude Code harness fires a system-reminder block on a wall-clock heuristic that over-fires badly in practice — observed ~12 times/session even during active task-tool use. The reminder begins with the stable substring:

```
The task tools haven't been used recently. If you're working on tasks
that would benefit from tracking progress
```

This substring is the recognition handle. T9 must verify against current fires before committing to the exact bytes — sampling discipline matches the v1 reference-resolution skill's "trust the running data" reflex.

### 8.2 Interception contract

`hooks/intercept-task-tools-reminder.sh` reads stdin (UserPromptSubmit payload, JSON), inspects the prompt for the recognized substring, decides via the relevance gate, and emits the modified prompt to stdout. Three paths:

| Decision | Trigger | Action |
|---|---|---|
| **Strip** | Recognized substring matches AND relevance gate returns "agent used task tools in the last N turns" (default N=3). | Remove the entire system-reminder block; pass the rest of the prompt through unchanged. |
| **Preserve** | Recognized substring matches AND relevance gate returns "no task-tool call has fired in N turns AND pending tasks exist". | Pass the prompt through unchanged (the reminder is doing its intended job). Alternative: replace with a contextual variant naming specific stale tasks; this variant is opt-in via `TOOLKIT_HOOK_CONTEXTUAL_REPLACE=1`. |
| **Fail open** | Recognized substring does NOT match (text drift) OR the hook can't reach the relevance-gate endpoint. | Pass the prompt through unchanged AND log a single-line maintenance signal to `/tmp/toolkit-hook-drift.log`. Never silently strip an unrecognized reminder; never block the user's prompt on hook failure. |

### 8.3 Relevance gate endpoint

The relevance gate queries the toolkit-server HTTP daemon (the Go binary's `observehttp` surface, port resolved from the same source-of-truth the dashboard reads — `go/launch.sh` and `go/internal/observehttp/router.go`; not hardcoded). The exact endpoint shape is decided in T4 design; the contractual property is that the gate returns a structured response of the form:

```json
{
  "recent_task_tool_usage": {
    "fired_in_last_n_turns": true,
    "last_call_ts": "2026-05-18T14:32:00Z",
    "n_turns": 3
  },
  "pending_tasks_exist": true,
  "stale_tasks": ["task-id-1", "task-id-2"]
}
```

The hook computes the strip/preserve decision from this response. If the endpoint is unreachable, fail open per §8.2.

### 8.4 CHECK-widening migration

T9 ships `crates/shared-db/migrations/<next>_grounding_events_harness_interception_source.sql` adding `'harness_reminder_interception'` to the `query_source` CHECK (per Path A in §6.1). T9's migration extends T5's migration, so the sequencing is **T5 first, T9 second** (chain task order matches).

### 8.5 Telemetry for hook fires

Every strip or replace decision emits a `grounding_events` row with `query_source='harness_reminder_interception'`. The row's `query_text` carries the matched substring (truncated to 200 chars for analytics); `source_refs` is the structured relevance-gate response. This makes the interception analytically visible:

```sql
-- How often is the upstream reminder firing vs being stripped?
SELECT
  DATE(created_at) AS day,
  COUNT(*) AS total_fires,
  SUM(CASE WHEN json_extract(source_refs, '$.decision') = 'strip' THEN 1 ELSE 0 END) AS stripped,
  SUM(CASE WHEN json_extract(source_refs, '$.decision') = 'preserve' THEN 1 ELSE 0 END) AS preserved
FROM grounding_events
WHERE query_source = 'harness_reminder_interception'
  AND created_at >= datetime('now', '-30 days')
GROUP BY day;
```

If `total_fires` collapses to zero, either upstream Claude Code fixed the firing heuristic (the hook becomes a no-op without code changes) or the recognized substring drifted (T9's fail-open log surfaces the maintenance signal).

### 8.6 Generalization path (deferred)

T9's pattern (`UserPromptSubmit` interception with a substring match and a structured relevance gate) generalizes to other harness reminders (parallel-Bash hard-cancel, MCP-reconnect notices). The generalization is **NOT** in scope for T9 — the hook handles one known case. If the pattern proves out, a follow-on task widens the hook to a registry-of-patterns shape; the T9 deliverable documents the extension point.

### 8.7 Relevance threshold tuning

`N=3 turns` is the default for "task-tool fired recently"; the threshold is a config knob (env var `TOOLKIT_HOOK_TASK_TOOL_WINDOW=3`) so tuning doesn't require a code change. Different sessions have different cadences; a config knob lets per-host tuning without a redeploy.

### 8.8 T9 implementation notes — landed 2026-05-18

- `crates/shared-db/migrations/041_grounding_events_harness_interception_source.sql` widens the `grounding_events.query_source` CHECK to admit `'harness_reminder_interception'`. Synced to `go/internal/db/migrations/` and `go/internal/testutil/migrations/` via `scripts/sync-migrations.sh`. Sequencing matches the T1 commitment: T5 (migration 040) lands first, T9 (migration 041) second.
- `go/internal/observehttp/harness_reminder_relevance.go` adds `GET /admin/harness-reminder-relevance`. Mounted on the ServeMux outside the `state.Pool` gate because the v1 endpoint is filesystem-only — reads the session transcript using the same four-layer resolution as `grounding-events-processor.sh` (`transcript_path` hint → `cwd`-slugged → server `pwd`-slugged → glob across `~/.claude/projects/*/<session>.jsonl`). `taskToolNames` is the closed `{TaskCreate, TaskUpdate, TaskGet, TaskList}` set — a code-change surface, not a config knob, because the upstream tool set changing is event-worthy.
- `pending_tasks_exist` and `stale_tasks` are stubbed `false` / `[]` in v1: Claude Code's in-session task list isn't persisted to toolkit.db, so a v1 honest answer is no-signal. The strip path (over-fire defense) is the high-value case; the preserve path stays accurate-by-default because the missing pending signal drives the hook to preserve on no-recent-usage — correct fallback.
- `hooks/intercept-task-tools-reminder.sh` is the hook script. Recognized substring is `The task tools haven't been used recently. If you're working on tasks that would benefit from tracking progress` (matched via `grep -Fq`, no regex; drift is a code-change surface). Telemetry insert via `sqlite3` direct — keeps the install footprint minimal.
- `hooks/test-intercept-task-tools-reminder.sh` covers the four named cases (strip / preserve / drift-passthrough+log / empty-passthrough). Runs hermetically against a Python-served stub gate.
- `scripts/hooks/README.md` documents the install snippet, the env-var knobs, the drift signal, and the rollback procedure.
- Live verification artifact: `process-docs/adhoc/reference-resolution-t9-harness-interception-verification.md`. The session that produced T9 itself observed nine fires of the upstream reminder during the working window — first-person friction confirming the over-fire pattern.
- Settings.json install is **not** performed by T9. T8 (chain retrospective) installs the hook after the verification artifact lands and the user reviews. Skipped here to keep T9 a code-only landing per the same "executing actions with care" gate that staged T6's settings.json edit.

### 8.9 Coverage gap — mid-stream system-reminder fires (bug 1424)

T9 ships at `UserPromptSubmit`. Post-ship observation (2026-05-18 reference-resolution-migration session) showed the upstream reminder fires in **two structurally distinct places** in the assistant transcript:

1. **`UserPromptSubmit` envelope** — bundled with the user's prompt. The hook strips these per §8.2.
2. **Mid-stream `tool_result` envelopes** — injected by the harness into the result content of an agent tool call, in the same assistant turn, with no fresh user prompt. **The hook never sees these fires.** Observed ~20 fires across one session, the majority mid-stream.

The hook's wire test (`process-docs/adhoc/reference-resolution-t9-harness-interception-verification.md`) exercised UserPromptSubmit payloads and passed; it didn't catch the wrong-event-shape because it tested the hook's behavior on the SHAPE it was given, not on the FREQUENCY of that shape relative to total fires. T8's retrospective evidence string ("nine fires observed") conflated the two paths.

**Why mid-stream fires can't be intercepted.** Researched 2026-05-18 against Claude Code's hook documentation:

- `PostToolUse` is **observation-only**. It can add `additionalContext` but cannot modify the `tool_result` content the model reads. The system-reminder is appended to the tool result *after* all pre-execution hooks fire and *before* `PostToolUse` runs.
- `PreToolUse` modifies tool *input* only; no visibility into post-execution content.
- Only `UserPromptSubmit` and `UserPromptExpansion` (slash command expansion) can rewrite content before the model processes it. Neither sees `tool_result` injection.
- No settings.json flag, env var, or `preferredNotifChannel` value suppresses task-tools system-reminders.

**Status: upstream-blocked.** The mid-stream coverage gap is a Claude Code architectural limitation, not a substrate issue. The hook remains the right defense for the user-prompt-bundled subset (which is real and worth eliminating). The mid-stream subset waits on one of:

- A hook event that fires *after* tool execution but *before* the model reads the result, with content-mutation capability.
- A settings flag to suppress task-tools system-reminders globally.
- Direct `tool_result` content mutation from `PostToolUse`.

No corresponding Claude Code GitHub issue found at research time; filing upstream is the follow-on.

**What the hook still does well.** The strip path for UserPromptSubmit-bundled fires; the fail-open log for substring drift; the telemetry row per fire; the relevance gate (which is useful infrastructure independent of which event surfaces it). Pulling the hook would lose all of that for the subset of fires it does catch.

---

## 9. v1-skill evolution

The v1 skill (`skills/reference-resolution.md`, drafted 2026-05-16) stays valid post-chain. Its trigger discipline (when to fire, when not to fire, presentation conventions) is **teaching, not mechanism**. The mechanism gets sharper; the teaching stays.

### 9.1 What changes in the skill body

T4 updates the skill body with two additions, not rewrites:

1. **Unified-dispatch alternative.** The per-shape dispatch table in §"Reference shape taxonomy + dispatch table" gains a row at the bottom:

   > Any combination of shapes: `knowledge(action='resolve_references', params={message_text: "<user message>"})` — calls the unified resolver; returns one `ResolvedReference` per detected reference with `PresentedAs` strings ready to incorporate.

   The per-shape rows stay. Agents can still call `chain_find` directly for a single known slug; the unified action is for the multi-shape case or for the deeper telemetry signal (per §6).

2. **Friction-shape category** added to the dispatch table (after T6 lands):

   > | Friction shape | observation-of-friction phrases | suggest filing as bug via `work(action='forge', schema_name='bug', ...)` |

### 9.2 What stays unchanged

- The "when to fire" rules (ambient at session start; fires on shape-not-uncertainty).
- The "when NOT to fire" rules (trivial messages, generic English, already-bound references, inline-bound references, judgment-shape questions).
- The presentation discipline (name what you found; surface explicitly).
- The confidence rules (single-exact, fuzzy, no-hit, weak-domain, cross-category).
- The latency budget conversation (cheap lookups first; cap total resolution around 2s per turn).

The v1 skill is the **teaching surface**; the MCP action is the **mechanism**. Both can exist simultaneously, and one updates the other without invalidating the other.

### 9.3 Skill TOML changes

`skills/reference-resolution.toml` adds the `mcp__toolkit-server__knowledge` action `resolve_references` to its `tools_available` list (already includes the meta-tool). No new keyword triggers; the existing keyword list ("start work on", "what is", "look up", etc.) covers the cases the unified action helps with.

---

## 10. Future-ML boundary

T7 of this chain integrates trained ML models from the (not-yet-forged) ML capability chain. The integration boundary is specified here so T7 doesn't have to re-derive it.

### 10.1 What gets replaced when ML capability lands

| Component | Cold-start (this chain) | Trained mode (T7 + ML capability chain) |
|---|---|---|
| Domain-term classifier | Qwen rubric at `blueprints/rubrics/reference-domain-term-detector.toml` invoked via `go/internal/measure/`. | Trained sklearn classifier (per `local-ml-roadmap.md` §2.x). Same rubric infra contract; trained model registered with the router replaces Qwen as the default. |
| Cross-encoder reranker | Not present; resolver outputs are ordered by per-resolver score (chain_find's relevance, vault_search's pass2 score, etc.). | Cross-encoder ONNX model (per `local-ml-roadmap.md` §1.1) scores all candidates for a given reference together. The resolver registry's dispatcher gains a post-resolve rerank step. |

### 10.2 Hot-swap mechanism

The ML capability chain ships an admin action of the form:

```
admin(action='set_model_version', params={role: 'domain-term-classifier', version: 'sklearn-v3'})
```

Calling this action causes the next `resolve_references` call to use the new model **without server restart**. Implementation lives in the ML capability chain; this chain's only obligation is that the domain-term detector reads the active version from the router at call-time, not at startup. T7's acceptance test verifies hot-swap.

### 10.3 Fallback semantics

If the trained classifier is unavailable (router error, model load failure), the detector falls back to the Qwen rubric path. The fallback is logged but does not fail the resolve. This matches the v1 skill's resilience: missing infrastructure shouldn't block the discipline.

### 10.4 Cold-start vs trained mode comparison

T7's acceptance criteria include a comparison run: the same set of test messages resolved twice, once with Qwen rubric only, once with trained classifier + reranker. The diff report (committed at `process-docs/adhoc/reference-resolution-ml-upgrade-comparison.md` at T7 close) shows which references resolved differently, with eyeball-checkable rationale. This is the substrate-to-ML proof that the trained models earned their keep.

### 10.5 Deferral path

T7 may be deferred to a follow-on chain (`reference-resolution-ml-upgrade`) if the ML capability chain hasn't shipped by the time T6 closes. The chain's substrate value (T1–T6 + T8 + T9) lands without trained models; the trained models are the icing. The retrospective (T8) is **not** blocked on T7. This is the "T7 optional for chain closure" framing in the chain's `completion_condition`.

---

## 11. The four proactive-injection decisions, answered

`~/.claude/vault/learnings/general/2026-05-15_proactive-injection-feature-design.md` frames "proactive RAG hook" as four independent decisions stacked. The reference-resolution chain answers them differently from the deferred proactive-injection feature — agent-driven instead of system-fired — but the four decisions still apply.

| Decision | Reference-resolution answer | Proactive-injection answer (deferred) |
|---|---|---|
| **WHEN to fire.** | Agent decides per the v1 skill's "when to fire" rules. The detector and dispatcher are infrastructure; firing is a discipline. The harness-reminder interception hook (§8) is a special-case *interception* of one well-known case, not a general fires-on-every-message surface. | System-fires on every user message (with a should-fire classifier gating). Sentinel difference: agent-vs-system as the firing actor. |
| **WHAT to query with.** | The detected reference's `Token` field — a substring, not an embedding. The dispatcher passes the token to the appropriate resolver, which uses its native lookup (FTS5 for vault_search, slug-equality for chain_find, etc.). | Message embedding + extracted entities. Different query shape entirely; needs trained models. |
| **HOW to score and filter.** | Per-resolver native scoring → confidence-tier classification in the dispatcher (§3.4). Cross-encoder rerank lands in T7 (§10) when ML capability ships. | Top-3 above a fixed score initially; cross-encoder rerank when ML capability ships. The reranker is shared between the two features. |
| **HOW to present / inject.** | Verbose-and-explicit `PresentedAs` strings (§4.1). The agent surfaces the named binding to the user. **Never silent injection.** | Possible silent injection via `<system-reminder>` blocks OR visible blocks. Decision deferred to that feature's chain. |

Reference-resolution is the **sharper primitive** because it asks the more answerable question: "what specific token did the user name that I can't bind?" is a low-false-positive shape question. "What context might be useful?" is a fuzzy, system-driven, false-positive-prone framing.

The two are **complementary**, not competing. Both can run in the same session post-substrate; the reference resolver fires on agent's reflex (when the user names something), and the future proactive-injection layer fires on system's clock (when it might be useful to inject). They share the cross-encoder reranker, the four telemetry fields (`query_source`, `was_injected`, `injection_position`, `injection_was_user_visible`), and the cross-substrate join contract.

---

## 12. Worked example — end-to-end

A user message with three reference-shape tokens, resolved by the substrate.

### 12.1 The user message

> "Start work on `ableton-wine-setup` — also look up `vault-pull-discipline` and check the `Tripolar Invariant` notes."

### 12.2 Detection

`refresolve.Detect(ctx, message)` returns:

```
References:
  { Token: "ableton-wine-setup", Shape: chain_slug,    Confidence: 1.0, Method: "regex+list_match",      StartPos: 14, EndPos: 32 }
  { Token: "vault-pull-discipline", Shape: skill_name, Confidence: 1.0, Method: "filename_match",       StartPos: 50, EndPos: 71 }
  { Token: "Tripolar Invariant",  Shape: domain_term,  Confidence: 0.78, Method: "rubric",              StartPos: 88, EndPos: 106 }
```

Total detection time: 312ms (rubric is the dominant cost).

### 12.3 Dispatch

`refresolve.Dispatch(ctx, references)` runs three resolvers in priority order:

- `chain_resolver.Resolve("ableton-wine-setup")` → `chain_find` returns one exact match. HitSet: `{Tier: single_exact, Candidates: [{ID: "ableton-wine-setup", Title: "Ableton under Wine setup", Score: 1.0, SourceRef: "chain:ableton-wine-setup"}], RetrievalCostMs: 8}`.
- `skill_resolver.Resolve("vault-pull-discipline")` → filename-match returns the skill file. HitSet: `{Tier: single_exact, Candidates: [{ID: "vault-pull-discipline", Title: "vault-pull-discipline skill", Score: 1.0, SourceRef: "skill:vault-pull-discipline"}], RetrievalCostMs: 2}`.
- `domain_term_resolver.Resolve("Tripolar Invariant")` → `knowledge_search` returns three vault notes. HitSet: `{Tier: weak_domain, Candidates: [{ID: "vault/learnings/glyphs/2026-04-30_tripolar-invariant.md", Score: 0.62, SourceRef: "vault:learnings/glyphs/2026-04-30_tripolar-invariant.md"}, ...], RetrievalCostMs: 740}`.

Total dispatch time: 750ms.

### 12.4 Telemetry emits

Three `grounding_events` rows written (one per reference), all sharing `span_id`, `prompt_id`, `session_id`, `user_message_id`. Each row carries `query_source='reference_resolution'`, `query_text=<token>`, `source_refs=<JSON of candidates>`.

### 12.5 The action's response envelope

```json
{
  "References": [
    {
      "Token": "ableton-wine-setup",
      "Shape": "chain_slug",
      "ConfidenceTier": "single_exact",
      "PresentedAs": "`ableton-wine-setup` → chain in mcp-servers, 6 tasks (4 pending, 2 blocked), open since 2026-05-10",
      "RecommendedAction": "use_directly",
      "GroundingEventID": 42101
    },
    {
      "Token": "vault-pull-discipline",
      "Shape": "skill_name",
      "ConfidenceTier": "single_exact",
      "PresentedAs": "`vault-pull-discipline` → skill at skills/vault-pull-discipline/SKILL.md",
      "RecommendedAction": "use_directly",
      "GroundingEventID": 42102
    },
    {
      "Token": "Tripolar Invariant",
      "Shape": "domain_term",
      "ConfidenceTier": "weak_domain",
      "PresentedAs": "`Tripolar Invariant` may refer to: vault note vault/learnings/glyphs/2026-04-30_tripolar-invariant.md (rank 1, weak)",
      "RecommendedAction": "mention_as_possibly_relevant",
      "GroundingEventID": 42103
    }
  ],
  "ResolutionTimeMs": 1062,
  "ResolverCallsMade": 3,
  "NoHitTokens": [],
  "PartialFailures": [],
  "TruncatedByBudget": false
}
```

### 12.6 The agent's response

```
Found `ableton-wine-setup` in mcp-servers — 6 tasks (4 pending, 2 blocked).
I also pulled the `vault-pull-discipline` skill (skills/vault-pull-discipline/SKILL.md).
For "Tripolar Invariant" I found a possibly-relevant vault note at
vault/learnings/glyphs/2026-04-30_tripolar-invariant.md (rank 1, weak match) —
is that what you meant?

Proceeding with `ableton-wine-setup`. Reading the first pending task...
```

If the agent's response mentions `chain:ableton-wine-setup` (substring or markdown link), the existing four-tier click_kind detector emits a `cited` `query_interactions` row in the next session's Stop-hook pass. The training data picks up the (token, candidate, label) triple, which feeds the eventual cross-encoder reranker via `proj_training_data_for_reranker`.

---

## 13. Cross-substrate seam

This chain consumes the contracts from `agent-first-substrate` and `query-telemetry-substrate`. The seam is **read-only and additive** — no contracts get redefined, no migrations get rewritten.

### 13.1 Inherited from `agent-first-substrate`

- **`span_id`** — per-MCP-`tools/call` UUIDv4, dispatcher-minted. The `resolve_references` handler inherits the span_id from ctx; every `grounding_events` row this chain emits carries it. Cross-substrate join with `events` table on `span_id` works directly.
- **Event envelope** — this chain does NOT emit new write-side events except the closing audit (§13.3). The events ledger remains the source of truth for state mutations; reference resolution is a read-side operation.
- **Rationale enforcement** — `resolve_references` is read-only and does not require rationale. Friction-shape resolutions are *suggestions* (not mutations); the actual `forge(bug, …)` call the agent makes from the suggestion goes through the dispatcher's rationale check normally.

### 13.2 Inherited from `query-telemetry-substrate`

- **`grounding_events`** — schema unchanged except for the CHECK-widening migrations in T5 and T9. The new `query_source` values flow through the same emit path.
- **`query_interactions`** — schema unchanged. The four-tier `click_kind` detector picks up `resolve_references`-sourced rows naturally.
- **`query_resolutions`** — schema unchanged. A bug resolved after a reference-resolution citation flows through the resolution-detection pass the same as any other.
- **Three projections** — `query_volume_by_source`, `retrieval_success_per_query`, `training_data_for_reranker` all pivot by `query_source` and pick up the new values automatically once the CHECK migration lands.
- **`prompt_id` / `session_id` / `parent_span_id`** — all inherited from ctx; this chain does not stamp them, it propagates them.

### 13.3 Outgoing audit event from this chain

T8 (retrospective) emits one write-side event: `ReferenceResolutionAuditCompleted` (schema at `blueprints/events/ReferenceResolutionAuditCompleted.json`). This is the self-hosting check: the substrate uses the write-side substrate to record its own closing audit. The shape matches `ArchitectureAuditCompleted` (`agent-first-substrate` T8) and `TelemetryAuditCompleted` (`query-telemetry-substrate` TT4) — a closed three-substrate trilogy where each chain ends by writing through the prior chain's surface.

### 13.4 The Reserved namespace

`docs/EVENT_CATALOG.md` "Reserved namespace" gets one new prefix entry: `Reference*` is reserved for events from this chain. T8 ships exactly one event under that prefix. Future reference-resolution-adjacent chains (e.g., the ML upgrade chain in §10.5) MAY emit additional `Reference*` events; the namespace is reserved for them.

---

## 14. Open questions for review

These are decisions I am proposing but the user may want to override before the doc lands. None block downstream tasks if accepted as-stated.

1. **`reference_resolution` and `harness_reminder_interception` as first-class `query_source` values (Path A).** Alternative: Path B (`'other'` + a new `query_subsource` column). Path A is the recommendation per §6.1; the rationale is "stable, named, well-bounded use cases warrant first-class enum values." Confirm.

2. **Per-resolver `tier_thresholds` block in `dispatch-policy.toml` (§3.4).** Alternative: hardcode the thresholds in each resolver. The dispatch-policy file is the single source of truth for dispatcher policy (per `agent-first-substrate` T3); thresholds are dispatcher concern, not resolver concern. Confirm.

3. **Friction-shape resolver returns a *suggestion*, not a binding (§7).** The HitSet's `confidence_tier='friction_observed'` is a non-resolver tier (it doesn't match `single_exact` / `fuzzy_multi` / `weak_domain` / `no_hit`). Alternative: treat friction as a special category outside the resolver registry, with its own handler. The "register as a resolver returning a non-binding suggestion" path keeps the dispatcher's contract uniform (every shape goes through `Resolve(ctx, ref) (HitSet, error)`); the cost is one additional `ConfidenceTier` value. Confirm uniform-contract preference.

4. **`resolve_references` is one MCP `tools/call` but emits N `grounding_events` rows** (§6.2). Alternative: one row per call with a denormalized array of references. Per-reference rows match the existing telemetry shape (one row per retrieval pass); per-call denormalization would require parallel projection logic. Confirm per-reference.

5. **Hook supersession verification threshold of ≥90% true-positive coverage (§7.2).** Alternative: 100%, or an absolute number rather than a percentage. 90% allows the contextual detector to be *broader* than the hook's literal trigger-phrase list while not falling below the hook's coverage; absolute thresholds would conflate with historical fire volume. Confirm.

6. **Fail-open hook behavior (§8.2) — never strip on text drift.** Alternative: strip on best-effort substring fuzzy match. Fail-open is the conservative choice: silently stripping a reminder we *don't fully recognize* could mask a different upstream warning. The maintenance log is the trip-wire. Confirm conservative posture.

7. **`top_k_per_shape=5` default in `resolve_references` (§5.1).** At higher values, the response payload grows; at lower values, fuzzy-multi cases get truncated. 5 is a round middle. Confirm.

8. **Detection priority: `friction_shape` is whole-message, not token-level (§2.2).** Alternative: extract a specific friction-token. Friction observations are usually phrasal, not token-shaped ("annoying that ABC keeps doing XYZ"); the whole-message scope matches the v1 skill's existing trigger discipline. Confirm.

---

## 15. Glossary

| Term | Meaning |
|---|---|
| **Reference** | A token in a user message that names a specific thing the agent should bind to context before responding. Eleven shape categories (§2.1). |
| **Shape** | The detected category of a reference (chain_slug, path, skill_name, domain_term, …). Detection is shape-based, not uncertainty-based. |
| **Detector** | The `go/internal/refresolve/Detect(ctx, text) ([]Reference, error)` function. Rule-based for deterministic shapes; rubric-based for domain-term and friction-shape. |
| **Resolver** | A pluggable `Resolver` interface implementation, one per shape category, wrapping the existing tool for that shape (chain_find, knowledge_search, kiwix_search, …). |
| **HitSet** | The output of one `Resolver.Resolve(ctx, ref)` call: `{ResolverName, ConfidenceTier, Candidates, RetrievalCostMs, Err}`. |
| **Candidate** | One result returned by a resolver: `{ID, Title, Score, SourceRef, DebugNotes}`. |
| **Confidence tier** | One of `single_exact` / `fuzzy_multi` / `weak_domain` / `no_hit`. Classified by the dispatcher, not per-resolver. |
| **Presentation recommendation** | What the agent should do with the resolution: `use_directly` / `ask_user_to_disambiguate` / `mention_as_possibly_relevant` / `acknowledge_no_hit_and_ask`. |
| **Resolution** | A `ResolvedReference` returned by the `resolve_references` action: the formatted, telemetered, agent-consumable output for one detected reference. |
| **`query_source='reference_resolution'`** | The new `grounding_events.query_source` value introduced by T5 (CHECK-widening migration). Distinguishes resolver-emitted grounding events from agent-initiated and proactive-injection rows. |
| **`query_source='harness_reminder_interception'`** | The new `grounding_events.query_source` value introduced by T9. Records UserPromptSubmit hook decisions on the upstream task-tools reminder. |
| **Friction shape** | A reference category that does not resolve to a binding; the resolver returns a "consider filing as bug" suggestion. The supersession mechanism for `friction-filing-reminder.sh`. |
| **Hook supersession** | The one-way migration from always-on Stop-hook reminders to contextual reference-resolution fires. Replacement ships first, hook retires second, verification third. |

---

## 16. Cross-references

- `skills/reference-resolution.md` — the v1 skill; this chain's mechanism backs the skill's discipline post-T4.
- `skills/reference-resolution.toml` — the trigger/ambient/keyword shape; T4 adds `resolve_references` to `tools_available`.
- `docs/EVENT_SUBSTRATE.md` — write-side ledger. §6 (span_id contract) and §5 (rationale enforcement) are inherited.
- `docs/TELEMETRY_SUBSTRATE.md` — read-side telemetry. §3 (`query_interactions`), §6 (projections), §8 (proactive-injection prerequisites), §12 (cross-substrate seam) are all consumed.
- `docs/TELEMETRY_LABEL_SPIKE.md` — TT1.5 closure; confirms the four-tier `click_kind` enum the chain's citation detection inherits.
- `docs/PROJECTIONS.md` — projection contract details; `proj_training_data_for_reranker` is the bridge to T7's trained reranker.
- `docs/EVENT_CATALOG.md` — `Reference*` reserved namespace entry added in T8.
- `crates/shared-db/migrations/037_telemetry_substrate.sql` — the existing `query_source` CHECK that T5 + T9 widen via Path A.
- `crates/shared-db/migrations/<next>_grounding_events_reference_resolution_source.sql` — T5's migration (lands first).
- `crates/shared-db/migrations/<next+1>_grounding_events_harness_interception_source.sql` — T9's migration (lands second).
- `action-manifests/resolve-references.toml.skeleton` — design artifact; T4 promotes to `.toml`.
- `action-manifests/instructions/resolve-references.md` — T4 lands.
- `action-manifests/dispatch-policy.toml` — adds `[knowledge.resolve_references]` block with `requires_rationale = false`; the per-resolver `tier_thresholds` block lives alongside (open question #2).
- `blueprints/rubrics/reference-domain-term-detector.toml` — T2 lands the cold-start rubric.
- `blueprints/events/ReferenceResolutionAuditCompleted.json` — T8 lands the closing audit event.
- ~~`hooks/intercept-task-tools-reminder.sh` — T9 lands.~~ **[REMOVED 2026-05-25, commit 5b037b41 — see §8 banner.]**
- `scripts/hooks/README.md` — ~~T9 documents the install snippet for `~/.claude/settings.json`.~~ **[T9 install section removed 2026-05-25; the file remains for the other hooks.]**
- `~/.claude/hooks/friction-filing-reminder.sh` — the Stop hook T6 supersedes (script preserved as rollback artifact).
- `~/.claude/settings.json` — T6 removes the friction-filing-reminder Stop hook entry; T9 adds the UserPromptSubmit hook entry.
- `~/.claude/vault/learnings/general/2026-05-15_proactive-injection-feature-design.md` — the four-decisions framing this chain answers (§11).
- `~/.claude/vault/learnings/general/2026-05-15_trust-the-data-not-the-descriptor.md` — the verification reflex this layer institutionalizes.
- `~/.claude/vault/learnings/general/2026-05-15_ml-capability-vs-models-framing.md` — the "build capability once" principle (§10).
- `~/.claude/vault/learnings/general/2026-05-17_tiered-implicit-feedback-for-rag-telemetry.md` — the four-tier `click_kind` rationale this chain's citation detection inherits.
- Chain `agent-first-substrate` retrospective: `docs/SUBSTRATE_RETROSPECTIVE_2026-05-17.md`.
- Chain `query-telemetry-substrate` retrospective: `docs/TELEMETRY_RETROSPECTIVE_2026-05-17.md`.
