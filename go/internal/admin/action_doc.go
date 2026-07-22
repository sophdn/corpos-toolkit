package admin

// action_doc.go is the descriptor-registry seam for the admin surface's action
// docs (chain migrate-admin-action-docs-to-derive-contract — the fourth
// per-surface instantiation of the contract established on work, after
// knowledge and measure). It is the single source of the admin surface's action
// docs: each param's TYPE is DERIVED from the handler's typed param struct, and
// only the irreducible semantics (purpose, param name-list/order/required/
// description, errors, notes, envelope-requirements, examples, returns) are
// authored in a co-located Go descriptor.
//
// admin is the INVERSE of knowledge/measure on the binding-style axis: it is
// FULLY struct-backed. Every admin handler json.Unmarshals into a typed param
// struct — there are NO mcpparam map-bound actions — so the knowledge audit's
// "verify the binding style first" warning resolved the opposite way here. Every
// params-bearing action DERIVES its types (authored Type left empty, gate-enforced
// by TestAdminRegistryDerivedParamsHaveEmptyAuthoredType): vault_search_metrics
// (← vaultSearchMetricsParams), action_describe (← ActionDescribeParams), and —
// added in chain finalize-action-docs-epic T4 (bug 943) — project_register
// (← projectRegisterParams), host_register (← hostRegisterParams), host_list
// (← hostListParams), host_remove (← hostRemoveParams), and remote_exec
// (← remoteExecParams). Those five handlers previously documented NO params
// despite REQUIRING several (a docs-only caller hit a guaranteed required-param
// error); the T4 doc-completeness expansion hoisted their inline anon structs to
// named co-located types so the registry can derive, and the descriptors author
// the required flags to match each handler's `if p.X == ""` enforcement.
//
// The generated corpus + admin.action_describe(admin, X) derive from this
// registry via AdminActionSpecs(); byte-parity is pinned by the T1
// characterization net (internal/actiondocs/surface_contract_net_test.go). The
// enumerated blessed delta is the project_register/host_*/remote_exec param
// documentation added in T4 (bug 943) — the founding admin migration moved no cell.

import (
	"reflect"

	"toolkit/internal/actionspec"
)

// standardRationaleEnvelope is the dispatch-policy rationale gate shared
// verbatim by the mutating admin actions that carry the canonical
// action-manifests/dispatch-policy.toml reason text: project_register,
// host_register, host_remove, schema_reload. vault_integrity_sweep is mutating
// too but authors its own sweep-specific reason (below), so it is NOT in this
// set. The slice is read-only — DeriveSpec assigns it by reference and SpecToDoc
// only reads it — so a single shared value is safe and keeps the four identical
// blocks DRY.
var standardRationaleEnvelope = []actionspec.ActionEnvelopeReq{{
	Field:               "rationale",
	Required:            true,
	Reason:              "Dispatcher policy gate (action-manifests/dispatch-policy.toml). Lives at the call envelope level (next to action/params/project), NOT inside params. Rejected on empty / whitespace / boilerplate / <6-char rationales with error=rationale_required.",
	AppliesToActorKinds: []string{"agent"},
}}

// ── Health / version / schema (no params) ──

var healthDoc = actionspec.ActionDoc{
	Purpose: "Alias for server_health — returns toolkit server liveness, DB connectivity, registered surfaces. See server_health for the canonical chunk.",
}

var serverHealthDoc = actionspec.ActionDoc{
	Purpose: "Return the toolkit server's health status — liveness, DB connectivity, registered surfaces. Accepts the alias 'health' (see admin.health for the alias chunk).",
	Notes:   "The health action name is registered as an alias for the same handler — admin.action_describe('admin', 'health') resolves to the alias chunk; the canonical name is server_health.",
}

var serverVersionDoc = actionspec.ActionDoc{
	Purpose: "Report the running binary's build-time git SHA + timestamp + package version, so an agent can compare against 'git rev-parse HEAD' to detect deploy lag (running binary older than HEAD = restart the daemon before exercising new dispatch code).",
}

var schemaVersionDoc = actionspec.ActionDoc{
	Purpose: "Return the loaded schema registry's version (e.g. count of schemas, source dir, last reload timestamp). Useful for confirming the running binary is reading the schema set you expect.",
}

