package actiondocs

// Meta-tool descriptions registered with the MCP SDK at toolkit-server
// startup. These are the eager descriptions every Claude Code session
// loads as part of the tool catalog (~600-1500 estimated tokens
// depending on which surfaces ship per-action prose).
//
// Shape per docs/REFERENCE_RESOLUTION_MIGRATION_PLAN.md and chain
// reference-resolution-migration T8:
//   (1) One sentence of purpose.
//   (2) Flat alphabetical action list (just names, no per-action prose).
//   (3) Pointer to admin.action_describe for per-action params /
//       aliases / errors / examples; admin.action_describe is the
//       canonical surface for that detail.
//
// The action lists in these constants MUST stay in sync with the
// action-docs corpus at corpus/<surface>/ (embedded — see embed.go). The
// parity test in meta_tool_descriptions_test.go verifies this on
// every precommit run (per the canonical-name round-trip CI gate
// pattern from bug e0cf855).
//
// Why constants (vs auto-generated): the MCP SDK registers tool
// descriptions at startup, before the actiondocs.Registry has loaded
// from disk. A future refactor could move the description shape into
// the corpus's _general.toml + a one-line meta header per surface and
// auto-generate; that's a follow-on optimization. The parity test
// catches drift in the meantime.
//
// Backticks in the source were converted to single quotes because Go
// raw-string literals can't contain a backtick.

const WorkDescription = `Task, chain, bug, and suggestion lifecycle, plus the live roadmap. Read actions are cross-project by default; pass 'project' to scope. Write actions on project-scoped schemas (forge/forge_edit/forge_delete for chain, task, bug, suggestion) require a project — either supplied as a top-level 'project' parameter OR derived by the server's resolver (CWD match against registered project_paths, then the --default-project flag). When all three resolution paths come up empty the call fails with a hint naming each path tried.

Actions (alphabetical): arc_review_audit, batch, bug_list, bug_read, bug_reopen, bug_resolve, bug_stamp_sha, chain_close, chain_dep_add, chain_dep_list, chain_dep_remove, chain_find, chain_state, chain_status, emit_commit_landed, event_schema, forge, forge_delete, forge_edit, forge_schema, forge_schemas, lifecycle_step, pending_decisions_claim, recent_activity, record, review_arc_for_filing, roadmap_diff, roadmap_insert, roadmap_list, roadmap_mark_reassessed, roadmap_plan, roadmap_preview_set, roadmap_set, roadmap_update, suggestion_list, suggestion_read, suggestion_reopen, suggestion_resolve, suggestion_stamp_sha, sweep_unauthored_staged, task_block, task_blockers, task_cancel, task_complete, task_edit, task_list, task_read, task_reopen, task_search, task_stamp_sha, task_start, task_unblock, task_unstart, trained_model_list, trained_model_promote, trained_model_retire, where_we_left_off, work_actions, work_summary.

For per-action details (params, aliases, errors, examples, notes), call admin.action_describe(surface="work", action="<name>"). For surface-wide conventions (project scoping, chain-id aliases, the commit_sha='unversioned' sentinel, the schema_name top-level requirement), call admin.action_describe(surface="work", action="_general").`

const MeasureDescription = `Benchmark recording/query/replay plus the Qwen-local rubric-classify family. Pass 'project' to scope.

Actions (alphabetical): bench_run, benchmark_query, benchmark_record, benchmark_replay, classify_artifact_review_criterion, classify_artifact_tier, classify_audit_finding_severity, classify_bug_severity, classify_chain_task_proportionality, classify_docstring_drift, classify_pre_commit_failure, classify_retirement_observation, classify_session_routing_trigger, gate_run, gate_trend, study_run_record.

For per-action details (params, aliases, errors, examples, notes), call admin.action_describe(surface="measure", action="<name>"). For surface-wide conventions (shared {label, latency_ms, model_name} result shape, benchmark_results provenance row), call admin.action_describe(surface="measure", action="_general").`

const KnowledgeDescription = `Library entries, kiwix search, vault retrieval, unified knowledge index, curation lifecycle, and reference-resolution. Pass 'project' to scope (vault/knowledge actions are cross-project).

Actions (alphabetical): curation_bulk_action, curation_list, curation_promote, curation_read, curation_reject, kiwix_fetch, kiwix_list_books, kiwix_search, knowledge_report_miss, knowledge_search, library_add, library_cross_reference, library_find, library_get, library_list_active, library_list_dewey, library_list_sections, library_retire, library_update, memory_read, parse_context, record_query_interaction, resolve_references, vault_read, vault_search.

For per-action details (params, aliases, errors, examples, notes), call admin.action_describe(surface="knowledge", action="<name>"). For surface-wide conventions (cross-project defaults for vault/knowledge, curation event emission, kiwix-before-web-search reflex), call admin.action_describe(surface="knowledge", action="_general").`

