// Package work's actions_discovery.go closes bug 1335: the work
// surface exposes ~25 actions but only forge / forge_edit / forge_delete
// have parameter discovery (via forge_schemas). The other actions — task_block,
// task_blockers, task_search, roadmap_set, bug_resolve, etc. — had no
// machine-readable parameter list, forcing cold callers to guess from
// observed responses or trial-and-error against error messages.
//
// HandleWorkActions returns a stable spec describing every action's required
// and optional params with short examples, DERIVED from the co-located
// descriptor registry (actionRegistry + the ActionDoc descriptors in
// action_doc.go): adding a new action means adding its descriptor + one
// actionRegistry entry, not editing a monolithic catalog. The package-test
// `TestWorkActions_CoversEveryRegisteredAction` (see actions_discovery_test.go)
// enforces parity between the registry and the table built in BuildTable so
// additions cannot silently drift.

package work

import (
	"context"
	"encoding/json"
	"reflect"

	"toolkit/internal/actionspec"
	"toolkit/internal/arcreview/arcparams"
)

// The action-doc spec types live in package actionspec now — the surface-
// agnostic source contract, factored out so the knowledge / measure / admin / ml
// migrations reuse one implementation rather than re-declaring them per package.
// Work aliases them so every co-located descriptor and every external
// `work.ActionSpec` reference keeps compiling unchanged; the JSON shapes are
// byte-identical (the contract net is the parity oracle). See
// actionspec/descriptor.go and docs/ACTION_DOC_CONTRACT.md.
type (
	ActionParam       = actionspec.ActionParam
	ActionValueAlias  = actionspec.ActionValueAlias
	ActionError       = actionspec.ActionError
	ActionEnvelopeReq = actionspec.ActionEnvelopeReq
	ActionReturn      = actionspec.ActionReturn
	ActionSpec        = actionspec.ActionSpec
)

// WorkActionsResult is the wire shape for work_actions.
type WorkActionsResult []ActionSpec

// MarshalJSON ensures empty results marshal as `[]` not `null`.
func (w WorkActionsResult) MarshalJSON() ([]byte, error) {
	if w == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]ActionSpec(w))
}

// rationaleEnv returns the standard envelope-level rationale requirement
// shared by the 21 mutating work actions the dispatcher gates with
// requires_rationale=true (action-manifests/dispatch-policy.toml). batch
// and lifecycle_step carry custom multi-grain reasons and build their own
// EnvelopeRequirements inline.
func rationaleEnv() []ActionEnvelopeReq {
	return []ActionEnvelopeReq{{
		Field:               "rationale",
		Required:            true,
		Reason:              "Dispatcher policy gate (action-manifests/dispatch-policy.toml). Lives at the call envelope level (next to action/params/project), NOT inside params. Rejected on empty / whitespace / boilerplate / <6-char rationales with error=rationale_required.",
		AppliesToActorKinds: []string{"agent"},
	}}
}

// ── Action-doc descriptors: forge family, work_actions, arc-close ──────
//
// These live here (not in a per-handler file) because their handlers are
// either central (work_actions) or registered externally on the work table
// (the arc-close family lives in package arcreview; the forge family routes
// through package forge). The forge family + work_actions have no typed param
// struct in package work (ParamStruct == nil), so their param Types are
// AUTHORED here, not derived. The arc-close family DERIVES from the arcparams
// structs (Type left empty).