var schemaReloadDoc = actionspec.ActionDoc{
	Purpose:              "Re-scan the bundled blueprints/forge-schemas dir + BLUEPRINTS_PROJECT_DIR (if set) and atomically replace the in-memory registry — use after adding or editing schemas on disk to avoid restarting the server.",
	EnvelopeRequirements: standardRationaleEnvelope,
}

// ── Project / host CRUD (struct-backed; params derive from the hoisted
// *Params types; required flags authored to match the handler `if p.X == ""`
// guards — chain finalize-action-docs-epic T4, bug 943) ──

var projectRegisterDoc = actionspec.ActionDoc{
	Purpose: "Register a new project with the toolkit. Creates the project_id row that other actions (forge_*, chain_*, task_*, bug_*) scope against via the 'project' parameter. Idempotent upsert on id.",
	Params: []actionspec.DocParam{
		{Name: "id", Required: true, Description: "Project id (the kebab-case slug other actions pass as the 'project' parameter, e.g. 'mcp-servers'). Required — the handler rejects a missing id."},
		{Name: "name", Required: true, Description: "Human-readable project name. Required — the handler rejects a missing name."},
		{Name: "path", Required: false, Description: "Filesystem path of the project root, used by the CWD-based project resolver. Optional."},
	},
	EnvelopeRequirements: standardRationaleEnvelope,
}

var projectListDoc = actionspec.ActionDoc{
	Purpose: "List every registered project. Use this when you need to discover what projects exist or confirm a project_id before scoping a write action against it.",
}

var hostRegisterDoc = actionspec.ActionDoc{
	Purpose: "Register a remote host the server can dispatch remote_exec calls to. Idempotent upsert on id (the host slug).",
	Params: []actionspec.DocParam{
		{Name: "id", Required: true, Description: "Host slug (the identifier remote_exec's 'host' param references). Required."},
		{Name: "hostname", Required: true, Description: "Network address / hostname to SSH to (stored as the host's addr). Required."},
		{Name: "ssh_user", Required: true, Description: "SSH login user. Required."},
		{Name: "ssh_port", Required: false, Description: "SSH port. Defaults to 22 when omitted or 0."},
		{Name: "ssh_key", Required: false, Description: "Path to the SSH private key (stored as ssh_key_path; NULL when omitted, falling back to the agent's default key resolution)."},
		{Name: "description", Required: false, Description: "Free-form notes about the host."},
		{Name: "passwordless_sudo", Required: false, Description: "Whether the host grants passwordless sudo to ssh_user. Defaults to false."},
	},
	EnvelopeRequirements: standardRationaleEnvelope,
}

var hostListDoc = actionspec.ActionDoc{
	Purpose: "List the registered remote hosts available to remote_exec. By default only active (non-retired) hosts are returned.",
	Params: []actionspec.DocParam{
		{Name: "include_retired", Required: false, Description: "When true, include hosts that have been retired (host_remove sets retired_at). Defaults to false (active hosts only)."},
	},
}

var hostRemoveDoc = actionspec.ActionDoc{
	Purpose: "Remove a registered remote host (soft-retire: sets retired_at, never DELETEs). Companion to host_register; subsequent remote_exec calls targeting the removed host fail with an unknown-host envelope.",
	Params: []actionspec.DocParam{
		{Name: "id", Required: true, Description: "Host slug to retire. Required — the handler rejects a missing id and returns host_not_found when no matching active host exists."},
	},
	EnvelopeRequirements: standardRationaleEnvelope,
}

// ── Vault telemetry + integrity ──

// vault_search_metrics is one of admin's two struct-backed documented actions:
// its params derive from vaultSearchMetricsParams (reflect.TypeOf in the
// registry). since→optional_string (string field, not required), recent_n→
// integer (int64). The descriptor authors order + descriptions; Types stay
// empty so they derive.
var vaultSearchMetricsDoc = actionspec.ActionDoc{
	Purpose: "Return aggregate stats — total_calls, p50/p95 latency, mean results-count, recent queries — over the most recent N vault_search invocations (or all calls since 'since'); used to evaluate the stage-2 trigger conditions in the agent-vault-read-discipline chain.",
	Params: []actionspec.DocParam{
		{Name: "since", Required: false, Description: "Lower-bound timestamp; when supplied, aggregates over every vault_search call after this point."},
		{Name: "recent_n", Required: false, Description: "Aggregate over the most recent N invocations. Defaults to 50."},
	},
}

