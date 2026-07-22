package knowledge

// action_doc.go is the descriptor-registry seam for the knowledge surface's
// action docs (chain migrate-knowledge-action-docs-to-derive-contract — the
// per-surface instantiation of the contract established on work). It is the
// single source of the knowledge surface's action docs: each param's TYPE is
// DERIVED from the handler's typed param struct where one exists, and only the
// irreducible semantics (purpose, param name-list/order/required/description,
// errors, notes, envelope-requirements, examples) are authored in a co-located
// Go descriptor.
//
// Knowledge is a MIXED surface. The curation_* handlers + the
// parse_context/resolve_references handlers json.Unmarshal into typed param
// structs, so those derive their types (ParamStruct set, authored Type left
// empty — gate-enforced by TestKnowledgeRegistryDerivedParamsHaveEmptyAuthoredType).
// The remaining handlers (vault_* / kiwix_* / knowledge_search /
// knowledge_report_miss / library_*) bind params by map-indexing via the
// mcpparam helpers (mcpparam.String/Int64), with no param struct to reflect —
// the forge-family pattern from docs/ACTION_DOC_CONTRACT.md. Those author their
// param Types (ParamStruct == nil).
//
// parse_context / resolve_references live in package refresolve, which imports
// knowledge — so their param struct cannot be referenced from here directly
// (that edge cycles). It lives in the dependency-light leaf package
// internal/refresolve/refparams, imported below, exactly as the work registry
// reflects the arc-close family via internal/arcreview/arcparams.
//
// The generated corpus + admin.action_describe(knowledge, X) derive from this
// registry via KnowledgeActionSpecs(); byte-parity is pinned by the T1
// characterization net (internal/actiondocs/surface_contract_net_test.go).
// See docs/ACTION_DOC_CONTRACT.md.

import (
	"reflect"

	"toolkit/internal/actionspec"
	"toolkit/internal/refresolve/refparams"
)

// rationaleEnv returns the standard envelope-level rationale requirement shared
// by the mutating library actions the dispatcher gates with requires_rationale=true
// (action-manifests/dispatch-policy.toml). Mirrors work.rationaleEnv — the text
// is the shared dispatcher-gate contract, authored identically per surface.
func rationaleEnv() []actionspec.ActionEnvelopeReq {
	return []actionspec.ActionEnvelopeReq{{
		Field:               "rationale",
		Required:            true,
		Reason:              "Dispatcher policy gate (action-manifests/dispatch-policy.toml). Lives at the call envelope level (next to action/params/project), NOT inside params. Rejected on empty / whitespace / boilerplate / <6-char rationales with error=rationale_required.",
		AppliesToActorKinds: []string{"agent"},
	}}
}

// ── Vault (map-bound: mcpparam.String/Int64; ParamStruct == nil) ──

var vaultSearchDoc = actionspec.ActionDoc{
	Purpose: "Rank notes from ~/.claude/vault/ via local Qwen2.5-32B over the full path list. Call this when picking up a task in a domain you've worked in before, BEFORE assuming you remember the prior decisions; the agent's vault is the canonical store of cross-session decisions/learnings/reference.",
	Params: []actionspec.DocParam{
		{Name: "query", Required: true, Description: "Free-text search query.", Type: "string"},
		{Name: "top_k", Required: false, Description: "Number of top results to return. Defaults to 5.", Type: "int64"},
	},
}

var vaultReadDoc = actionspec.ActionDoc{
	Purpose: "Return the note content + parsed frontmatter for one vault note. The path comes from vault_search results.",
	Params: []actionspec.DocParam{
		{Name: "path", Required: true, Description: "Vault note path, as returned by vault_search.", Type: "string"},
	},
}

var memoryReadDoc = actionspec.ActionDoc{
	Purpose: "Return the materialized memory digest for a project — the owned memory-read path. Aggregates every user-kind entry (fanned out to all projects) plus the feedback/project/reference entries scoped to the project, as a bullet digest (memory_markdown) plus structured entries. The read side of the memory organ for a session-start context injection; pairs with the record action that writes entries to the vault.",
	Params: []actionspec.DocParam{
		{Name: "project", Required: true, Description: "Project slug to scope project/feedback/reference entries to; user-kind entries are always included.", Type: "string"},
	},
}

