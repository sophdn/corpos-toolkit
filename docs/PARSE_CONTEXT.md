# `parse_context` Action — Design

**Chain:** reference-resolution-migration (id 598)
**Task:** T4 — parse-context-design
**Date:** 2026-05-18
**Sibling doc:** `docs/REFERENCE_RESOLUTION.md` (the substrate baseline this design extends)

`parse_context` is the canonical successor to `resolve_references`.
Same machinery, broader surface coverage, new filter layer for
per-session cache dedup, and a new role in the agent's workflow: it
becomes the agent's **first call on every user prompt** (modulo the
discernment skill's skip-rules from T6).

This doc is the load-bearing output of T4. T5 ships the implementation.

---

## 1. Why parse_context (not just resolve_references)

`resolve_references` resolves shape-tagged tokens (chain/task/bug slugs,
paths, skill names, etc.) into bindings. It exists, telemetry flows
through it, and the 2026-05-18 substrate trilogy closed with it as the
canonical reference-resolution surface.

What it doesn't do well today:
- The agent reaches for per-shape direct surfaces (`chain_find`,
  `bug_read`, `Read`, `admin.action_describe`) instead, because the
  per-shape calls feel faster on the single-call dimension.
- The agent has to know what to ask for — the substrate is downstream
  of agent intent rather than upstream of agent orientation.
- Skills, memory entries, vault notes, and kiwix hits live on separate
  surfaces (`vault_search`, `kiwix_search`, the available-skills list)
  with their own retrieval discipline.

`parse_context` inverts this:
- The agent's first move on every user prompt is one call:
  `parse_context(message_text=<user's verbatim message>)`.
- The substrate scans for every shape AND every domain-conditional
  surface (skills, memory, vault, kiwix), returning a unified envelope
  of Candidates.
- The agent acts on whichever Candidates are relevant; per-shape direct
  calls become the second move (for action operations on already-resolved
  bindings: `task_complete(slug=X)` after parse_context surfaced X).

Sophi's framing: "you're a librarian who is used to finding things
manually ... your team bought you a computer to help save you time
... but you're still doing things the old way, and the computer is
gathering dust." parse_context is the computer's load-bearing surface.

---

## 2. Action signature

```
mcp__toolkit-server__knowledge(
    action='parse_context',
    params={
        message_text: string,                     # required
        session_id: string,                       # optional; populated from ctx if omitted
        top_k_per_shape: int = 5,                 # default 5
        include_no_hits: bool = false,            # default false
        total_budget_ms: int = 4000,              # default 4s (revised from resolve_references's 2.5s)
        cache_policy_override: string,            # optional; "fresh" forces no-cache
    }
)
```

Envelope mirrors `ResolveReferencesResult`:

```typescript
{
  references: ResolvedReference[],
  resolution_time_ms: int64,
  resolver_calls_made: int,
  no_hit_tokens?: string[],
  partial_failures?: string[],
  truncated_by_budget?: bool,
  cache_hits?: int,        // NEW
  cache_misses?: int,      // NEW
  error?: string,
}
```

`ResolvedReference` gains two fields:

```typescript
{
  token: string,
  shape: ShapeCategory,
  confidence_tier: ConfidenceTier,
  presented_as: string,
  top_candidates: Candidate[],
  recommended_action: PresentationRecommendation,
  grounding_event_id?: int64,
  from_cache?: bool,            // NEW — true if this Resolved came from cache
  cache_policy?: string,        // NEW — which policy applied (e.g. "indefinite-within-session")
}
```

### 2.1 `resolve_references` as alias

