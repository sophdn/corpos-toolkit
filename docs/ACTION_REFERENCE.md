# toolkit-server Action Reference

Canonical reference for every MCP action exposed by toolkit-server. Four meta-tools
route all calls: `work`, `knowledge`, `measure`, `admin`. Each meta-tool accepts
`{action, params, project}`.

Action manifests (TOML + instructions) in `action-manifests/` cover the work-lib
actions in detail. This document is the single source of truth for parameter shapes
and return types; skill instruction files cross-reference here rather than embedding
signatures inline.

---

## `mcp__toolkit-server__work`

Task, chain, and bug lifecycle plus the live roadmap. Read actions are cross-project
by default; write actions require a top-level `project` parameter.
Go-owned since T57 (2026-05-14).

Detailed per-action instructions live in `action-manifests/instructions/<action>.md`.
The table below is the quick-reference index.

The **Rationale** column flags whether the dispatcher enforces a
non-empty `rationale` envelope field on agent-actor calls (per chain
`agent-first-substrate` T3 — see `docs/EVENT_SUBSTRATE.md` §5 +
`action-manifests/dispatch-policy.toml`). The policy registry is the
source of truth; this column tracks it for reader convenience.

| Action | Kind | Rationale | Emits event | Brief |
|--------|------|---|---|-------|
| `chain_status` | read | — | — | Project-wide chain overview (slug, status, task counts) |
| `chain_state` | read | — | — | Task table for one chain |
| `chain_find` | read | — | — | Fuzzy-match chain by name fragment |
| `chain_close` | write | required | `ChainClosed` | Close a chain with closure summary |
| `task_read` | read | — | — | Full task record by slug or id |
| `task_search` | read | — | — | Search tasks across chains; accepts `chain` filter |
| `task_start` | write | required | `TaskTransitioned` | Transition task to active |
| `task_complete` | write | required | `TaskCompleted` | Transition task to closed |
| `task_cancel` | write | required | `TaskCancelled` | Cancel a task |
| `task_reopen` | write | required | `TaskTransitioned` | Reopen a closed/cancelled task |
| `task_block` | write | required | `TaskTransitioned` (with `blocker_slug` when supplied) | Mark task blocked on another task |
| `task_unblock` | write | required | `TaskTransitioned` (when status returns to pending) | Clear a blocker |
| `task_blockers` | read | — | — | List blockers for a task |
| `task_edit` | write | required | `TaskEdited` (with `updated_fields`) | Edit task fields |
| `task_stamp_sha` | write | required | `TaskStamped` | Stamp a commit SHA on a closed task |
| `bug_list` | read | — | — | List bugs; requires project or at least one filter |
| `bug_read` | read | — | — | Full bug record by slug or id |
| `bug_resolve` | write | required | `BugResolved` | Resolve a bug (fixed/wontfix/upstream/dup/routed) |
| `bug_reopen` | write | required | `BugReopened` (with `previous_resolution`) | Reopen a resolved bug |
| `bug_stamp_sha` | write | required | `BugStamped` | Stamp a commit SHA on a bug |
| `roadmap_list` | read | — | — | Cross-project roadmap view |
| `roadmap_set` | write | required | — (see [EVENT_CATALOG][catalog] reserved namespace) | Set roadmap entries |
| `roadmap_insert` | write | required | — (see [EVENT_CATALOG][catalog] reserved namespace) | Insert a roadmap entry |
| `roadmap_diff` | read | — | — | Diff roadmap against current chain state |
| `roadmap_mark_reassessed` | write | required | — (see [EVENT_CATALOG][catalog] reserved namespace) | Mark a roadmap entry as reassessed |
| `forge` | write | required | `ChainCreated` / `TaskCreated` / `BugReported` per schema; — for non-tracked schemas | Create any typed artifact (chain, task, bug, …) |
| `forge_edit` | write | required | `ChainEdited` / `TaskEdited` / `BugEdited` per schema; — for non-tracked schemas | Edit an existing typed artifact |
| `forge_delete` | write | required | — (see [EVENT_CATALOG][catalog] non-emitting actions) | Delete; rejected for chain/task/bug |
| `forge_schemas` | read | — | — | List all registered schemas |
| `forge_schema` | read | — | — | Fields for one schema |

[catalog]: EVENT_CATALOG.md

See `docs/EVENT_CATALOG.md` for the per-type payload-summary table and the "Intentionally non-emitting actions" note covering `forge_delete` and the task_block-without-status-change branch.

The `rationale` field sits at the top level of the call envelope
(alongside `action`, `params`, `project`, `cwd`), not inside `params`.
Boilerplate / whitespace / <6-char rationales on agent-actor calls are
rejected with `error: "rationale_required"` before the handler runs.
See `skills/rationale-discipline.md` for the agent-side discipline.