var recordQueryInteractionDoc = actionspec.ActionDoc{
	Purpose: "Record one click-signal (a query_interactions row) against the grounding_events row of a prior search call, identified by that call's span_id. The owned write path for a client that detects click signals from its own transcript (the RAG implicit-feedback loop) — the grounding event is resolved server-side from span_id, so the client only needs the span id off its tool result plus the clicked hit's source_ref. A span with no grounding_event is a soft no-op.",
	Params: []actionspec.DocParam{
		{Name: "span_id", Required: true, Description: "The search call's substrate span id (off the tool result), used to resolve its grounding_event.", Type: "string"},
		{Name: "source_ref", Required: true, Description: "The clicked/used hit's source ref (e.g. a vault note path).", Type: "string"},
		{Name: "click_kind", Required: true, Description: "One of: followed | cited | mentioned | resolved-from.", Type: "string"},
		{Name: "session_id", Required: true, Description: "The session the interaction occurred in.", Type: "string"},
		{Name: "position", Required: false, Description: "1-based rank of the clicked hit in the result list.", Type: "integer"},
	},
}

// ── Kiwix (map-bound; ParamStruct == nil) ──

var kiwixSearchDoc = actionspec.ActionDoc{
	Purpose: "Query the local Kiwix instance for matching pages in a chosen ZIM archive — 371 GB of offline DevDocs (Rust, Python, JS/TS, Postgres, Redis, SQLite, Node, MDN, etc.), the Rust Book, StackExchange snapshots, and Wikipedia. Reach for this BEFORE web search or guessing a function signature: authoritative, offline, faster.",
	Params: []actionspec.DocParam{
		{Name: "zim_id", Required: true, Description: "The versioned ZIM identifier (e.g. devdocs_en_rust_2026-04). Call kiwix_list_books to enumerate available ZIMs when you don't know which to query.", Type: "string"},
		{Name: "pattern", Required: true, Description: "Search pattern.", Type: "string"},
	},
}

var kiwixFetchDoc = actionspec.ActionDoc{
	Purpose: "Fetch a specific page from a Kiwix ZIM archive by path. Companion to kiwix_search — search finds the path, fetch returns the body.",
	Params: []actionspec.DocParam{
		{Name: "zim_id", Required: true, Description: "The versioned ZIM identifier.", Type: "string"},
		{Name: "path", Required: true, Description: "The page path within the ZIM.", Type: "string"},
	},
}

var kiwixListBooksDoc = actionspec.ActionDoc{
	Purpose: "Enumerate the available ZIM archives on the local Kiwix instance. Use this when you don't know which zim_id to query.",
}

// ── Unified knowledge index (map-bound; ParamStruct == nil) ──

var knowledgeSearchDoc = actionspec.ActionDoc{
	Purpose: "Unified FTS5+Qwen retrieval over all indexed knowledge sources (vault, kiwix, tasks, chains, library, bugs); returns ranked hits with id, source_type, source_ref, question, invoke_when, quality_score. Reach for this when picking up an unfamiliar problem area BEFORE web search or guessing — the index points to the most likely existing answer across every surface this server knows about.",
	Params: []actionspec.DocParam{
		{Name: "query", Required: true, Description: "Free-text search query.", Type: "string"},
		{Name: "top_k", Required: false, Description: "Number of top results to return. Defaults to 5.", Type: "int64"},
	},
}

var knowledgeReportMissDoc = actionspec.ActionDoc{
	Purpose: "When a followed pointer turns out to be wrong or stale, call this to increment negative_feedback_count and optionally set staleness_hint. Takes pointer_id from knowledge_search results.",
	Params: []actionspec.DocParam{
		{Name: "pointer_id", Required: true, Description: "The pointer_id from a knowledge_search result.", Type: "int64"},
		{Name: "staleness_reason", Required: false, Description: "Optional reason describing why the pointer was stale (used as staleness_hint).", Type: "string"},
	},
}

// ── Library catalogue (map-bound; ParamStruct == nil) ──