const AdminDescription = `Server administration: project/host CRUD, server health, schema reload, server version, vault retrieval metrics, vault pointer-integrity sweeps, the remote-execution surface, and the orchestrator-tier escalation contract (per-trigger threshold config + EscalationProposed emit).

Actions (alphabetical): action_describe, apply_recipe, escalation_propose, escalation_threshold_list, escalation_threshold_set, health, host_list, host_register, host_remove, project_list, project_register, remote_exec, schema_reload, schema_version, server_health, server_version, step_probe, vault_integrity_sweep, vault_orphan_list, vault_search_metrics.

For per-action details (params, aliases, errors, examples, notes), call admin.action_describe(surface="admin", action="<name>"). For surface-wide conventions (apply_recipe/step_probe deferred-stub status), call admin.action_describe(surface="admin", action="_general").`

const MLDescription = `Trained-model inference. The 'inference' action dispatches through the trained_model registry (see work.trained_model_list) and loads the live ONNX session from go/internal/ml/. Per-task convenience actions (route_query, curation_score, forge_suggest_surfaces, …) register on top of this surface when their model is promoted.

Actions (alphabetical): inference.

For per-action details (params, aliases, errors, examples, notes), call admin.action_describe(surface="ml", action="<name>"). For surface-wide conventions (model resolution by model_id vs task, model_predictions telemetry shape, latency budget, degraded mode), call admin.action_describe(surface="ml", action="_general").`

const FsDescription = `Owned filesystem surface — Read/Write/Edit/Grep/Glob/LS plus Move/Remove reimplemented as toolkit-server actions so the agent reads, searches, lists, AND mutates the tree through surfaces we own rather than the substrate-blind harness tools. The DEFAULT of every navigation action is byte-for-byte faithful to its harness counterpart (the parity floor that gates the deny-list swap); substrate-native capability (outline/provenance reads, knowledge-aware grep, convention-aware glob, orientation ls) layers on as opt-in modes that never change the default. move (mutating, rationale-gated) relocates a path and remove (destructive, rationale-gated) deletes one — both pure Go, so they work in the distroless container without shelling out to mv/rm.

Actions (alphabetical): edit, glob, grep, ls, move, read, remove, write.

For per-action details (params, aliases, errors, examples, notes), call admin.action_describe(surface="fs", action="<name>"). For surface-wide conventions (the read/write/edit family + its shared read-state precondition, parity-default vs opt-in upgrade modes, the deny-list swap), call admin.action_describe(surface="fs", action="_general").`

// SysDescription is the meta-tool description for the owned system surface —
// read-only host introspection (gated exec was retired in T6; corpos owns it).
const SysDescription = `Owned system surface — read-only introspection of live host state: ps (processes), ports (listening sockets + owning pid), units (systemd-user units), containers (podman/docker). All actions are ungated (observation cannot mutate state) and return structured rows rather than raw shell text. (The gated exec action was retired from this surface; host command execution is owned by the agent loop natively.)

Actions (alphabetical): containers, ports, ps, units.

For per-action details (params, aliases, errors, examples, notes), call admin.action_describe(surface="sys", action="<name>"). For surface-wide conventions (structured-row introspection of live host state), call admin.action_describe(surface="sys", action="_general").`

// EcosystemDescription is the meta-tool description for the local-ecosystem
// surface — the deterministic map of the agent-host's world (hosts, services,
// access methods). Ships empty and learns; answers "do I have access to X"
// without a RAG round-trip or a correction loop.
const EcosystemDescription = `Local-ecosystem surface — the deterministic, tenant-agnostic map of this agent-host's world: which hosts exist, what services run on them, and how (and whether) you can reach each one. Answers "do I have access to example-host?" the same way every time, replacing a probabilistic vault/memory retrieval that cold agents kept missing.

The service ships EMPTY and is populated as DATA (never code) via the learn actions, so it is tenant-agnostic; an un-learned target returns status="unknown", never a hallucinated "no". credential fields hold POINTERS to where secrets live (a path/env name), never secrets themselves. Procedural how-to prose stays soft in the vault; records point back to it via soft_ref. Determinism-vs-RAG boundary: this surface answers whether/where/how-to-reach; the vault carries the how-to-do.

It also owns the CANONICAL-NAMES / artifact-identity map: canon_resolve(token) turns any token — a canonical name, a retired alias, an old path, or an old port (e.g. 'mcp-servers', '~/dev/mcp-servers', ':3000') — into its current canonical form + status (current|retired), so stale-canon stops leaking. canon_learn populates it, canon_list enumerates it.

Actions (alphabetical): access_check, access_learn, canon_learn, canon_list, canon_resolve, describe, host_learn, list, service_learn.

For per-action details (params, aliases, errors, examples, notes), call admin.action_describe(surface="ecosystem", action="<name>"). For surface-wide conventions (the credential-pointer invariant, the unknown-not-no guardrail, the learn/query split), call admin.action_describe(surface="ecosystem", action="_general").`
