# ML Capability Substrate — Design

> **Status:** Draft for review. Produced by chain `ml-capability-substrate` T1 (`design-ml-capability-substrate`). Decisions here are durable; downstream tasks T2–T8 bind to them. Amendments after this doc lands require a chain-level decision, not a unilateral task edit.
>
> **Reading order:** §1 framing → §2 three-project architecture → §3 ONNX as the deployment contract → §4 `trained_model` forge schema + lifecycle → §5 `go/internal/ml/` Go subsystem → §6 MCP inference surface → §7 prediction telemetry → §8 A/B harness + promotion gate → §9 five ml-temp candidates walked through the substrate → §10 future-AT migration path → §11 `ml-opportunity-scan` skill → §12 open questions.
>
> **Companion docs:** `docs/EVENT_SUBSTRATE.md` (write-side audit ledger; this chain inherits the `span_id` contract and the `_envelope.json` shape). `docs/TELEMETRY_SUBSTRATE.md` (read-side telemetry; this chain consumes the `query_source` enum, the `grounding_events` row shape, and `proj_training_data_for_reranker`). `docs/REFERENCE_RESOLUTION.md` (reference-binding layer; its §10 names this chain as the materialization site for T7's deferred classifier and reranker integration).
>
> **Cross-chain dependencies:** `agent-first-substrate` T1 + T5 (event envelope + `span_id`, closed 2026-05-17). `query-telemetry-substrate` TT1 + TT2 + TT4 (telemetry shape + migration 037 + `proj_training_data_for_reranker`, closed 2026-05-17). `reference-resolution-substrate` (closed 2026-05-18; its T7 cancelled-as-deferred INTO this chain — see §8.4).

---

## 1. What this substrate is and isn't

The agent-first observation: every time an agent burns tokens reasoning about routing, scoring, tagging, or filtering — when the answer is deterministic given enough labeled history — that's a trained-model opportunity. The vault note `2026-05-15_ml-capability-vs-models-framing.md` makes the load-bearing claim: **build the capability once; each model is a small marginal cost on top.** This chain is that once-build.

What ships:

- A new **`~/dev/ml-training/`** Python sibling project — training scripts, ONNX exporters, eval harnesses, and the `models/<task>/<version>/` artifact tree.
- A new **`go/internal/ml/`** subsystem in mcp-servers wrapping ONNX Runtime via `onnxruntime_go` — in-process inference inside the canonical toolkit-server Go binary.
- A **`trained_model`** forge artifact with a five-state lifecycle (`training` → `evaluating` → `ab_testing` → `promoted` → `retired`).
- A new **`inference(model_id, features)`** MCP action plus a registerable convenience-action pattern (`route_query`, `curation_score`, `forge_suggest_surfaces`, etc.).
- An **A/B hot-swap harness** that compares baseline (Qwen rubric / in-LLM scoring) against the trained model on every call until promotion.
- The **`ml-opportunity-scan`** skill — discipline for triaging future ML candidates.

What does *not* ship:

- No specific trained model. The five `~/Documents/files/ideas-to-process/ml-temp/` candidates become their own chains AFTER this substrate lands. The first model exists only as a *placeholder source-router* in `ml-training/` proving the end-to-end loop.
- No GPU-serving infrastructure. Training may use GPUs; serving is CPU-only by commitment (§3.2).
- No model marketplace, external fetch, or third-party-hosted models.
- No dependency on `atomic-tasks`. AT is still in pre-offload hardening (chain 256, open); this substrate ships under regular chain+tasks orchestration with the AT migration path documented in §10.
- No retirement of existing Qwen-rubric implementations. They stay as baselines for the A/B harness to compare against. Retirement is a per-model decision after promotion.

### 1.1 Fourth substrate, completing the agent-first quartet

| Chain | Surface | Records | Status |
|---|---|---|---|
| `agent-first-substrate` | Write-side audit ledger (`events`, `_envelope.json`, rationale enforcement) | One row per agent mutation. | Closed 2026-05-17. |
| `query-telemetry-substrate` | Read-side telemetry (`grounding_events`, `query_interactions`, `query_resolutions`, `proj_training_data_for_reranker`) | One row per search + per click signal + per terminal resolution. | Closed 2026-05-17. |
| `reference-resolution-substrate` | Reference detector + resolver registry + `resolve_references` action + hook supersession | One row per detected reference + resolution. | Closed 2026-05-18. |
| `ml-capability-substrate` (this chain) | Trained-model serving layer (`go/internal/ml/`, `trained_model` schema, `inference` action, A/B harness) | One row per prediction + per A/B comparison. | In progress. |

After this chain closes, every read / write / reference path has a trained-model deployment story. The five `ml-temp` candidates are no longer roadmap entries — they become forge-able chains that each spend ~20% of this chain's plumbing budget plus their own model-specific work (§9).

### 1.2 Why Go-native, not Rust

The chain originally forged on 2026-05-18 specified a Rust `ml-serve` crate using the `ort` crate. A pre-T1 audit on 2026-05-19 surfaced a contradiction: mcp-servers consolidated onto Go through T20–T69 of the prior substrate work. The Cargo.toml comments at workspace root explicitly document the retirement of the Rust `toolkit-server`, `work-lib`, `measure-lib`, `forge-lib`, `knowledge-lib`, and `rubric-lib` crates in favor of `go/internal/...` packages. Adding a new Rust crate cuts against that trajectory.

`onnxruntime_go` wraps the same C++ onnxruntime kernel that `ort` wraps. Inference performance is comparable. The Go-native path wins on:

- **Zero FFI / zero sidecar.** Inference lives in-process with the canonical toolkit-server Go binary that the MCP surface, hooks, dashboard, and agents all already reach.
- **No new language boundary.** Existing patterns in `go/internal/measure/`, `go/internal/inference/`, `go/internal/qwenretrieve/` transfer directly — same conventions, same testing infrastructure, same migration pattern.
- **No new build dimension.** Cargo workspace, precommit gate, post-commit advisor, and stdio-binary-staleness ritual (mcp-servers `CLAUDE.md` §"Stdio-session preservation") all keep working as-is.

Adjacent ONNX state on this machine (audited 2026-05-19) does **not** dictate this choice:

- **seed-packet `tools/embedder/`** uses Rust + `fastembed = 4` + `ort 2.0.0-rc.9` (ONNX runtime cached at `~/.cache/ort.pyke.io/`). Installed via chain `embedding-substrate` (id 151, closed 2026-04-30). Different project; different use case (embedding for LOCI/navigation search, not trained-model inference). Stays as-is.

The mcp-servers `go/internal/ml/` install is independent; the audit found no cross-project coupling that would constrain the build choice.

---

## 2. Three-project architecture

```
~/dev/
├── ml-training/                  ← NEW (this chain, T2): Python; training + export
│   ├── pyproject.toml
│   ├── training/<task>/          ← per-model training scripts
│   ├── export/                   ← shared ONNX-export helpers
│   ├── eval/                     ← held-out harnesses
│   ├── data/                     ← loaders against toolkit.db
│   └── models/<task>/<version>/  ← artifact tree (model.onnx + manifest.toml + eval_report.json)
│
├── mcp-servers/                  ← THIS REPO (T3, T4, T5, T6)
│   ├── go/internal/ml/           ← Go ONNX serving via onnxruntime_go
│   ├── blueprints/forge-schemas/trained_model.toml  ← T4
│   ├── go/internal/work/         ← trained_model lifecycle actions (T4)
│   └── docs/ML_CAPABILITY_SUBSTRATE.md  ← this doc
│
└── atomic-tasks/                 ← NOT load-bearing for this chain; see §10
```

### 2.1 `ml-training` — Python; training + export only

Why Python: the ML ecosystem (sklearn, transformers, optimum, sentence-transformers, datasets) is overwhelmingly Python-native. Trying to train in Go or Rust would forfeit the entire ecosystem for marginal language consistency.

Why a sibling project, not a subdirectory of mcp-servers: separation of concerns. mcp-servers is Go-with-some-Rust-history; bringing Python into the same repo means a Python toolchain in CI, new precommit-gate stages, and a tangled dependency story. Greenfield sibling repo keeps each project's toolchain story clean.

Why a single project, not one per model: the vault-note framing trap. Per-model projects re-derive plumbing inconsistently; one project with a `training/<task>/` subdirectory per model amortizes shared infrastructure.

Layout:

| Path | Purpose |
|---|---|
| `pyproject.toml` | Python 3.11+; deps via `uv` (seed-packet precedent doesn't dictate but is consistent). Dev deps include sklearn, transformers, optimum, sentence-transformers, onnxruntime (for sanity-checking exports). |
| `training/<task>/` | One subdir per ML task (`source-router/`, `curation-classifier/`, `cross-encoder-reranker/`, …). Each has `train.py`, fixtures, README. |
| `export/` | Shared helpers: `sklearn_to_onnx.py`, `transformers_to_onnx.py`, `quantize_int8.py`. |
| `eval/` | Held-out harness templates; per-task eval scripts subclass these. |
| `data/` | Data loaders that read `~/.local/share/toolkit/data/toolkit.db` (read-only). Includes a `proj_training_data_for_reranker` query helper exposed to all per-task `train.py` scripts. |
| `models/<task>/<version>/` | Artifact tree. Each version contains `model.onnx`, `manifest.toml` (training-dataset signature, eval metrics, exporter version), and `eval_report.json` (held-out scores). Git-tracked directly for small models (<1MB); LFS for ONNX exports >50MB. |

The `ML_MODELS_ROOT` env var defaults to `~/dev/ml-training/models/`. `go/internal/ml/` reads from there at startup (lazy load).

### 2.2 `mcp-servers` — Go serving

The serving side adds:

- `go/internal/ml/` — ONNX runtime wrapper, model registry, inference handler.
- `blueprints/forge-schemas/trained_model.toml` — the `trained_model` schema (§4).
- `go/internal/work/trainedmodel/` — `trained_model_list`, `trained_model_promote`, `trained_model_retire` actions.
- New MCP action `inference(model_id, features) → prediction` in the existing `mcp__toolkit-server__measure` or a new `mcp__toolkit-server__ml` surface — §6.1 decides.
- A/B harness wrapper utility in `go/internal/ml/abtest/` — §8.
- Migration `NN_add_ab_comparisons.sql` (canonical at `crates/shared-db/migrations/`, mirrored to `go/internal/db/migrations/` and `go/internal/testutil/migrations/` per mcp-servers `CLAUDE.md` §"Migrations").

### 2.3 `atomic-tasks` — not load-bearing

AT is in pre-offload hardening (chain 256, open). The substrate is designed to migrate cleanly into AT chain orchestration when AT matures (§10), but ships today under regular chain+tasks orchestration. T2 bootstraps `ml-training` but does NOT ship an AT chain template; the AT migration path is documentation, not a dependency.

---

## 3. ONNX as the deployment contract

### 3.1 Train in any framework; export to ONNX; serve from Go

ONNX (Open Neural Network Exchange) is the substrate's pivot point. The contract is:

1. Training writes `models/<task>/<version>/model.onnx` plus `manifest.toml` and `eval_report.json`.
2. Serving (`go/internal/ml/`) loads `model.onnx` by `(task, version)`, validates input/output shapes against `manifest.toml`, runs inference.

Framework choice during training is open: sklearn (via `skl2onnx`), PyTorch (via `torch.onnx.export` or `optimum`), HuggingFace transformers (via `optimum.exporters.onnx`). The serving layer never sees the source framework.

Concretely, the five candidates map as:

| Candidate | Training framework | Export route | Model size class |
|---|---|---|---|
| Source router | sklearn `MultiOutputClassifier` over sentence-transformer embeddings | `skl2onnx` for the LR head; embedding model is a separate ONNX (cached in `models/embedders/all-MiniLM-L6-v2/v1/`) | Tiny LR head + ~80MB shared embedder |
| Curation classifier | sklearn TF-IDF + logistic regression | `skl2onnx` | <1MB |
| Cross-encoder reranker | HuggingFace `cross-encoder/ms-marco-MiniLM-L-6-v2` fine-tuned | `optimum.exporters.onnx`, int8 quantize | ~80MB → ~25MB int8 |
| Bug surface tagger | sklearn TF-IDF + multi-label LR | `skl2onnx` | <1MB |
| Skill auto-loader | sentence-transformer + sklearn LR | `skl2onnx` for the LR head; shared embedder reused | Tiny LR head + shared embedder |

The "shared embedder" pattern (source router + skill auto-loader both depend on `all-MiniLM-L6-v2` ONNX export) is handled by treating embedders as just another model in the registry — `models/embedders/<name>/<version>/`. The per-task model declares it as a prerequisite in its `manifest.toml`.

### 3.2 CPU-first inference (always)

Per local-ml-roadmap principle: "if a model needs a GPU to serve, it's the wrong model." All inference is CPU. Quantization to int8 is the standard size/latency optimization. Models that can't hit <50ms on CPU at int8 are wrong-shaped for this substrate and need re-scoping at training time, not infrastructure changes at serving time.

GPU is eligible for **training** (which lives in `ml-training/` and can use whatever's available on the dev box). Never for serving.

### 3.3 Per-version artifacts, not per-version directories

`models/<task>/<version>/` lets multiple versions coexist on disk. The `trained_model` forge schema (§4) tracks each `(task, version)` pair as a row with status. Promotion is a metadata flip on the row, not a file move.

`manifest.toml` carries:

```toml
[model]
task = "source-router"
version = "v3"
exporter = "skl2onnx-1.16.0"
export_date = "2026-05-21"

[input]
schema = ["query_text:string"]
preprocess = "embed:all-MiniLM-L6-v2/v1; vectorize:none"

[output]
schema = ["per_source_probability:vector<float32, 7>"]
postprocess = "sigmoid; threshold:0.5"

[training]
dataset_signature = "proj_training_data_for_reranker@2026-05-20T18:42:11Z;rows=2841"
config_hash = "9c4d18e3..."

[eval]
held_out_size = 568
metric = "macro_f1"
score = 0.71
baseline_qwen_rubric_score = 0.58
```

The signature in `dataset_signature` is the canonical drift-detection handle. When the training data shifts (new rows added, projection changes), the signature changes; the serving layer's prediction-logging carries this through telemetry (§7).

---

## 4. The `trained_model` forge schema + lifecycle

### 4.1 Schema

`blueprints/forge-schemas/trained_model.toml`:

| Field | Type | Required | Description |
|---|---|---|---|
| `slug` | string | yes | Kebab-case identifier, unique per project. Convention: `<task>-<version>` (e.g. `source-router-v1`). |
| `task` | string | yes | What the model does (one short sentence). |
| `version` | string | yes | Semver-shape (`v1`, `v1.1`, `v2`). Increments are author-controlled per-task. |
| `training_dataset_signature` | string | yes | The projection-snapshot identifier carried through training. Used for drift detection. |
| `eval_metrics` | JSON | yes | Held-out scores; keys are metric names. Always includes `baseline_score` for the metric the A/B gate compares on. |
| `status` | enum | yes | `training` \| `evaluating` \| `ab_testing` \| `promoted` \| `retired`. |
| `artifact_path` | string | yes | Relative path under `ML_MODELS_ROOT` — e.g. `source-router/v1/model.onnx`. |
| `created_at`, `updated_at` | timestamp | auto | Standard. |

### 4.2 Lifecycle state machine

```
   forge → training ─────────► evaluating ─────────► ab_testing ─────────► promoted
                                    │                                          │
                                    │                                          ▼
                                    └─────────────► retired ◄──────── retired
```

State transitions:

- **`training`** — created at the start of a training run; the row exists so logs can reference the model_id, but no artifact is on disk yet.
- **`evaluating`** — training run complete; `eval_metrics` populated; artifact on disk. Model is loadable but is NOT yet wired to any code path. Use for held-out validation and corpus spot-checks.
- **`ab_testing`** — A/B harness is firing baseline + this model in parallel; per-call comparisons accumulate.
- **`promoted`** — the path uses this model as primary; baseline (if any) is dropped or kept as fallback.
- **`retired`** — superseded by a newer version, replaced by a different baseline, or determined unfit. Artifact stays on disk for git-traceability.

State enforced via CHECK constraint AND schema validator (belt-and-suspenders, per existing forge-schema convention).

### 4.3 Lifecycle actions

Action surface (lives in `go/internal/work/trainedmodel/`):

| Action | Effect |
|---|---|
| `trained_model_list` | Compact projection: `(id, slug, task, version, status, eval_metric_summary)`. Filterable by `task`, `status`. |
| `trained_model_promote` | `ab_testing` → `promoted`. Validates A/B promotion gate has fired (§8.3) unless `force=true` (audited override). |
| `trained_model_retire` | Any state → `retired`. Records reason. |

`forge(trained_model, …)` creates rows; `forge_edit` updates `eval_metrics` and `status`. Status transitions enforce the state machine via validator (no skipping; no reversing except via explicit `trained_model_retire` from `promoted`).

### 4.4 No `inference` against `training` / `retired`

The serving layer (§5) refuses to load a model whose row is in `training` or `retired` status. `evaluating` is loadable but not exposed via convenience actions (it's spot-check territory). `ab_testing` and `promoted` are the live-traffic states.

---

## 5. The `go/internal/ml/` Go subsystem

### 5.1 Package shape

```
go/internal/ml/
├── doc.go             ← package overview, conventions
├── registry.go        ← model-registry: load-by-(task, version) via trained_model rows
├── runtime.go         ← onnxruntime_go wrapper; session pool; load/infer/unload
├── inference.go       ← inference handler: validate → infer → return + telemetry
├── abtest/            ← A/B harness; subpackage (§8)
└── inference_test.go  ← stub-model integration test
```

### 5.2 Dependencies

`go.mod` adds:

```
github.com/yalue/onnxruntime_go v1.x
```

(Or the chosen Go binding for ONNX Runtime. Selection criterion: actively maintained, wraps the official C++ kernel, supports CPU EP and int8 quantization. `yalue/onnxruntime_go` is the current default; T3 verifies before final commit.)

The ONNX Runtime native library (`libonnxruntime.so.1.x.x`) ships via one of two paths — T3 picks one:

1. **Auto-download at build time** — the Go binding's build script fetches the binary into a project-local cache (similar to seed-packet's `fastembed`/`ort-download-binaries` pattern).
2. **System-installed** — the binary is installed at `/usr/local/lib/libonnxruntime.so` (or equivalent) by an explicit setup step; the Go binding uses `LD_LIBRARY_PATH`.

Option 1 is the default; option 2 is the fallback if option 1 doesn't work cleanly on the dev box.

### 5.3 Model loading + the registry

`registry.go` exposes:

```go
type Registry struct { /* internal: cache, db handle */ }

// LoadByPromoted picks the currently-promoted model for a task.
func (r *Registry) LoadByPromoted(ctx context.Context, task string) (*Model, error)

// LoadByID picks a specific (task, version) by trained_model.id.
func (r *Registry) LoadByID(ctx context.Context, id int64) (*Model, error)

// Reload drops cached models and re-resolves from DB. Called by trained_model_promote.
func (r *Registry) Reload(ctx context.Context) error
```

Models are cached as `onnxruntime_go.Session` instances. Cache eviction is LRU with a configurable max-loaded-models budget (default 8 — enough for the five candidates plus the shared embedder plus headroom).

`Reload` is the hot-swap primitive. When `trained_model_promote` flips a row from `ab_testing` to `promoted`, it calls `Registry.Reload()`. Subsequent `LoadByPromoted` calls return the new model immediately. No process restart.

### 5.4 Inference handler

```go
type Prediction struct {
    Output      any          // type-dispatched: []float32, map[string]float32, etc.
    LatencyMs   float64
    ModelID     int64
    FeatHash    string       // SHA-256 of canonical-serialized features
}

func (m *Model) Infer(ctx context.Context, features any) (Prediction, error)
```

The handler:

1. Validates `features` against the model's `manifest.toml` input schema.
2. Runs preprocessing per `manifest.toml` (e.g. embed-then-vectorize for the source-router).
3. Calls `Session.Run()`.
4. Runs postprocessing (e.g. sigmoid + threshold for multi-label).
5. Returns `Prediction` with latency and feature-hash for telemetry (§7).

Errors:
- `ErrModelNotFound` — no row, or row in `training`/`retired` state.
- `ErrInputShape` — features don't match manifest.
- `ErrInferenceTimeout` — > 5s (sanity-check; healthy models are <50ms).
- `ErrPostprocess` — output doesn't match manifest's output schema.

### 5.5 Stub-model integration test

T3 ships `inference_test.go` exercising:
- Load a fixture ONNX (a trivial sklearn model on synthetic features, committed under `go/internal/ml/testdata/`).
- Call `Infer` with valid input → assert latency <100ms, output shape matches manifest.
- Call `Infer` with invalid input → assert `ErrInputShape`.

This is the substrate's heartbeat test — if it breaks, every downstream ML chain breaks. Wired into the precommit gate.

---

## 6. The MCP inference surface

### 6.1 Surface placement

A new MCP surface `mcp__toolkit-server__ml` (or fold under existing `mcp__toolkit-server__measure` — T5 picks). Recommendation: **new surface**, because the lifecycle actions, the A/B comparison reads, and the inference action form a coherent vocabulary that wants its own namespace. Existing `measure` is rubric-evaluation; mixing trained-model dispatch under it muddies both.

### 6.2 The `inference` action

```
ml.inference(model_id, features) → {
    output:      <type-dispatched per model>,
    latency_ms:  number,
    model_id:    int64,
    feat_hash:   string,
    span_id:     string,        // inherited from agent-first envelope
}
```

`model_id` is the `trained_model.id`. To call the currently-promoted model for a task, the agent calls `trained_model_list(task=<x>, status=promoted)` first OR uses a convenience action (§6.3).

`features` shape is per-model (declared in `manifest.toml`). The MCP layer doesn't enforce shape — it forwards to the inference handler which validates.

Every call emits a `model_predictions` telemetry row (§7).

### 6.3 Convenience actions — registerable, per-task

The bare `inference` action is the substrate. Per-model convenience actions are the agent-facing ergonomics:

| Model | Convenience action |
|---|---|
| Source router | `knowledge.route_query(query) → [{source, probability}]` (folds into `knowledge_search`'s internal pipeline OR exposes as standalone) |
| Curation classifier | `knowledge.curation_score(candidate_id) → promote_probability` (plus threshold-aware `curation_bulk_action` enhancement) |
| Cross-encoder reranker | Transparent — `knowledge_search` and `vault_search` keep their signatures; the rerank step swaps Qwen-rubric for ONNX call internally. No new agent-facing action. |
| Bug surface tagger | `work.forge_suggest_surfaces(title, problem_statement) → [{tag, confidence}]` (and `forge(bug, ...)` optionally auto-calls when `auto_tag_surface=true`) |
| Skill auto-loader | New action under `agent` surface OR fires from `UserPromptSubmit` hook (extends `reference-resolution-substrate` T9's pattern); see §9 |

The pattern: each convenience action wraps `inference(model_id=<promoted-for-task>, features=<extracted-from-args>)` and post-processes into a task-shaped response. The `trained_model` registry resolves `(task=<x>, status=promoted)` to the model_id once per call (with caching).

### 6.4 Backward-compat: opt-out

Every convenience action behaves identically to the pre-substrate path when no model is promoted (status=promoted has no row for that task). The Qwen-rubric baseline (or whatever existed before) continues serving. This is the substrate's no-regression guarantee.

---

## 7. Prediction telemetry shape

### 7.1 A new `model_predictions` table

Migration `NN_add_model_predictions.sql` adds:

```sql
CREATE TABLE model_predictions (
    id                INTEGER PRIMARY KEY,
    model_id          INTEGER NOT NULL REFERENCES trained_model(id),
    features_hash     TEXT NOT NULL,         -- SHA-256 of canonical-serialized features
    output_summary    TEXT NOT NULL,         -- short JSON: top-class + score, or first-N entries of vector
    latency_ms        REAL NOT NULL,
    span_id           TEXT NOT NULL,         -- per agent-first-substrate envelope
    grounding_event_id INTEGER,              -- nullable; populated when this prediction was triggered by a search
    created_at        TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now')),

    -- Drift detection: distinct (model_id, features_hash) values over time chart input-distribution shift
    UNIQUE (model_id, features_hash, span_id)
);

CREATE INDEX idx_model_predictions_model_time ON model_predictions(model_id, created_at);
CREATE INDEX idx_model_predictions_grounding ON model_predictions(grounding_event_id);
```

### 7.2 What each row records

- **`features_hash`** — content-addressable input. Used for: drift detection (input distribution shifts over time), caching opportunities (identical features → identical output), debugging (re-run the same inference offline).
- **`output_summary`** — bounded-size representation of the output. For a 7-class probability vector this is the JSON of the vector; for a 100-source router with sparse high-confidence tags this is `{"top": [{"source": "vault", "p": 0.97}, …]}`. Not the raw bytes — those are derivable from feat_hash + model_id.
- **`latency_ms`** — every prediction. Latency is the primary regression signal: a healthy production model is sub-50ms; drift to 80ms is a warning, 200ms is a bug.
- **`span_id`** — agent-first-substrate's envelope identifier. Lets a session's predictions be joined to its events, search calls, and resolutions.
- **`grounding_event_id`** — nullable FK to `grounding_events`. Populated when the prediction was triggered by a search (cross-encoder reranker, source router). Lets the eval projection (§8.3) join predictions to outcomes.

### 7.3 Cross-substrate seam

The telemetry-substrate's `grounding_events` and `query_interactions` tables (chain `query-telemetry-substrate`, closed 2026-05-17) are the read-side state. This table sits **alongside** them, joined via `span_id` and the optional `grounding_event_id` FK.

A future `proj_model_eval` projection joins `model_predictions` to `query_interactions` (did the rerank's top result get clicked?) and `events` (was the curation candidate ultimately promoted?). That projection is the eval gate body (§8.3).

---

## 8. A/B hot-swap harness + promotion gate

### 8.1 Pattern: dual-fire while in `ab_testing`

A code path that wants to use a trained model gains a `trained_model_id` parameter (or wraps via `abtest.Dispatch(...)`):

```go
result, err := abtest.Dispatch(ctx, abtest.Config{
    BaselineFn:    qwenRubricRerank,   // existing function
    ModelID:       loadedModelID,      // from registry
    Features:      featuresFromArgs,
    Policy:        abtest.PreferBaseline,  // or PreferTrained / Alternate
})
```

When the model row is in `ab_testing` status, `abtest.Dispatch`:

1. Fires the baseline function and the trained-model inference in parallel.
2. Records both outputs in `ab_comparisons` (table below).
3. Returns the output selected by `Policy` to the caller.

When the model row is in `promoted` status, the harness short-circuits: only the trained-model path fires (baseline is no longer called).

When the row is in `evaluating` or `retired`, the harness short-circuits the other way: only baseline fires.

### 8.2 The `ab_comparisons` table

Migration `NN_add_ab_comparisons.sql`:

```sql
CREATE TABLE ab_comparisons (
    id                  INTEGER PRIMARY KEY,
    model_id            INTEGER NOT NULL REFERENCES trained_model(id),
    features_hash       TEXT NOT NULL,
    baseline_output     TEXT NOT NULL,     -- JSON
    trained_output      TEXT NOT NULL,     -- JSON
    used_path           TEXT NOT NULL CHECK (used_path IN ('baseline','trained')),
    span_id             TEXT NOT NULL,
    grounding_event_id  INTEGER,
    created_at          TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now'))
);

CREATE INDEX idx_ab_comparisons_model_time ON ab_comparisons(model_id, created_at);
```

`used_path` records which output the caller actually consumed — for downstream `query_interactions` rows to be joinable to "did the user click the trained-model's pick or the baseline's pick?"

### 8.3 The promotion gate projection

`proj_ab_promotion_gate(model_id)` joins `ab_comparisons` to `query_interactions` (for retrieval models) or to `events` (for classifier models that drive `curation_promote` / `forge(bug)`):

| Signal | Source | Interpretation |
|---|---|---|
| Trained-output top-1 was clicked | `ab_comparisons.trained_output[0]` matched a subsequent `query_interactions` row's `pointer_id` with `kind='cited'` or `'followed'` | Positive for trained path |
| Baseline-output top-1 was clicked instead | Same join with `baseline_output[0]` | Positive for baseline path |
| Neither was clicked | No matching interaction | Neutral (not a signal) |
| Trained `auto_promote` decision was reversed in subsequent edit/reject | curation-classifier specific: prediction said `promote_probability > 0.95`, candidate was rejected within 24h | Strong negative for trained path |

The gate fires "trained model beats baseline" when:

- N ≥ 100 comparisons accumulated, AND
- (trained-positive rate − baseline-positive rate) > 0.05, AND
- The gap is stable over the last 3 evaluation windows (rolling 7-day windows).

When the gate fires, `trained_model_list` flags the model as `promotion_ready`. Promotion is still manual (call `trained_model_promote`) — the gate suggests, doesn't auto-promote. Force-promote with `force=true` is available for emergencies (audited).

### 8.4 Forward dependencies satisfied

The chain doc records two cross-chain absorptions:

- **`reference-resolution-substrate` T7** (cancelled-as-deferred 2026-05-18) — the trained classifier-and-reranker that the reference detector's domain-term shape was waiting on. This substrate's `cross-encoder-reranker` and `domain-term-classifier` candidate chains land via this harness when they ship.
- **`toolsearch-rerank-hook`** (closed 2026-05-18, both tasks cancelled) — same model, same swap pattern. Re-forge as a follow-on chain once the trained reranker proves out via the A/B harness.

---

## 9. Five `ml-temp` candidates walked through the substrate

This is the substrate's smell test — does the architecture above support all five candidates end-to-end without per-candidate special cases?

### 9.1 Source router (`01-source-router.md`)

| Step | Substrate path |
|---|---|
| Training | `ml-training/training/source-router/train.py` loads from `proj_training_data_for_reranker` filtered through source-routing lens; trains sklearn `MultiOutputClassifier` over `all-MiniLM-L6-v2` embeddings; exports both LR head and (cached) embedder to `models/source-router/v1/`. |
| Registration | `forge(trained_model, slug='source-router-v1', task='source-router', version='v1', status='evaluating', ...)`. |
| Validation | Held-out F1 + per-source PR curves spot-checked in `eval/source-router/eval_report.json`. Status flipped to `ab_testing`. |
| Serving | `knowledge.route_query(query)` convenience action wraps `inference`. Returns `[{source, probability}]`. |
| A/B | `abtest.Dispatch` with baseline = "search-everything-and-filter" (existing). |
| Promotion gate | `proj_ab_promotion_gate` measures: did the trained-router's source-subset contain the eventual `cited`/`followed` result? Compare to baseline rate. |
| Promotion | After gate fires + manual review, status → `promoted`. `knowledge_search`'s internal pipeline starts narrowing source-set via `route_query` first. |

✓ Substrate covers it.

### 9.2 Curation classifier (`02-curation-classifier.md`)

| Step | Substrate path |
|---|---|
| Training | `training/curation-classifier/train.py` reads `CurationCandidatePromoted` / `CurationCandidateRejected` events from `events` table; trains sklearn TF-IDF + LR; exports to `models/curation-classifier/v1/`. |
| Serving | `knowledge.curation_score(candidate_id)` convenience action. Plus `curation_list` is extended to include scores per row (cheaper for the agent than per-row calls). |
| A/B | Baseline = in-LLM scoring (per-row); trained path = ONNX call. Comparison is "do we agree with the eventual human/agent decision?" |
| Bulk-action enhancement | `curation_bulk_action(filter, action='promote_if_score_gt', threshold=0.95)` — threshold tunable from the gate's calibration curve. |
| Promotion gate | `proj_ab_promotion_gate` joined to subsequent `curation_promote`/`curation_reject` events: did the classifier's prediction match the eventual human decision? |

✓ Substrate covers it.

### 9.3 Cross-encoder reranker (`03-cross-encoder-reranker.md`)

| Step | Substrate path |
|---|---|
| Training | `training/cross-encoder-reranker/train.py` reads `proj_training_data_for_reranker`; fine-tunes `cross-encoder/ms-marco-MiniLM-L-6-v2`; exports via `optimum`; int8 quantize. |
| Serving | Transparent — `knowledge_search` and `vault_search` keep their signatures; the rerank step swaps Qwen-rubric for `inference` call internally. **No new agent-facing action.** |
| A/B | `abtest.Dispatch` with baseline = Qwen-rubric rerank (existing); trained path = ONNX cross-encoder. |
| Promotion gate | Joined to `query_interactions`: did the trained reranker's top result get a `cited`/`followed` interaction more often than baseline's top result? |
| Forward unblock | When promoted, `reference-resolution-substrate` T7 (deferred into this chain) and `toolsearch-rerank-hook` (closed-cancelled) are structurally satisfied — both name this exact swap. Re-forge each as a small follow-on chain to wire the integration cleanly. |

✓ Substrate covers it; this is the headline-win candidate.

### 9.4 Bug surface tagger (`04-bug-surface-tagger.md`)

| Step | Substrate path |
|---|---|
| Training | `training/bug-surface-tagger/train.py` reads `bugs` table (title + problem_statement → surface multi-label); trains sklearn TF-IDF + multi-label LR; exports. |
| Serving | `work.forge_suggest_surfaces(title, problem_statement) → [{tag, confidence}]`. `forge(bug, …, auto_tag_surface=true)` auto-calls and pre-fills `surface` field with predictions where confidence > 0.5. |
| A/B | Baseline = agent-authored tags (no model); trained = predicted tags. Comparison records: did the agent override the prediction at forge time? |
| Promotion gate | Override-rate < threshold over a stable window. |

✓ Substrate covers it. The "predictions pre-filled, agent overrides" pattern is one the substrate accommodates via the convenience-action design (output is suggestions, not commands).

### 9.5 Skill auto-loader (`05-skill-auto-loader.md`)

| Step | Substrate path |
|---|---|
| Training | Needs a **pre-task** in its own chain: capture skill-activation events into `events` (or a new `skill_activations` table) with session-window features. Once labeled data exists, `training/skill-auto-loader/train.py` trains sentence-transformer embedding + multi-label LR. |
| Serving | Hybrid: a `UserPromptSubmit` hook (extending `reference-resolution-substrate` T9) auto-loads high-confidence skills at session-start; an `agent.suggest_skills(context)` action lets the agent re-query mid-session. |
| A/B | Baseline = current manual-reflex-prompt behavior (zero auto-loading); trained = auto-loaded set. Comparison: did the auto-loaded skills get invoked? Did the agent need to reflex-prompt anyway? |
| Promotion gate | Reduction in manual reflex-prompts per session, plus per-skill invocation rates within auto-loaded sets. |

⚠️ Substrate covers it conditionally — the training-data gap is the pre-task. The chain that ships this model needs a T0 capturing skill activations with enough fidelity to train on. Substrate plumbing (registry, inference action, A/B harness) is reusable as-is.

### 9.6 Smell-test summary

All five candidates land on the substrate without per-candidate plumbing. The shared pieces (`trained_model` schema, `go/internal/ml/`, `inference` action, A/B harness, telemetry) carry every case. Per-candidate work is:

- The training script (in `ml-training`).
- The convenience action (in `mcp-servers`) — typically <100 LOC per model.
- The model-specific eval projection — typically a SQL query, sometimes a new join.

Total per-candidate budget after the substrate: ~1–2 days each, dominated by data exploration and model tuning, not plumbing.

---

## 10. Future-AT migration path

`atomic-tasks` (chain 256 open: `atomic-tasks-pre-offload-hardening`) is the agent-task offload system the user is benchmarking. When it matures, the model-training pipeline becomes a natural AT consumer: each training run is a task that fits the AT "discrete computational job, definite inputs, definite outputs, scriptable" mold.

The migration path is **swap of the outer orchestration layer only**:

- Today: a user/agent forges a chain `train-source-router-v2`, the chain's tasks invoke `ml-training/training/source-router/train.py` via shell.
- Future-AT: the same `train.py` script becomes an AT task spec. A new AT chain template `train-and-register-model.yaml` automates: pull data snapshot → run training → run eval → forge `trained_model` row → flip to `evaluating`.

What does NOT change in the migration:

- The `ml-training` Python project structure and scripts.
- The `go/internal/ml/` serving subsystem.
- The `trained_model` forge schema.
- The MCP inference surface.
- The A/B harness and promotion gate.

Substrate-shape commitments that make the migration clean:

- `train.py` scripts are pure functions of `(data_snapshot, training_config) → (model_artifacts, eval_report)`. No side-effects beyond writing artifact files. AT-compatible by construction.
- The dataset signature is captured at training-time, not derived later. AT runs in detached environments where the original DB may not be reachable — the signature must travel with the artifact.
- `trained_model` rows are forge-able from outside this repo (any process that can talk to toolkit-server's MCP can forge). AT runner forges rows like any other agent.

T2 documents the migration path explicitly in `ml-training/README.md`; T2 does NOT ship an AT chain template — that's the AT-migration follow-on chain's job.

---

## 11. The `ml-opportunity-scan` skill (T7)

T7 lives outside the substrate's code surface — it's a `~/.claude/skills/ml-opportunity-scan/` skill that codifies the discipline for triaging future ML candidates. The skill:

- Names the **agent-first triage rubric**: per-call frequency, token savings, determinism win, round-trips eliminated, data readiness.
- Specifies the **substrate-signals checklist**: which logs/projections to grep for candidate-suggesting patterns.
- Defines the **output structure**: numbered build-sequence files in `~/Documents/files/ideas-to-process/ml-temp/`, plus an "incubation" overflow in `General Ideas/`.
- Declares **trigger conditions**: a new substrate ships; a chain's design names a future trained model; the user explicitly asks for an ML scan.

The skill is the discipline; this substrate is the mechanism. New candidates triaged via the skill become chains that consume the substrate plumbing.

---

## 12. Open questions

Listed here so they're durable across the rest of the chain's tasks. Not blocking T1 closure; resolved as they come up in T2–T8 or in follow-on chains.

- **Go ONNX binding choice.** `yalue/onnxruntime_go` is the current default. If it doesn't expose all features needed (e.g. session-pool reuse for the cross-encoder's batched inputs), T3 may evaluate alternatives.
- **Native library install path.** Auto-download vs system-installed. T3 picks based on dev-box ergonomics and CI considerations.
- **MCP surface namespace.** `mcp__toolkit-server__ml` vs folding under `measure`. §6.1 recommends new surface; T5 finalizes.
- **Promotion-gate N and Δ.** §8.3 picks 100 comparisons and 0.05 delta as defaults. Per-task tuning may be needed (high-stakes models like the curation auto-promoter want stricter gates; low-stakes models like the bug tagger can promote on weaker signal).
- **Embedder versioning.** Shared embedder (e.g. `all-MiniLM-L6-v2`) is used by source-router AND skill-auto-loader. Treated as a model in the registry. Open: when the embedder version bumps, do both downstream models need re-training? Likely yes — captured in the dataset signature.
- **Drift detection cadence.** A future projection `proj_model_drift_signal` would alert when input-feature distributions shift beyond a threshold. Not in this chain's scope; reserved for a future observability follow-on.
- **Sentence-transformer ONNX export gotchas.** Tokenizer ships separately from model in some HuggingFace exports. T2's placeholder script exposes whether this is friction; documents the pattern for follow-ons.

---

*— ml-capability-substrate T1, 2026-05-19*
