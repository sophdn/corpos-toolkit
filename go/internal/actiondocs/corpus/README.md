# action-docs corpus

Per-action documentation for every MCP meta-tool action this server registers.
Each chunk is a typed TOML file the server loads at startup and surfaces
through `admin.action_describe(surface, action)`. Agents pull the one chunk
they need on demand instead of scanning the whole `*Description` constant in
`go/cmd/toolkit-server/main.go`.

## Layout

```
go/internal/actiondocs/corpus/   # go:embed'd into the binary
├── _schema.toml           # field-set declaration (this directory's schema)
├── README.md              # this file
├── work/
│   ├── _general.toml      # surface-wide cross-cutting prose (reserved name)
│   ├── bug_resolve.toml
│   ├── task_complete.toml
│   └── …                  # one file per registered work action
├── measure/
│   ├── _general.toml
│   └── …
├── knowledge/
│   ├── _general.toml
│   └── …
└── admin/
    ├── _general.toml
    └── …                  # admin.action_describe lives here too
```

File naming: `<surface>/<action>.toml`, where `<action>` matches the literal
name registered in the surface's `BuildTable` (e.g. `work/bug_resolve.toml`,
`measure/classify_artifact_tier.toml`). The reserved literal `_general` marks
a surface-wide chunk for cross-cutting prose (cross-project defaults, alias
conventions that span multiple actions, sentinel values that apply to a
whole family of actions). The loader treats `_general` as findable through
`admin.action_describe(surface, "_general")` but excludes it from
list-by-surface enumerations of real actions.

## Generated vs hand-authored surfaces

Three surfaces' action chunks are **GENERATED**, not hand-authored — do NOT edit
their `<action>.toml` files directly (the no-diff gate will reject the drift):

- **`work/`** — generated from `work.actionRegistry` (chains
  `single-source-action-describe` → `establish-action-doc-contract-on-work`).
- **`knowledge/`** — generated from `knowledge.knowledgeActionRegistry` (chain
  `migrate-knowledge-action-docs-to-derive-contract`).
- **`measure/`** — generated from `measure.measureActionRegistry` (chain
  `migrate-measure-action-docs-to-derive-contract`). measure is almost entirely
  map-bound, so only `bench_run` derives its param types from a struct; the rest
  author their types in the descriptor (the forge-family pattern).
- **`admin/`** — generated from `admin.adminActionRegistry` (chain
  `migrate-admin-action-docs-to-derive-contract`).
- **`ml/`** — generated from `ml.mlActionRegistry` (chain
  `migrate-ml-action-docs-to-derive-contract`). ml has one statically-registered
  action (`inference`); per-task convenience actions register dynamically at
  model-promotion time and carry no corpus chunk.

For these surfaces the co-located Go descriptor registry is the single source of
truth; each param's *type* derives from its handler's param struct (where one
exists). To change a generated chunk, edit the descriptor in the surface's package
(`go/internal/<surface>/action_doc.go`) and run `scripts/action-docs-corpus-gen`,
then stage the regenerated chunk. The precommit no-diff gate
(`action-docs-corpus-gen --check`) fails if any on-disk chunk diverges from what
the registry projects.

No full surface is hand-authored any more — work, knowledge, measure, admin, and
ml all generate their per-action chunks from their registries. Every surface's
`_general.toml` is always hand-authored cross-cutting prose, exempt from
generation regardless of surface.

## Loader pickup

The Go-side registry (`go/internal/actiondocs/`, built in T2) scans this
directory at server startup, parses each `.toml` into an `ActionDoc` struct,
and validates against `_schema.toml`. Parse errors surface at startup with
file path + reason; a single bad chunk does not fatal the binary. Adding a
new action on a HAND-AUTHORED surface is: drop a new `<surface>/<action>.toml`,
restart the server (or use `admin.schema_reload` if that's wired for action-docs
too — currently it only reloads forge schemas). On a GENERATED surface (see
"Generated vs hand-authored surfaces" above) add the descriptor to the registry
and regenerate instead.

## When to edit corpus vs main.go TOC

The corpus is the source of truth for per-action prose. The four
`*Description` constants in `go/cmd/toolkit-server/main.go` (workDescription,
measureDescription, knowledgeDescription, adminDescription) are the
table-of-contents: one-paragraph surface purpose + alphabetical action list
+ a pointer to `admin.action_describe`. No per-action prose lives in the
constants.

- **Adding or changing per-action behaviour?** Every surface is generated
  (work / knowledge / measure / admin / ml) — edit the co-located descriptor + run
  `scripts/action-docs-corpus-gen` (see "Generated vs hand-authored surfaces").
  If the change introduces a new action, also add the action's name to the TOC
  list in the surface's `*Description` constant.
- **Changing a surface-wide convention?** Edit the corresponding
  `<surface>/_general.toml`. The `*Description` constant's opening
  paragraph may also need a one-line update if the change is
  caller-visible at the surface level.
- **Renaming an action?** Move the chunk file, update the action list in
  the TOC, and add a `param_aliases` or notes entry to the renamed
  chunk if the old name is still accepted for back-compat.

The TOC constants are hand-kept thin so any drift between them and the
corpus is obvious on inspection — no Markdown duplication elsewhere
(CLAUDE.md, top-level READMEs) for per-action prose.

## Authoring conventions

- Each chunk is self-contained. A reader landing on `bug_resolve.toml` should
  not need to open a second chunk to understand it; quote the relevant
  surface-wide context inline rather than referencing `_general` by name.
- Lead the `purpose` field with the call's result, not its mechanism — the
  corpus skims well when every purpose answers "what do I get back?" in its
  first sentence.
- Document only caller-controlled errors in `errors`; runtime/infra failures
  (DB down, network) are out of scope.
- `param_aliases` (key renames like `sha`→`commit_sha`) and `value_aliases`
  (value normalizations like `fix`→`fixed`) are separate fields; choose by
  whether the rename is on the parameter NAME or on its VALUE.
- `notes` is where bug numbers, sentinel-value history, and see-also pointers
  go. Keep it self-contained: quote context inline, don't redirect.

## Field set

The full field declaration lives in `_schema.toml`. The required fields are
`surface`, `action`, `purpose`; the optional fields are `params`,
`param_aliases`, `value_aliases`, `errors`, `examples`, `notes`.
