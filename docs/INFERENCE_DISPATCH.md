# Inference dispatch — package boundaries

The four Go packages that coordinate around LLM dispatch have a
load-bearing reason to stay separate. This document is the audit
deliverable for harvest-the-consolidation T5 (2026-05-23) — the chain
asked the question "should these four merge?" and the answer is no.

## The four packages

| Package | LOC | Owns |
|---|---|---|
| `go/internal/inference/router` | ~260 | Model selection (local Qwen via llama.cpp, optional remote Anthropic), retry policy, per-call telemetry stamps (InvocationRecord). |
| `go/internal/qwenretrieve` | ~570 | Retrieve-shape prompt composition + parsing + the two-pass DispatchTwoPassRetrieve orchestration over a candidate list. |
| `go/internal/rubric` | ~280 | Classify-shape prompt composition + parsing (ParseSingleClass / ParseResult) + a TOML rubric registry with hot-reload. |
| `go/internal/measure` (classify.go) | ~410 | MCP action handlers per rubric (HandleClassify…). Orchestrates router + rubric and persists benchmark rows via db.RecordBenchmarkDispatch. |

The boundary was inherited from the Rust ancestors
(inference-clients + rubric-dispatch + measure-lib +
qwen-retrieve sub-crates). Post-Rust-retirement T6 the question was
whether the boundary still earns its keep in a single-language Go
shape. It does.

## Import graph

```
                      ┌─────────────────────────────────┐
                      │ inference/router                │
                      │  Router / Generate / Invoc…     │
                      └────────┬───────────────┬────────┘
                               │               │
              ┌────────────────┘               │
              │                                │
              ▼                                ▼
      ┌──────────────────┐            ┌──────────────────┐
      │ qwenretrieve     │            │ rubric           │
      │  Compose retrieve│            │  Compose classify│
      │  Parse retrieve  │            │  Parse classify  │
      │  DispatchTwoPass │            │  TOML Registry   │
      └────────┬─────────┘            └────────┬─────────┘
               │                               │
               │                               │
               │            ┌──────────────────┘
               │            │
               │            ▼
               │   ┌────────────────────────────────┐
               │   │ measure (classify.go)          │
               │   │  ClassifyDeps                  │
               │   │  per-rubric Handle* handlers   │
               │   └─────────────┬──────────────────┘
               │                 │
               │                 │
               ▼                 ▼
        (knowledge/*,     (arcreview/*,
         arcreview/*,      refresolve/*,
         benchmarks/types) cmd/toolkit-server/*)
```