// forge has no typed param struct (ParamStruct == nil) — every param Type is
// AUTHORED here, not derived.
var forgeDoc = ActionDoc{
	Purpose: "DEPRECATED (chain 311 T7 Stage 5 — prefer `record`): create a new artifact for a registered schema. The forge create surface now routes through the construct umbrella and is mirrored by `record` with a forge-shaped sugar mode — `record({schema_name, slug, fields})` (op defaults to create). forge still works identically and is archived at Stage 6. For per-schema field details + call envelope, use forge_schema(schema_name='<name>').",
	Params: []DocParam{
		{Name: "schema_name", Required: true, Description: "Schema to forge. Alias: kind.", Type: "string"},
		{Name: "kind", Required: false, Description: "Alias of schema_name.", AliasOf: "schema_name", Type: "string"},
		{Name: "slug", Required: true, Description: "Slug for the new artifact. May be auto-derived from `title` for schemas that have a title field.", Type: "string"},
		{Name: "project", Required: false, Description: "Top-level envelope param (not inside fields) scoping the create to a project. Required for project-scoped schemas (chain, task, bug, suggestion) unless the server resolver derives it from CWD or --default-project.", Type: "string"},
		{Name: "fields", Required: false, Description: "Field payload. May also be passed as top-level sugar keys instead of nested fields object.", Type: "object"},
	},
	SeeAlso:              "forge_schema, forge_schemas",
	Notes:                "forge accepts two equivalent param shapes — structured {schema_name, slug, fields: {<name>: <value>, ...}} OR sugar {schema_name, slug, <field-name>: <value>, ...} (matches forge_edit's sugar form). Reserved control words (slug/date/project/commit_sha) are excluded from the sugar pass so they aren't mis-collected as field values.",
	EnvelopeRequirements: rationaleEnv(),
}

// forge_edit has no typed param struct (ParamStruct == nil) — Types AUTHORED.
var forgeEditDoc = ActionDoc{
	Purpose: "DEPRECATED (chain 311 T7 Stage 5 — prefer `record`): update an existing artifact. Mirrored by `record({schema_name, slug, fields, op:\"update\"})`. forge_edit still works identically and is archived at Stage 6. For task schema, chain_slug is required at the top level (slug isn't globally unique). See forge_schema(schema_name='<name>') for the per-schema call_envelopes.",
	Params: []DocParam{
		{Name: "schema_name", Required: true, Description: "Schema to edit.", Type: "string"},
		{Name: "kind", Required: false, Description: "Alias of schema_name.", AliasOf: "schema_name", Type: "string"},
		{Name: "slug", Required: true, Description: "Slug of the artifact to edit.", Type: "string"},
		{Name: "chain_slug", Required: false, Description: "Required for task schema (disambiguates the row).", Type: "string"},
		{Name: "project", Required: false, Description: "Top-level envelope param (not inside fields) scoping the edit to a project. Resolved from CWD or --default-project when omitted.", Type: "string"},
		{Name: "fields", Required: false, Description: "Updated field values. May also be passed as top-level sugar keys.", Type: "object"},
	},
	SeeAlso:              "forge_schema, forge_schemas",
	Notes:                "forge_edit on a task accepts the same {chain_slug, slug, <fields…>} top-level shape as task_edit (the composite key is synthesized from chain_slug + slug); the structured form on tasks needs key: {chain_slug, slug}.",
	EnvelopeRequirements: rationaleEnv(),
}

// forge_delete has no typed param struct (ParamStruct == nil) — Types AUTHORED.
var forgeDeleteDoc = ActionDoc{
	Purpose: "DEPRECATED (chain 311 T7 Stage 5 — prefer `record`): delete an artifact for schemas that support deletion. Mirrored by `record({schema_name, slug, op:\"delete\"})`. forge_delete still works identically and is archived at Stage 6. Same key shape as forge_edit.",
	Params: []DocParam{
		{Name: "schema_name", Required: true, Description: "Schema of the artifact to delete.", Type: "string"},
		{Name: "slug", Required: true, Description: "Slug of the artifact to delete.", Type: "string"},
		{Name: "chain_slug", Required: false, Description: "Required for task schema.", Type: "string"},
	},
	SeeAlso:              "forge_schema, forge_schemas",
	EnvelopeRequirements: rationaleEnv(),
}

var forgeSchemasDoc = ActionDoc{
	Purpose: "List all registered schemas (name, supported_ops, source_file).",
	Params:  []DocParam{},
	SeeAlso: "forge_schema",
}

// forge_schema has no typed param struct (ParamStruct == nil) — Types AUTHORED.
var forgeSchemaDoc = ActionDoc{
	Purpose: "Return a schema's full field list AND per-supported-op call envelopes (which params go at top level vs inside `fields` for create / update / delete).",
	Params: []DocParam{
		{Name: "schema_name", Required: true, Description: "Schema to describe. Alias: name.", Type: "string"},
	},
	Example: `{"schema_name":"task"}`,
}