`resolve_references` becomes an alias of `parse_context` for back-compat.
Existing callers (the T8 retrospective verification in the substrate
trilogy; any future agent-side scripts) continue to work without
modification. The alias produces an identical response shape for the
shape categories `resolve_references` originally covered (the new
parse_context-only shapes — `skill_trigger`, `memory_entry`,
`vault_candidate`, `kiwix_bridge`, `discipline_skill` — are still
returned in the response, since they live in the same `references`
array; callers that don't care about them iterate past them).

Deprecation is **soft**: `resolve_references` stays indefinitely.
Documentation points at `parse_context` as canonical going forward.

---

## 3. Resolver coverage

### 3.1 Inherited from `resolve_references` (unchanged)

Twelve shape categories already in the substrate:

- `chain_slug` — chain_find
- `task_slug` — task_search
- `bug_slug` — bug_list
- `path` — filesystem walker
- `skill_name` — filesystem matcher (`~/.claude/skills/<name>/` and `mcp-servers/skills/<name>/`)
- `project_name` — project catalog
- `tool_name` — tool catalog
- `forge_schema` — blueprints/forge-schemas/
- `domain_term` — Qwen rubric (cold-start; trained classifier when ml-capability-substrate lands)
- `external_technical` — Qwen rubric + (NEW per §3.3) kiwix bridge
- `library_entry` — library catalog
- `friction_shape` — phrase-based detector

### 3.2 New resolvers added by parse_context

#### 3.2.1 `skill_trigger`

Matches against the manifest's `trigger_keywords` per `[[skill]]` entry.
Returns the skill's body path + bucket + description as a Candidate.

```typescript
// Example: user message contains "I need to write a new Rust crate for X"
// Resolver returns:
{
  token: "Rust",
  shape: "skill_trigger",
  confidence_tier: "single_exact",
  presented_as: "`Rust` triggers skill `rust-conventions` → mcp-servers/skills/rust-conventions/SKILL.md",
  top_candidates: [{
    ID: "rust-conventions",
    Title: "skill rust-conventions",
    Score: 1.0,
    SourceRef: "skill:mcp-servers/skills/rust-conventions/SKILL.md",
    DebugNotes: "bucket=pure-lazy trigger=rust",
  }],
  recommended_action: "use_directly",  // load the skill body
}
```

Implementation: reads `mcp-servers/skills/_manifest.toml` (T2 design) at
startup; rebuilds the keyword → skill map on `admin.schema_reload`.

#### 3.2.2 `memory_entry`

Matches against `MEMORY.md` index lines + linked entries' `description`
frontmatter. Returns the memory body's path as a Candidate.

Cache policy: indefinite within session (memory bodies don't mutate
mid-conversation in normal use).

#### 3.2.3 `vault_candidate`

Bridges to `vault_search` for cross-project insights. Returns
matching vault notes as Candidates with their frontmatter + path.

Differences from direct `vault_search`:
- parse_context calls vault_search ONLY when the message's domain
  terms or external_technical shapes warrant — not on every message.
- Results surface alongside other Candidates in the unified envelope,
  so the agent sees vault hits in context with skill hits and other
  bindings.

Cache policy: indefinite within session (vault content is rarely
edited mid-conversation).

#### 3.2.4 `kiwix_bridge`

For external_technical tokens, parse_context optionally calls
`kiwix_search` to retrieve offline-doc snippets. Returns top hits as
Candidates with their book + URL + brief excerpt.

Gated behind a config flag — kiwix queries are cheap but the bridge
adds latency. Default: enabled if external_technical confidence tier
is `single_exact` or `weak_domain`.

Cache policy: indefinite within session.

#### 3.2.5 `discipline_skill`

The discipline-skill resolver is the load-bearing innovation. For each
condense-lazy or keep-ambient discipline declared in the manifest:
- Check the discipline's **trigger condition**, not just keywords.
  Trigger conditions are richer than substring match — they consult
  the existing shape detectors (friction_shape for bug-filing-discipline;
  domain-match-found for vault-pull-discipline; insight-shape for
  vault-filing-discipline; code-being-written for coding-philosophy; etc.).
- When a discipline's trigger fires, the resolver returns the discipline's
  SKILL.md body as a Candidate. The agent reads it inline and applies
  the discipline.

This is what lets disciplines move from ambient-loaded to lazy:
they ride the substrate's existing shape detection.

```typescript
// Example: user message: "the way the dashboard banner kept reappearing
// is annoying, that's a paper-cut"
// friction_shape detector fires.
// discipline_skill resolver checks: bug-filing-discipline's trigger
// is friction_shape phrases. Match.
// Resolver returns:
{
  token: "paper-cut",
  shape: "discipline_skill",
  confidence_tier: "single_exact",
  presented_as: "Trigger for `bug-filing-discipline` → file via forge(bug, ...)",
  top_candidates: [{
    ID: "bug-filing-discipline",
    Title: "skill bug-filing-discipline",
    Score: 1.0,
    SourceRef: "skill:mcp-servers/skills/bug-filing-discipline/SKILL.md",
    DebugNotes: "triggered_by=friction_shape:paper-cut",
  }],
  recommended_action: "use_directly",  // apply the discipline
}
```

Trigger-condition declarations live in each discipline-skill's
`SKILL.toml` frontmatter (or the manifest's per-entry metadata). T3
+ T7 finalize the trigger taxonomy as part of the migration.

### 3.3 Resolver coverage matrix

| Shape | Today (resolve_references) | parse_context | Cache policy |
|---|---:|---:|---|
| chain_slug | ✓ | ✓ | short (5 turns; invalidate on chain-state events) |
| task_slug | ✓ | ✓ | short (5 turns; invalidate on task-state events) |
| bug_slug | ✓ | ✓ | short (5 turns; invalidate on bug-state events) |
| path | ✓ | ✓ | indefinite within session |
| skill_name | ✓ | ✓ | indefinite within session |
| project_name | ✓ | ✓ | indefinite within session |
| tool_name | ✓ | ✓ | indefinite within session |
| forge_schema | ✓ | ✓ | indefinite within session |
| domain_term | ✓ | ✓ | indefinite within session |
| external_technical | ✓ | ✓ | indefinite within session |
| library_entry | ✓ | ✓ | indefinite within session |
| friction_shape | ✓ | ✓ | never (every observation is a candidate filing moment) |
| **skill_trigger** | ✗ | ✓ NEW | indefinite within session |
| **memory_entry** | ✗ | ✓ NEW | indefinite within session |
| **vault_candidate** | ✗ | ✓ NEW | indefinite within session |
| **kiwix_bridge** | ✗ | ✓ NEW | indefinite within session |
| **discipline_skill** | ✗ | ✓ NEW | re-evaluate per call (cheap; just shape-match check) |

---

## 4. Filter layer (per-session cache)

### 4.1 Mechanism

Substrate-side, in-process. Single map keyed by `(session_id, token)`:

```go
type ParseContextCache struct {
    mu     sync.RWMutex
    byKey  map[cacheKey]cacheEntry
}

type cacheKey struct {
    sessionID string
    token     string
    shape     ShapeCategory  // disambiguates same token across shapes
}

type cacheEntry struct {
    candidates    []Candidate
    confidenceTier ConfidenceTier
    cachedAt      time.Time
    policy        CachePolicy
}
```

On each `parse_context` call:
1. Parse the message; identify candidate (shape, token) pairs.
2. For each (shape, token), check the cache.
3. If present + policy allows reuse → return cached Candidates with `from_cache: true`.
4. If absent or policy denies → resolve fresh, populate cache, return with `from_cache: false`.

The same response envelope shape regardless of cache hit/miss; the
markers let telemetry distinguish.

### 4.2 Per-resolver cache policies (from §3.3 matrix)

- **indefinite within session** — most resolvers. Filesystem walks, schema lookups, domain-term rubric (deterministic). Cache lasts until session end.
- **short (5 turns; invalidate on state events)** — slugs (chain/task/bug). State mutates; cache for ~5 turns then auto-stale, OR invalidate immediately when a related state-change event fires (`TaskCompleted`, `BugResolved`, etc.). The substrate's events table is the invalidation signal.
- **never** — friction_shape. Every observation matters; caching would lose filings.
- **re-evaluate per call** — discipline_skill triggers. The trigger condition is a shape match, which is itself cheap to re-evaluate; no benefit to caching.

Policies declared in the manifest's per-resolver metadata (substrate-internal config). Tunable without a server rebuild via `admin.schema_reload`.

### 4.3 Cache invalidation via events

When a state-changing event fires (e.g. `TaskCompleted` event id 0x12abc in chain reference-resolution-substrate task chain-retrospective), the substrate's event bus posts to the cache's invalidation channel. The cache walks its entries; any cache entry whose `source_refs` include the affected slug gets evicted.

Implementation: in-process pubsub on the existing `eventbus.Bus`. Cheap; constant-time lookup keyed by slug.

### 4.4 Session-scope

`session_id` comes from the request context (every MCP call carries it
via `span_id` from agent-first-substrate's contract). Each session has
its own cache; sessions don't share. When a session ends (Stop hook),
its cache entries get cleaned up by a periodic sweeper or on next
parse_context call to that session_id (lazy clean).

Cross-session caching would be a future optimization if substrate
workload warrants it; current scope is session-local.

---

## 5. Latency budget

`resolve_references` today budgets 2.5s total. parse_context's coverage
is broader (more resolvers) so the budget needs revising.

**New budget: 4 seconds total**, with per-resolver sub-budgets:

| Resolver | Budget |
|---|---:|
| Cheap (string/regex/filesystem matchers): chain/task/bug/path/skill_name/project_name/tool_name/forge_schema | ≤100ms each |
| Qwen rubric: domain_term / external_technical | ≤1.5s each |
| skill_trigger | ≤50ms |
| memory_entry | ≤50ms (just MEMORY.md index walk) |
| vault_candidate (bridges to vault_search; Qwen-backed) | ≤1.5s |
| kiwix_bridge (bridges to kiwix_search) | ≤500ms |
| discipline_skill (shape-match re-evaluation) | ≤100ms |

Per-resolver budgets allow parallelism: most resolvers run concurrently;
their results gather into the envelope. Total wall-clock should stay
under 4s on representative messages.

Sophi's latency tolerance is high (stated in chain design_decisions),
so a slow parse_context call is acceptable. The filter cache addresses
the cumulative-cost concern (every prompt, redundant work skipped).

On budget breach: the response includes `truncated_by_budget: true`
and `partial_failures` listing which resolvers timed out. Same contract
as `resolve_references` today.

---

## 6. First-call discipline contract

T6 authors the `parse-context-first-call` skill. The substrate side
documents the expectation here:

> Agents consuming the toolkit-server knowledge surface SHOULD call
> `parse_context` as their first action on every user prompt (modulo
> the discernment skill's skip rules: short conversational
> acknowledgments, direct slash-commands, pure-continuation messages).
> The action is cheap when the cache hits; expensive but bounded when
> it misses. The substrate's value (telemetry feeding the future
> reranker, friction-shape detection, cross-substrate context) only
> accrues when this discipline is followed.

Telemetry tracking: every parse_context call emits a `grounding_events`
row with `query_source='reference_resolution'` (existing value from
the substrate trilogy's T5). The cache-hit/miss markers feed
`proj_training_data_for_reranker` indirectly via the row's
`source_refs` JSON.

The skill body's calibration warning (bias toward firing; skip only on
clearly-redundant messages) lives on the agent side. Substrate just
provides the surface.

---

## 7. Rename vs alias decision

**Alias.** `resolve_references` stays; `parse_context` is the canonical
name going forward.

Rationale:
- Existing callers (substrate trilogy T8 verification, any agent-side
  scripts) continue to work.
- Migration is gradual: documentation points new callers at parse_context;
  old callers migrate at their own pace.
- The action-doc TOML for `resolve_references` updates with a "this is
  an alias of `parse_context`; see parse_context.toml for the canonical
  documentation" note.

Hard rename would require coordinated callers update + an action_describe
deprecation cycle. Soft alias is the lower-friction path.

---

## 8. Action-doc + CI gate

`go/internal/actiondocs/corpus/knowledge/parse_context.toml` ships with full
canonical params per the action-doc schema. `resolve_references.toml`
updates to mark it as an alias.

The action-doc canonical-name CI gate (bug e0cf855 fix) verifies every
documented param is reachable as a `json:"<name>"` tag (or quoted
literal) under `go/internal/`. parse_context's params (`message_text`,
`session_id`, `top_k_per_shape`, `include_no_hits`, `total_budget_ms`,
`cache_policy_override`) get the same gate coverage.

T5 ships the handler struct with the matching tags; the CI gate fails
the build if any param canonical name lacks a binding.

---

## 9. Response envelope examples

### 9.1 Short conversational message (skip-rule territory)

Input: `parse_context(message_text="thanks")`

Output:
```json
{
  "references": [],
  "resolution_time_ms": 12,
  "resolver_calls_made": 0,
  "cache_hits": 0,
  "cache_misses": 0
}
```

No shapes detected → no resolvers called → empty references. Cheap.
(The discernment skill from T6 would have skipped parse_context entirely
for this message; cost when not skipped is still negligible.)

### 9.2 Shape-heavy first message

Input: `parse_context(message_text="please finish T8 and T9 in reference-resolution-substrate (7 is blocked)")`

Output (representative):
```json
{
  "references": [
    {
      "token": "reference-resolution-substrate",
      "shape": "chain_slug",
      "confidence_tier": "single_exact",
      "presented_as": "`reference-resolution-substrate` → chain in mcp-servers, 9 tasks, 1 cancelled, 8 closed",
      "top_candidates": [...],
      "recommended_action": "use_directly",
      "grounding_event_id": 200,
      "from_cache": false,
      "cache_policy": "short-5-turns"
    },
    {
      "token": "T8",
      "shape": "task_slug",
      "confidence_tier": "single_exact",
      "presented_as": "`T8` → task chain-retrospective in chain reference-resolution-substrate. status=closed task_id=6337",
      ...
    },
    {
      "token": "T9",
      "shape": "task_slug",
      ...
    },
    {
      "token": "T7",
      "shape": "task_slug",
      ...
    }
  ],
  "resolution_time_ms": 380,
  "resolver_calls_made": 4,
  "cache_hits": 0,
  "cache_misses": 4
}
```

### 9.3 Cached re-call later in same session

Input: `parse_context(message_text="now also close T8 and the retrospective")` (same session)

Output:
```json
{
  "references": [
    {
      "token": "T8",
      "shape": "task_slug",
      ...
      "from_cache": true,
      "cache_policy": "short-5-turns"
    }
  ],
  "resolution_time_ms": 8,
  "resolver_calls_made": 0,
  "cache_hits": 1,
  "cache_misses": 0
}
```

8ms vs 380ms; the cache earns its keep on every re-mention.

### 9.4 Shape with new resolver — discipline trigger

Input: `parse_context(message_text="the way the dashboard banner kept reappearing is annoying, that's a paper-cut")`

Output:
```json
{
  "references": [
    {
      "token": "paper-cut",
      "shape": "friction_shape",
      "confidence_tier": "single_exact",
      "presented_as": "You observed friction at \"the way the dashboard banner kept reappearing\". Consider filing via: work(action='forge', schema_name='bug', ...)",
      ...
    },
    {
      "token": "paper-cut",
      "shape": "discipline_skill",
      "confidence_tier": "single_exact",
      "presented_as": "Trigger for `bug-filing-discipline` → file via forge(bug, ...). Skill body at mcp-servers/skills/bug-filing-discipline/SKILL.md",
      "top_candidates": [{
        "ID": "bug-filing-discipline",
        "Title": "skill bug-filing-discipline",
        "Score": 1.0,
        "SourceRef": "skill:mcp-servers/skills/bug-filing-discipline/SKILL.md",
        "DebugNotes": "triggered_by=friction_shape:paper-cut"
      }],
      "recommended_action": "use_directly"
    }
  ],
  "resolution_time_ms": 95,
  "resolver_calls_made": 2,
  "cache_hits": 0,
  "cache_misses": 2
}
```

The friction_shape resolver flags the observation; the discipline_skill
resolver surfaces the discipline body. Agent applies the discipline
without it needing to be loaded ambient.

---

## 10. Implementation notes for T5

T5 ships the implementation across multiple commit phases. As-built
pointers:

- **Handler:** `go/internal/refresolve/handler.go` — `HandleParseContext`
  alongside `HandleResolveReferences`; both dispatch into
  `handleParseContextCore` which threads the action name through
  grounding-events telemetry. Envelope additions (`cache_hits`,
  `cache_misses` outer; `from_cache`, `cache_policy` per ref) ship
  with omitempty.
- **Params:** `parseContextParams` struct in `handler.go` adds
  `session_id` and `cache_policy_override` to the existing
  `message_text` / `top_k_per_shape` / `include_no_hits` /
  `total_budget_ms`. The action-doc canonical-name CI gate
  (`go/internal/actiondocs/param_tag_gate_test.go`) is green via
  these struct tags.
- **Cache:** `go/internal/refresolve/cache.go` — `ParseContextCache`
  is the in-process map keyed by `(sessionID, token, shape)`. Per-
  shape policy from `PolicyForShape`. Time-based TTL for short-5-turns
  (5-minute proxy); indefinite within-session for the rest;
  PolicyNever / PolicyReEvaluatePerCall bypass entirely. Event-bus
  invalidation is a follow-on refinement; the TTL fallback meets the
  v1 promise.
- **Resolvers** (one file each under `go/internal/refresolve/`):
  - `resolver_skill_trigger.go` — reads `skills/_manifest.toml`,
    matches trigger keywords; single_exact for one-skill, fuzzy_multi
    for shared keywords.
  - `resolver_discipline_skill.go` — second-pass driven; resolver
    maps discipline name back to its body in the manifest.
  - `resolver_vault_candidate.go` — bridges to `knowledge.HandleVaultSearch`
    with query=token; returns top hits as Candidates with
    `vault:<path>` SourceRef.
  - `resolver_kiwix_bridge.go` — bridges to `knowledge.HandleKnowledgeSearch`
    and filters to `source_type="kiwix_reference"` (sidesteps the
    per-corpus ZIM-routing problem `kiwix_search` would impose).
  - `resolver_memory_entry.go` — shell only in T5 ship; full lookup
    lands with T10 (memory-domain-conditional-lazy-routing) which
    owns the source-of-truth for which entries are domain-conditional
    vs ambient.
- **Manifest reader:** `skills_manifest.go` — parses
  `skills/_manifest.toml` and exposes `TriggerIndex()` +
  `TriggerKeywords()` helpers. `LoadCatalogs` populates the catalogs
  struct with the parsed manifest and the deduplicated trigger
  keyword list.
- **Detection:**
  - `detect_skill_trigger.go` — whole-word match against
    `Catalogs.SkillTriggers`.
  - `detect_discipline_skill.go` — second pass; emits
    `ShapeDisciplineSkill` for friction_shape → bug-filing-discipline
    (only trigger condition wired today; PARSE_CONTEXT §11.2 tracks
    expansion).
  - `detect_bridge_shapes.go` — second pass; promotes domain_term
    primary references to `ShapeVaultCandidate` and external_technical
    primary references to `ShapeKiwixBridge`. Dedupes per
    (shape, token).
- **Action registration:** `go/cmd/toolkit-server/main.go` —
  `knowledgeTable["parse_context"] = refresolve.BuildParseContextHandler(...)`
  alongside the resolve_references entry. Shared `HandlerDeps`
  passes the cache (`refresolveCache := refresolve.NewParseContextCache()`).
- **action-docs:** `go/internal/actiondocs/corpus/knowledge/parse_context.toml`
  ships with the six canonical params; `resolve_references.toml`
  marks itself a soft alias of parse_context. CI gate is green.
- **Tests:** `handler_test.go` Phase-1 alias test;
  `cache_test.go` covers cache hit / cross-session isolation /
  cache_policy_override=fresh bypass / friction never-cache /
  per-shape policy table parity; `resolvers_new_test.go` covers
  skill_trigger single + fuzzy, discipline_skill, bridge detection
  promotion, end-to-end friction → discipline_skill via the handler.

---

## 11. Open items + caveats

1. **Manifest reading at startup:** T5 needs to read `mcp-servers/skills/_manifest.toml` to populate the skill_trigger + discipline_skill resolvers. The manifest doesn't exist until T3 lands. T5 implementation can ship the resolvers gated behind "if manifest exists" — they no-op until T3 populates it. T7 then exercises them.

2. **Trigger-condition declarations:** per-discipline trigger conditions (e.g. friction_shape phrases → bug-filing-discipline) live in each skill's `SKILL.toml` frontmatter. T3 + T7 land this; T5 reads it.

3. **Latency budget honesty:** 4s is generous; actual measured latency on representative messages should land much lower (most resolvers are sub-100ms). The budget is a ceiling, not a target. T12 measures actuals.

4. **Cross-session caching deferred:** future optimization if session-local cache hit rate proves insufficient.

5. **Backwards-compat with resolve_references:** verified by T5's tests (existing resolve_references callers produce identical responses to today's behavior, with the new envelope-level fields all set to defaults / zero / empty).