var libraryAddDoc = actionspec.ActionDoc{
	Purpose:              "Add a new entry to the project's library catalogue. Mutates the library state.",
	EnvelopeRequirements: rationaleEnv(),
}

var libraryUpdateDoc = actionspec.ActionDoc{
	Purpose: "Update an existing library entry's fields. Mutates the library state.",
	Params: []actionspec.DocParam{
		{Name: "slug", Required: true, Description: "The library entry's slug.", Type: "string"},
	},
	EnvelopeRequirements: rationaleEnv(),
}

var libraryGetDoc = actionspec.ActionDoc{
	Purpose: "Return one library entry by slug. The single-row read for the project's knowledge catalogue.",
	Params: []actionspec.DocParam{
		{Name: "slug", Required: true, Description: "Library entry slug.", Type: "string"},
	},
}

var libraryFindDoc = actionspec.ActionDoc{
	Purpose: "Free-text find across the project's library entries. Entry point for catalogue lookup when you don't know the slug.",
	Params: []actionspec.DocParam{
		{Name: "query", Required: true, Description: "Free-text search query.", Type: "string"},
	},
}

var libraryRetireDoc = actionspec.ActionDoc{
	Purpose: "Mark a library entry retired — removes it from library_list_active enumerations while preserving the row for history. Inverse of library_add for stale entries.",
	Params: []actionspec.DocParam{
		{Name: "slug", Required: true, Description: "The library entry's slug.", Type: "string"},
	},
	EnvelopeRequirements: rationaleEnv(),
}

var libraryCrossReferenceDoc = actionspec.ActionDoc{
	Purpose: "Create or list cross-reference edges between library entries — the relational view of the catalogue.",
}

var libraryListActiveDoc = actionspec.ActionDoc{
	Purpose: "List every active library entry in the project's catalogue. Retired entries are excluded; use a different enumeration if you want the full set.",
}

var libraryListDeweyDoc = actionspec.ActionDoc{
	Purpose: "Enumerate library entries grouped by Dewey code — the structural classification view of the project's catalogue.",
}

var libraryListSectionsDoc = actionspec.ActionDoc{
	Purpose: "Enumerate library entries grouped by section — the section taxonomy view of the project's catalogue.",
}

// ── Curation lifecycle (struct-backed: json.Unmarshal into curation*Params;
// param Types DERIVE from the struct — authored Type left empty) ──

var curationListDoc = actionspec.ActionDoc{
	Purpose: "List pending curation_candidates for the project (compact projection).",
	Params: []actionspec.DocParam{
		{Name: "origin", Required: false, Description: "Filter by candidate origin."},
		{Name: "scored", Required: false, Description: "Filter by whether the candidate has been scored."},
		{Name: "limit", Required: false, Description: "Result limit. Defaults to 50."},
	},
}

var curationReadDoc = actionspec.ActionDoc{
	Purpose: "Return the full candidate body for one curation_candidate.",
	Params: []actionspec.DocParam{
		{Name: "id", Required: true, Description: "The candidate id."},
	},
}

var curationPromoteDoc = actionspec.ActionDoc{
	Purpose: "Promote a pending candidate to a knowledge_pointer. Emits a CurationCandidatePromoted substrate event visible in AuditLedger. Optional overrides refine metadata at promotion time.",
	Params: []actionspec.DocParam{
		{Name: "id", Required: true, Description: "The candidate id."},
		{Name: "override_question", Required: false, Description: "Override the candidate's question at promotion time."},
		{Name: "override_invoke_when", Required: false, Description: "Override the candidate's invoke_when at promotion time."},
		{Name: "override_description", Required: false, Description: "Override the candidate's description at promotion time."},
	},
}

var curationRejectDoc = actionspec.ActionDoc{
	Purpose: "Mark a candidate rejected. Emits a CurationCandidateRejected substrate event visible in AuditLedger. Reason is REQUIRED and non-empty.",
	Params: []actionspec.DocParam{
		{Name: "id", Required: true, Description: "The candidate id."},
		{Name: "reason", Required: true, Description: "Why the candidate is being rejected. REQUIRED and non-empty."},
	},
	Errors: []actionspec.ActionError{
		{Condition: "missing or empty reason", Message: "curation_reject requires a non-empty reason; rejection without rationale is not accepted."},
	},
}