var workActionsDoc = ActionDoc{
	Purpose: "Return this catalog: every registered work-surface action with its parameter spec + example. Self-referential entry so the catalog is fully discoverable from itself.",
	Params:  []DocParam{},
	Example: `{}`,
	Notes: "Filed via bug action-docs-corpus-t3-spec-gaps-on-prose-less-actions-and-orphans — work_actions was registered in work.BuildTable but not named in workDescription's Actions: list, so chain action-docs-corpus T3 produced no chunk for it. This chunk closes that orphan; admin.action_describe(surface=\"work\", action=\"work_actions\") now resolves.\n\n" +
		"The handler lives in go/internal/work/actions_discovery.go (HandleWorkActions). The catalog itself is the static `actionSpecs` slice in the same file — each entry carries name, description, params, example. Updating the catalog is a manual edit to that slice; the lint to keep it in sync with the dispatch.Table is at go/internal/work/actions_discovery_test.go.",
}

var reviewArcForFilingDoc = ActionDoc{
	Purpose: "Run an arc-close filing review for one session. Reads the transcript snapshot, runs a two-pass Qwen review, returns typed filing decisions partitioned into auto_execute / surface_for_confirm / skip. Debounced per session_id (60s backoff). Fail-open: Qwen-unreachable / parse failure / empty snapshot all return non-error responses with a typed status. See docs/ARC_CLOSE_FILING_REVIEW.md.",
	Params: []DocParam{
		{Name: "session_id", Required: true, Description: "Caller-provided session identifier; the debouncer keys off this."},
		{Name: "transcript_path", Required: true, Description: "Absolute path to the Claude Code JSONL transcript."},
		{Name: "triggers", Required: false, Description: "Trigger slugs the detector matched (e.g. counter_user_turns_5, user_shape_done)."},
		{Name: "fired_at", Required: false, Description: "ISO-8601 timestamp the detector emitted the trigger payload at."},
		{Name: "user_turns_since_review", Required: false, Description: "Counter value at fire time (telemetry)."},
	},
	Example: `{"session_id":"sess-123","transcript_path":"/home/u/.claude/projects/.../session.jsonl","triggers":["user_shape_done"]}`,
	Errors: []ActionError{
		{Condition: "missing session_id or transcript_path", Message: "Returns a non-error response with status='skipped' and a Reason naming the missing field. The action is not a precondition gate; it logs and continues."},
		{Condition: "debouncer suppression", Message: "status='debounced'. last_fire_at carries the prior fire timestamp; Reason names the elapsed seconds. The agent or harness should NOT retry — the suppression is by design."},
		{Condition: "Qwen unreachable / llama-server down", Message: "status='qwen_unreachable'. Reason wraps the dispatch error string. The current discipline (parse_context + agent-internalized skill-body firing) keeps working; this action's failure mode is purely additive."},
	},
	Notes: "Wired onto the work meta-tool because the typed forge actions the review's output dispatches to live there. The action does NOT itself execute the decisions — the caller (Claude Code Stop hook or bridge-harness post_turn hook) reads the partition and dispatches forges in-band. Per design Q5: auto_execute contains forge_bug / forge_vault_note / memory_write at confidence ≥ 0.85; surface_for_confirm contains skill_update at any confidence and other actions at confidence 0.50-0.85; skip contains everything below 0.50 plus all nothing_to_file decisions.",
}

var emitCommitLandedDoc = ActionDoc{
	Purpose: "Emit a CommitLanded event through the daemon's events substrate. Invoked by the post-commit-restart-advisor (via the commit-landed-emit binary) so the emit lands INSIDE the daemon's process — only that path fires the chained fold hook (SubstrateReviewObserver) that triggers the substrate-side review on every commit. Fail-open: invalid params / schema-validator failure / pool error all return status=skipped with a reason. See docs/EVENT_CATALOG.md §Commit-lifecycle.",
	Params: []DocParam{
		{Name: "commit_sha", Required: true, Description: "Full 40-char hex SHA of the commit that just landed."},
		{Name: "branch", Required: false, Description: "Branch name; null when HEAD is detached."},
		{Name: "files_changed_count", Required: false, Description: "Files-changed count parsed from `git show --stat`."},
		{Name: "author", Required: false, Description: "Commit author in 'Name <email>' form."},
		{Name: "subject", Required: false, Description: "First line of the commit message."},
	},
	Example: `{"commit_sha":"abc123...","branch":"main","files_changed_count":3}`,
}