var vaultOrphanListDoc = actionspec.ActionDoc{
	Purpose: "Read-only dry-run that returns every active vault knowledge_pointer whose source_ref file is missing on disk. Use to inspect what vault_integrity_sweep would retire without mutating any state. Walks source_type='vault' AND status='active' pointers, stat()s each path under the resolved vault root, partitions into files-present vs. orphans-found, and returns the orphan list alongside the totals.",
	Notes: `Chain forge-vault-note-schema-rework T5 shipped this action as the dry-run inspection surface for the integrity-sweep machinery. The companion mutator is admin.vault_integrity_sweep — that one state-transitions orphans to status='orphaned'. The two actions share the same scan path in internal/knowledge/pointers.ListVaultOrphans; the read-only branch never writes.

Vault root resolution follows internal/knowledge/vault.ResolveRoot precedence: TOOLKIT_VAULT_ROOT override → FORGE_MARKDOWN_ROOT/vault → $HOME/.claude/vault. The resolved root is returned in the response so callers can confirm which directory the sweep walked.

The response shape (vault_root / total_pointers / files_present / orphans_found / orphans) mirrors admin.vault_integrity_sweep minus orphans_retired. Dashboards can render both responses with the same code.
`,
}

var vaultIntegritySweepDoc = actionspec.ActionDoc{
	Purpose: "Walk every active source_type='vault' knowledge_pointer, stat() its source_ref under the resolved vault root, and state-transition any pointer whose file is missing to status='orphaned' (FTS5 inverted-index entry removed at the same time). Never DELETEs — the row is preserved with its audit columns intact so post-hoc forensics can see created_at / usage_count / last_used_at for retired pointers. Returns the counts (total / files_present / orphans_found / orphans_retired) plus the orphan list.",
	Notes: `Chain forge-vault-note-schema-rework T5 shipped this action as the ongoing-prevention engine for orphan vault pointers. The one-shot cleanup that zeroed the existing orphan count landed in chain vault-hygiene-extract-and-migrate T9 (2026-05-20); from that point forward, future orphans (file deleted without retiring its pointer row — hand-edits, dev churn, scope-change re-forges that race) get caught by this sweep.

Use the read-only dry-run sibling admin.vault_orphan_list to inspect what the sweep would retire before committing. Both actions share the same scan path in internal/knowledge/pointers.ListVaultOrphans; only this one drives RetireOrphan over the returned slice.

The startup hook lives in admin.RunStartupVaultIntegritySweep (fire-and-forget goroutine wired from main.go) so the sweep also runs once on every server boot. The hook never blocks bootstrap — a slow filesystem or missing vault root logs at warn level and the server proceeds normally.

OrphansRetired may be < OrphansFound when a concurrent caller has already retired a row between scan and write (ErrNotFound is treated as a benign race and skipped silently).
`,
	EnvelopeRequirements: []actionspec.ActionEnvelopeReq{{
		Field:               "rationale",
		Required:            true,
		Reason:              "Mutating admin action — the events ledger needs to know *why* the sweep ran (scheduled hygiene, post-incident cleanup, follow-up after a manual file deletion).",
		AppliesToActorKinds: []string{"agent"},
	}},
	Errors: []actionspec.ActionError{{
		Condition: "vault root unresolvable",
		Message:   "error: 'vault root not found' — TOOLKIT_VAULT_ROOT, FORGE_MARKDOWN_ROOT, and $HOME/.claude/vault all failed to resolve.",
	}},
}

// ── action_describe (the surface admin owns; the other struct-backed
// documented action — its params derive from ActionDescribeParams) ──