var curationBulkActionDoc = actionspec.ActionDoc{
	Purpose: "Apply promote/reject to all matching candidates. Filter must include at least one of {origin, unscored_only} to prevent whole-project nukes. Emits CurationCandidatePromoted / CurationCandidateRejected substrate events visible in AuditLedger.",
	Params: []actionspec.DocParam{
		{Name: "filter", Required: true, Description: "Match criteria for which candidates to act on. Must include at least one of {origin, unscored_only}."},
		{Name: "action", Required: true, Description: "promote or reject."},
		{Name: "reason", Required: false, Description: "Required when action=reject; supplies the rejection reason for every affected candidate."},
		{Name: "dry_run", Required: false, Description: "When true, return what would be done without mutating."},
		{Name: "limit", Required: false, Description: "Cap the number of candidates touched. Defaults to 100."},
	},
	Errors: []actionspec.ActionError{
		{Condition: "empty filter", Message: "curation_bulk_action requires a filter containing at least one of {origin, unscored_only} — prevents accidental whole-project nukes."},
	},
}

// ── Reference resolution (struct-backed: parse_context + resolve_references
// share refparams.ParseContextParams; param Types DERIVE) ──

var parseContextDoc = actionspec.ActionDoc{
	Purpose: "Canonical orienting call. Resolve every reference-shape token in a user message AND surface domain-conditional context (skills, memory entries, vault notes, kiwix hits, disciplines triggered by message shape) in one unified envelope. parse_context is the agent's first call on every user prompt (modulo the discernment skill's skip rules: short conversational acknowledgments, slash-commands, pure continuations). Replaces the prior pattern of per-shape direct calls (chain_find, task_search, vault_search, kiwix_search, the available-skills list, …) — one round-trip surfaces everything potentially relevant.",
	Params: []actionspec.DocParam{
		{Name: "message_text", Required: true, Description: "The user message (or any agent-facing text) to scan for unresolved references and domain-conditional context. Empty string returns an empty References list and zero resolver calls."},
		{Name: "session_id", Required: false, Description: "Session identifier for the filter-cache. Defaults to the span's trace ID when omitted (the substrate stamps it from the request context). Pass explicitly when calling outside an MCP session (smoke harness, scripts)."},
		{Name: "top_k_per_shape", Required: false, Description: "Cap on candidates each resolver returns after dispatcher post-processing. Defaults to 5."},
		{Name: "include_no_hits", Required: false, Description: "When true, references that resolved to no_hit stay in the References list (with recommended_action = acknowledge_no_hit_and_ask) instead of collapsing into the NoHitTokens slice. Default false."},
		{Name: "total_budget_ms", Required: false, Description: "Dispatcher wall-clock cap for the whole call, in milliseconds. Defaults to 2000 (2s) today; budget rises to 4000 (4s) when the broader resolver set lands. When the budget trips, truncated_by_budget=true is set on the response envelope; resolvers not reached surface ErrTotalBudgetExceeded as partial_failures."},
		{Name: "cache_policy_override", Required: false, Description: "Force a cache policy for this call. Accepted values: 'fresh' (skip cache lookup, write fresh; useful when callers know they want bypass), 'cache_only' (return cached hits only, no fresh resolution). Default unset — substrate uses per-resolver policy from the design's §3.3 matrix."},
		{Name: "inline_skill_bodies", Required: false, Description: "Chain 602. Per-request override of the TOOLKIT_PARSE_CONTEXT_INLINE_BODIES env var that gates skill-body inlining. When true (and a skill_trigger or discipline_skill ref resolves at single_exact), the envelope carries body_inlined / body_truncated / body_summary / body_bytes on the ref. Default unset → use env-var setting (default OFF during rollout). Pass false to force-disable for one call."},
		{Name: "inline_budget_bytes", Required: false, Description: "Chain 602. Overrides the default 20 KB envelope budget for inlined skill bodies. Zero or unset uses the default. The per-skill cap (8 KB) is not caller-tunable; raising the envelope budget is enough."},
	},
	Errors: []actionspec.ActionError{
		{Condition: "registry not configured", Message: "Returned when the server started without the refresolve registry wired (degraded mode). Returns {error: 'registry not configured'} on the response envelope instead of a Go error."},
	},
	Examples: []actionspec.ActionExample{
		{Description: "Basic call — orient on a shape-heavy user message.", Call: `{"action":"parse_context","params":{"message_text":"please finish T8 and T9 in reference-resolution-substrate; T7 is blocked"}}`},
		{Description: "Tighter budget when calling parse_context on a latency-sensitive path.", Call: `{"action":"parse_context","params":{"message_text":"look up budget-trip","total_budget_ms":500}}`},
		{Description: "Force a fresh resolve, bypassing the session cache.", Call: `{"action":"parse_context","params":{"message_text":"recheck T8 status","cache_policy_override":"fresh"}}`},
	},
	Notes: "Shape coverage:\n\n  • chain_slug / task_slug / bug_slug → DB row in the corresponding table\n  • path → filesystem under RepoRoot (file or directory)\n  • skill_name → skills/<name>/ directories\n  • tool_name → action-manifests/*.toml basenames\n  • forge_schema → blueprints/forge-schemas/*.toml basenames\n  • project_name → KnownProjects catalog\n  • library_entry → library_index.slug or title\n  • domain_term → vault_search via Qwen rubric classifier\n  • external_technical → kiwix_search + (when wired) kiwix_bridge resolver\n  • friction_shape → bug-filing suggestion (supersedes the friction-filing-reminder Stop hook for in-message detection)\n  • skill_trigger → skills/_manifest.toml trigger keywords (T5 follow-on)\n  • memory_entry → MEMORY.md index entries (T5 follow-on)\n  • vault_candidate → vault_search bridge with domain-condition gate (T5 follow-on)\n  • kiwix_bridge → kiwix_search bridge for external-technical terms (T5 follow-on)\n  • discipline_skill → discipline-skill bodies whose trigger condition fires on the message (T5 follow-on)\n\nConfidence tier semantics (see docs/REFERENCE_RESOLUTION.md §4):\n\n  • single_exact → recommended_action=use_directly; PresentedAs is the canonical attribution string.\n  • fuzzy_multi → recommended_action=ask_user_to_disambiguate; PresentedAs enumerates the top candidates.\n  • weak_domain → recommended_action=mention_as_possibly_relevant; PresentedAs hedges with the top hit and score.\n  • no_hit → recommended_action=acknowledge_no_hit_and_ask; PresentedAs says the token didn't resolve.\n\nEach resolved reference carries a grounding_event_id. Every call emits one grounding_events row per detected reference with query_source='reference_resolution'.\n\nFilter cache: per-session, keyed by (session_id, token, shape) with per-resolver policies (see docs/PARSE_CONTEXT.md §3.3 + §4). The cache_hits and cache_misses fields on the response envelope distinguish cached re-calls from fresh resolution. T5 follow-on phase wires the cache; the shell exposes the envelope fields with zero values until then.\n\n`resolve_references` is a soft alias of `parse_context` — both dispatch into the same handler core, so existing callers continue to work without modification. New callers prefer parse_context.\n\nHistory: shipped as a back-compat-preserving alias by chain reference-resolution-migration T5 (Phase 1). Follow-on phases: cache layer, then per-resolver shape additions (skill_trigger, memory_entry, vault_candidate, kiwix_bridge, discipline_skill).\n",
}

