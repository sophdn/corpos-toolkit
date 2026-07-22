# The action-doc contract (chain `establish-action-doc-contract-on-work`)

Status: **realized across all five surfaces and finalized.** The founding work chain shipped the
machinery — the characterization net (T1), the reflect-based shape extractor (T2,
`internal/actionspec`), the co-located descriptor registry (T3, `internal/work/action_doc.go`),
and the T4 flip that pointed every work doc-consumer at the registry via `deriveActionSpecs()` and
**deleted the hand-authored `actionSpecs` literal**. The four follow-on surface chains then
instantiated the same spine — `migrate-{knowledge,measure,admin,ml}-action-docs-to-derive-contract`
— so all five action-doc surfaces (work / knowledge / measure / admin / ml) now serve from
generated-from-registry + derive-types corpora. Chain `finalize-action-docs-epic` (2026-05-26)
consolidated the program: reconciled this doc to as-built (below), closed the
required-enforcement gap every retro named, consolidated the per-surface contract-net tests,
documented the handler-read params the migrations had scoped out, and recorded the Fork C
decision. The **As-built end-state** section is the authoritative description of the shipped
surface; the design-rationale sections after it record *why* the contract is shaped as it is.

Successor to chain `single-source-action-describe` (see
[`SINGLE_SOURCE_ACTION_DESCRIBE.md`](SINGLE_SOURCE_ACTION_DESCRIBE.md)). That chain made
`work.actionSpecs` the single typed catalog and **generated** the embedded TOML corpus from it.
This chain takes the next step from the scoping doc's **Fork B**: stop re-declaring each param's
**type** by hand, derive it from the handler's typed param struct, keep only the irreducible
semantics in a co-located Go **descriptor**, and delete the monolithic hand-authored
`actionSpecs` catalog.

---

## As-built end-state (the five shipped surfaces)

Authoritative as of chain `finalize-action-docs-epic` (2026-05-26). Every action-doc surface
serves `admin.action_describe(<surface>, X)` + the `GET /admin/action-docs?surface=<surface>`
payload from a generated, `go:embed`'d corpus whose source of truth is the surface's co-located
descriptor registry (`<pkg>/action_doc.go`). The corpus generator (`cmd/action-docs-corpus-gen`)
projects each registry through `actiondocs.SpecToDoc`; the precommit no-diff gate (`--check`) and
the per-surface contract net pin corpus ↔ registry ↔ served-output parity.

### Binding-style split (corrects scoping finding #2)

The founding scoping premise — "the 4 follow-on surfaces all use typed param structs" — was
**false**, and was the first thing every migration's pre-flight had to disprove (bug 935). The
real shape is a per-surface **binding-style split**: a param's type *derives* only when the
handler binds it through a typed `json.Unmarshal` struct; map-bound params (bound by
`mcpparam.String/Int64Opt` map-indexing, or the forge/`parseBenchmarkResult` tolerant maps) have
no struct to reflect, so their descriptor **authors** the type (`ParamStruct == nil`), exactly as
work's forge family does.

| Surface | Struct-backed (derive) | Map-bound (authored type) | mcpparam binder-parity gate |
|---|---|---|---|
| work | ~45/49 | forge family + `task_edit` + `roadmap_list` (+ 2 per-param: `chain_close.closure_summary`, `task_search.verbose`) | `TestActionDocParamTypes_MapBoundWorkActionsBinderParity` (rawStringParam/rawBoolParam) |
| knowledge | 7/23 (`curation_*`, `parse_context`, `resolve_references`) | 16/23 (`vault_*`, `kiwix_*`, `library_*`, `knowledge_search`, `knowledge_report_miss`) | `TestActionDocParamTypes_KnowledgeMapBoundBinderParity` |
| measure | 2/13 (`bench_run`, `benchmark_replay`) | 11/13 (`classify_*`, `benchmark_query`, `benchmark_record`) | `TestActionDocParamTypes_MeasureMapBoundBinderParity` (mcpparam **+** the `parseBenchmarkResult` `take*/opt*` scanner) |
| admin | 17/17 — FULLY struct-backed | none | — (no map-bound actions, so no binder gate) |
| ml | 1/1 (`inference`) | none | — (single struct-backed action) |

The rule for a map-bound surface is the forge-family pattern: author the param's `Type`, set
`ParamStruct == nil`, and pin the authored type against what the handler actually reads with a
per-surface **binder-parity gate** (an AST scan of the handler's binder calls keyed by the param
name). knowledge and measure needed one; admin (fully struct-backed) and ml (single struct action)
did not. The struct-backed actions on every surface leave their authored `Type` empty so the type
genuinely derives — gate-enforced by each surface's `Test<Surface>RegistryDerivedParamsHaveEmptyAuthoredType`.