---

## 12. Chain handoff

T4 closes. T5 (`parse-context-ship`) becomes unblocked — it's the
implementation against this design. T5 touches mcp-servers code only
(no user-file mutations), so it can proceed without user input.

T6 (`first-call-discipline-skill`) remains blocked by T3 + T5. T6
needs T3 because the skill must install via the symlink mechanism; T6
needs T5 because the discipline references parse_context as the
action to call.

T7 (`skill-body-paring`) remains blocked by T2 (closed), T3, T5.

T8, T9, T11 remain unblocked (single-blocker T1 satisfied); they can
proceed in parallel.

---

## 13. Directive-intent detection

**Chain:** parse-context-lean-orienting (id 599)
**Task:** T4 — directive-intent-design
**Date:** 2026-05-21

### 13.1 Motivation

The substrate today resolves message TOKENS but not the message's
DIRECTIVE INTENT. On a prompt like "please solve our open bug", the
deterministic shape detectors find no slugs / paths / domain terms, so
the envelope is mostly empty — yet the directive shape is clear:
"verify/fix work — surface open bugs." Calibration gap recorded in the
reference-resolution-migration T13 report card and the
`project_parse_context_directive_state_gap` memory entry.

Directive-intent detection is the orthogonal axis: a message-scope
classification that runs alongside the token-shape resolvers and lets
downstream resolvers (T6 work-state, T7 disciplines, T8 kiwix
fallback, T9 stdio drift) condition their firing on the prompt's
shape, not just its tokens.