var resolveReferencesDoc = actionspec.ActionDoc{
	Purpose: "Soft alias of parse_context (the canonical orienting call). Resolves every reference-shape token in a user message in one round-trip — chain/task/bug slugs, paths, skill names, project names, tool names, forge schemas, library entries, domain terms, external-technical terms, friction shapes. Returns per-reference shape + confidence tier + recommended_action + PresentedAs. New callers should prefer parse_context (which adds skills/memory/vault/kiwix/discipline coverage and a filter cache); existing callers keep working unchanged.",
	Params: []actionspec.DocParam{
		{Name: "message_text", Required: true, Description: "The user message (or any agent-facing text) to scan for unresolved references. Empty string returns an empty References list and zero resolver calls."},
		{Name: "top_k_per_shape", Required: false, Description: "Cap on candidates each resolver returns after dispatcher post-processing. Defaults to 5."},
		{Name: "include_no_hits", Required: false, Description: "When true, references that resolved to no_hit stay in the References list (with recommended_action = acknowledge_no_hit_and_ask) instead of collapsing into the NoHitTokens slice. Default false."},
		{Name: "total_budget_ms", Required: false, Description: "Dispatcher wall-clock cap for the whole call, in milliseconds. Defaults to 2000 (2s). When the budget trips, truncated_by_budget=true is set on the response envelope; resolvers not reached surface ErrTotalBudgetExceeded as partial_failures."},
	},
	Errors: []actionspec.ActionError{
		{Condition: "registry not configured", Message: "Returned when the server started without the refresolve registry wired (degraded mode). Returns {error: 'registry not configured'} on the response envelope instead of a Go error."},
	},
	Examples: []actionspec.ActionExample{
		{Description: "Basic call — detect every shape in a mixed message.", Call: `{"action":"resolve_references","params":{"message_text":"Working on reference-resolution-substrate and considering the Tripolar Invariant alongside docs/REFERENCE_RESOLUTION.md"}}`},
		{Description: "Tighter budget for latency-sensitive contexts. Default budget is 2000ms — pass a smaller value when you want to fail fast on slow resolvers (vault rerank, kiwix index).", Call: `{"action":"resolve_references","params":{"message_text":"look up budget-trip","total_budget_ms":500}}`},
		{Description: "Keep no_hit references inline so the agent can acknowledge each one explicitly — useful when authoring a response that names everything the user mentioned.", Call: `{"action":"resolve_references","params":{"message_text":"compare the alpha-spec and beta-spec proposals","include_no_hits":true}}`},
	},
	Notes: "Shape coverage and which catalog each shape resolves against:\n\n  • chain_slug / task_slug / bug_slug → DB row in the corresponding table\n  • path → filesystem under RepoRoot (file or directory)\n  • skill_name → skills/*.toml basenames\n  • tool_name → action-manifests/*.toml basenames\n  • forge_schema → blueprints/forge-schemas/*.toml basenames\n  • project_name → KnownProjects catalog\n  • library_entry → library_index.slug (case-sensitive) OR title (case-insensitive)\n  • domain_term → vault_search via Qwen rubric classifier (when configured)\n  • external_technical → kiwix_search\n  • friction_shape → a synthetic 'binding' suggesting a bug-filing call; supersedes the friction-filing-reminder Stop hook for in-message detection\n\nConfidence tier semantics (see docs/REFERENCE_RESOLUTION.md §4):\n\n  • single_exact → recommended_action=use_directly; PresentedAs is the canonical attribution string.\n  • fuzzy_multi → recommended_action=ask_user_to_disambiguate; PresentedAs enumerates the top candidates.\n  • weak_domain → recommended_action=mention_as_possibly_relevant; PresentedAs hedges with the top hit and score.\n  • no_hit → recommended_action=acknowledge_no_hit_and_ask; PresentedAs says the token didn't resolve.\n\nEach resolved reference also carries a grounding_event_id (T5). Every call emits one grounding_events row per detected reference with query_source='reference_resolution' so downstream telemetry (query_interactions, retrieval-success scoring) attributes the work to the resolver layer rather than the underlying per-shape tools.\n\nThe v1 reference-resolution skill (action-manifests/resolve-references.toml + .md) teaches the trigger discipline and presentation conventions; this MCP action is the mechanism the skill delegates to. Call this action directly when authoring an agent loop that needs reference binding without invoking the skill.\n\nDefault total_budget_ms is 2000. Pre-bug-1410, truncated_by_budget only fired on caller-supplied budgets; the handler now applies defaults locally so the flag fires on the default 2s ceiling too.\n\nHistory: shipped by chain reference-resolution-substrate T4 (commit 8a88a0e); T5 added grounding-events emission; T6 added the friction_shape detector + supersession of the Stop-hook reminder. This action-docs chunk was backfilled when bug `resolve-references-missing-from-action-docs-corpus` flagged that the action was invisible to admin.action_describe despite being live in the dispatch table. Reference-resolution-migration T5 demoted it to a soft alias of parse_context; both dispatch into the same handler core.\n",
}