The graph is a **tree** — no cycles, no diamond. router is a leaf;
rubric is a leaf; qwenretrieve sits on router; measure/classify.go
sits on router + rubric. Every other consumer in the repo
(arcreview, knowledge, refresolve, benchmarks, the cmd/* binaries)
hangs off the bottom, never the middle.

## Public surface each exposes

### router
- Type: `Router`, `InvocationRecord`, `RecordInvocationFunc`,
  `GenerateResult` (transitive from llamacpp).
- Functions: `New(llamaURL)`, `NewWithClients`, `(*Router).Generate`,
  `(*Router).GenerateRemote`, `(*Router).ModelName`,
  `(*Router).SetInvocationRecorder`.
- The HOW of an inference call. Nothing about prompts or labels.

### qwenretrieve
- Type: `RetrieveCandidate`, `RetrieveResult`,
  `TwoPassRetrieveResult`, `CorpusShape` enum.
- Functions: `BuildPrompt`, `ParseRetrieveResponse`,
  `ComposeRetrieve`, `DispatchTwoPassRetrieve`.
- The WHAT of retrieve-shape prompts: an ordered list of candidates
  in, an ordered list of paths out.

### rubric
- Type: `RubricDef`, `Registry`, `ParseResult`, `ParsedLabel` enum.
- Functions: `NewRegistry(dir)`, `(*Registry).Get`, `(*Registry).Reload`,
  `ComposeClassify(def, input)`, `ParseSingleClass(response, allowed)`.
- The WHAT of classify-shape prompts: free-text in, one label out.

### measure (classify.go)
- Type: `ClassifyDeps`, `ClassifyResponse`.
- Functions: `HandleClassifyChainTaskProportionality`,
  `HandleClassifyAgentRoutability`, `HandleClassifyVerbVocab`, …
  (one Handle* per deployed rubric).
- The MCP-action wrapper that turns "run classify_<rubric>" tool
  calls into a router.Generate + rubric.ComposeClassify +
  rubric.ParseSingleClass + db.RecordBenchmarkDispatch cycle.

## Why each split earns its keep

### router is a leaf because it has consumers beyond the cluster
External (non-inference-dispatch) callers of `router`:
- `arcreview/dispatch.go`, `arcreview/handler.go` — review dispatch
- `knowledge/handler.go` — knowledge_search rerank
- `refresolve/domain_term_classifier.go` — reference resolution
- `qwenretrieve/retrieve.go` — retrieve orchestration
- `measure/classify.go` — classify orchestration

Folding `router` into any of the other three would force the
external callers to depend on whichever orchestrator package
absorbed it (qwenretrieve, rubric, or measure). Each of those has
its own scope; pulling router into one increases their public
surface for callers that don't care about prompts/rubrics/handlers.

### rubric is a leaf because it has consumers beyond classify handlers
External (non-measure) callers of `rubric`:
- `refresolve/domain_term_classifier.go` — uses `RubricDef` +
  `ParseSingleClass` to score domain-term candidates without going
  through a classify handler.

Folding `rubric` into `measure` would force refresolve to import
measure to access ParseSingleClass. measure imports db (for
RecordBenchmarkDispatch) and obs (for Logger); refresolve gains a
runtime-telemetry dependency it doesn't currently have.

### qwenretrieve and rubric are separate because their shapes are
non-overlapping
Both compose model prompts. They share zero code:

| | classify | retrieve |
|---|---|---|
| Input | one free-text string | a query + N candidates |
| System prompt | "Output ONLY the chosen label(s)… 'unclassifiable'" | "Rank the candidates… 'no match'" |
| Output | one label from a closed enum | ordered subset of candidate paths |
| Parser | label-set matcher | path-known-set matcher |
| Telemetry | benchmark_results row | retrieve-shape telemetry per pass |

The only structural overlap is "call router.Generate with a (system,
user) pair." That overlap lives in router itself; merging the two
prompt-shape packages would mean two unrelated `Parse…` functions
sharing a namespace.

### measure/classify.go stays in measure because it's MCP-handler-shape
The per-rubric `Handle*` functions are MCP action surface — they
accept `json.RawMessage`, return a `ClassifyResponse`, and live
alongside the other measure handlers
(HandleBenchmarkReplay, HandleTeamContext, HandleSummary…). Folding
classify.go into rubric would either (a) split the measure surface
across two packages (classify-handlers vs. non-classify-handlers),
or (b) drag the rest of measure's handlers into rubric. Neither is
better than what's there.

## Decision: LEAVE-ALONE

The four-package boundary stays. The load-bearing invariants:

1. **Tree-shaped import graph, no cycles.** Adding a merge step
   that introduces a cycle would be evidence the split is correct;
   the current shape proves the leaves (router, rubric) are
   genuinely orthogonal.
2. **Both leaves have consumers outside the cluster.** router is
   used by 5+ packages; rubric is used by refresolve in addition to
   measure. Merging either into an orchestrator widens its public
   surface for callers that didn't need the extra.
3. **The two orchestrators (qwenretrieve, measure/classify.go) own
   non-overlapping concerns.** Retrieve and classify share zero
   code; merging them would just be a renamed namespace with two
   independent ParseXxx functions.

## Implications for future work

- New classify-shape consumers (a rubric for routing, severity, etc.)
  should import `rubric` directly, not `measure`. Only add a
  `HandleClassify…` wrapper to `measure` if the consumer is the MCP
  action surface.
- New retrieve-shape consumers should import `qwenretrieve` directly.
  The CorpusShape enum exists so new corpora land as enum variants,
  not new packages.
- If router gains a third client (a hosted reranker, a different
  remote model), it lives in `inference/<client>` like
  `inference/llamacpp` and `inference/anthropic` do today; router
  composes them. No move into qwenretrieve or measure.
- If a circular import ever shows up between two of the four —
  e.g. measure starting to import qwenretrieve, or rubric starting
  to import router — that's a design smell. The tree shape is
  load-bearing.