### 13.2 Intent vocabulary (closed set)

The starter set captures the directive shapes observed across the
mcp-servers session corpus. The set is CLOSED — a prompt that doesn't
fit any of these returns `intent: none` and the existing token-shape
resolvers carry on. T5's implementation MUST NOT invent speculative
shapes; vocabulary extensions go through a new chain task.

| Shape       | Definition                                                                                                                                                 |
|-------------|------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `verify`    | Confirm that something works / sanity-check current state. The agent reads, runs tests, or compares against a known-good baseline. Read-mostly.            |
| `implement` | Build new behavior or wire a new feature. Writes substantive code; touches multiple files.                                                                 |
| `fix`       | Diagnose + repair a reported defect. Starts from a specific symptom or bug slug; ends with a regression test.                                              |
| `audit`     | Survey current state for issues, debt, or cleanup candidates. Output is a findings list; the user picks what to act on next.                               |
| `execute`   | Drive named existing work — a chain/task, or the open-bug backlog — to completion. "work through X", "complete X on a worktree", "pick up X", "finish up X", "X please resume". Added §14 (the dominant directive shape). |
| `explain`   | Describe what something is or does. Read-only; the response is prose, not code changes.                                                                    |
| `summarize` | Compress a body of state (chain progress, recent commits, a long thread) into a brief overview. Read-only.                                                 |
| `status`    | Report current pending/active/blocked state for a chain/task/bug or for a project's roadmap. Read-only; tabular or list output.                            |
| `list`      | Enumerate items of a known kind (open bugs, pending tasks, chains in a project). Read-only; the surface is a query, not a question.                       |
| `none`      | No directive shape detected (greetings, conversational acknowledgments, ambiguous prose). Substrate returns nothing intent-scoped; token resolution still fires. |