var actionDescribeDoc = actionspec.ActionDoc{
	Purpose: "Return the per-action TOML chunk for one (surface, action) lookup. The corpus is go:embed'd into the binary from go/internal/actiondocs/corpus/<surface>/<action>.toml; this getter is how agents read it on demand instead of scanning the surface *Description constants for one piece of prose.",
	Notes:   "Always-registered: this action is part of admin.BuildTable regardless of whether the corpus loaded, so dispatch behavior is consistent. When the corpus is absent, the handler returns the corpus-not-loaded envelope rather than 'action not registered'. Wire shape: success returns the ActionDoc fields at the top level (same JSON keys as the source TOML); error returns an envelope with `error` plus surface/action/registered_surfaces/registered_actions/hint where populated.",
	Params: []actionspec.DocParam{
		{Name: "surface", Required: true, Description: "The meta-tool surface: one of {work, measure, knowledge, admin}."},
		{Name: "action", Required: true, Description: "The action name as registered in the surface's BuildTable. Pass action=\"_general\" for surface-wide cross-cutting prose (cross-project defaults, alias conventions that span actions, sentinel values)."},
	},
	Errors: []actionspec.ActionError{
		{Condition: "missing surface", Message: "Returns error envelope: action_describe: surface is required."},
		{Condition: "missing action", Message: "Returns error envelope: action_describe: action is required (with a hint pointing at action=\"_general\")."},
		{Condition: "unknown surface", Message: "Returns error envelope with error=surface_not_found and registered_surfaces=[...] listing the surfaces this binary has loaded."},
		{Condition: "unknown action under known surface", Message: "Returns error envelope with error=action_not_found, surface, action, and registered_actions=[...] (excludes _general; the Hint mentions _general separately)."},
		{Condition: "corpus not loaded", Message: "Returns error envelope explaining --action-docs-dir was not resolved at startup; admin.action_describe is disabled for this binary instance."},
	},
	Examples: []actionspec.ActionExample{
		{Description: "Fetch a specific action's chunk.", Call: "{surface: \"work\", action: \"bug_resolve\"}"},
		{Description: "Fetch a surface's cross-cutting prose.", Call: "{surface: \"work\", action: \"_general\"}"},
		{Description: "Cross-surface lookup — admin.action_describe works for every surface's actions, not just admin's.", Call: "{surface: \"measure\", action: \"classify_bug_severity\"}"},
	},
}

// ── Remote execution + deferred recipe stubs ──

var remoteExecDoc = actionspec.ActionDoc{
	Purpose: "Dispatch a shell command to one of the registered remote hosts (see host_register / host_list). Returns the command's stdout/stderr/exit status (stdout/stderr capped in the response; full output persisted in the remote_ops audit row).",
	Params: []actionspec.DocParam{
		{Name: "host", Required: true, Description: "Host slug to run the command on (must be registered via host_register). Required — the handler rejects a missing host. An unregistered slug returns a host_not_registered envelope."},
		{Name: "cmd", Required: true, Description: "Shell command to execute on the remote host. Required."},
		{Name: "transport_backend", Required: false, Description: "Transport to use. Defaults to 'system_ssh' (the only supported backend in the Go server; any other value returns a transport_not_implemented envelope)."},
		{Name: "project_id", Required: false, Description: "Project to attribute the remote_ops audit row to. Defaults to 'mcp-servers' (the project housing the admin meta-tool)."},
	},
}

var applyRecipeDoc = actionspec.ActionDoc{
	Purpose: "Apply a setup-recipe to a host (deferred stub in the current Go server). Callers receive a structured deferred-stub envelope identifying the action rather than a real execution.",
	Notes:   "Deferred stub: the full port lands when a concrete need (host re-setup) drives it. Until then, calls return the deferred envelope unchanged.",
}

var stepProbeDoc = actionspec.ActionDoc{
	Purpose: "Probe whether one recipe-step would apply against a host (deferred stub in the current Go server). Callers receive a structured deferred-stub envelope identifying the action rather than a real probe result.",
	Notes:   "Deferred stub: the full port lands when a concrete need (host re-setup) drives it. Until then, calls return the deferred envelope unchanged.",
}

// ── Orchestrator-tier escalation contract (chain orchestrator-tier-
// escalation-contract T2; struct-backed, types derive from the param structs
// in escalation.go) ──

var escalationThresholdListDoc = actionspec.ActionDoc{
	Purpose: "Read the EFFECTIVE per-trigger escalation threshold config for a project: the global-default rows (project_id='') overlaid by any project-specific override for the same trigger_kind. With no project_id, returns the global defaults alone. See docs/ORCHESTRATOR_ESCALATION.md §6.",
	Params: []actionspec.DocParam{
		{Name: "project_id", Required: false, Description: "Project whose overrides to overlay on the global defaults. Optional — omit (or '') to read the global-default config alone."},
	},
}