var arcReviewAuditDoc = ActionDoc{
	Purpose: "Read-side audit of ArcCloseFilingReviewed events joined with pending_decisions dispatch state and heuristic user-correction signals from subsequent BugReopened / BugEdited / TaskCancelled / BugResolved / ChainEdited events within a configurable window (default 24h). Feeds chain T9 (threshold + Qwen-prompt tuning) and T10 (ML follow-on corpus exports). Read-only. Heuristic correction join is best-effort; the inline heuristic_correction_note field on the response documents the false-positive / false-negative classes so consumers can triage signals manually before treating any as ground truth.",
	Params: []DocParam{
		{Name: "since", Required: false, Description: "ISO-8601 lower bound on review ts (default: 7 days ago)."},
		{Name: "correction_window_hours", Required: false, Description: "Look-ahead window for the heuristic correction-signal scan (default 24)."},
	},
	Example: `{"since":"2026-05-13T00:00:00Z","correction_window_hours":24}`,
}

var pendingDecisionsClaimDoc = ActionDoc{
	Purpose: "Drain pending arc-close filing decisions for the project. Atomically SELECTs the oldest undispatched rows (LIMIT) and UPDATEs dispatched_at + dispatch_session_id in one tx so concurrent claims serialize via SQLite's writer lock (exactly-once dispatch per row). The Stop hook calls this after the session_registry UPSERT, formats each claimed row into a system-reminder block on stdout, and exits. See docs/ARC_CLOSE_FILING_REVIEW_SUBSTRATE_LISTENER.md §Q3.",
	Params: []DocParam{
		{Name: "session_id", Required: true, Description: "Calling session's id; lands on each claimed row as dispatch_session_id."},
		{Name: "limit", Required: false, Description: "Max rows to claim in one call (default 10)."},
	},
	Example: `{"session_id":"sess-123","limit":10}`,
}

var sweepUnauthoredStagedDoc = ActionDoc{
	Purpose: "Forge the unreviewed-fallback for staged arc-close decisions the in-session agent never authored (chain arc-close-decision-authoring-split T5). Body-heavy decisions (vault-note / memory_write) in the auto-execute band are staged for the agent to author rather than auto-forged with Qwen's draft; if the seat disengages, this captures the retained draft flagged `unreviewed`+`qwen-authored` so capture is never lost. The in-session trigger (reap-on-next-fire) is built into review_arc_for_filing; this is the explicit session-end / skip surface a reaper hook calls. Honors a grace window (TOOLKIT_ARCCLOSE_FALLBACK_GRACE) and suppresses the forge when the agent already authored a matching note.",
	Params: []DocParam{
		{Name: "session_id", Required: true, Description: "Session whose still-staged, past-grace decisions should be swept."},
	},
	Example: `{"session_id":"sess-123"}`,
}