Representative agent prompts per shape live in the fixture file
(§13.7) so T5's tests can pin the classifier without re-deriving
the corpus.

### 13.3 Decision matrix (intent → downstream resolvers)

The matrix names which resolvers fire WHEN the intent is detected,
ABOVE AND BEYOND the existing token-shape resolvers (which always
run). The downstream tasks T6–T9 each own one column.

| Intent    | T6 work-state | T7 disciplines              | T8 kiwix fallback | T9 stdio drift           |
|-----------|---------------|-----------------------------|-------------------|--------------------------|
| verify    | ✓ (open bugs, in-progress tasks, recent chain state) | ✓ (requesting-code-review + code-review) | (skip — not external-doc territory) | ✓ (mismatch → surface drift signal) |
| implement | ✓ (in-progress tasks for context) | ✓ (apply coding-philosophy + lang-conventions if code-being-written) | (skip) | (skip — implementer already running) |
| fix       | ✓ (open bugs in scope) | ✓ (bug-fixing-discipline + systematic-debugging; bug-filing-discipline if friction named) | (skip) | ✓ (verify ritual is part of "fix landed") |
| audit     | ✓ (open bugs + open suggestions for the surface) | ✓ (refactoring-discipline if refactor-shape; vault-filing-discipline if cross-project signal; suggestion-filing-discipline if improvement-ideas request) | ✓ if envelope is mostly no-hits + external-technical tokens | (skip) |
| execute   | ✓ (open bugs/active tasks — the work itself when no slug is named; deduped against a named slug already in references[]) | ✓ (scratchpad-discipline — the chain-execution reflex; no lang-conventions, work shape not inferable from the verb) | (skip) | (skip) |
| explain   | (skip)        | (skip — docs-shape intent; no intent-mapped disciplines) | ✓ if external-technical tokens present | (skip) |
| summarize | ✓ (recent chain state, task counts) | (skip — explanation surface, not action surface) | (skip) | (skip) |
| status    | ✓ (the load-bearing case — surfaces chain/task/bug state Candidates) | (skip) | (skip) | (skip) |
| list      | ✓ (mirrors status; query-shape resolution) | (skip) | (skip) | (skip) |
| none      | (skip)        | (existing trigger-keyword path only — no intent-mapped additions) | (skip) | (skip) |

**Invariant:** missing-intent never gates the existing token-shape
resolvers. Every shape detector that runs today still runs after T5
ships intent detection.

**Source of truth for the T7 column:** `intentDisciplineMap` in
`go/internal/refresolve/discipline_intent.go` is authoritative for which
disciplines each intent surfaces. This table is a human-readable mirror —
when the map changes, update this column in the same commit. (The two
drifted once — bug `intent-discipline-map-diverges-from-parse-context-doc-
audit-explain-rows` — because the table was written at T7 design time and
the implemented map evolved past it; the note exists so that can't recur
silently.)

### 13.4 Envelope shape — top-level field

```typescript
{
  // existing fields …
  references: ResolvedReference[],
  resolution_time_ms: int64,
  resolver_calls_made: int,
  cache_hits?: int,
  cache_misses?: int,

  // NEW (T5 wires; this design pins the contract)
  intent?: {
    shape: "verify" | "implement" | "fix" | "audit" | "explain" |
           "summarize" | "status" | "list" | "none",
    confidence: number,   // [0, 1]; pattern matches always 1.0; Qwen-rubric path returns the rubric score
    detected_via: "pattern" | "rubric" | "default",
  },
}
```