var escalationThresholdSetDoc = actionspec.ActionDoc{
	Purpose: "Upsert one (project_id, trigger_kind) escalation threshold row. Omitted enabled / de_escalation_turns preserve the existing row's values (read-then-upsert); a brand-new row defaults to enabled=true, de_escalation_turns=2. See docs/ORCHESTRATOR_ESCALATION.md §6.",
	Params: []actionspec.DocParam{
		{Name: "project_id", Required: false, Description: "Project the row belongs to. Optional — omit (or '') to write a GLOBAL-DEFAULT row applied to every project lacking an override."},
		{Name: "trigger_kind", Required: true, Description: "One of the 5 closed trigger kinds: retry_exhaustion, low_confidence, repeated_tool_error, parse_failure, explicit_handoff. Required; an unknown kind is rejected (also enforced by the DB CHECK constraint)."},
		{Name: "threshold_value", Required: true, Description: "The trigger's threshold (a JSON number). Semantics per trigger: a count for retry_exhaustion/repeated_tool_error/parse_failure/explicit_handoff; a confidence floor in [0,1] for low_confidence. Required."},
		{Name: "enabled", Required: false, Description: "Whether the trigger is active. A disabled trigger never fires regardless of signals. Optional — preserves the existing value (or defaults true for a new row)."},
		{Name: "de_escalation_turns", Required: false, Description: "Hysteresis K: the router stays escalated until K consecutive clean turns elapse before returning to the cheap tier. Must be >= 1. Optional — preserves the existing value (or defaults 2 for a new row)."},
	},
	EnvelopeRequirements: standardRationaleEnvelope,
}

var escalationProposeDoc = actionspec.ActionDoc{
	Purpose: "Emit one EscalationProposed event through the write-side ledger — the orchestrator-tier escalation contract's observable escalation/de-escalation signal. The reason is recorded as the event rationale. Entity is the orchestrator_session (slug=session_id), project-scoped when project_id is supplied. See docs/ORCHESTRATOR_ESCALATION.md §3.",
	Params: []actionspec.DocParam{
		{Name: "trigger", Required: true, Description: "Which of the 5 closed trigger kinds fired (retry_exhaustion / low_confidence / repeated_tool_error / parse_failure / explicit_handoff). Required."},
		{Name: "from_model", Required: true, Description: "Identifier of the model proposing the handoff — the cheap orchestrator model on an escalate edge (e.g. deepseek-v4-pro), the strong model on a de-escalation edge. Required."},
		{Name: "to_model", Required: true, Description: "Identifier of the model the next turn is proposed to run on — the strong model on an escalate edge (e.g. claude-opus-4-7), the cheap model on a de-escalation edge. Required."},
		{Name: "session_id", Required: true, Description: "The orchestrator session the proposal belongs to (also the event entity slug). Required."},
		{Name: "turn_index", Required: false, Description: "Which turn produced the proposal (0-based). Optional; defaults to 0."},
		{Name: "state_before", Required: true, Description: "Router state at proposal time: cheap, escalated, or de_escalated. Required."},
		{Name: "state_after", Required: true, Description: "Router state after the recorded transition: cheap, escalated, or de_escalated. An escalate edge is cheap->escalated; a de-escalation edge is escalated->de_escalated. Required."},
		{Name: "trigger_detail", Required: false, Description: "Free-form evidence the detector captured (retries used, observed confidence, error kind). Optional; human-readable, not parsed."},
		{Name: "fired_threshold", Required: false, Description: "Snapshot of the threshold_value (a JSON number) that fired, recorded on the event's threshold_value field so a reader sees the config at decision time. Named distinctly from threshold_set's required threshold_value param. Optional; omit on de-escalation edges."},
		{Name: "project_id", Required: false, Description: "Project to scope the event entity to. Optional — omit for a cross-cutting (project-agnostic) escalation event, as a harness-agnostic library would."},
		{Name: "reason", Required: false, Description: "Why the contract proposed this transition — recorded as the event's envelope rationale so the 'why' lands even for the system-actor HTTP path. Optional but recommended."},
	},
	EnvelopeRequirements: standardRationaleEnvelope,
}