### The two type-parity gates are now vestigial — KEPT as a landing spot (ratified)

`param_type_parity` (AST-compares a documented primitive type against the handler struct field
kind) and `param_tag_gate` (every documented param name binds to a handler tag) were built to
guard **hand-authored** surfaces. After all five surfaces migrated, both are **no-ops**:
`param_type_parity`'s `surfacePackages` map is empty `{}`, and `param_tag_gate` lists all five
surfaces (work/knowledge/measure/admin/ml) in its skip set. Every surface now either derives its
types by construction (pinned by the contract net) or authors them under a binder-parity gate, so
neither original gate has anything to check.

**Ratified decision (finalize-action-docs-epic): KEEP both as a documented landing spot.** A
future surface that lands hand-authored *before* migrating onto the contract would re-enter
`surfacePackages` / drop out of the skip set and be guarded immediately; the synthetic-drift
self-tests (`Test*_GateCatchesSyntheticDrift`) keep proving the comparison logic works even while
the live coverage is empty. The alternative — deleting them — was rejected: the contract's own
migration template (below) keeps "drop the surface from these gates" as a step, so the gates are
load-bearing *machinery* for onboarding a surface even though their *live coverage* is currently
nil. Their no-op state is therefore a correct steady state, not dead code, and the code comments
in `param_type_parity_test.go` say so.

### Spec-vocabulary limitation: no numeric tokens