**Rationale (intent is message-scope, not per-reference):** a single
message has exactly one directive shape; multiple tokens within the
message do not get individual intents. Encoding it on every
`ResolvedReference` would duplicate the same value N times and
mis-suggest that tokens carry intent independently. Top-level field.

`intent` is `omitempty` on the JSON wire — when the field is absent,
callers behave exactly as today (no intent-driven branching). The
substrate sets `intent.shape = "none"` on prompts that don't match;
callers MAY read the field and skip intent-driven resolvers, or MAY
ignore it entirely.

### 13.5 Detection mechanism — pattern-first, Qwen opt-in

**Decision:** pattern-first. Qwen-rubric fallback is OPT-IN, fires
only when pattern confidence is below threshold AND a config flag
admits it.

**Why pattern-first:**

- parse_context fires as the **first action on every user prompt**.
  Live envelope latency is ~10ms typical (per the `resolution_time_ms`
  field). A Qwen-rubric call adds ~300–500ms on the warm path, plus
  initialization variance on cold-start. A default-Qwen design would
  multiply parse_context latency by 30–50×.
- The intent vocabulary is small (8 shapes + none). The starter-set
  prompts (§13.7 fixture) cluster around verb stems: "please solve",
  "please fix", "please implement", "please verify", "what's the
  status", "list open", "audit X", "explain Y", "summarize Z". A
  pattern set per shape covers the dominant case.
- Recall over precision: a message matching multiple shapes' patterns
  takes the highest-priority shape (priority order: fix > implement
  > verify > audit > status > list > explain > summarize > none). A
  borderline prompt that misses every pattern returns `intent: none`
  cleanly; downstream resolvers gracefully skip intent-conditional
  branches.

**When Qwen rubric fires:** the implementation may add a rubric path
later as a confidence backstop, gated behind
`TOOLKIT_PARSE_CONTEXT_INTENT_RUBRIC=1`. Default OFF. The rubric runs
ONLY for messages that the pattern detector tagged `none` AND that
exceed a length threshold (e.g. >50 chars; short prompts are not
worth the inference round-trip). Qwen latency budget: 500ms cap; on
budget exceeded, fall through to `intent: none`.

The constraint from T4: "Pattern-first if pattern-set covers ≥80%."
The §13.7 fixture is the empirical evidence the constraint asks for:
T5 builds the pattern set, runs it against the fixture, and only
ships the rubric path if pattern recall lands below 80%. If patterns
hit ≥80% on the fixture (likely outcome), the rubric path may be
deferred to a follow-on chain entirely.

### 13.6 Detector signature

```go
// IntentShape is the closed vocabulary in §13.2. Values mirror the
// JSON wire shape exactly.
type IntentShape string

const (
    IntentVerify    IntentShape = "verify"
    IntentImplement IntentShape = "implement"
    IntentFix       IntentShape = "fix"
    IntentAudit     IntentShape = "audit"
    IntentExplain   IntentShape = "explain"
    IntentSummarize IntentShape = "summarize"
    IntentStatus    IntentShape = "status"
    IntentList      IntentShape = "list"
    IntentNone      IntentShape = "none"
)

// IntentResult is the typed output the detector and the
// (optional) rubric path both return. Surfaces in the envelope's
// `intent` field after Marshal.
type IntentResult struct {
    Shape       IntentShape `json:"shape"`
    Confidence  float64     `json:"confidence"`
    DetectedVia string      `json:"detected_via"` // "pattern" | "rubric" | "default"
}

// IntentClassifier is the optional Qwen-rubric backstop. Production
// HandlerDeps may set this to nil (pattern-only mode); tests inject a
// stub. Must return promptly (≤500ms target) and degrade gracefully
// on classifier outage.
type IntentClassifier interface {
    ClassifyIntent(ctx context.Context, message string) (IntentResult, error)
}

// DetectIntent is the pure-Go pattern-based detector. Deterministic,
// stateless, safe for concurrent use. Returns IntentNone when no
// pattern matches; callers downstream MAY consult an IntentClassifier
// as a fallback (handler-orchestrated, not embedded in DetectIntent
// itself).
func DetectIntent(message string) IntentResult
```

**Composition with the existing detector:** intent detection runs in
the handler (`handleParseContextCore`), NOT inside `Detector.Detect`.
The existing detector returns `[]Reference`; intent is a single
message-scope value. Keeping them separate avoids polluting the
Reference shape and matches the envelope split (top-level field, not
per-reference).

Handler call order:

```go
// existing
cats := LoadCatalogs(...)
detector := NewDetector(cats, classifier)
refs := detector.Detect(ctx, messageText)

// NEW — runs in parallel with cache split, before Dispatch
intent := DetectIntent(messageText)
if intent.Shape == IntentNone && deps.IntentClassifier != nil && shouldConsultRubric(messageText) {
    if rubricResult, err := deps.IntentClassifier.ClassifyIntent(ctx, messageText); err == nil {
        intent = rubricResult
    }
}

// existing cache split + Dispatch as before
// …

// NEW — intent-conditional resolvers (T6/T7/T8/T9 own each branch)
intentRefs := dispatchIntentConditional(ctx, deps, intent, /* existing context */)

out.References = append(out.References, intentRefs...)
out.Intent = &intent
```

`dispatchIntentConditional` is the T5+ seam: each downstream task
(T6–T9) registers a handler that takes `(ctx, deps, intent)` and
returns additional `[]ResolvedReference` to splice into the envelope.

### 13.7 Test fixture

`go/internal/refresolve/testdata/directive_intent_fixtures.json`
ships with this commit. Schema:

```json
{
  "$schema": "internal — see refresolve/intent_detect.go test loader",
  "fixtures": [
    {
      "shape": "verify",
      "prompts": [
        "please sanity check go-toolkit-dry-extraction-audit-followup",
        "verify that the cache invalidation is firing on TaskStarted",
        "confirm the migration ran cleanly on staging",
        "are the new envelope fields surfacing in the dashboard?",
        "double-check the latency budget hasn't regressed since T8"
      ]
    },
    { "shape": "implement", "prompts": [ … ] },
    …
  ]
}
```