// adminActionRegistry is the ordered, co-located descriptor registry — the
// single source of the admin surface's action docs. AdminActionSpecs() derives
// the catalog the corpus generator + admin.action_describe consume. The T1
// characterization net (internal/actiondocs/surface_contract_net_test.go) is the
// byte-parity oracle. Every params-bearing action sets ParamStruct so its types
// derive (vault_search_metrics, action_describe, project_register, host_register,
// host_list, host_remove, remote_exec); the genuinely param-less actions
// (health/version/schema_*/project_list/vault_orphan_list/vault_integrity_sweep/
// apply_recipe/step_probe) leave ParamStruct == nil. Order mirrors
// admin.BuildTable; for admin (describe-only consumer) order is cosmetic, but kept
// aligned with the handler wiring.
var adminActionRegistry = []actionspec.ActionEntry{
	// Health / version / schema
	{Name: "health", Doc: healthDoc, ParamStruct: nil},
	{Name: "server_health", Doc: serverHealthDoc, ParamStruct: nil},
	{Name: "server_version", Doc: serverVersionDoc, ParamStruct: nil},
	{Name: "schema_version", Doc: schemaVersionDoc, ParamStruct: nil},
	{Name: "schema_reload", Doc: schemaReloadDoc, ParamStruct: nil},

	// Project / host CRUD (struct-backed after T4's anon-struct hoist, bug 943)
	{Name: "project_register", Doc: projectRegisterDoc, ParamStruct: reflect.TypeOf(projectRegisterParams{})},
	{Name: "project_list", Doc: projectListDoc, ParamStruct: nil},
	{Name: "host_register", Doc: hostRegisterDoc, ParamStruct: reflect.TypeOf(hostRegisterParams{})},
	{Name: "host_list", Doc: hostListDoc, ParamStruct: reflect.TypeOf(hostListParams{})},
	{Name: "host_remove", Doc: hostRemoveDoc, ParamStruct: reflect.TypeOf(hostRemoveParams{})},

	// Vault telemetry + integrity
	{Name: "vault_search_metrics", Doc: vaultSearchMetricsDoc, ParamStruct: reflect.TypeOf(vaultSearchMetricsParams{})},
	{Name: "vault_orphan_list", Doc: vaultOrphanListDoc, ParamStruct: nil},
	{Name: "vault_integrity_sweep", Doc: vaultIntegritySweepDoc, ParamStruct: nil},

	// action_describe (admin owns this surface) + remote execution + stubs
	{Name: "action_describe", Doc: actionDescribeDoc, ParamStruct: reflect.TypeOf(ActionDescribeParams{})},
	{Name: "remote_exec", Doc: remoteExecDoc, ParamStruct: reflect.TypeOf(remoteExecParams{})},
	{Name: "apply_recipe", Doc: applyRecipeDoc, ParamStruct: nil},
	{Name: "step_probe", Doc: stepProbeDoc, ParamStruct: nil},

	// Orchestrator-tier escalation contract (chain orchestrator-tier-
	// escalation-contract T2)
	{Name: "escalation_threshold_list", Doc: escalationThresholdListDoc, ParamStruct: reflect.TypeOf(escalationThresholdListParams{})},
	{Name: "escalation_threshold_set", Doc: escalationThresholdSetDoc, ParamStruct: reflect.TypeOf(escalationThresholdSetParams{})},
	{Name: "escalation_propose", Doc: escalationProposeDoc, ParamStruct: reflect.TypeOf(escalationProposeParams{})},
}

// AdminActionSpecs returns the admin surface's full action catalog, derived from
// the co-located descriptor registry. Every params-bearing action derives its
// param types from its handler struct (vault_search_metrics, action_describe,
// project_register, host_register, host_list, host_remove, remote_exec); the rest
// document no params. This is what the corpus generator projects into
// corpus/admin/*.toml and what admin.action_describe(admin, X) serves once the
// corpus is generated.
func AdminActionSpecs() []actionspec.ActionSpec {
	return actionspec.DeriveSpecs(adminActionRegistry)
}