// knowledgeActionRegistry is the ordered, co-located descriptor registry — the
// single source of the knowledge surface's action docs. KnowledgeActionSpecs()
// derives the catalog the corpus generator + admin.action_describe consume. The
// T1 characterization net (internal/actiondocs/surface_contract_net_test.go)
// is the byte-parity oracle. ParamStruct is set for the struct-backed actions
// (their param Types derive) and nil for the map-bound actions (Types authored).
var knowledgeActionRegistry = []actionspec.ActionEntry{
	// ── Vault ──
	{Name: "vault_search", Doc: vaultSearchDoc, ParamStruct: nil},
	{Name: "vault_read", Doc: vaultReadDoc, ParamStruct: nil},
	{Name: "memory_read", Doc: memoryReadDoc, ParamStruct: nil},
	{Name: "record_query_interaction", Doc: recordQueryInteractionDoc, ParamStruct: nil},

	// ── Kiwix ──
	{Name: "kiwix_search", Doc: kiwixSearchDoc, ParamStruct: nil},
	{Name: "kiwix_fetch", Doc: kiwixFetchDoc, ParamStruct: nil},
	{Name: "kiwix_list_books", Doc: kiwixListBooksDoc, ParamStruct: nil},

	// ── Unified knowledge index ──
	{Name: "knowledge_search", Doc: knowledgeSearchDoc, ParamStruct: nil},
	{Name: "knowledge_report_miss", Doc: knowledgeReportMissDoc, ParamStruct: nil},

	// ── Library catalogue ──
	{Name: "library_add", Doc: libraryAddDoc, ParamStruct: nil},
	{Name: "library_update", Doc: libraryUpdateDoc, ParamStruct: nil},
	{Name: "library_get", Doc: libraryGetDoc, ParamStruct: nil},
	{Name: "library_find", Doc: libraryFindDoc, ParamStruct: nil},
	{Name: "library_retire", Doc: libraryRetireDoc, ParamStruct: nil},
	{Name: "library_cross_reference", Doc: libraryCrossReferenceDoc, ParamStruct: nil},
	{Name: "library_list_active", Doc: libraryListActiveDoc, ParamStruct: nil},
	{Name: "library_list_dewey", Doc: libraryListDeweyDoc, ParamStruct: nil},
	{Name: "library_list_sections", Doc: libraryListSectionsDoc, ParamStruct: nil},

	// ── Curation lifecycle (struct-backed) ──
	{Name: "curation_list", Doc: curationListDoc, ParamStruct: reflect.TypeOf(curationListParams{})},
	{Name: "curation_read", Doc: curationReadDoc, ParamStruct: reflect.TypeOf(curationReadParams{})},
	{Name: "curation_promote", Doc: curationPromoteDoc, ParamStruct: reflect.TypeOf(curationPromoteParams{})},
	{Name: "curation_reject", Doc: curationRejectDoc, ParamStruct: reflect.TypeOf(curationRejectParams{})},
	{Name: "curation_bulk_action", Doc: curationBulkActionDoc, ParamStruct: reflect.TypeOf(curationBulkActionParams{})},

	// ── Reference resolution (struct-backed via refparams) ──
	{Name: "parse_context", Doc: parseContextDoc, ParamStruct: reflect.TypeOf(refparams.ParseContextParams{})},
	{Name: "resolve_references", Doc: resolveReferencesDoc, ParamStruct: reflect.TypeOf(refparams.ParseContextParams{})},
}

// KnowledgeActionSpecs returns the knowledge surface's full action catalog,
// derived from the co-located descriptor registry. Each param's type is derived
// from its handler struct (struct-backed actions) or authored (map-bound). This
// is what the corpus generator projects into corpus/knowledge/*.toml and what
// admin.action_describe(knowledge, X) serves once the corpus is generated.
func KnowledgeActionSpecs() []actionspec.ActionSpec {
	return actionspec.DeriveSpecs(knowledgeActionRegistry)
}