// actionRegistry is the ordered, co-located descriptor registry — the single
// source of the work surface's action docs (action_doc.go). work_actions /
// CallShape / the corpus generator / WorkDescription all derive from it via
// deriveActionSpecs(). Order is load-bearing: work_actions returns the catalog
// in declaration order. The T1 characterization net
// (internal/actiondocs/contract_net_test.go) is the byte-parity oracle. See
// docs/ACTION_DOC_CONTRACT.md.
var actionRegistry = []ActionEntry{
	// ── Chain lifecycle ──
	{Name: "chain_status", Doc: chainStatusDoc, ParamStruct: reflect.TypeOf(chainSlugParams{})},
	{Name: "chain_state", Doc: chainStateDoc, ParamStruct: reflect.TypeOf(chainSlugParams{})},
	{Name: "chain_find", Doc: chainFindDoc, ParamStruct: reflect.TypeOf(chainFindParams{})},
	{Name: "chain_close", Doc: chainCloseDoc, ParamStruct: reflect.TypeOf(chainCloseParams{})},

	// ── Task lifecycle ──
	{Name: "task_read", Doc: taskReadDoc, ParamStruct: reflect.TypeOf(taskReadParams{})},
	{Name: "task_search", Doc: taskSearchDoc, ParamStruct: reflect.TypeOf(taskSearchParams{})},
	{Name: "task_list", Doc: taskListDoc, ParamStruct: reflect.TypeOf(taskListParams{})},
	{Name: "task_start", Doc: taskStartDoc, ParamStruct: reflect.TypeOf(taskTransitionParams{})},
	{Name: "task_complete", Doc: taskCompleteDoc, ParamStruct: reflect.TypeOf(taskCompleteParams{})},
	{Name: "task_cancel", Doc: taskCancelDoc, ParamStruct: reflect.TypeOf(taskTransitionParams{})},
	{Name: "task_reopen", Doc: taskReopenDoc, ParamStruct: reflect.TypeOf(taskTransitionParams{})},
	{Name: "task_unstart", Doc: taskUnstartDoc, ParamStruct: reflect.TypeOf(taskTransitionParams{})},
	{Name: "task_stamp_sha", Doc: taskStampShaDoc, ParamStruct: reflect.TypeOf(taskStampParams{})},
	{Name: "task_block", Doc: taskBlockDoc, ParamStruct: reflect.TypeOf(taskBlockParams{})},
	{Name: "task_unblock", Doc: taskUnblockDoc, ParamStruct: reflect.TypeOf(taskBlockParams{})},
	{Name: "task_blockers", Doc: taskBlockersDoc, ParamStruct: reflect.TypeOf(taskBlockersParams{})},
	{Name: "task_edit", Doc: taskEditDoc, ParamStruct: nil},

	// ── Bug CRUD ──
	{Name: "bug_list", Doc: bugListDoc, ParamStruct: reflect.TypeOf(bugListParams{})},
	{Name: "bug_read", Doc: bugReadDoc, ParamStruct: reflect.TypeOf(bugReadParams{})},
	{Name: "bug_resolve", Doc: bugResolveDoc, ParamStruct: reflect.TypeOf(bugResolveParams{})},
	{Name: "bug_reopen", Doc: bugReopenDoc, ParamStruct: reflect.TypeOf(bugReopenParams{})},
	{Name: "bug_stamp_sha", Doc: bugStampShaDoc, ParamStruct: reflect.TypeOf(bugStampParams{})},

	// ── Suggestion CRUD ──
	{Name: "suggestion_list", Doc: suggestionListDoc, ParamStruct: reflect.TypeOf(suggestionListParams{})},
	{Name: "suggestion_read", Doc: suggestionReadDoc, ParamStruct: reflect.TypeOf(suggestionReadParams{})},
	{Name: "suggestion_resolve", Doc: suggestionResolveDoc, ParamStruct: reflect.TypeOf(suggestionResolveParams{})},
	{Name: "suggestion_reopen", Doc: suggestionReopenDoc, ParamStruct: reflect.TypeOf(suggestionReopenParams{})},
	{Name: "suggestion_stamp_sha", Doc: suggestionStampShaDoc, ParamStruct: reflect.TypeOf(suggestionStampParams{})},

	// ── Trained-model lifecycle ──
	{Name: "trained_model_list", Doc: trainedModelListDoc, ParamStruct: reflect.TypeOf(trainedModelListParams{})},
	{Name: "trained_model_promote", Doc: trainedModelPromoteDoc, ParamStruct: reflect.TypeOf(trainedModelPromoteParams{})},
	{Name: "trained_model_retire", Doc: trainedModelRetireDoc, ParamStruct: reflect.TypeOf(trainedModelRetireParams{})},

	// ── Recent-activity resume briefing ──
	{Name: "recent_activity", Doc: recentActivityDoc, ParamStruct: reflect.TypeOf(recentActivityParams{})},
	{Name: "where_we_left_off", Doc: whereWeLeftOffDoc, ParamStruct: nil},

	// ── Chain dependency edges + computed roadmap ──
	{Name: "chain_dep_add", Doc: chainDepAddDoc, ParamStruct: reflect.TypeOf(chainDepAddParams{})},
	{Name: "chain_dep_remove", Doc: chainDepRemoveDoc, ParamStruct: reflect.TypeOf(chainDepAddParams{})},
	{Name: "chain_dep_list", Doc: chainDepListDoc, ParamStruct: reflect.TypeOf(chainDepListParams{})},
	{Name: "roadmap_plan", Doc: roadmapPlanDoc, ParamStruct: reflect.TypeOf(roadmapPlanParams{})},

	// ── Roadmap ──
	{Name: "roadmap_list", Doc: roadmapListDoc, ParamStruct: nil},
	{Name: "roadmap_set", Doc: roadmapSetDoc, ParamStruct: reflect.TypeOf(roadmapSetParams{})},
	{Name: "roadmap_preview_set", Doc: roadmapPreviewSetDoc, ParamStruct: reflect.TypeOf(roadmapSetParams{})},
	{Name: "roadmap_insert", Doc: roadmapInsertDoc, ParamStruct: reflect.TypeOf(roadmapInsertParams{})},
	{Name: "roadmap_update", Doc: roadmapUpdateDoc, ParamStruct: reflect.TypeOf(roadmapUpdateParams{})},
	{Name: "roadmap_diff", Doc: roadmapDiffDoc, ParamStruct: nil},
	{Name: "roadmap_mark_reassessed", Doc: roadmapMarkReassessedDoc, ParamStruct: nil},

	// ── Forge ──
	{Name: "forge", Doc: forgeDoc, ParamStruct: nil},
	{Name: "forge_edit", Doc: forgeEditDoc, ParamStruct: nil},
	{Name: "forge_delete", Doc: forgeDeleteDoc, ParamStruct: nil},
	{Name: "forge_schemas", Doc: forgeSchemasDoc, ParamStruct: nil},
	{Name: "forge_schema", Doc: forgeSchemaDoc, ParamStruct: nil},
	{Name: "work_actions", Doc: workActionsDoc, ParamStruct: nil},
	{Name: "work_summary", Doc: workSummaryDoc, ParamStruct: reflect.TypeOf(workSummaryParams{})},

	// ── Batched ops ──
	{Name: "batch", Doc: batchDoc, ParamStruct: reflect.TypeOf(BatchParams{})},
	{Name: "lifecycle_step", Doc: lifecycleStepDoc, ParamStruct: reflect.TypeOf(LifecycleStepParams{})},
	{Name: "record", Doc: recordDoc, ParamStruct: reflect.TypeOf(RecordParams{})},
	{Name: "event_schema", Doc: eventSchemaDoc, ParamStruct: reflect.TypeOf(EventSchemaParams{})},

	// ── Arc-close filing review ──
	{Name: "review_arc_for_filing", Doc: reviewArcForFilingDoc, ParamStruct: reflect.TypeOf(arcparams.ReviewArcForFilingParams{})},
	{Name: "emit_commit_landed", Doc: emitCommitLandedDoc, ParamStruct: reflect.TypeOf(arcparams.EmitCommitLandedParams{})},
	{Name: "arc_review_audit", Doc: arcReviewAuditDoc, ParamStruct: reflect.TypeOf(arcparams.ArcReviewAuditParams{})},
	{Name: "pending_decisions_claim", Doc: pendingDecisionsClaimDoc, ParamStruct: reflect.TypeOf(arcparams.PendingDecisionsClaimParams{})},
	{Name: "sweep_unauthored_staged", Doc: sweepUnauthoredStagedDoc, ParamStruct: reflect.TypeOf(arcparams.SweepUnauthoredStagedParams{})},
}

// HandleWorkActions implements work.work_actions. Returns the catalog derived
// from the co-located descriptor registry (action_doc.go); ignores params (no
// filtering yet — surface stays small enough that callers consume the whole
// list at once).
func HandleWorkActions(_ context.Context, _ string, _ json.RawMessage) (WorkActionsResult, error) {
	return WorkActionsResult(deriveActionSpecs()), nil
}