Five-or-more prompts per shape — minimum 45 across the closed set
(excluding `none`). Includes the four real-session prompts T4 named:

| Prompt                                                          | Shape       | Note                                            |
|-----------------------------------------------------------------|-------------|-------------------------------------------------|
| "please sanity check go-toolkit-dry-extraction-audit-followup"  | `verify`    | T4 acceptance criteria; named real-session prompt |
| "please implement that fix after filing it"                     | `implement` | T4 acceptance criteria; named real-session prompt |
| "Any cleanup to do?"                                            | `audit`     | T4 acceptance criteria; named real-session prompt |
| "I'd like the banner to work properly"                          | `fix`       | T4 acceptance criteria; named real-session prompt |

A `none` bucket exists in the fixture too, capturing conversational
prompts that MUST classify as `none` (these are the false-positive
guards for the pattern detector).

### 13.8 Out-of-scope (cold-pickup guard)

This design **does NOT** subsume the harness-level
TaskCreate/TodoWrite over-firing reminders. Those reminders fire from
the Claude Code harness's task-tool heuristic, NOT from any signal
parse_context could plausibly catch — the harness emits them as
system-reminders independent of message content. Three local fix
attempts have failed; upstream escalation lives in the
harness-swap-validation arc.

A cold-pickup agent reading T5+ should NOT try to repurpose intent
detection (or any other parse_context layer) as a workaround for the
over-firing reminders. The fix lives in the harness; this substrate
stays out of its path.

### 13.9 Chain handoff (parse-context-lean-orienting)

T4 closes with this addendum + the fixture. T5
(`T5-directive-intent-ship`) becomes unblocked — it implements
`DetectIntent` + the pattern set, exercises it against the fixture,
and adds the `intent` envelope field.

T6/T7/T8/T9 each consume `intent` to gate their own resolver work.
They become unblocked once T5 lands the seam.

## 14. Directive-intent extension (the `execute` gap)

**Chain:** parse-context-directive-intent-extension (id 305)
**Task:** T1 — scope-directive-intent-resolver
**Date:** 2026-05-26

This section EXTENDS §13. It is additive: every contract in §13.4
(top-level `intent` field, `omitempty`, message-scope) holds
unchanged. §13.2 declares the vocabulary CLOSED and routes extensions
through "a new chain task that revisits the design" — **this chain is
that authorized revisit** (the §13.8 cold-pickup guard is respected:
nothing here repurposes parse_context as a harness-reminder workaround).

### 14.1 Why revisit — the grounded coverage gap

§13.2's vocabulary was derived from a corpus that clustered on
fresh-action verb stems ("please solve", "please fix", "please
implement"). A re-grounding against the live read-side telemetry
substrate (`grounding_events.query_text`, the captured corpus of real
session prompts) tells a sharper story.

**Method:** 125 distinct `parse_context` prompts (the rows where
`query_text` is the user message), classified through the *current*
`DetectIntent` pattern set.

**Result:**

| Bucket                                   | Count | Share |
|------------------------------------------|-------|-------|
| Classified by current vocabulary         | 34    | 27%   |
| `none` (no shape detected)               | 91    | 73%   |

Decomposing the 91 `none` prompts by the directive shape they *should*
carry:

| Latent shape (in `none` today)           | Count | Example prompt |
|------------------------------------------|-------|----------------|
| **execute** — drive named work to completion | **45** | "please work through `<chain>`", "please complete `<chain>` on a new worktree", "please pick up `<chain>` where we left off", "can we finish up `<chain>`?", "`<chain>` please resume" |
| solve — drain a work backlog              | 4     | "please solve our open bugs", "clear the bug backlog using the bug fixing discipline", "empty our bug backlog" |
| review — survey/look over a surface       | 3     | "please take a look at the mcp-servers roadmap", "look through `<chain>` to sanity-check it" |
| genuinely non-directive / tail            | 39    | memory reminders ("remember the gitea-api detail"), context dumps, conversational, and mis-anchored multi-clause directives ("now please implement…", "open a worktree and complete…") |

**The headline finding:** the single dominant directive shape in real
usage — **"execute / drive a named chain or task to completion"** —
is ~36% of all distinct prompts (~49% of the `none` bucket) and is
**entirely absent from the §13.2 closed vocabulary.** The chain's
framing examples ("solve our X / review Y / ship Z / plan Z")
under-represent it: `ship Z` is already covered (`IntentImplement`
matches `ship\s`), `solve`/`review` are small folds, and the real
mass is the chain-execution family the framing never names. Grounding
the inventory (not inventing it) is what surfaces this — it is the
load-bearing scoping result.

A subtlety the design must respect: for execute prompts that **name a
chain/task slug**, the slug is already detected and the chain already
surfaces at `use_directly` via the existing reference resolvers — the
envelope is *not* empty. The gap for those prompts is narrower than
§13.1's "mostly empty envelope": `intent.shape` is `none` (no directive
signal for the agent or downstream consumers) and **no execution-reflex
discipline** (scratchpad) surfaces. The "mostly empty envelope" case
proper is the **no-slug** execute/solve prompt ("clear the bug
backlog", "pick up the next ready task", "solve our open bugs"), where
work-state is the primary — and currently missing — surface.

### 14.2 Detection mechanism — extend the pattern set

**Decision: extend the pure-Go pattern detector in `intent_detect.go`.
Do NOT reach for the Qwen rubric.** Rationale, consistent with §13.5:

- The gap is dominated by a small, stable set of verb stems
  (complete / work through / pick up / continue / start / resume /
  finish / solve / review). That is precisely the pattern-first sweet
  spot; the rubric's value is on prose that resists keyword anchoring,
  which this is not.
- parse_context is the first action on every prompt (§13.5 latency
  argument unchanged). The rubric stays deferred and opt-in
  (`TOOLKIT_PARSE_CONTEXT_INTENT_RUBRIC=1`, default OFF).
- Additivity: extending the pattern map + adding one enum constant
  touches no non-directive path. The §13.3 invariant holds — missing
  intent never gates the token-shape resolvers.

### 14.3 Vocabulary change

Minimize new shapes; fold where an existing shape's downstream wiring
already fits (the refactor⊆audit precedent from
`refactor-intent-discipline-surfacing`).

| Change | Form | Downstream wiring |
|--------|------|-------------------|
| **`execute`** | NEW `IntentShape` constant | new entries in `workStateFiringIntents` + `intentDisciplineMap` (§14.4) |
| `solve` | FOLD into `fix` — extend `intentPatterns[IntentFix]` (`solve our…`, `clear/empty … backlog`) | inherits fix's existing work-state (open bugs) + `bug-fixing-discipline` |
| `review` | FOLD into `audit` — extend `intentPatterns[IntentAudit]` (`review`, `take a look at`, `look through`) | inherits audit's existing work-state + conditional disciplines |
| `plan` | OPTIONAL new shape (`IntentPlan`), deferrable | work-state (chains/roadmap) + `writing-plans` discipline |

Only **one** new constant (`execute`) is required for the core win;
`solve`→fix and `review`→audit are pattern-only extensions that reuse
existing wiring. `plan` is low-frequency (1–2 in corpus) and is carved
out as a separate, droppable build task.

**Priority placement.** Insert `execute` in the priority order
*below* `fix`/`implement`/`verify` but the relationship to those is
near-disjoint (execute verbs don't collide with fix/implement/verb
stems). The load-bearing ordering case: a prompt like "please work
through the fix for bug 1426" — leading verb is "work through"
(execute), so execute must be reachable; "fix" here is a noun. Mirror
of the existing "implement that fix" → implement precedent. Calibrate
against the grown fixture.

### 14.4 Per-shape surface spec

**`execute`** (extends the §13.3 decision matrix):

| Column | Behavior |
|--------|----------|
| work-state | **✓** — add `IntentExecute` to `workStateFiringIntents`. For no-slug prompts this is the primary surface (the open bugs/active tasks ARE the work). **Dedup requirement:** when work-state would surface a chain/task already present in `references[]` from slug detection, drop the duplicate — the build must reconcile work-state refs against the already-resolved token refs so a named-chain execute prompt doesn't double-list its own chain. |
| disciplines | **✓ `scratchpad-discipline`** (single entry; cap stays 2). Chain-execution sessions should maintain a scratchpad — the documented reflex, currently never surfaced for these prompts. NO language-conventions / coding-philosophy: the work shape isn't inferable from an execute verb (the named chain's tasks decide it), and surfacing speculative coding disciplines re-introduces the over-firing failure mode §13.3/T7 guards against. |
| kiwix fallback | skip (not external-doc territory). |
| stdio drift | skip (execution directive, not a verify ritual). |