### Cross-substrate telemetry (resolution-class actions)

`bug_resolve`, `task_complete`, `task_cancel`, and `chain_close` emit
their terminal write-side event as listed above. A corresponding row
in the read-side `query_resolutions` table is materialised **post-session
by the grounding-events Stop hook**, not by the handler — the hook is
where `prompt_id` is known (transcript JSONL) and the trajectory's
preceding `grounding_events.id` set can be fully assembled. The hook
calls `telemetry.EmitResolution` with `write_event_ids=[the just-emitted
event_id]` plus the grounding-event ids and any `resolved-from` click
rows from the same prompt arc. See `docs/TELEMETRY_SUBSTRATE.md` §4 and
§12 for the cross-substrate FK contract.

Why post-session, not in-handler: at handler time `prompt_id` is not
yet stamped (live emits leave it NULL; the Stop hook stamps it from
the transcript's `promptId` field), and the searches that fed the
resolution have *different* `span_id`s than the resolving event (each
`tools/call` is its own span). The hook is the natural locus where
both axes are known together.

---

## `mcp__toolkit-server__knowledge`

All knowledge actions — served by the Go toolkit-server (migrated 2026-05-13).
Library entries, kiwix search, reference management, vault retrieval, and the unified
knowledge index.

### Telemetry contract (vault_search / kiwix_search / knowledge_search)

Every search call writes one row to `grounding_events` at dispatch time with the
columns added through migration 037 (query-telemetry-substrate TT2):
`span_id` (per-`tools/call`), `query_source` (defaults to `agent_initiated`;
`proactive_hook` / `dashboard_user` / `other` available), `user_message_id`
(when the dispatcher knows it), and `query_text` (the raw query string).
`prompt_id` and `parent_span_id` are stamped post-session by the
grounding-events Stop hook.

Downstream click signals (`followed` / `cited` / `mentioned` / `resolved-from`,
per `docs/TELEMETRY_SUBSTRATE.md` §5 and the TT1.5 spike close in
`docs/TELEMETRY_LABEL_SPIKE.md` §4) produce one row per fired tier in
`query_interactions`, unique on `(span_id, source_ref, click_kind)`.

### vault_search

Ranks notes from `~/.claude/vault/` via local Qwen2.5-32B over the full path list.

```
action: "vault_search"
params:
  query:  string   # task domain in one sentence
  top_k:  int      # default 5
# no `project` param — vault is always cross-project
```

Returns: array of `{path, score}` sorted by relevance. Empty array = no match (not a
hallucination). Typical latency: 600 ms–1.5 s.

### vault_read

Fetches the full content + parsed frontmatter of one vault note.

```
action: "vault_read"
params:
  path: string   # path from vault_search result
```

Returns: `{path, frontmatter: {...}, content: string}`

### knowledge_search

Unified FTS5 + Qwen retrieval over all indexed knowledge sources: vault, kiwix, tasks,
chains, library, bugs.

```
action: "knowledge_search"
params:
  query:  string   # task domain in one sentence
  top_k:  int      # default 5
```

Returns:

```json
{
  "results": [
    {
      "id": 120,
      "source_type": "vault | kiwix | library | task | chain | bug",
      "source_ref": "path_or_ref",
      "question": "...",
      "invoke_when": "...",
      "quality_score": 0.8,
      "usage_count": 3,
      "negative_feedback_count": 0
    }
  ],
  "results_count": 5,
  "query": "...",
  "qwen_fell_back": false
}
```

`source_ref` format: for kiwix entries, `project_id::kiwix_ref_id` (not `zim_id@id`).

### knowledge_fetch

Resolves a knowledge pointer to its full content, routing by `source_type`.

```
action: "knowledge_fetch"
params:
  source_type: string   # from knowledge_search result
  source_ref:  string   # from knowledge_search result
```

Routing:
- `vault` → reads file at `source_ref`
- `kiwix` / `kiwix_reference` → fetches article
- `library` → returns library entry
- `task` → returns `handoff_output`
- `chain` → returns `closure_summary`
- `bug` → returns `problem_statement` + resolution

Returns: `{status: "stale"}` if the source no longer exists — call `knowledge_report_miss`.

### resolve_references

Scan a user message; emit one resolved reference per detected reference-shape token across eleven shape categories (chain/task/bug slug, path, skill / project / tool name, forge schema, library entry, domain term, external technical). Unified-dispatch alternative to per-shape lookups; verbose-and-explicit presentation.

```
action: "resolve_references"
params:
  message_text:    string  # required — the user message to scan
  top_k_per_shape: int     # optional, default 5 — per-shape candidate limit
  include_no_hits: bool    # optional, default false — return references that returned no_hit
  total_budget_ms: int     # optional, default 2000 — dispatcher-wide latency cap
```

Returns: `ResolveReferencesResult { references: [{token, shape, confidence_tier, presented_as, top_candidates, recommended_action}], resolution_time_ms, resolver_calls_made, no_hit_tokens, partial_failures, truncated_by_budget }`.

Confidence tiers: `single_exact` / `fuzzy_multi` / `weak_domain` / `no_hit`. Recommended actions: `use_directly` / `ask_user_to_disambiguate` / `mention_as_possibly_relevant` / `acknowledge_no_hit_and_ask`. The agent surfaces `presented_as` verbatim or paraphrased — never silently injects.

See `docs/REFERENCE_RESOLUTION.md` for the full design (shape taxonomy, dispatch contract, tier rules, telemetry shape, hook supersession). The agent-side discipline lives in `skill:reference-resolution`.

### knowledge_report_miss

Signals that a followed pointer was wrong or stale.

```
action: "knowledge_report_miss"
params:
  pointer_id:       int     # id from knowledge_search result
  staleness_reason: string  # optional
```

Increments `negative_feedback_count` and optionally sets a staleness hint.

### kiwix_list_books

Returns the catalog of available ZIM corpora.

```
action: "kiwix_list_books"
# no params
```

Returns: array of `{zim_id, title, article_count}`. Use `zim_id` as the `books` param
in `kiwix_search`. Note: ZIM IDs from `kiwix_list_books` lack the version suffix that
search results carry — use the ZIM ID from a `kiwix_search` hit for `snapshot_id` in
`reference_add`.

### kiwix_search

Full-text search within one ZIM, Qwen-reranked.

```
action: "kiwix_search"
params:
  zim_id:  string   # from kiwix_list_books
  pattern: string   # search query
  limit:   int      # default 10
```

Returns:

```json
{
  "hits": [{"title": "...", "article_ref": "slug-or-path", ...}],
  "qwen_fell_back": false,
  "hits_in": 20,
  "hits_out": 10
}
```

`article_ref` from a hit is the `slug` param for `kiwix_fetch`. Typical latency: ~2.2 s.

### kiwix_fetch

Fetches the full text of one ZIM article.

```
action: "kiwix_fetch"
params:
  zim_id: string   # from kiwix_list_books or kiwix_search hit
  slug:   string   # article_ref from kiwix_search hit
```

### reference_add

Pins a canonical external reference to the unified DB.

```
action: "reference_add"
params:
  zim_id:       string    # ZIM from kiwix_search hit
  article_slug: string    # article_ref from kiwix_search hit
  snapshot_id:  string    # versioned ZIM ID from kiwix_search hit (not kiwix_list_books)
  why:          string    # 1–2 sentences explaining relevance
  tags:         [string]  # kebab-case tokens
```

### reference_find

Locates pinned references by text or tags.

```
action: "reference_find"
params:
  text: string     # free-text search
  tags: [string]   # optional tag filter
```

### reference_retire

Archives an obsolete pinned reference.

```
action: "reference_retire"
params:
  id: int   # reference id
```

---

## `mcp__toolkit-server__measure`

All measure actions are now served by the Go toolkit-server (migrated
2026-05-13 — `classify_*` in T32, `benchmark_record` + `benchmark_query`
in T46). The retired surfaces — session journal, emotive batteries,
friction analytics, full-form retrieval — are no longer available
anywhere; do not reach for `mcp__toolkit-server__measure`.

Dispatch rubrics to local Qwen2.5-32B and write a `benchmark_results` row.
Typical latency: 600–1800 ms (full worked-example prompts).

### benchmark_record

Insert one benchmark_results row. Required for telemetry callers that
write outside the auto-recording classify path (e.g. external benchmark
runners, regression suites).

```
action: "benchmark_record"
params:
  scenario_id:        string   # required; per-scenario identifier
  tool_name:          string   # required; action being benchmarked
  model_name:         string   # required; e.g. "qwen2.5-32b"
  run_at:             int      # required; unix seconds
  wall_clock_ms:      int      # required
  invocation_ok:      int|bool # required; 1/true for success
  id:                 string   # optional; uuid v4 generated if omitted
  run_id:             string   # optional; groups rows from one run
  input_tokens:       int      # optional
  output_tokens:      int      # optional
  invoked_contextually: int|bool # optional; default 1
  args_match:         int|bool # optional
  extracted_args:     string   # optional
  interpretation_ok:  int|bool # optional
  detected_tool:      string   # optional
  notes:              string   # optional
  task_shape:         string   # optional; one of Extract/Classify/Retrieve/Summarize
  accuracy_score:     float    # optional
  honesty_score:      float    # optional
  ranking_quality_score: float # optional
  within_budget_score:   float # optional
project: string   # required (top-level field)
```

Returns: `{ok: true, id: "<uuid-v4>"}`.

### benchmark_query

Query benchmark_results with optional filters. Most-recent-first ordering.

```
action: "benchmark_query"
params:
  tool_name:  string   # optional filter
  model_name: string   # optional filter
  run_id:     string   # optional filter
  since:      int      # optional; unix seconds — run_at >= since
  limit:      int      # optional row cap
project: string   # optional; scopes to one project
```

Returns: an array of BenchmarkResult rows. All 22 columns are returned;
nullable columns appear as JSON `null` when unset.

### classify_chain_task_proportionality

Evaluates whether a task's scope is proportionate to a chain step.

```
action: "classify_chain_task_proportionality"
params:
  task_spec:            string   # problem statement + acceptance criteria
  team_context_override: string  # optional; derived from telemetry if omitted
project: string   # required for telemetry derivation
```

Returns: `{label, latency_ms, model_name, team_context_prose}`
`label` ∈ `{proportionate, disproportionate, unclear}`. Latency: 200–300 ms.

Act on `unclear` by consulting the user; do not silently default to proportionate.

### classify_session_routing_trigger

Classifies a user input to determine which routing path to take.

```
action: "classify_session_routing_trigger"
params:
  user_input: string   # raw user message
```

Returns: `{label, latency_ms, model_name}`
`label` ∈ `{context-handoff, execute-document, retirement-dispatch,
chain-execution, tool-suggest, no-trigger}`.

### classify_artifact_tier

Classifies an artifact by the session tier at which it should be loaded.

```
action: "classify_artifact_tier"
params:
  artifact_descriptor: string   # path + one-line prose description
```

Returns: `{label, latency_ms, model_name}`
`label` ∈ `{tier-zero, tier-one, tier-two, tier-three}` (word-form; digit-suffix form
triggers a parser bug — never use `tier-0` etc.). Latency: ~150 ms.

### classify_retirement_observation

Classifies a project activity observation by retirement artifact-type.

```
action: "classify_retirement_observation"
params:
  observation_text: string   # self-contained prose: artifact name, metric, context
```

Returns: `{label, latency_ms, model_name}`
`label` ∈ `{tool-retirement, skill-retirement, workflow-retirement,
not-retirement}`. Latency: ~190 ms.

### classify_artifact_review_criterion

Evaluates one criterion against one artifact excerpt under a named review purpose.

```
action: "classify_artifact_review_criterion"
params:
  artifact_excerpt: string   # the content under review
  purpose:          string   # safety | completeness | scope-fit | quality |
                             # coherence | scope-drift | custom
  criterion:        string   # the specific criterion to evaluate
```

Returns: `{label, latency_ms, model_name}`
`label` ∈ `{pass, fail, mixed, n-a}`. Bias toward `mixed` over `fail` when the
violation is partial. Latency: 130–750 ms.

### classify_audit_finding_severity

Classifies one agentic-architecture-audit finding by consequence severity.

```
action: "classify_audit_finding_severity"
params:
  finding_prose: string   # one finding, self-contained
```

Returns: `{label, latency_ms, model_name}`
`label` ∈ `{critical, high, medium, low}`. Severity tracks consequence, not effort:
- `critical` — actively harmful (trust/data violation)
- `high` — structurally wrong, compounds over time
- `medium` — naive, misses opportunity
- `low` — hygiene, no architectural consequence

Latency: 130–760 ms.

### classify_pre_commit_failure

Classifies the dominant failure cause in a pre-commit hook stderr dump.

```
action: "classify_pre_commit_failure"
params:
  stderr: string   # raw hook output
```

Returns: `{label, latency_ms, model_name}`
`label` ∈ `{lint, typecheck, test, lifecycle, unclassifiable}`.

### classify_docstring_drift

Detects whether a function's doc comment has drifted from its implementation.

```
action: "classify_docstring_drift"
params:
  function_snippet: string   # full function body with doc comment
```

Returns: `{label, latency_ms, model_name}`
`label` ∈ `{matches, doesn't_match, unclear}`. Note inverted semantics: `matches`
means drift is detected; `doesn't_match` means the docstring is still accurate.
Use `unclear` as a fallback trigger for manual review. Smoke: ~70% accuracy.

---

## `mcp__toolkit-server__admin`

Server management. All admin actions are read-only except `project_register` and
`schema_reload`.

| Action | Brief |
|--------|-------|
| `project_list` | List registered project IDs |
| `project_register` | Register a new project ID |
| `server_health` / `health` | Liveness check |
| `schema_version` | Current schema registry version |
| `server_version` | Binary version |
| `host_list` | Known host entries |
| `vault_search_metrics` | Vault search hit/miss rates |
