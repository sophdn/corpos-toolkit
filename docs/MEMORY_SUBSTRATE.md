# Memory Substrate — Design

> **Status:** Draft for review. Produced by chain `memory-substrate-within-vault` T1 (`design-memory-substrate-shape`). Decisions here are durable; T2–T7 bind to them. Amendments after this doc lands require a chain-level decision, not a unilateral task edit.
>
> **Reading order:** §1 framing → §2 schema-shape decision → §3 frontmatter shape (legacy-compatible) → §4 path shape + routing → §5 `memory` forge schema fields → §6 dispatch wiring → §7 event types → §8 parse_context behaviour → §9 migration story → §10 open questions.
>
> **Companion docs:** `docs/ARC_CLOSE_FILING_REVIEW.md` (§Filing-dispatch defines the `memory_write` wire shape the arc-close hook produces; this substrate is its dispatch target). `docs/EVENT_CATALOG.md` (where new `Memory*` event types catalog). `docs/EVENT_SUBSTRATE.md` (write-side audit ledger; this chain inherits the `span_id` contract and rationale enforcement). `docs/REFERENCE_RESOLUTION.md` (read-side; parse_context surfaces memory entries via the existing vault walker).
>
> **Cross-chain dependencies:** `arc-close-filing-review` T4–T5 (closed 2026-05-19) — wire shape for the `memory_write` filing decision lands as a forge call into this substrate. `forge-vault-note-schema-rework` (closed 2026-05-11) — the cross-project + routing-note pattern this schema mirrors.

---

## 1. What this substrate is and isn't

Claude Code's auto-memory at `~/.claude/projects/<project-dir>/memory/<name>.md` is the harness-default agent recall surface — terse facts about the user, feedback rules, project state, and external-system pointers that load on every session start. Two structural problems with the default path:

1. **Direct filesystem writes.** Memory entries are authored via `Write` (per the auto-memory section of `~/.claude/CLAUDE.md`). No schema enforcement, no event ledger, no routing — silent drift between entries is the default. `linguistic-tics.md` (written 2026-05-19 during the arc-close-filing-review T6 validation arc) is the canonical example: it landed with ad-hoc frontmatter extras (`node_type`, `originSessionId`, `observed_first`, `source`) the spec doesn't document.