The derived type vocabulary (`actionspec.SpecType`) is `int64` / `string` / `bool` / `string[]` /
`object[]` / `object` / `json`. It has **no numeric tokens** below `int64`: `SpecType` collapses
every `float32`/`float64` to `object` and every non-string slice (including numeric arrays like
ml's `features_data []float32` / `features_shape []int64`) to `object[]`. So a *derived* numeric
param is type-lossy — its precise shape lives in the param **description** ("Flattened input
tensor ([]float32), row-major"), per the contract's "sub-shape lives in the description, not a
sibling param" rule.

Two consequences worth stating:

- **For a DERIVED param, the loss stands.** ml's `features_data`/`features_shape` document
  `object[]`; the numeric element type is in the description. Adding `float[]` / `int[]` tokens to
  the vocabulary was **considered and declined** (finalize-action-docs-epic): it would touch
  `SpecType`, `docType`, the corpus, and every consumer for one surface's two params, and the
  description already carries the shape. Revisit only if numeric params proliferate.
- **For an AUTHORED map-bound param, use the precise token.** Where there is no struct to derive
  from (so `SpecType`'s collapse never runs), the descriptor authors the most accurate token.
  `benchmark_record`'s four `*_score` columns are authored `float` rather than the lossy `object`
  a derive would have produced — an agent reading `accuracy_score (float)` knows to pass a number.
  `float` is not a gated primitive family (`docPrimitiveFamily` knows only string/integer/bool), so
  the binder-parity gate skips it; the contract net pins it. This authored-vs-derived asymmetry is
  deliberate and parallels the forge family (authored types are free to be precise; derived types
  are bounded by `SpecType`).

### Required-flag enforcement is now gated

Every retro named the same residual: the descriptor *authors* each param's `Required`, but
nothing cross-checked it against the handler's actual required-enforcement, so a descriptor could
claim `required=false` for a param the handler rejects and only the output net (not the handler)
would notice. `finalize-action-docs-epic` T2 closed this for the three known enforcement classes,
in `param_type_parity_test.go`'s "Required-vs-enforcement gate" section:

- **work id-OR-slug one-of** — `TestActionDocRequired_IdentifierOneOfParity` (keyed off
  `IdentifierRequiredError("<action>")` call sites): both arms documented, neither marked Required.
- **ml `model_id`-OR-`task` one-of** — `TestActionDocRequired_ModelOneOfParity` (keyed off
  `resolveModel`'s "requires either model_id or task" literal): same one-of shape. (The
  "project when resolving by task" rule is a dispatch-envelope requirement, not a params flag.)
- **admin plain requireds** — `TestActionDocRequired_PlainRequiredParity` reads the enforced param
  names out of the `if p.X == ""` guards' `params.X ... required` error literals and checks both
  the agent-blocking direction (an enforced name documented `Required=false`) and the presence
  direction (an enforced name no action documents `Required=true`).

Each has a synthetic-drift self-test. These gates plus the per-surface binder-parity (type) gates
plus the contract nets (output) together pin all three of: documented type, documented required,
and served output — against the handler.

### Doc-completeness expansions landed

The per-surface migrations deliberately scoped out **doc-completeness expansion** (documenting a
handler-read param the corpus had historically omitted) because it changes served output — a
behavior change, not a behavior-preserving migration. `finalize-action-docs-epic` T4 landed the
two that mattered as enumerated, net-reviewed deltas: measure `benchmark_record`/`benchmark_query`/
`benchmark_replay` and admin `project_register`/`host_register`/`host_list`/`host_remove`/
`remote_exec` now document the params their handlers read (several REQUIRED — a docs-only caller
previously hit a guaranteed required-param error). Where a handler bound via an inline anonymous
struct, that struct was hoisted to a named co-located type so the registry can derive its types
(`benchmarkReplayParams`, `projectRegisterParams`, `hostRegisterParams`, `hostListParams`,
`hostRemoveParams`, `remoteExecParams`); `benchmark_record` stayed map-bound (its
`parseBenchmarkResult` tolerance is deliberate) with authored types under the extended binder gate.

---

## The decision (Route A — co-located descriptor)

Author decision (2026-05-24), chosen against five axes — agent-friendly, clean, durable,
extensible, and easy for less-capable LLMs (DeepSeek / local Qwen, per the qwen-offload
direction). Route A wins or ties on all five; the only thing the rejected route (per-param doc in
`doc:` struct tags) won was narrow textual DRY, which is illusory because the param name on the
struct is a *binding* requirement (the `json` tag is needed for `Unmarshal` regardless), not
documentation.

**TYPE is derived from each handler's param-struct field. Everything else per param
(name-list, order, required, description, alias-of) is authored in a co-located Go descriptor.
Action-level semantics (purpose, value-aliases, errors, notes, envelope-requirements, examples,
returns) live in the same descriptor. The monolithic `actionSpecs` slice is replaced by
co-located per-action descriptors registered into an ordered registry.**

Why each property holds:

- **Agent-friendly (authoring).** Descriptions are normal Go string literals (multi-line,
  escapable) — not crammed into single-line backtick struct tags where backticks are impossible
  (the codebase already hit this in `meta_tool_descriptions.go`) and `reflect.StructTag`'s
  `key:"val"` grammar breaks on embedded quotes/colons. Consuming the docs is byte-identical
  either way (the parity requirement), so consumption is unaffected.
- **Clean.** The struct is the *binding* concern (the `Unmarshal` target); the descriptor is the
  *documentation* concern. They are linked only by param name, cross-checked by the existing
  `param_tag_gate`. The descriptor reads like the doc it produces.
- **Durable.** Struct field *order* stays a free-to-change implementation detail (Route A reads
  param order from the descriptor, never from struct layout). No `doc:"-"` sentinels that rot
  into "hidden or just forgotten?".
- **Extensible.** A new per-param attribute is a typed field on the descriptor's param type, one
  place — not another stringly-typed tag key. Generalizes to the 4 follow-on surfaces — though
  NOT because they are all typed: scoping finding #2's "all use typed param structs" was wrong (see
  the binding-style split in the As-built section, bug 935). It generalizes because the descriptor
  carries the authored semantics for map-bound params the same way it carries them for
  struct-backed ones — a map-bound surface simply authors the `Type` too (`ParamStruct == nil`),
  pinned by a per-surface binder-parity gate. The descriptor shape is uniform across both bindings.
- **Weak-LLM-friendly.** "Copy an existing descriptor param entry; change name + description" is
  declarative, typed, and pattern-matchable. No struct-tag DSL, no prose-escaping, no positional
  doc-order semantics to get silently wrong. Minimum-ambiguity, matching the offload tightening
  direction.

---

## The contract, concretely

### 1. Param struct (existing — binding + type source)

Each typed handler already unmarshals into an `xParams` struct (`taskBlockParams`,
`roadmapSetParams`, …). Under the contract this struct keeps its existing job (the `Unmarshal`
target) and additionally becomes the **single source of each param's type**: the Go field kind,
read by the shape-extractor, is the type. No hand-authored type anywhere.

### 2. Action descriptor (new — co-located Go, authored semantics)

Co-located with its handler (in `task.go`, `bug.go`, …). Carries everything that cannot be read
off a struct. Proposed shape (T3 finalizes the exact type names):

```go
type ActionDoc struct {
    Purpose              string            // one-line "what it does"
    Params               []DocParam        // ORDERED; order is authoritative for output
    Example              string
    SeeAlso              string            // work_actions/CallShape-only hint (no corpus field)
    ValueAliases         []ActionValueAlias
    Errors               []ActionError
    Notes                string
    EnvelopeRequirements []ActionEnvelopeReq
    Returns              *ActionReturn
}

type DocParam struct {
    Name        string // MUST match a struct json tag (gate-checked) unless the action has no struct
    Required    bool
    Description string
    AliasOf     string // canonical name this param aliases; "" == canonical
    Type        string // USUALLY EMPTY (derived from the struct). Authored only when no struct field
                       // backs the param (the forge family) — see §"Type derivation & reconciliation".
}
```

`DocParam` deliberately has **no Type for the common case** — the registry fills it from the
struct. Keeping the field but requiring it empty for struct-backed params (gate-enforced) lets the
forge family author its types in the same shape rather than needing a second descriptor type.

### 3. Shape-extractor (new — production, T2)

```go
// ExtractParamShape returns the json-tag name → derived spec-type for every
// documented-eligible field of a param struct, in field order. Reflect-based:
// the registry hands it a reflect.Type, so there is no file I/O (unlike the
// param_type_parity gate, which AST-walks source because it has no type in hand).
func ExtractParamShape(t reflect.Type) []DerivedParam // {JSONName, SpecType}
```

**Reflect, not AST** (the T1 reflect-vs-AST call): the production path *has* the concrete struct
type (the registry holds `reflect.TypeOf(xParams{})`), so `reflect` gives field order + json tag +
kind directly and stays sans-IO/testable. The AST machinery in `param_type_parity_test.go` stays
as the *gate* for unmigrated surfaces (it scans source generically, with no type in hand). What
T2 **shares/generalizes** from that machinery is the *type-family vocabulary* (the Go-kind →
type-string mapping), factored so the reflect path and the AST gate agree — not the traversal.

The extractor emits the **spec vocabulary** (the `actionSpecs.Type` strings:
`int64` / `string` / `bool` / `string[]` / `object[]` / `object` / `json`), because
`work_actions` and `CallShape` render that vocabulary verbatim (`id (int64)`). The existing
`docType(specType, required)` bridge in the corpus generator continues to map spec-vocab →
doc-vocab (`integer` / `optional_string` / `list` / …) for the embedded corpus. Keeping the
extractor on the spec vocabulary is what makes BOTH consumer families byte-identical.

Go kind → spec-vocab mapping (must reproduce today's strings):

| Go field kind | spec type |
|---|---|
| `int*` / `uint*` | `int64` (every current id field is `int64`) |
| `string` (and `*string`) | `string` |
| `bool` | `bool` |
| `[]string` | `string[]` |
| `[]<struct>` / `[]<other>` | `object[]` |
| `struct` / `map` | `object` |
| `json.RawMessage` | `json` |

### 4. Registry seam (new — T3)

An **ordered** slice of registrations (order is load-bearing: `work_actions` returns the catalog
in declaration order, grouped chains→tasks→bugs→…). Each entry references a co-located
descriptor + the param-struct type:

```go
type ActionEntry struct {
    Name        string
    Descriptor  ActionDoc
    ParamStruct reflect.Type // nil for the forge family (no param struct)
    // handler ref as today (registration stays where BuildTable wires it)
}
```

A thin ordered index remains in `actions_discovery.go` (it must, to pin order), but it now
*references* co-located descriptors instead of inlining the catalog — so "the hand-authored
`actionSpecs` catalog is gone" holds: the monolithic content catalog is replaced by co-located
descriptors + a thin order index.

**Merge (produces the ActionSpec-equivalent the consumers read):** for each entry, walk the
descriptor's params in authored order; for each param, if `ParamStruct != nil` and a field carries
`json:"<name>"`, set its type to the derived spec-type (the authored `Type` must be empty — gate);
otherwise use the authored `Type` (forge family / no-backing-field). Output is the same
`[]ActionParam` shape `work_actions` / `CallShape` / the corpus generator / `WorkDescription`
consume today. **Order comes from the descriptor; type comes from the struct by name-lookup** —
so struct field order is never load-bearing (the clean property).

### 5. Identifier-group (`id` OR `slug`) — expressed exactly as today

No new construct. The id-or-slug actions list `id` / `slug` / `chain_slug` as `Required:false`
params (one-of enforced in the handler, not the doc). `IdentifierRequiredError` /`CallShape`
already key off `hasParam(spec,"chain_slug")` and the optional flags; that logic is preserved
verbatim. Byte-identical by construction.

---

## Type derivation & reconciliation (the honest edges)

The "derive type from the struct" premise holds for most of the 49 work actions; the authored-type
exceptions (recorded so the next agent doesn't re-derive them) are:

1. **Map-bound actions with no typed param struct** — the forge family (`forge` / `forge_edit` /
   `forge_delete` / `forge_schema`), `task_edit`, and `roadmap_list`. These bind control params via
   `parseParamMap` + `rawStringParam` / `_ = json.Unmarshal(...)` map-indexing — there is no struct
   to reflect. **Resolution:** their descriptor entries carry **authored** `Type` (`ParamStruct ==
   nil`). Principled, not an escape hatch: forge's real per-schema shape is the forge *schema*
   system (`forge_schema`); the action-doc only documents the control envelope.

   **Typing decision (RESOLVED — keep authored).** The map binding is *deliberately
   error-tolerant* — a non-string value is treated as absent, not rejected — so making a struct the
   literal `json.Unmarshal` target (F1) would CHANGE rejection behavior (a behavior change, not a
   same-shape refactor). Both sibling triages landed on **leave-authored**:
   - **forge** → chain `refactor-forge-shape-dispatch` T4 D1: not typed. F1 rejected as
     behavior-changing; F2 (a reflection-only type-derivation struct) is structurally impossible
     anyway because forge's params are *per-schema dynamic* (the forge *schema* system owns the
     real shape; the action-doc only documents the control envelope).
   - **task_edit** → chain `task-edit-control-param-typing` (the `type-task-edit-control-param-envelope`
     sibling task): not typed. F1 rejected for the same reason (the tolerance is intentional and
     pinned). F2 *was* achievable here (task_edit has a fixed param set, and the authored doc already
     simplifies `acceptance_criteria`/`context_required` to `string`, so a shim struct would derive
     matching types) but was declined: it would single task_edit out of this permanent map-bound
     authored category (forge can never be struct-derived, so the category never empties), encode a
     cosmetic `string` shim over the binding's real string-or-list shape with no accuracy gain
     (derivation gates primitives — no union types), and add a struct that must stay in sync with
     `taskEditFieldSpec`. The authored treatment is principled, not debt.

   So `task_edit` and `roadmap_list` stay alongside the forge family as `ParamStruct == nil`. The
   tolerance is pinned by `go/internal/forge/param_binding_test.go`.

   **Two per-param authored cases inside otherwise-derived actions:** `chain_close.closure_summary`
   (the struct's json tag is `summary`; `closure_summary` is a custom-`UnmarshalJSON` alias with no
   derivable tag) and `task_search.verbose` (read off the raw params, not a field of
   `taskSearchParams`). The merge rule handles these uniformly: a param's type is derived iff its
   name matches a struct json tag, else authored.

   **The arc-close family DOES derive.** `review_arc_for_filing` / `emit_commit_landed` /
   `arc_review_audit` / `pending_decisions_claim` had their param structs extracted to the
   dependency-light leaf package `internal/arcreview/arcparams` (T3) so the work registry can
   reflect them without coupling package `work` to the Qwen inference stack.

2. **`batch.ops` type string.** `BatchParams.Ops` is `[]BatchOp` (a slice → derives `object[]`),
   but `actionSpecs` declares `"object"`; `roadmap_set.items` (`[]RoadmapSetInput`, the *same* Go
   kind) declares `"object[]"`. A uniform derivation cannot reproduce both byte-for-byte — the
   catalog is internally inconsistent here. **Resolution (a forced, enumerated, blessed delta —
   not an escape hatch):** correct `batch.ops` to the derived `object[]`. It is *more* accurate
   (ops is a list) and *more* agent-first (the description already says "List of {op,params,
   rationale} triples"). The alternative — a per-param authored-type override that wins over the
   derived value — is rejected as an escape hatch (it would reintroduce exactly the hand-authored
   type this chain deletes). So uniform derivation stands and `batch.ops` shifts by one cell.
   **Landed in T4** — the corpus chunk + the four net goldens were regenerated for exactly this
   cell (see the note below).

3. **Any further non-primitive deltas** surface mechanically in **T3's equivalence assertion**
   (registry-derived specs == current `actionSpecs`). The policy: derived type wins; each
   delta is either a no-op (already agrees, e.g. `roadmap_set.items`) or a blessed correction
   enumerated like #2. Primitive-type drift cannot occur — `param_type_parity` already gates it.

> **Net-regeneration note (LANDED in T4).** T1 captured `batch.ops = "object"` verbatim. The
> `batch.ops → object[]` correction (delta #2) was the *single* intended output change in the whole
> chain; T4 regenerated the corpus chunk (`corpus/work/batch.toml`) and the four net goldens
> (`work_actions.json`, `call_shapes.json`, `describe_work.json`, `admin_action_docs_work.json`) for
> exactly that one cell, reviewed the diff against this enumerated list, and confirmed byte-identity
> everywhere else (`work_description.txt` unchanged). A disciplined one-cell re-baseline of a known
> delta, not a silent wholesale one. (Net regeneration remains legitimate only for this enumerated
> delta and in T5 when the net is promoted to a standing gate.)

---

## The characterization net (the standing parity oracle for every surface)

External test package `actiondocs_test` (to avoid an import cycle with
`admin`/`observehttp`), golden fixtures under `go/internal/actiondocs/testdata/contract_net/`.
Two files (consolidated in `finalize-action-docs-epic` T3 — see suggestion 38):

- **`surface_contract_net_test.go`** — the shared describe / admin-payload / meta-tool-description
  **triple**, run for EVERY surface by one table-driven test `TestContractNet_Surfaces` over the
  `surfaceNets` table (one row per surface). Each surface contributes
  `describe_<surface>.json`, `admin_action_docs_<surface>.json`, `<surface>_description.txt`.
- **`contract_net_test.go`** — the two outputs unique to work (`work_actions.json` from
  `HandleWorkActions`; `call_shapes.json` from `CallShape` + `IdentifierRequiredError`) plus the
  shared fixture helpers (`netDir` / `compareOrUpdate` / `marshalIndent` / `embeddedReg`).

| Golden | Pins |
|---|---|
| `work_actions.json` | full `HandleWorkActions` catalog |
| `call_shapes.json` | `CallShape` + `IdentifierRequiredError` for every catalog action |
| `describe_<surface>.json` | `admin.action_describe(<surface>, X)` for all docs (incl `_general`), from the embedded corpus |
| `admin_action_docs_<surface>.json` | `GET /admin/action-docs?surface=<surface>` payload, flagless/embedded |
| `<surface>_description.txt` | the `<Surface>Description` meta-tool constant |

Regenerate (single correct command — bug 941's silent-partial-regen footgun was structurally
dissolved by the subtest structure): all surface goldens with
`UPDATE_CONTRACT_NET=1 go test -tags sqlite_fts5 ./internal/actiondocs/ -run TestContractNet_Surfaces`;
one surface with `-run TestContractNet_Surfaces/<surface>`; work's two extra goldens with
`-run 'TestContractNet_(WorkActions|CallShapes)'`. Under refactoring-discipline the goldens are
the oracle: a byte difference is a DEFECT to fix, not a baseline to update — legitimate
regenerations are only each surface's establishing baseline and the enumerated blessed-delta cells
(the per-surface header in `surface_contract_net_test.go` lists them). `write_actions` is excluded
from the HTTP-payload golden (it is dispatch-policy-derived, not action-doc-derived; covered by
`observehttp/actiondocs_test.go`) to avoid coupling the net to an unrelated artifact.

---

## Audit findings (recorded so the next agent doesn't re-derive them)

- **The struct is a re-ordered superset of the documented param set.** `chainSlugParams`
  (shared by chain_status / chain_state) carries `id` / `chain_id` fields the doc omits;
  `taskBlockParams` carries `blocked_by` / `blocked_by_chain` the doc omits, in a different order
  than the 9 documented params. Route A is immune (it reads names+order from the descriptor, not
  the struct), so these stay omitted byte-identically. The omissions split into two kinds:
  - **Intentional, leave omitted.** `task_block`'s `blocked_by` / `blocked_by_chain` are a
    *deliberately-unadvertised* legacy alias — advertising `blocked_by` as canonical is exactly
    the prior bug `task-block-action-doc-advertises-blocked_by-canonical-but-handler-only-accepts-blocker_slug`
    (fixed ea953e8); it's kept for back-compat but not documented. `chain_status` ignores `id`
    entirely (resolves by slug only via `p.resolved()`), so omitting `id` there is *correct*.
  - **A genuine doc gap — RESOLVED.** `chain_state` *does* resolve by `id` / `chain_id`
    (`p.resolvedID()`) but its action-doc originally listed only slug/chain/chain_slug. Filed as a
    low-severity follow-up and FIXED in commit `d75b5577` (bug
    `chain-state-action-doc-omits-id-chain-id-params-handler-accepts`): chain_state now advertises
    `id` (int64) + `chain_id` (alias of id), and the net was re-baselined for that cell at the time.
    chain_state's descriptor lists id/chain_id and they derive from `chainSlugParams` normally.
- **`param_type_parity` only gates primitives**, which is why the `batch.ops` non-primitive
  inconsistency survived. T5 keeps the gate for unmigrated surfaces; for work the contract makes
  it tautological (type == struct kind by construction).

---

## Per-surface migration template

All five surfaces have now migrated on this spine (`knowledge` → `measure` → `admin` → `ml`, each
its own refactoring-discipline-shaped chain reusing the machinery built on work). The template is
retained for a **future** hand-authored surface that needs onboarding, and as the reference spine
for any future derive-contract-style program (e.g. a Fork C implementation). The machinery
(extractor, registry, descriptor type, merge) is built once on work; a new surface reuses it.

0. **Pre-flight audit (DO THIS FIRST — suggestion 40).** Every migration that skipped this hit a
   wrong premise: knowledge's spec assumed "all surfaces are typed" (false — it was 16/23
   map-bound, bug 935); ml's spec assumed "the corpus is empty, this is a create" (false — it had
   two served prose-params chunks, bug 939). The template tells you WHAT to do but must not let you
   ASSUME the surface's current shape. Verify, per action, before authoring:
   - **Binding style.** For each action, is the param envelope a typed `json.Unmarshal` struct
     (derive) or map-bound via `mcpparam.*` / a tolerant map (author the type + add a binder gate)?
     Grep the handler. Do not assume "typed."
   - **Corpus + serve state.** Does a corpus already exist and serve? Run
     `admin.action_describe(<surface>, X)` and `ls go/internal/actiondocs/corpus/<surface>/`. A
     non-empty corpus means MIGRATE, not create.
   - **Prose-vs-structured.** If a chunk exists, are its params/errors/returns structured
     (`[[params]]`/`[[errors]]`/`[returns]`) or buried as PROSE in `notes`? Degenerate prose means
     the real work is a prose→structured restructure (the ml shape), an enumerated reviewed output
     change — not a byte-identical no-op.
1. **Characterization net (gate).** Snapshot the surface's observable doc outputs byte-exact:
   `admin.action_describe(<surface>, X)` for every action, the `<Surface>Description` meta-tool
   constant, and the `GET /admin/action-docs?surface=<surface>` payload. (Surfaces other than work
   have *only* `describe` as a doc consumer — no `work_actions`/`CallShape` equivalent — so the net
   is smaller.) Since T3's consolidation this is a **one-row** add to the `surfaceNets` table in
   `surface_contract_net_test.go` (`{<surface>, <Surface>Description}`), not a new test file. Commit
   goldens.
2. **Register via the contract.** Author a co-located `ActionDoc` descriptor per action (purpose,
   ordered params with required/description/aliasOf, value-aliases, errors, notes, envelope-reqs,
   examples), point each registration at its param-struct `reflect.Type`. Map-bound actions with
   no struct author their types (the forge-family pattern).
3. **Flip consumers / derive the corpus.** Generate the surface's corpus from the registry (extend
   the generator to the surface); `describe` serves the generated+embedded corpus. Net must pass
   byte-identical for unchanged cells; enumerate and review any intended output change.
   > **Correction (migrate-ml chain, 2026-05-26).** This step originally claimed `ml` "authors its
   > currently-empty corpus (corpus/ml is empty today — `describe(ml, X)` serves nothing)". That was
   > **false**: corpus/ml shipped two hand-authored, embedded, served chunks (`_general.toml` +
   > `inference.toml`, added by chain `single-source-action-describe` T6, commit 19c0c829), so ml was
   > a MIGRATE like knowledge, not a create. The non-trivial twist was different: `inference`
   > documented its params/returns/errors as PROSE inside `notes` (no structured blocks), so the flip
   > **restructured** that prose into struct-derived `[[params]]` + `[[errors]]` + `[returns]` — an
   > enumerated, reviewed output change re-baselined in the net (like work's `batch.ops` cell). The
   > general lesson for the remaining surfaces: **verify the actual corpus + serve state before
   > assuming empty.** (Filed: bug `ml-action-doc-migration-spec-wrongly-says-corpus-empty`.)
4. **Delete the hand-TOML.** Remove the surface's hand-authored corpus chunks; the generated corpus
   is now the only source. Keep the no-diff gate.
5. **Scope the gates.** Drop the surface from `param_type_parity`'s `surfacePackages` /
   `param_tag_gate`'s skip set (its shape is now struct-derived by construction OR map-bound under a
   binder gate); promote its net to a standing parity gate. If the surface has map-bound actions,
   add a per-surface binder-parity gate (the knowledge/measure pattern). Add a
   `Test<Surface>RegistryDerivedParamsHaveEmptyAuthoredType` so struct-backed params can't
   re-author their type. (These two original gates are now no-ops kept as the landing spot — see
   the As-built section; onboarding a surface is exactly the step that would re-populate them.)
6. **Required-enforcement.** If the handler enforces a required-param contract (an id-OR-X one-of,
   or plain `if p.X == ""` guards), add it to the "Required-vs-enforcement gate" section so the
   descriptor's `Required` flags are pinned against the handler, not just the output net.
7. **Frontend / serve verify.** Confirm the dashboard ActionDocs page renders the surface, the
   `?reload` affordance, and the dispatch-policy cross-link (per `ACTION_DOCS_FRONTEND.md`).
8. **Retrospective + close.** File `docs/*_RETROSPECTIVE_<date>.md`; honest gaps; forge no
   further chains (this is a leaf).

One chain per surface — never batch surfaces. Keep each chain's spine identical so the program
stays uniform.

> **Dynamically-registered convenience actions (suggestion 39).** ml's per-task convenience
> actions (`route_query`, `curation_score`, …) register at RUNTIME via `BuildTable`'s
> `RegisterConvenience` when a downstream model is promoted — they are NOT in the build-time
> `mlActionRegistry`. The first one that ships would be dispatchable as `ml.<action>` but invisible
> to the action-doc system: no generated corpus chunk, `admin.action_describe(ml, route_query)`
> returns a MISS, and the no-diff/orphan gates (which compare build-time registry ↔ on-disk corpus)
> can't see it. **The decided path:** a convenience-action-shipping chain MUST add a co-located
> descriptor + an `mlActionRegistry` entry at build time (so it generates + gates exactly like
> `inference`) — option (i), not a speculative runtime-doc machinery. This is recorded here and in
> `ml/action_doc.go` so the next model-promotion chain sees it before the first convenience action
> goes live. (Nothing is broken today — no convenience action is registered.)

---

## Scope (originally deferred by the founding work chain — now dispositioned)

The founding work chain deliberately deferred three things; all are now resolved:

- **Fork C (drop the corpus entirely; serve the registry directly).** Was "revisit after the
  contract proves out across a surface or two." Now proven across all five — decided as the final
  step of `finalize-action-docs-epic`; see the **Fork C decision** section below.
- **Doc-completeness expansions.** DONE — `finalize-action-docs-epic` T4 documented the measure
  `benchmark_*` and admin `project_register`/`host_*`/`remote_exec` handler-read params as
  enumerated, net-reviewed deltas (bugs 940 + 943). See the As-built section.
- **The follow-on surfaces.** All four (`knowledge`/`measure`/`admin`/`ml`) migrated on the
  per-surface template; none remains on hand-authored TOML. The surface-agnostic `describe`
  interface is unchanged.

---

## Fork C decision (DEFER — finalize-action-docs-epic, 2026-05-26)

**Fork C** = drop the generated + `go:embed`'d TOML corpus as the served artifact and serve action
docs from the surface registries directly (the corpus becomes, at most, a pure build-time check).
The founding work chain deferred it "until the contract proves out across a surface or two";
every subsequent retro noted it as "now proven across N surfaces; revisitable." It is now proven
across **all five** surfaces with zero new shared machinery added past knowledge. So the evidence
the deferral was gated on exists — and on that evidence the decision is to **DEFER, deliberately**,
not adopt.

**Why defer (the decisive reason is structural, not preference).** The corpus + `go:embed`
indirection is the **decoupling seam** that keeps the doc-SERVING layer free of the surface
packages. Today:

- `internal/actiondocs` (the `Registry` that `admin.HandleActionDescribe` and the
  `GET /admin/action-docs` handler both serve from) imports **no** surface package — only
  `BurntSushi/toml` + stdlib. It serves the embedded TOML.
- The all-surfaces coupling lives entirely in `cmd/action-docs-corpus-gen`, a separate `package
  main` that imports every surface's `*ActionSpecs()` to project registries → TOML **at build
  time**.

Fork C would move that all-surfaces import into the **runtime** serve path: `admin` / `observehttp`
would have to import `work` + `knowledge` + `measure` + `ml` (+ `actionspec`) to assemble specs per
request. That is exactly the coupling the contract net is written as an external test package to
avoid ("to avoid an import cycle with `admin`/`observehttp`"), pushed into production. Beyond the
coupling, dropping the corpus also loses three concrete affordances:

1. **The grep-able on-disk artifact.** `corpus/<surface>/<action>.toml` is readable (and
   greppable) without running the server — an affordance the work retro explicitly valued.
2. **Reviewable diffs.** A doc change shows up as a corpus diff + a contract-net golden diff in the
   PR; the no-diff gate makes an un-regenerated registry edit fail loudly. Serving live erases that
   review surface.
3. **A home for `_general.toml`.** The hand-authored surface-wide cross-cutting chunks are not
   registry-derived; they live in the corpus. Fork C needs a separate mechanism for them.

Against all that, Fork C's upside is modest: it removes a generated intermediate + the (cheap,
one-command, gated) no-diff check. The dynamic-action argument that once motivated it is also moot
— suggestion 39 decided that ml's runtime convenience actions get **build-time** descriptors, which
fits the corpus-generation model exactly rather than needing live registry serving.

**Reopening conditions.** Revisit Fork C if any of these change:

- The surface registries are extracted to dependency-light **leaf packages** (the way work's
  arc-close family went to `internal/arcreview/arcparams` and refresolve to `refparams`), so the
  serve path could import them without pulling in the full surface packages — dissolving the
  coupling objection.
- Corpus generation/maintenance becomes a real burden (it is not today — one command, gated).
- A consumer genuinely needs docs that cannot be a build-time snapshot (none does today; convenience
  actions are handled by build-time descriptors per suggestion 39).

No implementation sub-chain is forged (that is the ADOPT branch's obligation). The corpus stays
generated + embedded; suggestion 29 is resolved **deferred** against this decision.