**`fix` (via solve fold)** and **`audit` (via review fold)**: no map
changes — they inherit the existing rows. Only the detection patterns
grow. The §13.3 doc table's `fix`/`audit` rows already describe the
surface; no mirror edit needed for the folds.

**`plan` (optional)**: work-state ✓ (chains + roadmap being planned);
disciplines `writing-plans`. Add a §13.3 row if built.

The §13.3 table is the human-readable mirror of `intentDisciplineMap`
(it has drifted before — see the note under §13.3). The build that
lands `execute` MUST add the `execute` row to §13.3 **and** the
`execute` shape to §13.2's vocabulary table in the same commit.

### 14.5 Parity net — the gate

Non-directive shapes must be unchanged. The net is built and captured
**before** the detection change lands (characterization-first), and is
the gate the detection + wiring changes must pass.

1. **Classification parity (golden snapshot).** Freeze the current
   `DetectIntent` output for (a) the full §13.7 fixture and (b) the
   125-prompt real corpus, into a golden table. Partition into
   *intentionally-changing* (the execute/solve/review prompts, which
   move `none → execute|fix|audit`) and *must-not-change* (everything
   else). After the change: the must-not-change partition is asserted
   byte-identical; the intentionally-changing partition is asserted to
   move to its new expected shape.
2. **Envelope parity (golden snapshot).** For a sample of non-directive
   prompts (pure reference tokens, conversational, docs-intent
   explain/summarize), assert the *full handler envelope* is
   byte-identical before/after. This catches a new firing-intent or
   pattern leaking into a non-directive path (e.g. work-state firing
   where it shouldn't).
3. **Precision guard (false-positive bucket).** The 39 genuinely-
   non-directive `none` prompts (memory reminders, context dumps,
   conversational) must STILL classify `none` after the new patterns.
   Add the clean non-directive ones to the fixture `none` bucket as
   FP guards. (The mis-anchored multi-clause directives — "now please
   implement…", "open a worktree and complete…" — are a *separate*
   pre-existing anchor limitation; explicitly out of scope here, noted
   as a follow-on candidate, NOT patched in this chain.)
4. **Recall gate holds.** `TestDetectIntent_FixtureRecall` (≥80%, §13.5)
   must pass after the fixture grows with execute/solve/review buckets
   — the new patterns must hit their own new fixture prompts.

### 14.6 Build-task decomposition

Forged into chain 305 (this scoping task is T1):

- **T2 — Characterization & parity net (gate; lands first).** Capture
  the golden classification snapshot (§14.5.1) + non-directive envelope
  snapshot (§14.5.2). Establishes the must-not-change baseline before
  any behavior moves.
- **T3 — Detection extension.** Add `IntentExecute`; extend
  `intentPatterns` for execute + the solve/review folds; place execute
  in the priority order (§14.3); grow `directive_intent_fixtures.json`
  with execute/solve/review buckets (grounded in the corpus) + the new
  `none` FP guards. Gate: parity net must-not-change partition green;
  `FixtureRecall` ≥80% holds; intentionally-changing partition moves.
- **T4 — Surface wiring.** `workStateFiringIntents += IntentExecute`
  with the references[] dedup (§14.4); `intentDisciplineMap[IntentExecute]
  = [scratchpad-discipline]`; update PARSE_CONTEXT.md §13.2 vocabulary
  table + §13.3 decision-matrix row in the same commit. Tests: execute
  surfaces work-state (no-slug case) + scratchpad-discipline; named-slug
  case does not double-list its chain.
- **T5 — (OPTIONAL) `plan` shape.** `IntentPlan` + `writing-plans`
  discipline + work-state + §13.2/§13.3 rows + fixture bucket.
  Deferrable: the core win lands without it. Build only if T2–T4 leave
  budget.
- **T6 — Retrospective + chain close.** File the retro; verify the
  completion_condition (directive shapes classified & surfaced against
  the directive-prompt sample; non-directive parity green); close the
  chain.