2. **Project-local placement of cross-project knowledge.** User-kind memories (facts about the user's role, expertise, preferences) live in one project's harness dir but are semantically cross-project. The current shape has no fan-out mechanism.

This substrate replaces the direct-Write authoring path with a forge-mediated one, lands entries in the vault under `vault/memory/<kind>/<name>.md`, and re-materializes them into the harness's expected per-project dirs via a SessionStart hook (T3). The auto-load behaviour is preserved verbatim; the storage layer flips.

What ships:

- A new **`memory`** forge schema with the four-kind taxonomy (`user` / `feedback` / `project` / `reference`).
- A new **`go/internal/work/memory.go`** dispatch handler writing to `vault/memory/<kind>/<name>.md`.
- New **`MemoryWritten` / `MemoryEdited` / `MemoryDeleted`** event payloads + JSON schemas.
- A **session-start materialization hook** (T3) preserving the harness auto-load contract.
- A **one-shot migration** of ~80 existing harness-dir memories into the vault (T4).
- An **updated `arc-close-filing-review-hook.sh::write_memory()`** routing through forge instead of the current hardcoded direct-Write path.

What does *not* ship:

- No retirement of the harness's auto-load mechanism. The materialization hook preserves it; this chain doesn't touch the harness itself.
- No new ranking surface in vault_search. Memory entries surface through the existing recursive vault walker (verified at `go/internal/knowledge/vault/vault.go:189`).
- No replacement of `~/.claude/CLAUDE.md`'s auto-memory authoring instructions. Those still describe the agent-facing contract; the storage details are an implementation concern below the line.

---

## 2. Schema-shape decision: new `memory` schema (Candidate B)

The chain spec listed two candidates: (A) extend `vault-note`'s `note_kind` enum to include the four memory kinds, or (B) introduce a new `memory` forge schema. **The decision is B.**

### 2.1 Why not extend vault-note (Candidate A)

Three structural arguments against:

1. **Frontmatter shape is non-negotiable and different.** The auto-memory frontmatter contract per `~/.claude/CLAUDE.md` is `name / description / metadata.type`. `vault-note`'s frontmatter is `date / slug / note_kind / scope / tags / topic`. The harness's auto-load reads the materialized file's frontmatter and expects the auto-memory shape; vault-note's shape doesn't match. Extending vault-note would force either (a) conditional rendering of two different frontmatter shapes inside one schema based on `note_kind` (ugly), or (b) breaking the harness auto-load contract (non-starter).

2. **Memory-specific fields don't belong on vault-note.** The auto-memory taxonomy needs `source` (which arc-close fire / which Stop hook / which user prompt produced the entry), `observed_first` (ISO date the pattern first surfaced), and `recurrence_count` (how many times the agent has seen the pattern — load-bearing for the future de-duplication path). These are foreign to vault-note's "long-form durable knowledge artifact" frame.

3. **`reference` kind collision.** vault-note's `note_kind=reference` is *project-or-domain reference material* (durable how-to docs, RAG-architecture notes, hardware specs). The auto-memory `reference` kind is *pointers to external systems* (Linear projects, Grafana dashboards, Slack channels). The two are semantically distinct enough that conflating them inside one enum loses signal at retrieval time.

### 2.2 Why a separate schema (Candidate B)

- **Clean separation.** vault-note and memory have different read patterns (vault-note → human-readable retrieval via `vault_search`; memory → agent recall via harness auto-load + parse_context). Different surfaces deserve different schemas.
- **Memory-specific fields land natively.** `source`, `observed_first`, `recurrence_count` carry telemetry the arc-close-filing-review-hook ladder needs to assess filing precision (see §7).
- **Event-type separation.** `MemoryWritten` is distinct from `VaultNoteCreated`; downstream consumers (the arc-close-filing-review telemetry pipeline, the future trained-arc-close-filing-classifier) can track memory writes independently without filtering on a kind subfield of a shared event.
- **Substrate-quartet pattern alignment.** Each substrate (`agent-first`, `query-telemetry`, `reference-resolution`, `ml-capability`) gets its own typed events. Memory makes five; the established convention is "one substrate, one event family."

Implementation cost: ~5-7 files (schema toml, dispatch handler, three event payload structs + JSON schemas, EVENT_CATALOG entry, actions_discovery entry, manifest registration). The pattern is well-trodden via `vault-note.toml` + `trained_model.toml`.

---

## 3. Frontmatter shape (legacy-compatible)

The schema renders this exact frontmatter:

```yaml
---
name: <kebab-slug>
description: <one-line description, no newlines>
metadata:
  type: <user|feedback|project|reference>
  source: <optional; e.g. arc-close-filing-review, manual, migration>
  observed_first: <optional ISO date YYYY-MM-DD>
  recurrence_count: <optional integer>
---
<body markdown>
```

The shape is **forward-compatible with the auto-memory spec** at `~/.claude/CLAUDE.md`:

- `name`, `description`, `metadata.type` match exactly.
- `metadata.source`, `metadata.observed_first`, `metadata.recurrence_count` extend the `metadata` block (optional; harness auto-load is tolerant of additional metadata fields per linguistic-tics.md's existing extras).

**Preservation rule on read (T4 migration):** existing harness entries carry varied frontmatter (some with `metadata.type`, some with top-level `type:`, some adding `originSessionId` or `node_type` extras). The migration normalizes to the canonical shape above; the materialization hook (T3) writes whatever the vault entry carries verbatim.

**Body conventions** carry over verbatim from the auto-memory spec — for `feedback` and `project` kinds, the body leads with the rule/fact, then a `**Why:**` line, then a `**How to apply:**` line, with `[[name]]` links to related memories. The schema doesn't enforce body structure (body is a free-form markdown field); it documents the convention in the field description so forge callers see it.

---

## 4. Path shape and routing

```
vault/
└── memory/
    ├── user/<name>.md         ← cross-project (about the user)
    ├── feedback/<name>.md     ← project-scoped (guidance from user corrections + confirmations)
    ├── project/<name>.md      ← project-scoped (ongoing work / decisions / incidents)
    └── reference/<name>.md    ← project-scoped (pointers to external systems)
```

Routing is **kind-driven**, not scope-driven (unlike `vault-note`'s `learning` kind which routes by scope). Each entry has exactly one kind subdir.

**Scope semantics** are stored on the entry's `metadata.project` frontmatter field (stamped by forge from the dispatch envelope's project arg). The materialization hook (T3) routes by kind:

- **user** kind → fans out to *every* project's harness dir (cross-project facts).
- **feedback / project / reference** → routed to the project dir whose slug suffix matches `metadata.project` (so a session launched from within a project gets its own scoped set), **and additionally to the neutral dev-root harness dir** (`~/dev` → slug `-home-sophi-dev`).

The neutral dev-root dir is the **aggregate** surface — it receives *every* memory regardless of scope. This exists because Sophi works from the neutral `~/dev` cwd by design (so the agent doesn't prejudge the project per memory `workflow-project-agnostic-cwd`); a project-suffix-only routing left those sessions auto-loading only the user-kind fan-out and silently missing every project-scoped memory (bug `materialize-memory-hook-skips-project-scoped-memories-for-neutral-dev-cwd`). Project-specific dirs stay scoped; only the neutral aggregate is the superset.

---

## 5. `memory` forge schema fields

```toml
# blueprints/forge-schemas/memory.toml — outline; final shape in T2.
supported_ops = ["create", "update"]

[schema]
name = "memory"
prefix = ""
output_dir = "vault"
filename_pattern = "memory/{kind}/{name}.md"
cross_project = false  # see §4 — kind drives routing, project_id stamps DB attribution

[storage]
target = "markdown"
prefix = ""
output_dir = "vault"
filename_pattern = "memory/{kind}/{name}.md"

[[fields]]
name = "memory_kind"  # named _kind for the same forge-sugar-alias reason vault-note used note_kind
type = "string"
description = "One of: `user` (about the user — role, expertise, preferences; cross-project), `feedback` (guidance from corrections + confirmations; project-scoped), `project` (ongoing work / decisions / incidents; project-scoped), `reference` (pointers to external systems like Linear projects, Grafana dashboards, Slack channels; project-scoped). The taxonomy is closed."
enum_values = ["user", "feedback", "project", "reference"]
render_as = "frontmatter"
frontmatter_path = "metadata.type"  # renders nested per §3

[[fields]]
name = "name"
type = "string"
description = "Kebab-case slug. Becomes both the filename (`<name>.md`) and the frontmatter `name:` field."
render_as = "frontmatter"

[[fields]]
name = "description"
type = "string"
description = "One-line description. No newlines. Surfaced in MEMORY.md index rebuilds (T3) and in parse_context candidate listings."
render_as = "frontmatter"

[[fields]]
name = "body"
type = "string"
description = "Markdown body. For `feedback` and `project` kinds, lead with the rule/fact, then a `**Why:**` line, then a `**How to apply:**` line. Link related memories with `[[name]]`."

[[fields]]
name = "source"
type = "optional_string"
description = "Provenance — e.g. `arc-close-filing-review`, `manual`, `migration`. Surfaced in the `MemoryWritten` event for telemetry."
render_as = "frontmatter"
frontmatter_path = "metadata.source"

[[fields]]
name = "observed_first"
type = "optional_string"
description = "ISO date (YYYY-MM-DD) the pattern first surfaced. Optional; defaults to write date when absent."
render_as = "frontmatter"
frontmatter_path = "metadata.observed_first"

[[fields]]
name = "recurrence_count"
type = "optional_int"
description = "How many times the agent has observed this pattern. Optional; the future de-duplication path uses this to merge near-duplicate writes."
render_as = "frontmatter"
frontmatter_path = "metadata.recurrence_count"

[[sections]]
heading = ""  # no ## heading — the body is the full document below frontmatter
fields = ["body"]
```

**Validation:**

- `name` must be kebab-case (`^[a-z0-9-]+$`); enforced at forge-validate time.
- `description` is one-line (no newline characters); rejected if multi-line.
- `body` is non-empty markdown.
- `memory_kind` in the closed enum.
- The render emits the §3 frontmatter exactly — no extra fields, no reordering.

---

## 6. Dispatch wiring

- New file: `go/internal/work/memory.go` with `HandleMemoryForge` (writes the markdown via the standard forge create path, scoped to the `memory` schema).
- `go/internal/work/table.go` registers the action via the existing `forge` dispatcher (the `forge` action takes `schema_name = "memory"`; no separate top-level action — mirrors how vault-note is reached).
- `go/internal/forge/create.go` already routes by `schema.Meta.Name == "vault-note"`; add an analogous branch for `schema.Meta.Name == "memory"` that resolves the `{kind}` subdir from the `memory_kind` field and the `{name}` filename from the `name` field. No `date_` prefix on the filename (unlike vault-note); the path is `memory/<kind>/<name>.md`.
- `go/internal/work/actions_discovery.go` gets an entry pointing at the schema; `forge(schema_name=memory, ...)` is the canonical write surface.
- **Hook update (carried by T2):** `hooks/arc-close-filing-review-hook.sh::write_memory()` at lines 253-280 currently writes directly to `${HOME}/.claude/projects/-home-sophi-dev/memory` (hardcoded project path, broken cross-project). Replace with a curl POST against `MCP_URL` mirroring `forge_row()`'s shape (action=`forge`, schema_name=`memory`, params from the payload). Closes completion_condition (f).

---

## 7. Event types

One new event payload + JSON schema (ships now):

- **`MemoryWrittenPayload`** — emitted on every forge create.
  - Fields: `name`, `kind`, `description`, `source` (optional), `observed_first` (optional), `vault_path`, `body_length_bytes`. Entity reference + `project_id` live in the envelope.
  - Consumed by: arc-close-filing-review telemetry pipeline (count of memory writes per session, filing precision tracking); future trained-arc-close-filing-classifier training data.

**Deferred (no consumer today; pattern is well-trodden when one materializes):**

- `MemoryEditedPayload` — drift-detection consumer (frequently-edited memories as skill_update candidates) is hypothetical until observed.
- `MemoryDeletedPayload` — T3's materialization hook discovers deletions by walking the vault, not by listening. No consumer needs the typed event.

The schema / payload-struct / catalog-entry pattern is ~3 small files; revisit when an actual consumer appears. The completion contract (b) only requires MemoryWritten; this chain doesn't speculatively build the edit/delete siblings.

`MemoryWritten` follows the established event envelope pattern (`_envelope.json` shape with `span_id`, `rationale`, `project_id`) per `docs/EVENT_SUBSTRATE.md`. `docs/EVENT_CATALOG.md` gets one new entry under a new `## Memory lifecycle` section. `go/internal/events/events_test.go::TestIsRegisteredType` updated to include `MemoryWritten`.

---

## 8. parse_context behaviour

Two paths surface memory entries — neither requires new code:

**Path A — `vault_search` over `vault/memory/*`.** The vault walker at `go/internal/knowledge/vault/vault.go:189` recursively walks every subdir under `vault/` looking for `*.md` files. Memory entries land there alongside decisions/learnings/reference notes and surface in `vault_search` with `path` prefixed by `memory/<kind>/`. Verified live (2026-05-22):

```
vault_search(query="linguistic-tics agent appending -o testo", k=5) →
  results[0] = { path: "memory/user/linguistic-tics.md", title: "linguistic-tics", rank: 1 }
```

**Path B — `parse_context`'s `memory_entry` resolver.** Reads `MEMORY.md` from the per-project harness dir (`~/.claude/projects/<project-dir>/memory/MEMORY.md`). The resolver's source path is unchanged: T3's session-start materialization hook regenerates each project's `MEMORY.md` from the materialized vault entries, so every memory entry in the vault is reachable via `parse_context` once the next session boots. Verified live (2026-05-22, pre-T3-fire — works against the legacy harness file; T3 will keep working the same way post-fire because it preserves the same file shape):

```
parse_context("Sophi noticed me with linguistic-tics") →
  references[0] = {
    token: "linguistic-tics",
    shape: "memory_entry",
    candidates[0] = {
      ID: "linguistic-tics.md",
      Title: "Linguistic tics",
      SourceRef: "memory:/home/user/.claude/projects/<project>/memory/linguistic-tics.md",
    }
  }
```

The `SourceRef` shape carries the `memory:` prefix, which downstream resolvers use to distinguish memory candidates from other reference shapes. The path inside the SourceRef points at the materialized file (harness dir), not the vault entry — by design, since the harness already auto-loads the materialized files at session start and the resolver's contract is to point at "what the harness will read."

**No code change is needed.** Both paths were verified end-to-end against the live 76-entry corpus.

---

## 9. Migration story

T4 walks every `~/.claude/projects/<project-dir>/memory/<name>.md` (~80 entries across 6 project dirs as of 2026-05-22) and re-forges each via the `memory` schema. Edge cases enumerated in T4's problem statement; the key shapes:

- **Skip `MEMORY.md`** — it's an index file, not an entry. T3's materializer rebuilds it from the vault entries.
- **Frontmatter variance triage:** `metadata.type` → top-level `type:` → filename prefix (`feedback_`, `project_`, `reference_`, `user_`) → reject.
- **Canonical `name` from frontmatter, not filename.** Filenames vary (kebab vs snake; with vs without kind prefix); the vault path uses the frontmatter `name:` field.
- **Never delete the harness-dir source.** Materialization hook (T3) handles cleanup on the next session start. Race-free.

Migration emits `MemoryWritten` events with `source = "migration"` so the audit trail is queryable post-hoc.

---

## 10. Open questions

1. **Recurrence merging.** When the arc-close hook proposes a `memory_write` for a pattern an existing entry already captures, the substrate should either (a) increment `recurrence_count` on the existing entry, or (b) write a new entry and rely on T3 to surface the duplication. Today: (b) by default; (a) is a future enhancement gated on observed duplicate rate. Tracked as a candidate in T7's retrospective.

2. **User-kind fan-out cost.** T3 materializes user-kind entries into every project's harness dir on every session start. With N user-kind entries × M projects, fan-out is O(N·M). Both N and M are tiny today (~3 user memories, ~6 projects); not a concern. If either grows, the hook can switch to symlinks (gated on the harness's must-read-first guard tolerating them) or a single shared dir (would require harness-side cooperation).

3. **Reference-kind semantic cleanup.** Several existing `reference_*.md` entries (e.g. `reference_telemetry_warmup.md`, `reference_loci_v2_comparison.md`) look more like vault-note-grade reference material than external-system pointers. T4 migrates them as-is into `memory/reference/`; a cleanup pass could re-classify them as `vault-note reference` entries, but that's out of scope for this chain. T7 names it as a recommended follow-on if the misclassification rate is material.

4. **MEMORY.md regeneration ordering vs harness load.** T3's session-start hook must materialize before the harness reads MEMORY.md. SessionStart fires before UserPromptSubmit per the Claude Code hook surface, so the order is safe — but the fall-back path (degraded mode when SessionStart isn't available) reads MEMORY.md one session late. Documented as the degraded-mode gap.
