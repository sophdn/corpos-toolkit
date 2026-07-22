package events

import (
	"encoding/json"
	"fmt"
)

// This file declares the typed payload structs for every registered
// event type. Each struct mirrors blueprints/events/<type>.json — Go
// fields with json tags map to JSON Schema properties; omitempty drops
// optional fields when their pointer is nil. The Payload interface lets
// Emit accept any registered payload via a single function signature;
// the runtime validator (validator.go) cross-checks the marshaled JSON
// shape against the embedded schema, so a struct that drifts from its
// schema fails at the dispatch boundary, not in production silently.
//
// Adding a new event type means:
//  1. Add the JSON Schema to blueprints/events/<NewType>.json (and the
//     sync script copies it to schemas/ for embed).
//  2. Add the corresponding Go payload struct here.
//  3. Add the EventType() method returning the same string the schema's
//     filename uses (also matches docs/EVENT_CATALOG.md).
//  4. Add the type to docs/EVENT_CATALOG.md.
//
// The four steps are deliberately not single-source-derived: each step
// is a checkpoint where a human reads the others. A schema-only change
// without a Go struct fails IsRegisteredType in tests; a Go struct
// without a schema fails the validator at first emit.

// Payload is the typed-payload contract implemented by every event-type
// payload struct in this file. EventType returns the registered type
// name — must match a blueprints/events/<name>.json file.
type Payload interface {
	EventType() string
}

// ── Roadmap lifecycle ────────────────────────────────────────────────────

// RoadmapUpdatedPayload mirrors blueprints/events/RoadmapUpdated.json.
// Emitted by every roadmap-mutating action handler (roadmap_set,
// roadmap_insert, roadmap_update, roadmap_mark_reassessed) after the
// mutation commits. roadmap_preview_set is a dry-run and does NOT emit.
// The arcreview substrate listener subscribes to this type per chain
// arc-close-filing-review-substrate-listener-wiring T7.
type RoadmapUpdatedPayload struct {
	ActionKind string  `json:"action_kind"`
	Positions  []int64 `json:"positions,omitempty"`
	RefKind    *string `json:"ref_kind,omitempty"`
	RefSlug    *string `json:"ref_slug,omitempty"`
	ItemCount  *int    `json:"item_count,omitempty"`
	// Items is the post-T5-roadmap additive bump (2026-05-21) carrying
	// the per-position layout for "set" / "insert" / "update" / "delete"
	// action_kinds, so the fold can reconstruct proj_roadmap_view from
	// the event payload alone. Optional; nil for "mark_reassessed".
	Items []RoadmapItemPayload `json:"items,omitempty"`
}

// RoadmapItemPayload is the per-position layout shape carried in
// RoadmapUpdated.items. project_id is at the event envelope level
// (entity_project_id); items inherit it.
type RoadmapItemPayload struct {
	Position  int64   `json:"position"`
	RefKind   string  `json:"ref_kind"`
	RefSlug   string  `json:"ref_slug"`
	ChainSlug *string `json:"chain_slug,omitempty"`
	Note      *string `json:"note,omitempty"`
}

func (RoadmapUpdatedPayload) EventType() string { return "RoadmapUpdated" }

// UnmarshalJSON tolerates pre-T5-roadmap legacy events where the
// payload carried the per-position layout as denormalized top-level
// fields (positions[], ref_kind, ref_slug) instead of the items[]
// array T5's additive bump introduced (ca65006). When items is absent
// and the legacy fields are populated, synthesize an items[] entry
// per position so the fold's downstream insert/update branches don't
// trip the "lacks items[]" guard.
func (p *RoadmapUpdatedPayload) UnmarshalJSON(data []byte) error {
	type alias RoadmapUpdatedPayload
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*p = RoadmapUpdatedPayload(a)
	if len(p.Items) == 0 && len(p.Positions) > 0 && p.RefKind != nil && p.RefSlug != nil {
		p.Items = make([]RoadmapItemPayload, 0, len(p.Positions))
		for _, pos := range p.Positions {
			p.Items = append(p.Items, RoadmapItemPayload{
				Position: pos,
				RefKind:  *p.RefKind,
				RefSlug:  *p.RefSlug,
			})
		}
	}
	return nil
}

// ── Parse-context intent detection ───────────────────────────────────────

// ParseContextIntentResolvedPayload mirrors
// blueprints/events/ParseContextIntentResolved.json. Emitted by the
// parse_context handler on every call that ran the directive-intent
// detector (chain parse-context-lean-orienting T5). T10's
// measurement consumes the event for fire-rate, detection-path mix,
// and latency dashboards. MessageHash is a 16-char SHA-256 prefix
// so dedup analytics work without retaining the raw prompt.
type ParseContextIntentResolvedPayload struct {
	IntentShape    string `json:"intent_shape"`
	DetectionPath  string `json:"detection_path"`
	LatencyMs      int    `json:"latency_ms,omitempty"`
	FallbackToNone bool   `json:"fallback_to_none,omitempty"`
	MessageHash    string `json:"message_hash,omitempty"`
}

func (ParseContextIntentResolvedPayload) EventType() string {
	return "ParseContextIntentResolved"
}

// ── Parse-context intent-mapped discipline surface ───────────────────────

// ParseContextDisciplineSurfacedPayload mirrors
// blueprints/events/ParseContextDisciplineSurfaced.json. Emitted by
// the parse_context handler on every call that ran the intent →
// discipline surfacing pass (chain parse-context-lean-orienting T7).
// The four arrays partition the would-fire discipline set across
// the observable outcomes (surfaced / dedup'd / opted-out / recently-
// fired) so T10 can validate the noise-budget rule under live
// traffic.
type ParseContextDisciplineSurfacedPayload struct {
	IntentShape                       string   `json:"intent_shape"`
	DisciplinesSurfaced               []string `json:"disciplines_surfaced,omitempty"`
	DisciplinesSuppressedByDedup      []string `json:"disciplines_suppressed_by_dedup,omitempty"`
	DisciplinesSuppressedByOptout     []string `json:"disciplines_suppressed_by_optout,omitempty"`
	DisciplinesSuppressedByRecentFire []string `json:"disciplines_suppressed_by_recent_fire,omitempty"`
}

func (ParseContextDisciplineSurfacedPayload) EventType() string {
	return "ParseContextDisciplineSurfaced"
}

// ── Parse-context work-state surface ─────────────────────────────────────

// ParseContextWorkStateSurfacedPayload mirrors
// blueprints/events/ParseContextWorkStateSurfaced.json. Emitted by
// the parse_context handler on every call that ran the work-state
// resolver (chain parse-context-lean-orienting T6). The
// no-surfacings case (counts all zero) still emits so T10's
// measurement has a fire-rate denominator.
type ParseContextWorkStateSurfacedPayload struct {
	IntentShape string `json:"intent_shape"`
	BugsCount   int    `json:"bugs_count"`
	TasksCount  int    `json:"tasks_count"`
	ChainsCount int    `json:"chains_count"`
	CacheHit    bool   `json:"cache_hit"`
	ProjectID   string `json:"project_id,omitempty"`
}

func (ParseContextWorkStateSurfacedPayload) EventType() string {
	return "ParseContextWorkStateSurfaced"
}

// ── Parse-context low-confidence kiwix fallback ──────────────────────────

// ParseContextKiwixFallbackFiredPayload mirrors
// blueprints/events/ParseContextKiwixFallbackFired.json. Emitted by the
// parse_context handler on every call that evaluated the low-confidence
// kiwix fallback gate (chain parse-context-lean-orienting T8). Fires on
// BOTH the fire branch and every suppress branch so T10's measurement
// has a fire-rate denominator and a suppress-mix breakdown — the suppress
// mix tells T10 whether the gate is too loose / too tight.
//
// SuppressedReason is empty when Fired=true; otherwise one of the four
// gate-branch values pinned in the schema enum.
type ParseContextKiwixFallbackFiredPayload struct {
	IntentShape          string `json:"intent_shape"`
	Fired                bool   `json:"fired"`
	SuppressedReason     string `json:"suppressed_reason"`
	CandidatesReturned   int    `json:"candidates_returned"`
	KiwixSearchLatencyMs int    `json:"kiwix_search_latency_ms"`
}

func (ParseContextKiwixFallbackFiredPayload) EventType() string {
	return "ParseContextKiwixFallbackFired"
}

// ── Parse-context stdio drift surface ────────────────────────────────────

// ParseContextStdioDriftSurfacedPayload mirrors
// blueprints/events/ParseContextStdioDriftSurfaced.json. Emitted by the
// parse_context handler on every call that evaluates the stdio-drift
// surface — whether or not a Candidate ends up in the envelope. The
// drift_kind=none branch fires so T10's drift-rate measurement can
// compute a fire-rate denominator. Chain parse-context-lean-orienting T9.
type ParseContextStdioDriftSurfacedPayload struct {
	HeadSHA                string `json:"head_sha,omitempty"`
	StdioSHA               string `json:"stdio_sha,omitempty"`
	DriftKind              string `json:"drift_kind"`
	BootstrapPath          bool   `json:"bootstrap_path"`
	SuppressedByRecentFire bool   `json:"suppressed_by_recent_fire"`
}

func (ParseContextStdioDriftSurfacedPayload) EventType() string {
	return "ParseContextStdioDriftSurfaced"
}

// ── Commit lifecycle ─────────────────────────────────────────────────────

// CommitLandedPayload mirrors blueprints/events/CommitLanded.json. Emitted
// by go/cmd/commit-landed-emit (invoked from
// scripts/post-commit-restart-advisor.sh) on every post-commit fire.
// The arcreview substrate listener subscribes to this type per
// docs/ARC_CLOSE_FILING_REVIEW.md §Trigger-model B and chain
// arc-close-filing-review-substrate-listener-wiring T6.
type CommitLandedPayload struct {
	CommitSHA         string  `json:"commit_sha"`
	Branch            *string `json:"branch,omitempty"`
	FilesChangedCount *int    `json:"files_changed_count,omitempty"`
	Author            *string `json:"author,omitempty"`
	Subject           *string `json:"subject,omitempty"`
}

func (CommitLandedPayload) EventType() string { return "CommitLanded" }

// ── Substrate-listener-wiring closing audit ──────────────────────────────

// ArcCloseFilingReviewSubstrateListenerWiringCompletedPayload mirrors
// blueprints/events/ArcCloseFilingReviewSubstrateListenerWiringCompleted.json.
// Emitted by chain arc-close-filing-review-substrate-listener-wiring T10
// as the closing audit event. Self-host: the chain that built the
// SubstrateReviewObserver emits its own closing event through that
// observer's substrate.
type ArcCloseFilingReviewSubstrateListenerWiringCompletedPayload struct {
	AuditDoc             string                     `json:"audit_doc"`
	Summary              string                     `json:"summary"`
	TasksCompleted       []string                   `json:"tasks_completed,omitempty"`
	FollowOnChainSlugs   []string                   `json:"follow_on_chain_slugs"`
	CorpusSnapshotPath   *string                    `json:"corpus_snapshot_path,omitempty"`
	ObserverWiringSHA    string                     `json:"observer_wiring_sha"`
	RecommendedNextPhase *string                    `json:"recommended_next_phase,omitempty"`
	Findings             []ArchitectureAuditFinding `json:"findings,omitempty"`
}

func (ArcCloseFilingReviewSubstrateListenerWiringCompletedPayload) EventType() string {
	return "ArcCloseFilingReviewSubstrateListenerWiringCompleted"
}

// ── Bug lifecycle ──────────────────────────────────────────────────────────

// BugReportedPayload mirrors blueprints/events/BugReported.json. Captures
// the create-time field values when a bug is filed via forge(schema_name=
// "bug", ...). Subsequent mutations emit their own typed events; the bug's
// entity reference is in the envelope, so this payload covers only the
// fields specific to the create operation.
//
// QwenTaskID and RoutedSuggestionSlug added 2026-05-20 via T3 of
// agent-substrate-crud-retirement (§9.5 audit finding) — both optional
// pointers mirror their CRUD-side columns at write time so payload-only
// fold reconstruction (T5's contract) can recover them without joining.
type BugReportedPayload struct {
	Title                string   `json:"title"`
	ProblemStatement     string   `json:"problem_statement"`
	Surface              *string  `json:"surface,omitempty"`
	Severity             *string  `json:"severity,omitempty"`
	Source               *string  `json:"source,omitempty"`
	Tags                 *string  `json:"tags,omitempty"`
	AcceptanceCriteria   []string `json:"acceptance_criteria,omitempty"`
	Constraints          *string  `json:"constraints,omitempty"`
	QwenTaskID           *string  `json:"qwen_task_id,omitempty"`
	RoutedSuggestionSlug *string  `json:"routed_suggestion_slug,omitempty"`
}

func (BugReportedPayload) EventType() string { return "BugReported" }

// BugTriagedPayload mirrors blueprints/events/BugTriaged.json. Triage-shape
// metadata changed (severity and/or tags). Only the changed fields are
// populated as from/to pairs; unchanged fields stay nil-pointer.
type BugTriagedPayload struct {
	FromSeverity *string `json:"from_severity,omitempty"`
	ToSeverity   *string `json:"to_severity,omitempty"`
	FromTags     *string `json:"from_tags,omitempty"`
	ToTags       *string `json:"to_tags,omitempty"`
}

func (BugTriagedPayload) EventType() string { return "BugTriaged" }

// BugResolvedPayload mirrors blueprints/events/BugResolved.json. The
// `kind` field discriminates the semantic resolution; field-shape rules
// per kind live in the bug_resolve handler (e.g. routed→fixed requires
// commit_sha). The schema enforces only the kind enum.
type BugResolvedPayload struct {
	Kind            string  `json:"kind"`
	CommitSHA       *string `json:"commit_sha,omitempty"`
	DupOf           *string `json:"dup_of,omitempty"`
	RoutedChainSlug *string `json:"routed_chain_slug,omitempty"`
	RoutedTaskSlug  *string `json:"routed_task_slug,omitempty"`
	// RoutedSuggestionSlug added 2026-05-21 via T5-bugs (a payload gap T3
	// missed): the bug_resolve handler's reroute path SETs the column
	// in CRUD but didn't include it in the emit, so the post-T5 fold
	// couldn't reconstruct it. Optional / backward-compatible.
	RoutedSuggestionSlug *string `json:"routed_suggestion_slug,omitempty"`
	ResolutionNote       *string `json:"resolution_note,omitempty"`
}

func (BugResolvedPayload) EventType() string { return "BugResolved" }

// BugReopenedPayload mirrors blueprints/events/BugReopened.json. Carries
// a snapshot of the resolution being reversed so the ledger preserves
// round-trip history without forcing a walk back through events. The
// envelope's refs.caused_by_event_id should point at the BugResolved
// being reversed.
type BugReopenedPayload struct {
	PreviousResolution BugResolvedPayload `json:"previous_resolution"`
}

func (BugReopenedPayload) EventType() string { return "BugReopened" }

// ── Task lifecycle ────────────────────────────────────────────────────────

// TaskCreatedPayload mirrors blueprints/events/TaskCreated.json. Captures
// the create-time field values when a task is filed via forge(task, ...).
// The task's entity reference and project_id live in the envelope.
type TaskCreatedPayload struct {
	ChainSlug          string   `json:"chain_slug"`
	Position           *int     `json:"position,omitempty"`
	ProblemStatement   string   `json:"problem_statement"`
	AcceptanceCriteria []string `json:"acceptance_criteria,omitempty"`
	ContextRequired    *string  `json:"context_required,omitempty"`
	Constraints        *string  `json:"constraints,omitempty"`
	HandoffOutput      *string  `json:"handoff_output,omitempty"`
}

func (TaskCreatedPayload) EventType() string { return "TaskCreated" }

// UnmarshalJSON tolerates the migration 056 Option-A backfill drift
// where the synthetic TaskCreated events for pre-substrate tasks stamp
// acceptance_criteria as the raw CRUD column value (a JSON string,
// often newline-joined) instead of the canonical []string the schema
// declares. Production handlers always emit arrays; this fallback only
// trips on the 1760 legacy 2026-04-17 synthetic events surfaced by
// `toolkit-server rebuild-projections` (bug
// `task-synthetic-event-backfill-stringtypes-acceptance-criteria-constraints`).
// When the value is a string, it's wrapped as a single-element list so
// the fold's downstream join-on-"\n- " path produces the same
// projection bytes the snapshot-seed left.
func (p *TaskCreatedPayload) UnmarshalJSON(data []byte) error {
	type raw struct {
		ChainSlug          string          `json:"chain_slug"`
		Position           *int            `json:"position,omitempty"`
		ProblemStatement   string          `json:"problem_statement"`
		AcceptanceCriteria json.RawMessage `json:"acceptance_criteria,omitempty"`
		ContextRequired    *string         `json:"context_required,omitempty"`
		Constraints        *string         `json:"constraints,omitempty"`
		HandoffOutput      *string         `json:"handoff_output,omitempty"`
	}
	var r raw
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	p.ChainSlug = r.ChainSlug
	p.Position = r.Position
	p.ProblemStatement = r.ProblemStatement
	p.ContextRequired = r.ContextRequired
	p.Constraints = r.Constraints
	p.HandoffOutput = r.HandoffOutput

	if len(r.AcceptanceCriteria) == 0 || string(r.AcceptanceCriteria) == "null" {
		p.AcceptanceCriteria = nil
		return nil
	}
	if r.AcceptanceCriteria[0] == '[' {
		return json.Unmarshal(r.AcceptanceCriteria, &p.AcceptanceCriteria)
	}
	var s string
	if err := json.Unmarshal(r.AcceptanceCriteria, &s); err != nil {
		return fmt.Errorf("TaskCreated.acceptance_criteria: not string or []string: %w", err)
	}
	if s == "" {
		p.AcceptanceCriteria = nil
	} else {
		p.AcceptanceCriteria = []string{s}
	}
	return nil
}

// TaskAssignedToChainPayload mirrors blueprints/events/TaskAssignedToChain.json.
// Records a task moving between chains, or changing position within one.
// Initial assignment is captured by TaskCreated; this fires only for
// re-assignment.
type TaskAssignedToChainPayload struct {
	FromChainSlug *string `json:"from_chain_slug,omitempty"`
	ToChainSlug   string  `json:"to_chain_slug"`
	FromPosition  *int    `json:"from_position,omitempty"`
	ToPosition    *int    `json:"to_position,omitempty"`
}

func (TaskAssignedToChainPayload) EventType() string { return "TaskAssignedToChain" }

// TaskCompletedPayload mirrors blueprints/events/TaskCompleted.json.
// CommitSHA is the canonical structural link to the work that landed
// (or the sentinel "unversioned"); optional because the task_complete
// handler accepts close-without-SHA. ClosureSummary is operational color;
// the "why this counts as done" agent rationale lives in envelope.rationale.
type TaskCompletedPayload struct {
	// ChainSlug disambiguates which chain's task this event targets, so the
	// fold doesn't fan out across tasks that share a slug across chains (bug
	// `task-lifecycle-event-folds-fan-out-across-duplicate-task-slugs`).
	// Mirrors TaskCreatedPayload.ChainSlug. Omitempty: pre-fix events lack
	// it and the fold falls back to the legacy slug+project match.
	ChainSlug      string  `json:"chain_slug,omitempty"`
	CommitSHA      *string `json:"commit_sha,omitempty"`
	ClosureSummary *string `json:"closure_summary,omitempty"`
}

func (TaskCompletedPayload) EventType() string { return "TaskCompleted" }

// TaskCancelledPayload mirrors blueprints/events/TaskCancelled.json.
// Reason is optional free text — the cancellation categories are diverse
// and the current task_cancel handler does not require a reason from
// callers.
type TaskCancelledPayload struct {
	// ChainSlug — see TaskCompletedPayload.ChainSlug (anti-fanout
	// disambiguation; bug `task-lifecycle-event-folds-fan-out-across-
	// duplicate-task-slugs`).
	ChainSlug string  `json:"chain_slug,omitempty"`
	Reason    *string `json:"reason,omitempty"`
}

func (TaskCancelledPayload) EventType() string { return "TaskCancelled" }

// TaskRetiredPayload mirrors blueprints/events/TaskRetired.json. Emitted
// when a task row is removed entirely from the projection (distinct from
// TaskCancelled, which keeps the row at status='cancelled'). Used by
// the migration-062 synthetic-event backfill to retire the 6 phantom
// task slugs (flip-write-contract-*) that the pre-8f2cb87 buggy
// forge-task-delete path removed from CRUD without emitting a
// retirement event. The fold DELETEs the projection row and cleans up
// proj_task_blockers edges referencing it.
type TaskRetiredPayload struct {
	Reason *string `json:"reason,omitempty"`
}

func (TaskRetiredPayload) EventType() string { return "TaskRetired" }

// TaskTransitionedPayload mirrors blueprints/events/TaskTransitioned.json.
// Fires for task_start, task_block, task_unblock, task_reopen — the
// non-terminal status transitions. Terminal transitions (close, cancel)
// use TaskCompleted / TaskCancelled because they carry distinct required
// payload (commit_sha, reason).
//
// RemovedBlockerSlug added 2026-05-20 via T3 of agent-substrate-crud-
// retirement (§9.1 audit finding). For unblock transitions: the slug of
// the blocker edge being REMOVED. Multi-edge unblock-all emits one
// TaskTransitioned per removed edge; single-edge unblock emits one event
// with this field populated. Null for blocker-adding and non-blocker
// transitions. Same T3 change also lifts the L1181 guard in
// HandleTaskBlock so 2nd+ blocker INSERTs emit a (blocked → blocked)
// self-transition with BlockerSlug=<new>, letting payload-only fold
// reconstruct proj_task_blockers without joining the CRUD table.
type TaskTransitionedPayload struct {
	// ChainSlug — see TaskCompletedPayload.ChainSlug (anti-fanout
	// disambiguation; bug `task-lifecycle-event-folds-fan-out-across-
	// duplicate-task-slugs`).
	ChainSlug          string  `json:"chain_slug,omitempty"`
	FromStatus         string  `json:"from_status"`
	ToStatus           string  `json:"to_status"`
	BlockerSlug        *string `json:"blocker_slug,omitempty"`
	RemovedBlockerSlug *string `json:"removed_blocker_slug,omitempty"`
}

func (TaskTransitionedPayload) EventType() string { return "TaskTransitioned" }

// TaskEditedPayload mirrors blueprints/events/TaskEdited.json. Emitted by
// task_edit and forge_edit(task) for content-field changes (NOT status
// transitions, NOT chain reassignment). UpdatedFields lists the changed
// fields by snake_case name; UpdatedValues (added 2026-05-20 via T3 of
// agent-substrate-crud-retirement, §9.4 audit finding) carries the
// post-edit values for each changed column so payload-only fold
// reconstruction can rebuild proj_current_tasks without re-reading the
// CRUD row (T5's contract). Optional for backward compatibility with
// pre-T3 events. Values are always strings — every task content field
// has a string storage form (lists are joined to "\n- " strings at
// write time), so map[string]string suffices and avoids the bare-any
// forbidigo rule.
type TaskEditedPayload struct {
	// ChainSlug — see TaskCompletedPayload.ChainSlug (anti-fanout
	// disambiguation; bug `task-lifecycle-event-folds-fan-out-across-
	// duplicate-task-slugs`).
	ChainSlug     string            `json:"chain_slug,omitempty"`
	UpdatedFields []string          `json:"updated_fields"`
	UpdatedValues map[string]string `json:"updated_values,omitempty"`
}

func (TaskEditedPayload) EventType() string { return "TaskEdited" }

// TaskStampedPayload mirrors blueprints/events/TaskStamped.json. Emitted
// by task_stamp_sha when the SHA is stamped onto a task whose deliverable
// landed after task_complete. Distinct from TaskEdited so SHA-stamp
// readers can filter by type without parsing updated_fields.
type TaskStampedPayload struct {
	// ChainSlug — see TaskCompletedPayload.ChainSlug (anti-fanout
	// disambiguation; bug `task-lifecycle-event-folds-fan-out-across-
	// duplicate-task-slugs`).
	ChainSlug string `json:"chain_slug,omitempty"`
	CommitSHA string `json:"commit_sha"`
}

func (TaskStampedPayload) EventType() string { return "TaskStamped" }

// BugEditedPayload mirrors blueprints/events/BugEdited.json. Emitted by
// forge_edit(bug) for content-field changes; when triage-shape fields
// (severity, tags) change, BugTriaged also emits alongside this event.
// Status transitions emit BugResolved / BugReopened; SHA stamping emits
// BugStamped. UpdatedValues (added 2026-05-20 via T3 of agent-substrate-
// crud-retirement, §9.4 audit finding) carries the post-edit values for
// each changed column so payload-only fold reconstruction can rebuild
// proj_current_bugs without re-reading the CRUD row (T5's contract).
// Optional for backward compatibility with pre-T3 events. Values are
// always strings — every bug content field has a string storage form
// (lists are joined to "\n- " strings at write time).
type BugEditedPayload struct {
	UpdatedFields []string          `json:"updated_fields"`
	UpdatedValues map[string]string `json:"updated_values,omitempty"`
}

func (BugEditedPayload) EventType() string { return "BugEdited" }

// BugStampedPayload mirrors blueprints/events/BugStamped.json. Emitted
// by bug_stamp_sha when a fix's commit lands after the bug was resolved.
// Distinct from BugResolved(kind="fixed", commit_sha=...) because the
// stamping action is a separate MCP action that can fire on an
// already-resolved bug.
type BugStampedPayload struct {
	CommitSHA string `json:"commit_sha"`
}

func (BugStampedPayload) EventType() string { return "BugStamped" }

// ── Suggestion lifecycle ─────────────────────────────────────────────────

// SuggestionReportedPayload mirrors blueprints/events/SuggestionReported.json.
// Captures the create-time field values when a suggestion is filed via
// forge(schema_name="suggestion", ...). Subsequent mutations emit their
// own typed events (SuggestionResolved, SuggestionReopened, SuggestionStamped).
// Native vocabulary: `priority` not `severity`. See chain
// `agent-suggestion-box` for the separate-entity rationale.
type SuggestionReportedPayload struct {
	Title              string   `json:"title"`
	ProblemStatement   string   `json:"problem_statement"`
	Surface            *string  `json:"surface,omitempty"`
	Priority           *string  `json:"priority,omitempty"`
	Source             *string  `json:"source,omitempty"`
	Tags               *string  `json:"tags,omitempty"`
	AcceptanceCriteria []string `json:"acceptance_criteria,omitempty"`
	Constraints        *string  `json:"constraints,omitempty"`
}

func (SuggestionReportedPayload) EventType() string { return "SuggestionReported" }

// SuggestionResolvedPayload mirrors blueprints/events/SuggestionResolved.json.
// Kind-discriminated like BugResolved, but with the suggestion-side
// vocabulary (adopted / deferred / rejected) — see chain
// `agent-suggestion-box` design_decisions for why these are deliberately
// distinct from bug resolution kinds. `adopted` + routed_* + commit_sha
// is the canonical 'this shipped' shape; there is no separate
// `implemented` kind.
type SuggestionResolvedPayload struct {
	Kind            string  `json:"kind"`
	CommitSHA       *string `json:"commit_sha,omitempty"`
	RoutedChainSlug *string `json:"routed_chain_slug,omitempty"`
	RoutedTaskSlug  *string `json:"routed_task_slug,omitempty"`
	RoutedBugSlug   *string `json:"routed_bug_slug,omitempty"`
	ResolutionNote  *string `json:"resolution_note,omitempty"`
}

func (SuggestionResolvedPayload) EventType() string { return "SuggestionResolved" }

// SuggestionReopenedPayload mirrors blueprints/events/SuggestionReopened.json.
// Carries a snapshot of the resolution being reversed so the ledger
// preserves round-trip history without forcing a walk back through events.
type SuggestionReopenedPayload struct {
	PreviousResolution SuggestionResolvedPayload `json:"previous_resolution"`
}

func (SuggestionReopenedPayload) EventType() string { return "SuggestionReopened" }

// SuggestionStampedPayload mirrors blueprints/events/SuggestionStamped.json.
// Emitted by suggestion_stamp_sha when the implementing commit lands
// after adoption. Distinct from SuggestionResolved(kind="adopted",
// commit_sha=...) so SHA-stamp readers can filter by type.
type SuggestionStampedPayload struct {
	CommitSHA string `json:"commit_sha"`
}

func (SuggestionStampedPayload) EventType() string { return "SuggestionStamped" }

// SuggestionEditedPayload mirrors blueprints/events/SuggestionEdited.json.
// Emitted by forge_edit(suggestion) for content-field changes. Status
// transitions emit SuggestionResolved / SuggestionReopened (status
// carries set_by="suggestion_resolve"); SHA stamping emits
// SuggestionStamped. The sibling of BugEditedPayload — UpdatedValues
// carries the post-edit values for each changed column so payload-only
// fold reconstruction can rebuild proj_current_suggestions without
// re-reading a CRUD row (the suggestions CRUD table was retired in
// T5-suggestions). Values are always strings — every suggestion content
// field has a string storage form (lists join to "\n- " at write time).
// Added by bug `forge-edit-on-lifecycle-fields-bypasses-state-machine-
// and-suggestion-edit-broken`.
type SuggestionEditedPayload struct {
	UpdatedFields []string          `json:"updated_fields"`
	UpdatedValues map[string]string `json:"updated_values,omitempty"`
}

func (SuggestionEditedPayload) EventType() string { return "SuggestionEdited" }

// ── Chain lifecycle ──────────────────────────────────────────────────────

// ChainCreatedPayload mirrors blueprints/events/ChainCreated.json.
// The skeleton TaskCreated cascade emits set caused_by_event_id pointing
// at this ChainCreated.
type ChainCreatedPayload struct {
	Output              string   `json:"output"`
	DesignDecisions     string   `json:"design_decisions"`
	CompletionCondition string   `json:"completion_condition"`
	Tasks               []string `json:"tasks,omitempty"`
}

func (ChainCreatedPayload) EventType() string { return "ChainCreated" }

// ChainClosedPayload mirrors blueprints/events/ChainClosed.json.
// ClosureSummary is the load-bearing "what did we ship" record but is
// optional — the chain_close handler accepts a nil summary for legacy
// callers, and the schema captures whatever the handler supplies.
type ChainClosedPayload struct {
	ClosureSummary *string `json:"closure_summary,omitempty"`
}

func (ChainClosedPayload) EventType() string { return "ChainClosed" }

// ChainEditedPayload mirrors blueprints/events/ChainEdited.json. Emitted
// by forge_edit(chain) for content-field changes. ChainClosed is the
// distinct close event — closure_summary lands as a required payload
// field there rather than in this event's updated_fields list.
// UpdatedValues (added 2026-05-20 via T3 of agent-substrate-crud-
// retirement, §9.4 audit finding) carries the post-edit values for each
// changed column so payload-only fold reconstruction can rebuild
// proj_chain_status without re-reading the CRUD row (T5's contract).
// Optional for backward compatibility with pre-T3 events. Values are
// always strings — every chain content field has a string storage form.
type ChainEditedPayload struct {
	UpdatedFields []string          `json:"updated_fields"`
	UpdatedValues map[string]string `json:"updated_values,omitempty"`
}

func (ChainEditedPayload) EventType() string { return "ChainEdited" }

// ── Architecture / convention audit ──────────────────────────────────────

// ArchitectureAuditFinding is one row of an architecture-audit findings
// array. Status is closed; severity is closed (or omitted).
type ArchitectureAuditFinding struct {
	Item     string  `json:"item"`
	Status   string  `json:"status"`
	Evidence *string `json:"evidence,omitempty"`
	Severity *string `json:"severity,omitempty"`
}

// ArchitectureAuditCompletedPayload mirrors
// blueprints/events/ArchitectureAuditCompleted.json. Emitted by a chain
// retrospective task that verified the substrate against an architecture
// audit doc; the event is the self-hosting proof.
type ArchitectureAuditCompletedPayload struct {
	AuditDoc             string                     `json:"audit_doc"`
	Summary              string                     `json:"summary"`
	RecommendedNextPhase *string                    `json:"recommended_next_phase,omitempty"`
	Findings             []ArchitectureAuditFinding `json:"findings"`
}

func (ArchitectureAuditCompletedPayload) EventType() string {
	return "ArchitectureAuditCompleted"
}

// ConventionAuditFinding is one row of a convention-audit findings array.
// agent_impact and human_cost are the two costs the convention audit
// always weighs alongside the binary realised/absent verdict.
type ConventionAuditFinding struct {
	Axis        string  `json:"axis"`
	Status      string  `json:"status"`
	Evidence    *string `json:"evidence,omitempty"`
	AgentImpact *string `json:"agent_impact,omitempty"`
	HumanCost   *string `json:"human_cost,omitempty"`
}

// ConventionAuditCompletedPayload mirrors
// blueprints/events/ConventionAuditCompleted.json. Emitted by a chain
// retrospective that verified the agent-facing conventions against an
// audit doc — sibling to ArchitectureAuditCompleted.
type ConventionAuditCompletedPayload struct {
	AuditDoc string                   `json:"audit_doc"`
	Summary  string                   `json:"summary"`
	Findings []ConventionAuditFinding `json:"findings"`
}

func (ConventionAuditCompletedPayload) EventType() string {
	return "ConventionAuditCompleted"
}

// SubstrateFrontendAuditCompletedPayload mirrors
// blueprints/events/SubstrateFrontendAuditCompleted.json. Emitted by
// chain agent-substrate-frontend F5 to record that the dashboard
// surfaces serving the event ledger (per-entity timelines, audit-ledger
// page, dispatch-policy peek) have been verified against the F1 design
// doc and the chain's completion criteria. Self-hosting check parallel
// to ArchitectureAuditCompleted — the frontend exercising the substrate
// to record its own audit outcome. Reuses ArchitectureAuditFinding for
// the per-section row shape because the audit structure is identical
// (item / status / evidence / severity).
type SubstrateFrontendAuditCompletedPayload struct {
	AuditDoc             string                     `json:"audit_doc"`
	Summary              string                     `json:"summary"`
	RecommendedNextPhase *string                    `json:"recommended_next_phase,omitempty"`
	Findings             []ArchitectureAuditFinding `json:"findings"`
}

func (SubstrateFrontendAuditCompletedPayload) EventType() string {
	return "SubstrateFrontendAuditCompleted"
}

// TelemetryAuditCompletedPayload mirrors
// blueprints/events/TelemetryAuditCompleted.json. Emitted by chain
// query-telemetry-substrate TT4 retrospective to record that the
// read-side telemetry substrate (grounding_events + query_interactions
// + query_resolutions + the three query_* projections) has been
// verified against the chain's success criteria. Self-hosting check
// parallel to ArchitectureAuditCompleted — the read-side substrate
// uses the write-side substrate to record its own audit outcome,
// exercising the cross-substrate seam (events.event_id ↔
// query_resolutions.write_event_ids) in the act of closing the chain.
// Reuses ArchitectureAuditFinding for the per-section row shape.
type TelemetryAuditCompletedPayload struct {
	AuditDoc             string                     `json:"audit_doc"`
	Summary              string                     `json:"summary"`
	RecommendedNextPhase *string                    `json:"recommended_next_phase,omitempty"`
	Findings             []ArchitectureAuditFinding `json:"findings"`
}

func (TelemetryAuditCompletedPayload) EventType() string {
	return "TelemetryAuditCompleted"
}

// ReferenceResolutionAuditCompletedPayload mirrors
// blueprints/events/ReferenceResolutionAuditCompleted.json. Emitted by
// chain reference-resolution-substrate T8 retrospective. Third audit-
// event in the substrate trilogy (after ArchitectureAuditCompleted from
// agent-first-substrate and TelemetryAuditCompleted from query-telemetry-
// substrate); closes the cross-substrate self-hosting loop by emitting
// the closing audit through the write-side events ledger.
type ReferenceResolutionAuditCompletedPayload struct {
	AuditDoc             string                     `json:"audit_doc"`
	Summary              string                     `json:"summary"`
	RecommendedNextPhase *string                    `json:"recommended_next_phase,omitempty"`
	Findings             []ArchitectureAuditFinding `json:"findings"`
}

func (ReferenceResolutionAuditCompletedPayload) EventType() string {
	return "ReferenceResolutionAuditCompleted"
}

// ObservabilityAuditCompletedPayload mirrors
// blueprints/events/ObservabilityAuditCompleted.json. Emitted by chain
// per-tool-per-model-observability T13 retrospective to record that the
// inference-telemetry cluster (qwen_invocations → inference_invocations +
// the proj_inference_tool_model_performance projection + the repointed
// /inference endpoints) was relocated onto the read-side substrate with no
// behavioral regression — proven by the characterization net staying green
// and unmodified across the cutover. First chain of the telemetry-
// consolidation program; the closing audit lands through the write-side
// ledger as the self-hosting proof the consolidation preserved the seam.
// Reuses ArchitectureAuditFinding for the per-section row shape.
type ObservabilityAuditCompletedPayload struct {
	AuditDoc             string                     `json:"audit_doc"`
	Summary              string                     `json:"summary"`
	RecommendedNextPhase *string                    `json:"recommended_next_phase,omitempty"`
	Findings             []ArchitectureAuditFinding `json:"findings"`
}

func (ObservabilityAuditCompletedPayload) EventType() string {
	return "ObservabilityAuditCompleted"
}

// MLCapabilityAuditCompletedPayload mirrors
// blueprints/events/MLCapabilityAuditCompleted.json. Emitted by chain
// ml-capability-substrate T8 retrospective — fourth audit-event in the
// substrate quartet (after ArchitectureAuditCompleted, TelemetryAuditCompleted,
// ReferenceResolutionAuditCompleted). Closes the quartet's self-hosting
// loop by emitting the closing audit through the write-side events
// ledger.
type MLCapabilityAuditCompletedPayload struct {
	AuditDoc             string                     `json:"audit_doc"`
	Summary              string                     `json:"summary"`
	RecommendedNextPhase *string                    `json:"recommended_next_phase,omitempty"`
	Findings             []ArchitectureAuditFinding `json:"findings"`
}

func (MLCapabilityAuditCompletedPayload) EventType() string {
	return "MLCapabilityAuditCompleted"
}

// MemorySubstrateAuditCompletedPayload mirrors
// blueprints/events/MemorySubstrateAuditCompleted.json. Emitted by chain
// memory-substrate-within-vault T7 retrospective to record that the
// vault-mediated memory substrate (memory forge schema + dispatch +
// MemoryWritten event + SessionStart materialization hook + one-shot
// harness migration + parse_context memory-aware resolution + arc-close
// hook forge routing) has been verified against the chain's
// completion criteria. Reuses ArchitectureAuditFinding for the
// per-criterion row shape so the cross-substrate audit envelope stays
// consistent; the closing audit lands through the substrate's own
// write-side events ledger as the self-hosting proof.
type MemorySubstrateAuditCompletedPayload struct {
	AuditDoc             string                     `json:"audit_doc"`
	Summary              string                     `json:"summary"`
	RecommendedNextPhase *string                    `json:"recommended_next_phase,omitempty"`
	Findings             []ArchitectureAuditFinding `json:"findings"`
}

func (MemorySubstrateAuditCompletedPayload) EventType() string {
	return "MemorySubstrateAuditCompleted"
}

// ReferenceResolutionMigrationAuditCompletedPayload mirrors
// blueprints/events/ReferenceResolutionMigrationAuditCompleted.json.
// Emitted by chain reference-resolution-migration T12 retrospective.
// Successor to ReferenceResolutionAuditCompletedPayload: the substrate
// trilogy shipped the resolver layer; this migration converted the
// ambient surface to lazy-via-parse_context loading AND made mcp-servers
// self-contained (repo owns canonical skills/hooks/personas; install
// script symlinks into ~/.claude/<dir>/). Reuses ArchitectureAuditFinding
// for the per-criterion row shape so the cross-substrate audit envelope
// stays consistent.
type ReferenceResolutionMigrationAuditCompletedPayload struct {
	AuditDoc             string                     `json:"audit_doc"`
	Summary              string                     `json:"summary"`
	RecommendedNextPhase *string                    `json:"recommended_next_phase,omitempty"`
	Findings             []ArchitectureAuditFinding `json:"findings"`
}

func (ReferenceResolutionMigrationAuditCompletedPayload) EventType() string {
	return "ReferenceResolutionMigrationAuditCompleted"
}

// TelemetryFrontendAuditCompletedPayload mirrors
// blueprints/events/TelemetryFrontendAuditCompleted.json. Emitted by
// chain query-telemetry-substrate-frontend QF5 retrospective to record
// that the dashboard surfaces serving the read-side telemetry substrate
// (per-query trajectory inspector, analytics page over the three
// query_* projections, training-pair browser) have been verified
// against the chain's design doc and completion criteria. Self-hosting
// check parallel to SubstrateFrontendAuditCompleted — the read-side
// frontend uses the write-side audit ledger to record its own audit
// outcome, closing the substrate trilogy loop end-to-end. Reuses
// ArchitectureAuditFinding for the per-section row shape.
type TelemetryFrontendAuditCompletedPayload struct {
	AuditDoc             string                     `json:"audit_doc"`
	Summary              string                     `json:"summary"`
	RecommendedNextPhase *string                    `json:"recommended_next_phase,omitempty"`
	Findings             []ArchitectureAuditFinding `json:"findings"`
}

func (TelemetryFrontendAuditCompletedPayload) EventType() string {
	return "TelemetryFrontendAuditCompleted"
}

// ActionDocsFrontendAuditCompletedPayload mirrors
// blueprints/events/ActionDocsFrontendAuditCompleted.json. Emitted by
// chain action-docs-corpus-frontend AF3 retrospective to record that
// the dashboard surface serving the per-action documentation corpus
// (/docs/actions, /docs/actions/<surface>, /docs/actions/<surface>/<action>)
// has been verified against docs/ACTION_DOCS_FRONTEND.md and the
// chain's completion criteria. Self-hosting check parallel to
// SubstrateFrontendAuditCompleted — the dashboard surfaces that browse
// the action-docs corpus record their own audit outcome through the
// events ledger that those same surfaces (per-entity timeline /
// audit-ledger page) make readable. Reuses ArchitectureAuditFinding
// for the per-section row shape.
type ActionDocsFrontendAuditCompletedPayload struct {
	AuditDoc             string                     `json:"audit_doc"`
	Summary              string                     `json:"summary"`
	RecommendedNextPhase *string                    `json:"recommended_next_phase,omitempty"`
	Findings             []ArchitectureAuditFinding `json:"findings"`
}

func (ActionDocsFrontendAuditCompletedPayload) EventType() string {
	return "ActionDocsFrontendAuditCompleted"
}

// ReferenceResolutionFrontendAuditCompletedPayload mirrors
// blueprints/events/ReferenceResolutionFrontendAuditCompleted.json.
// Emitted by chain reference-resolution-substrate-frontend RF4
// retrospective to record that the Context Pull Inspector page
// (/context-pulls — list + drawer + EventTimeline references-resolved
// suffix block) has been verified against docs/REFERENCE_RESOLUTION_FRONTEND.md
// and the chain's completion criteria. Self-hosting check parallel to
// TelemetryFrontendAuditCompleted and SubstrateFrontendAuditCompleted —
// the dashboard surface that surfaces reference-resolution outcomes
// records its own audit outcome through the events ledger that
// agent-substrate-frontend's audit ledger renders. Closes the
// substrate-trilogy-frontend loop end-to-end. Reuses
// ArchitectureAuditFinding for the per-section row shape.
type ReferenceResolutionFrontendAuditCompletedPayload struct {
	AuditDoc             string                     `json:"audit_doc"`
	Summary              string                     `json:"summary"`
	RecommendedNextPhase *string                    `json:"recommended_next_phase,omitempty"`
	Findings             []ArchitectureAuditFinding `json:"findings"`
}

func (ReferenceResolutionFrontendAuditCompletedPayload) EventType() string {
	return "ReferenceResolutionFrontendAuditCompleted"
}

// ── Orchestrator-tier escalation ─────────────────────────────────────────

// EscalationProposedPayload mirrors blueprints/events/EscalationProposed.json.
// Emitted by the orchestrator-tier escalation contract when a trigger fires
// and the router proposes handing the next conversational turn from a cheap
// orchestrator model to a strong one (or, on the down-edge, records a
// de-escalation). See docs/ORCHESTRATOR_ESCALATION.md. The model identities
// live in FromModel/ToModel (payload), NOT the envelope actor — actor is
// transport-inferred (system over the HTTP /mcp route). TriggerDetail and
// ThresholdValue are optional (omitted on de-escalation edges / when the
// detector captured no evidence).
type EscalationProposedPayload struct {
	Trigger        string   `json:"trigger"`
	FromModel      string   `json:"from_model"`
	ToModel        string   `json:"to_model"`
	SessionID      string   `json:"session_id"`
	TurnIndex      int      `json:"turn_index"`
	StateBefore    string   `json:"state_before"`
	StateAfter     string   `json:"state_after"`
	TriggerDetail  *string  `json:"trigger_detail,omitempty"`
	ThresholdValue *float64 `json:"threshold_value,omitempty"`
}

func (EscalationProposedPayload) EventType() string { return "EscalationProposed" }

// EscalationContractAuditCompletedPayload mirrors
// blueprints/events/EscalationContractAuditCompleted.json. Emitted by chain
// orchestrator-tier-escalation-contract's retrospective (T5) to record that
// the contract has been verified against the chain's completion-condition
// items (a)-(f). Reuses ArchitectureAuditFinding for the per-item row shape,
// like the other *AuditCompleted events. Self-hosting check: the closing
// audit lands through the same write-side ledger the contract's
// EscalationProposed events use, re-emittable via
// go/cmd/audit-emit --spec specs/escalation-contract.json.
type EscalationContractAuditCompletedPayload struct {
	AuditDoc             string                     `json:"audit_doc"`
	Summary              string                     `json:"summary"`
	RecommendedNextPhase *string                    `json:"recommended_next_phase,omitempty"`
	Findings             []ArchitectureAuditFinding `json:"findings"`
}

func (EscalationContractAuditCompletedPayload) EventType() string {
	return "EscalationContractAuditCompleted"
}

// BridgeHarnessV1AuditCompletedPayload mirrors
// blueprints/events/BridgeHarnessV1AuditCompleted.json. Emitted by chain
// bridge-harness-mcp-client's retrospective (T9) to record that the
// bridge-harness v1 (clients/bridge-harness/) has been verified against the
// chain's completion-condition items (a)-(i). Reuses ArchitectureAuditFinding
// for the per-item row shape, like the other *AuditCompleted events.
// Self-hosting check: the harness's own tool-dispatch + EscalationProposed
// telemetry lands through the same write-side ledger this closing audit lands
// through, re-emittable via go/cmd/audit-emit --spec specs/bridge-harness-v1.json.
type BridgeHarnessV1AuditCompletedPayload struct {
	AuditDoc             string                     `json:"audit_doc"`
	Summary              string                     `json:"summary"`
	RecommendedNextPhase *string                    `json:"recommended_next_phase,omitempty"`
	Findings             []ArchitectureAuditFinding `json:"findings"`
}

func (BridgeHarnessV1AuditCompletedPayload) EventType() string {
	return "BridgeHarnessV1AuditCompleted"
}

// ── Benchmark lifecycle ──────────────────────────────────────────────────

// BenchmarkProvenance is the fixed-shape provenance bundle captured at
// the start of every benchmark run. Every field is required — a run that
// can't pin one of these is not replayable. Used as a sub-shape of
// BenchmarkRunStartedPayload; not itself a top-level event payload.
//
// T6 detail-filled the T1 draft: added RetrieverConfigHash (distinct from
// RetrieverVersion — version pins which retriever, config_hash pins its
// runtime knobs k/threshold/reranker-temp). Envelope event_time captures
// wall-clock-start; BenchmarkRunCompleted.WallClockMS captures end.
type BenchmarkProvenance struct {
	ModelID             string `json:"model_id"`
	ModelVersion        string `json:"model_version"`
	PromptTemplateHash  string `json:"prompt_template_hash"`
	CorpusHash          string `json:"corpus_hash"`
	RetrieverVersion    string `json:"retriever_version"`
	RetrieverConfigHash string `json:"retriever_config_hash"`
	Seed                int    `json:"seed"`
	EnvHash             string `json:"env_hash"`
}

// BenchmarkRunStartedPayload mirrors blueprints/events/BenchmarkRunStarted.json.
type BenchmarkRunStartedPayload struct {
	ScenarioID string              `json:"scenario_id"`
	Provenance BenchmarkProvenance `json:"provenance"`
}

func (BenchmarkRunStartedPayload) EventType() string { return "BenchmarkRunStarted" }

// BenchmarkRunCompletedPayload mirrors blueprints/events/BenchmarkRunCompleted.json.
// Provenance is NOT repeated here — it lives on the corresponding
// BenchmarkRunStarted event, joined via envelope refs.caused_by_event_id.
//
// ResultColumns added 2026-05-20 via T3 of agent-substrate-crud-
// retirement (§9.6 audit finding) — optional snapshot of the rubric-side
// columns of the benchmark_results CRUD row this completion produced,
// so payload-only fold reconstruction (T5's contract) can rebuild
// proj_benchmark_results without joining the CRUD table. nil for pre-T3
// events; once T5 lands, every new event carries the block.
type BenchmarkRunCompletedPayload struct {
	RunID         string                  `json:"run_id"`
	Score         *float64                `json:"score,omitempty"`
	WallClockMS   int                     `json:"wall_clock_ms"`
	InputTokens   *int                    `json:"input_tokens,omitempty"`
	OutputTokens  *int                    `json:"output_tokens,omitempty"`
	ToolUseTokens *int                    `json:"tool_use_tokens,omitempty"`
	ResultColumns *BenchmarkResultColumns `json:"result_columns,omitempty"`
	// T5-benchmarks additive bump (2026-05-21): identifying columns for
	// proj_benchmark_results reconstruction. The fold needs the row id
	// (benchmark_results.id is a separate UUID from run_id), the
	// project_id (cross-cutting events don't carry it in the envelope),
	// the scenario_id (lives on the BenchmarkRunStarted side), the
	// provenance_id (lives on benchmark_provenance), and run_at
	// (event.ts is a string; the column is INTEGER unix seconds). All
	// optional for backward-compatibility with pre-T5 events.
	BenchmarkResultID *string `json:"benchmark_result_id,omitempty"`
	ProjectID         *string `json:"project_id,omitempty"`
	ScenarioID        *string `json:"scenario_id,omitempty"`
	ProvenanceID      *string `json:"provenance_id,omitempty"`
	RunAt             *int64  `json:"run_at,omitempty"`
}

func (BenchmarkRunCompletedPayload) EventType() string { return "BenchmarkRunCompleted" }

// BenchmarkResultColumns mirrors the rubric-side columns of one
// benchmark_results row — used as the optional ResultColumns block on
// BenchmarkRunCompletedPayload (added 2026-05-20 via T3 of agent-
// substrate-crud-retirement, §9.6 audit finding). Every field mirrors a
// benchmark_results column verbatim so payload-only fold reconstruction
// can rebuild proj_benchmark_results without joining the CRUD table.
// ToolName / ModelName / InvocationOK / InvokedContextually are required
// in the schema because every row has them; rubric-score fields default
// to nil for runs that didn't score against those rubrics.
type BenchmarkResultColumns struct {
	ToolName            string   `json:"tool_name"`
	ModelName           string   `json:"model_name"`
	Layer               *string  `json:"layer,omitempty"`
	TaskShape           *string  `json:"task_shape,omitempty"`
	TaskID              *string  `json:"task_id,omitempty"`
	RunShape            *string  `json:"run_shape,omitempty"`
	AccuracyScore       *float64 `json:"accuracy_score,omitempty"`
	HonestyScore        *float64 `json:"honesty_score,omitempty"`
	RankingQualityScore *float64 `json:"ranking_quality_score,omitempty"`
	WithinBudgetScore   *float64 `json:"within_budget_score,omitempty"`
	InvocationOK        bool     `json:"invocation_ok"`
	ArgsMatch           *bool    `json:"args_match,omitempty"`
	ExtractedArgs       *string  `json:"extracted_args,omitempty"`
	InterpretationOK    *bool    `json:"interpretation_ok,omitempty"`
	DetectedTool        *string  `json:"detected_tool,omitempty"`
	Notes               *string  `json:"notes,omitempty"`
	InvokedContextually bool     `json:"invoked_contextually"`
}

// BenchmarkRunFailedPayload mirrors blueprints/events/BenchmarkRunFailed.json.
// ErrorKind matches CONVENTIONS.md §Error Enum Shape variant names so the
// failure is queryable by category.
type BenchmarkRunFailedPayload struct {
	RunID       string `json:"run_id"`
	ErrorKind   string `json:"error_kind"`
	ErrorDetail string `json:"error_detail"`
	WallClockMS *int   `json:"wall_clock_ms,omitempty"`
}

func (BenchmarkRunFailedPayload) EventType() string { return "BenchmarkRunFailed" }

// ── Study runs (corpos-lab behavioral assays) ────────────────────────────

// StudyRunScoreRow is one cell of a study run's score grid — one
// condition×run outcome. Mirrors the `items` schema of the `rows` array in
// blueprints/events/StudyRunRecorded.json. The controller's nested verdict
// object ({kind, reason}) is FLATTENED to VerdictKind / VerdictReason before
// it reaches the payload (in the study_run_record handler). Fields carry no
// omitempty so every cell serialises all six columns — the schema requires
// them and the fold reads them positionally into proj_study_run_scores.
type StudyRunScoreRow struct {
	Item          string `json:"item"`
	Condition     string `json:"condition"`
	Run           int    `json:"run"`
	VerdictKind   string `json:"verdict_kind"`
	VerdictReason string `json:"verdict_reason"`
	Rationale     string `json:"rationale"`
}

// StudyRunRecordedPayload mirrors blueprints/events/StudyRunRecorded.json —
// the parent provenance record plus the child score grid for one corpos-lab
// study run. Emitted by the measure surface's study_run_record action and
// folded into proj_study_runs (parent, 1 row) + proj_study_run_scores
// (child, N rows). ProjectID rides in the payload because the event is
// cross-cutting (entity_kind='study_run', envelope project_id null), so the
// fold needs the project id from here to write the namespaced rows. RunAt is
// an RFC 3339 string stored verbatim. MaterialsHashes carries small SHA-256
// hex digests, NOT blobs; ResponsesDir is a filesystem-path pointer.
type StudyRunRecordedPayload struct {
	RunID           string             `json:"run_id"`
	ProjectID       string             `json:"project_id"`
	Name            string             `json:"name"`
	Assay           string             `json:"assay"`
	ItemID          string             `json:"item_id,omitempty"`
	Image           string             `json:"image,omitempty"`
	ImageDigest     string             `json:"image_digest,omitempty"`
	Status          string             `json:"status"`
	Error           string             `json:"error,omitempty"`
	StudyDigest     string             `json:"study_digest,omitempty"`
	MaterialsHashes map[string]string  `json:"materials_hashes,omitempty"`
	ModelID         string             `json:"model_id,omitempty"`
	ModelVersion    string             `json:"model_version,omitempty"`
	ResponsesDir    string             `json:"responses_dir,omitempty"`
	RunAt           string             `json:"run_at"`
	Rows            []StudyRunScoreRow `json:"rows"`
}

func (StudyRunRecordedPayload) EventType() string { return "StudyRunRecorded" }

// ── Gate runs (corpos-gate trend storage) ────────────────────────────────

// GateCheckResult is one check's outcome within a gate run — one row of the
// child projection proj_gate_check_results. Mirrors the `items` schema of the
// `checks` array in blueprints/events/GateRunCompleted.json. Fields carry no
// omitempty so every check serialises all six columns — the schema requires
// name/tier/ok/skipped/duration_ms and the fold reads them positionally into
// proj_gate_check_results. Note is the one optional field (empty when the
// check has nothing to add).
type GateCheckResult struct {
	Name       string `json:"name"`
	Tier       string `json:"tier"`
	OK         bool   `json:"ok"`
	Skipped    bool   `json:"skipped"`
	DurationMS int    `json:"duration_ms"`
	Note       string `json:"note,omitempty"`
}

// GateRunCompletedPayload mirrors blueprints/events/GateRunCompleted.json —
// the aggregated verdict of one corpos-gate run plus its per-check grid.
// Emitted by the measure surface's gate_run action and folded into
// proj_gate_runs (parent, 1 row) + proj_gate_check_results (child, N rows) so
// coverage/mutation/verdict become an event-sourced time series per project.
// Project rides in the payload because the event is cross-cutting
// (entity_kind='gate_run', envelope project_id null), so the fold needs the
// project from here to write the namespaced rows. The nullable metric fields
// (CoveragePct / BranchPct / MutationScore) use -1 to signal N/A — the check
// did not run, was skipped, or produced no parseable metric. The verdict is
// the SAME internal/gate.Run core the CLI uses, so the persisted trend is
// faithful to `corpos-gate run`.
type GateRunCompletedPayload struct {
	Project       string            `json:"project"`
	CommitSHA     string            `json:"commit_sha,omitempty"`
	Tier          string            `json:"tier"`
	OverallOK     bool              `json:"overall_ok"`
	CoveragePct   float64           `json:"coverage_pct"`
	BranchPct     float64           `json:"branch_pct"`
	MutationScore float64           `json:"mutation_score"`
	DurationMS    int               `json:"duration_ms"`
	Checks        []GateCheckResult `json:"checks"`
}

func (GateRunCompletedPayload) EventType() string { return "GateRunCompleted" }

// MetricRecordedPayload mirrors blueprints/events/MetricRecorded.json.
// MetricValue is polymorphic in the schema (bool|number|string|null) and
// is therefore typed as json.RawMessage in Go — the schema validator sees
// the original JSON bytes; consumers parse with the appropriate concrete
// type per rubric metric.
type MetricRecordedPayload struct {
	RunID       string      `json:"run_id"`
	StepID      string      `json:"step_id"`
	MetricName  string      `json:"metric_name"`
	MetricValue MetricValue `json:"metric_value"`
	Rationale   *string     `json:"rationale,omitempty"`
}

func (MetricRecordedPayload) EventType() string { return "MetricRecorded" }

// MetricValue carries the polymorphic metric_value field from
// MetricRecordedPayload. Construct via MetricBool / MetricNumber /
// MetricString / MetricNull; the JSON marshaler emits the underlying
// concrete shape. The schema validator accepts any of the four; the
// rubric definition for a given metric name pins which one is expected.
type MetricValue struct {
	raw []byte // pre-marshaled JSON; "true" / "42" / "\"fast\"" / "null"
}

// MetricBool wraps a bool value for MetricRecordedPayload.MetricValue.
func MetricBool(b bool) MetricValue {
	if b {
		return MetricValue{raw: []byte("true")}
	}
	return MetricValue{raw: []byte("false")}
}

// MetricNumber wraps a float64 value (covers int and float metrics —
// JSON numbers don't distinguish).
func MetricNumber(n float64) MetricValue {
	return MetricValue{raw: []byte(formatFloat(n))}
}

// MetricString wraps a categorical string value.
func MetricString(s string) MetricValue {
	// Use json.Marshal so embedded quotes and unicode are escaped properly.
	data, _ := jsonMarshalString(s) // string marshal never errors in practice
	return MetricValue{raw: data}
}

// MetricNull is the explicit null variant — used for metrics that couldn't
// be scored (e.g. "completion-only smoke run, no rubric score").
func MetricNull() MetricValue { return MetricValue{raw: []byte("null")} }

// MarshalJSON returns the raw JSON bytes verbatim. If the value was
// zero-constructed (raw nil), emit "null" so the schema validator sees a
// valid JSON token rather than an empty payload field.
func (m MetricValue) MarshalJSON() ([]byte, error) {
	if len(m.raw) == 0 {
		return []byte("null"), nil
	}
	return m.raw, nil
}

// CurationCandidatePromotedPayload mirrors blueprints/events/CurationCandidatePromoted.json.
// Emitted when a candidate transitions from status='pending' to status='promoted',
// either via MCP curation_promote (human-driven) or curate-rescore (auto-promote
// at quality_score >= 0.85).
type CurationCandidatePromotedPayload struct {
	CandidateID           int64    `json:"candidate_id"`
	PointerID             int64    `json:"pointer_id"`
	SourceType            string   `json:"source_type"`
	SourceRef             string   `json:"source_ref"`
	Origin                string   `json:"origin"`
	QualityScore          *float64 `json:"quality_score,omitempty"`
	PromotedAutomatically bool     `json:"promoted_automatically"`
}

func (CurationCandidatePromotedPayload) EventType() string { return "CurationCandidatePromoted" }

// CurationCandidateRejectedPayload mirrors blueprints/events/CurationCandidateRejected.json.
// Emitted when a candidate transitions from status='pending' to status='rejected'
// via MCP curation_reject. Reason is required (the handler enforces non-empty).
type CurationCandidateRejectedPayload struct {
	CandidateID int64  `json:"candidate_id"`
	SourceType  string `json:"source_type"`
	SourceRef   string `json:"source_ref"`
	Origin      string `json:"origin"`
	Reason      string `json:"reason"`
}

func (CurationCandidateRejectedPayload) EventType() string { return "CurationCandidateRejected" }

// ── Arc-close filing review ──────────────────────────────────────────────

// FilingDecisionSummary is the lean summary of one FilingDecision the
// reviewer produced. Mirrors blueprints/events/ArcCloseFilingReviewed.json's
// decisions[] item. Full payload bodies are intentionally NOT carried on
// this event — the dispatcher's downstream forge calls (BugReported /
// TaskCreated / etc.) emit their own events with the bodies.
type FilingDecisionSummary struct {
	Action     string  `json:"action"`
	Confidence float64 `json:"confidence"`
	Reasoning  string  `json:"reasoning"`
}

// ArcCloseFilingReviewedPayload mirrors
// blueprints/events/ArcCloseFilingReviewed.json. Emitted by the
// work.review_arc_for_filing handler on every successful fire (status
// == "fired"). Debounced and short-circuit paths do NOT emit — only
// fires that ran through Qwen and produced a parsed decision set land
// here. The row is the per-fire training corpus for the eventual
// classifier swap per docs/ARC_CLOSE_FILING_REVIEW.md §Telemetry.
type ArcCloseFilingReviewedPayload struct {
	SessionID              string                  `json:"session_id"`
	Triggers               []string                `json:"triggers"`
	SnapshotTruncated      bool                    `json:"snapshot_truncated"`
	SnapshotTokenCount     int                     `json:"snapshot_token_count"`
	SnapshotMessageCount   int                     `json:"snapshot_message_count"`
	ArcSummary             *string                 `json:"arc_summary,omitempty"`
	Decisions              []FilingDecisionSummary `json:"decisions"`
	AutoExecuteCount       int                     `json:"auto_execute_count"`
	SurfaceForConfirmCount int                     `json:"surface_for_confirm_count"`
	SkipCount              int                     `json:"skip_count"`
	LatencyMS              int64                   `json:"latency_ms"`
	InputTokens            *int64                  `json:"input_tokens,omitempty"`
	OutputTokens           *int64                  `json:"output_tokens,omitempty"`
	// Chain arc-close-filing-review-dedupe-and-noise-reduction
	// telemetry fields (added 2026-05-21 for F5 retrospective
	// measurement). All three count fields default to 0 / null when
	// the dedupe pipeline didn't engage (e.g., Qwen returned an
	// empty decisions array organically).
	F4RejectedCount           int            `json:"f4_rejected_count"`
	F4RejectedReasons         map[string]int `json:"f4_rejected_reasons,omitempty"`
	F2DedupedCount            int            `json:"f2_deduped_count"`
	F3SameSessionDedupedCount int            `json:"f3_same_session_deduped_count"`
	// Chain arc-close-decision-authoring-split telemetry (T7). Both
	// default 0 when the split didn't engage this fire.
	StagedForAuthoringCount int `json:"staged_for_authoring_count,omitempty"`
	EnrichExistingCount     int `json:"enrich_existing_count,omitempty"`
}

func (ArcCloseFilingReviewedPayload) EventType() string { return "ArcCloseFilingReviewed" }

// ArcCloseAuthoringResolvedPayload mirrors
// blueprints/events/ArcCloseAuthoringResolved.json. Emitted by the
// unreviewed-fallback sweep (chain arc-close-decision-authoring-split
// T5/T7) when it resolves a session's staged body-heavy decisions; the
// author-vs-fallback split is the seat-strength instrument.
type ArcCloseAuthoringResolvedPayload struct {
	SessionID           string `json:"session_id"`
	AuthoredCount       int    `json:"authored_count"`
	FallbackForgedCount int    `json:"fallback_forged_count"`
	RowsMarked          int    `json:"rows_marked,omitempty"`
}

func (ArcCloseAuthoringResolvedPayload) EventType() string { return "ArcCloseAuthoringResolved" }

// ArcCloseFilingReviewSubstrateAuditCompletedPayload mirrors
// blueprints/events/ArcCloseFilingReviewSubstrateAuditCompleted.json.
// Emitted by chain arc-close-filing-review T7 retrospective. Sister
// to the substrate-quartet audit events (Architecture / Telemetry /
// ReferenceResolution / MLCapability): same Findings array shape;
// records the substrate audit against the chain's completion_condition
// items (a)-(h). The substrate proves itself self-hosting by emitting
// its own closing audit through the events ledger that the meta-tool
// handlers also write through.
type ArcCloseFilingReviewSubstrateAuditCompletedPayload struct {
	AuditDoc             string                     `json:"audit_doc"`
	Summary              string                     `json:"summary"`
	RecommendedNextPhase *string                    `json:"recommended_next_phase,omitempty"`
	Findings             []ArchitectureAuditFinding `json:"findings"`
}

func (ArcCloseFilingReviewSubstrateAuditCompletedPayload) EventType() string {
	return "ArcCloseFilingReviewSubstrateAuditCompleted"
}

// ArcReviewListenerFiredPayload mirrors
// blueprints/events/ArcReviewListenerFired.json. Emitted by the
// arcreview SubstrateReviewObserver on every Observe call — both
// fire and skip outcomes — so investigations have a structured single-
// source signal independent of process boundaries. Closes bug
// `stdio-process-observer-logs-not-captured-in-central-log-file`: stdio
// MCP processes' stderr is consumed by Claude Code and doesn't land in
// /tmp/toolkit-http.log, so the prior obs.Logger calls were invisible
// from centralized debugging; routing observer activity through the
// events ledger gives one read surface regardless of which process
// emitted the trigger.
type ArcReviewListenerFiredPayload struct {
	TriggerEventID   string  `json:"trigger_event_id"`
	TriggerEventType string  `json:"trigger_event_type"`
	TriggerSlug      string  `json:"trigger_slug"`
	ProjectID        *string `json:"project_id,omitempty"`
	Status           string  `json:"status"`
	SkipReason       *string `json:"skip_reason,omitempty"`
	SessionID        *string `json:"session_id,omitempty"`
	ReviewEventID    *string `json:"review_event_id,omitempty"`
	ReviewStatus     *string `json:"review_status,omitempty"`
	DecisionsCount   *int    `json:"decisions_count,omitempty"`
}

func (ArcReviewListenerFiredPayload) EventType() string { return "ArcReviewListenerFired" }

// MemoryWrittenPayload mirrors blueprints/events/MemoryWritten.json.
// Emitted by forge(schema_name="memory", ...) on every successful
// create. Consumed by the arc-close-filing-review telemetry pipeline
// (memory_write filing-precision tracking) and the future
// trained-arc-close-filing-classifier corpus per
// docs/MEMORY_SUBSTRATE.md §7.
//
// Edit / delete events are deferred — no consumer needs them today
// (T3's materialization hook discovers deletions by walking the vault,
// not by listening). When a consumer materializes, add
// MemoryEditedPayload + MemoryDeletedPayload as siblings; the schema /
// payload pattern is well-trodden.
type MemoryWrittenPayload struct {
	Name            string  `json:"name"`
	Kind            string  `json:"kind"`
	Description     string  `json:"description"`
	Source          *string `json:"source,omitempty"`
	ObservedFirst   *string `json:"observed_first,omitempty"`
	VaultPath       string  `json:"vault_path"`
	BodyLengthBytes int     `json:"body_length_bytes"`
}

func (MemoryWrittenPayload) EventType() string { return "MemoryWritten" }

// ArtifactWrittenPayload mirrors blueprints/events/ArtifactWritten.json.
// Emitted (OPT-IN) when fs.write commits a file in record mode — the write
// half of the write->read provenance loop. fs.read provenance mode folds these
// (matched on file_path) into a file's mutation history. The default fs.write
// does NOT emit; the entity reference (entity_kind=artifact, entity_slug=abs
// path) and the write intent (rationale) live in the envelope.
type ArtifactWrittenPayload struct {
	FilePath     string `json:"file_path"`
	BytesWritten int    `json:"bytes_written"`
	LineCount    int    `json:"line_count"`
	Created      bool   `json:"created"`
}

func (ArtifactWrittenPayload) EventType() string { return "ArtifactWritten" }

// ArtifactEditedPayload mirrors blueprints/events/ArtifactEdited.json.
// Emitted (OPT-IN) when fs.edit commits a replacement in record mode — the edit
// half of the write->read provenance loop. fs.read provenance mode folds these
// (matched on file_path) into a file's mutation history. The default fs.edit
// does NOT emit; the entity reference (entity_kind=artifact, entity_slug=abs
// path) and the edit intent (rationale) live in the envelope.
type ArtifactEditedPayload struct {
	FilePath     string `json:"file_path"`
	Replacements int    `json:"replacements"`
	Created      bool   `json:"created"`
}

func (ArtifactEditedPayload) EventType() string { return "ArtifactEdited" }

// ArtifactMovedPayload mirrors blueprints/events/ArtifactMoved.json. Emitted
// (OPT-IN) when fs.move relocates a path in record mode. Carries the source +
// final destination so fs.read provenance mode can fold the relocation into a
// file's mutation history. The default fs.move does NOT emit; the entity
// reference (entity_kind=artifact, entity_slug=destination abs path) and the
// move intent (rationale) live in the envelope.
type ArtifactMovedPayload struct {
	Source      string `json:"source"`
	Dest        string `json:"dest"`
	IsDir       bool   `json:"is_dir"`
	CrossDevice bool   `json:"cross_device"`
}

func (ArtifactMovedPayload) EventType() string { return "ArtifactMoved" }

// ArtifactRemovedPayload mirrors blueprints/events/ArtifactRemoved.json. Emitted
// (OPT-IN) when fs.remove deletes a path in record mode. Carries the removed
// path so fs.read provenance mode can record the deletion in a file's history.
// The default fs.remove does NOT emit; the entity reference (entity_kind=
// artifact, entity_slug=removed abs path) and the removal intent (rationale)
// live in the envelope.
type ArtifactRemovedPayload struct {
	FilePath string `json:"file_path"`
	WasDir   bool   `json:"was_dir"`
}

func (ArtifactRemovedPayload) EventType() string { return "ArtifactRemoved" }

// BatchOpResult is the per-op outcome record inside a BatchExecutedPayload.
// Mirrors blueprints/events/BatchExecuted.json's ops[] item shape.
//
// EventID is set only when the op's handler ran successfully AND the
// outer tx committed (rolled_back=false on the parent payload). On a
// rolled-back batch, ok=true entries retain their position/action/
// rationale for audit but EventID is empty because their cascade events
// were rolled back with the outer tx.
//
// ErrorKind / ErrorMessage are populated only when ok=false. ErrorKind
// is a short classifier ("TaskNotFound", "InvalidTransition",
// "UnknownAction", "NotAllowlisted") suitable for grouping in dashboards;
// ErrorMessage carries the verbatim handler error for diagnosis.
type BatchOpResult struct {
	Position     int     `json:"position"`
	Action       string  `json:"action"`
	OK           bool    `json:"ok"`
	Rationale    string  `json:"rationale"`
	EventID      *string `json:"event_id,omitempty"`
	ErrorKind    *string `json:"error_kind,omitempty"`
	ErrorMessage *string `json:"error_message,omitempty"`
}

// BatchExecutedPayload mirrors blueprints/events/BatchExecuted.json.
// Emitted once per work.batch call, carrying the per-op outcomes plus
// the batch-as-a-whole status. Cascade events (TaskCompleted,
// TaskStarted, BugResolved, etc.) emit with refs.caused_by_event_id
// pointing at this BatchExecuted's event_id — same pattern as
// ChainCreated → cascaded TaskCreated.
//
// RolledBack is true iff the outer tx rolled back, which only happens
// when ContinueOnError=false AND Failed>0. When true, ok=true entries
// in Ops DID execute but their writes did NOT commit; their EventID is
// empty and the events table contains neither the BatchExecuted row
// nor the cascade rows. (BatchExecuted itself is part of the rolled-
// back tx, so a rolled-back batch produces zero events — the payload
// shape is documented here for the case where a future T-task records
// rollback synthetically.)
//
// BatchRationale is distinct from envelope.rationale per the T1 design
// audit (docs/WORK_BATCHING_AND_FORGE_TEMPLATES_PLAN.md §3.1): envelope
// rationale answers "why this MCP call," BatchRationale answers "why
// these ops were grouped." Often duplicative; both are recorded when
// supplied so listeners can distinguish the grains.
type BatchExecutedPayload struct {
	OpCount         int             `json:"op_count"`
	Succeeded       int             `json:"succeeded"`
	Failed          int             `json:"failed"`
	ContinueOnError bool            `json:"continue_on_error"`
	RolledBack      bool            `json:"rolled_back"`
	BatchRationale  *string         `json:"batch_rationale,omitempty"`
	Ops             []BatchOpResult `json:"ops"`
}

func (BatchExecutedPayload) EventType() string { return "BatchExecuted" }

// TaskHandoffClosed mirrors the closed sub-object on
// blueprints/events/TaskHandoff.json. The fields denormalize the
// task_complete sub-op's inputs (slug, commit_sha, handoff_output,
// rationale) plus the cascade TaskCompleted event's id so a single
// TaskHandoff read answers "what closed and why."
type TaskHandoffClosed struct {
	TaskSlug      string  `json:"task_slug"`
	CommitSHA     *string `json:"commit_sha,omitempty"`
	HandoffOutput *string `json:"handoff_output,omitempty"`
	Rationale     string  `json:"rationale"`
	EventID       *string `json:"event_id,omitempty"`
}

// TaskHandoffStarted mirrors the started sub-object. Carries the next
// task's slug + the per-op rationale + the cascade TaskTransitioned
// event id (pending→active for the typical seam; the auto-clear path
// can fire an additional blocked→pending cascade whose id is NOT
// returned to this payload — the started.event_id points at the final
// pending→active transition).
type TaskHandoffStarted struct {
	TaskSlug  string  `json:"task_slug"`
	Rationale string  `json:"rationale"`
	EventID   *string `json:"event_id,omitempty"`
}

// TaskHandoffPayload mirrors blueprints/events/TaskHandoff.json. The
// composite event emitted by work.lifecycle_step in addition to the
// underlying TaskCompleted + TaskStarted cascade events — per the T1
// design audit (PLAN.md §3.2), the composite is the edge event listeners
// subscribe to when they want one signal per chain-task seam; the per-op
// events stay unchanged so existing subscriptions don't regress. Both
// ops MUST belong to the same chain; lifecycle_step rejects cross-chain
// handoffs at the validation gate before dispatch.
//
// The envelope's entity points at the closing task (the chain's
// perspective — the seam belongs to the task that just finished); the
// started task lives in Started.
type TaskHandoffPayload struct {
	ChainSlug string             `json:"chain_slug"`
	Closed    TaskHandoffClosed  `json:"closed"`
	Started   TaskHandoffStarted `json:"started"`
}

func (TaskHandoffPayload) EventType() string { return "TaskHandoff" }

// RetrospectiveForgedPayload mirrors blueprints/events/
// RetrospectiveForged.json. Emitted by forge(schema_name='retrospective')
// on every successful create. The chain's substrate-side closure event
// (architecture-audit, telemetry-audit, etc.) is the audit-completed
// counterpart; RetrospectiveForged is the content-side event recording
// that the prose retrospective doc landed on disk + indexed into
// knowledge_pointers.
type RetrospectiveForgedPayload struct {
	ChainSlug    string `json:"chain_slug"`
	ChainID      *int64 `json:"chain_id,omitempty"`
	FilePath     string `json:"file_path"`
	SectionCount int    `json:"section_count"`
}

func (RetrospectiveForgedPayload) EventType() string { return "RetrospectiveForged" }

// ReportCardForgedPayload mirrors blueprints/events/ReportCardForged.json.
// Sister event to RetrospectiveForged for the fresh sub-agent's post-
// chain external grade. Same shape so consumers that join chain-closure
// with chain-grading get a uniform projection.
type ReportCardForgedPayload struct {
	ChainSlug    string `json:"chain_slug"`
	ChainID      *int64 `json:"chain_id,omitempty"`
	FilePath     string `json:"file_path"`
	SectionCount int    `json:"section_count"`
}

func (ReportCardForgedPayload) EventType() string { return "ReportCardForged" }

// MigrationForgedPayload mirrors blueprints/events/MigrationForged.json.
// Emitted by forge(schema_name='migration') on every successful create —
// fresh-mint AND idempotent re-run. The Idempotent flag distinguishes
// the two so consumers (dashboard schema-history view, chain-close-
// completeness audit) don't have to infer from event ordering. FilePaths
// is always a two-element slice [canonical, mirror] — the schema's
// minItems=maxItems=2 keeps consumers from defensive-coding for arbitrary
// lengths.
type MigrationForgedPayload struct {
	MigrationNumber int      `json:"migration_number"`
	Slug            string   `json:"slug"`
	FilePaths       []string `json:"file_paths"`
	DocstringLength int      `json:"docstring_length"`
	SQLLength       int      `json:"sql_length"`
	Idempotent      bool     `json:"idempotent,omitempty"`
}

func (MigrationForgedPayload) EventType() string { return "MigrationForged" }

// BenchmarkForgedPayload mirrors blueprints/events/BenchmarkForged.json.
// Emitted by forge(schema_name='bench') on every successful create —
// both fresh and idempotent re-runs. Records the harness's identity +
// the canonical paths bench_run will use; the actual run-time output
// lives on BenchmarkDiff. T6 of work-batching-and-forge-templates.
type BenchmarkForgedPayload struct {
	Slug             string `json:"slug"`
	BinaryPath       string `json:"binary_path"`
	BaselineJSONPath string `json:"baseline_json_path"`
	ParseOutputAs    string `json:"parse_output_as"`
	TimeoutMs        *int   `json:"timeout_ms,omitempty"`
	Idempotent       bool   `json:"idempotent,omitempty"`
	// FlagSetJSON + GateMetrics were added (chain 311 T7 Stage 6 P2-A,
	// 2026-05-29) so the payload carries EVERY bench_harnesses column the
	// fold materializes — the pre-P2-A payload omitted them, leaving the
	// row un-reconstructable from the event alone. OPTIONAL on the wire
	// (omitempty, no schema minLength): the two historical pre-bump
	// BenchmarkForged events lack them and stay valid baseline (grandfathered),
	// while every new event populates them. The bench_harnesses fold treats
	// FlagSetJSON=="" as the pre-bump discriminator and skips those events
	// (the live row they describe is reproduced by the migration-085 synthetic
	// backfill instead). FlagSetJSON is the canonical normalized array form
	// (forge.NormalizeFlagSet); GateMetrics is the comma-separated gate
	// allowlist ("" = report-only).
	FlagSetJSON string `json:"flag_set_json,omitempty"`
	GateMetrics string `json:"gate_metrics,omitempty"`
}

func (BenchmarkForgedPayload) EventType() string { return "BenchmarkForged" }

// BenchmarkMetricDiff is one entry in BenchmarkDiffPayload.Metrics —
// baseline + observed + per-shape deltas. Baseline/Observed are
// json.RawMessage so the wire shape preserves numeric vs string vs
// null per metric without flattening to a single type. DeltaAbs +
// DeltaPct are nil for non-numeric or null-side metrics.
type BenchmarkMetricDiff struct {
	Name     string          `json:"name"`
	Baseline json.RawMessage `json:"baseline"`
	Observed json.RawMessage `json:"observed"`
	DeltaAbs *float64        `json:"delta_abs,omitempty"`
	DeltaPct *float64        `json:"delta_pct,omitempty"`
}

// BenchmarkDiffPayload mirrors blueprints/events/BenchmarkDiff.json.
// Emitted by measure.bench_run on every completed run (success OR
// error). On error, Metrics is empty and Error names the failure
// mode; RunLatencyMs equals TimeoutMs when the run timed out.
type BenchmarkDiffPayload struct {
	Slug            string                `json:"slug"`
	Metrics         []BenchmarkMetricDiff `json:"metrics"`
	RunLatencyMs    int                   `json:"run_latency_ms"`
	BaselineUpdated bool                  `json:"baseline_updated"`
	StderrLogPath   string                `json:"stderr_log_path,omitempty"`
	Error           string                `json:"error,omitempty"`
	// GatePassed is the deterministic-metric gate verdict: nil when the
	// harness declared no gate_metrics (report-only) or on the error
	// paths (gate not computed); else whether every matched gate metric
	// had a zero delta. GateFailures names the drifted gate metrics (or a
	// misconfiguration line when the patterns matched none). The durable
	// regression signal a perf-regression classifier / drift chart reads.
	GatePassed   *bool    `json:"gate_passed,omitempty"`
	GateFailures []string `json:"gate_failures,omitempty"`
}

func (BenchmarkDiffPayload) EventType() string { return "BenchmarkDiff" }

// BenchmarkBaselineUpdatedPayload mirrors blueprints/events/
// BenchmarkBaselineUpdated.json. Composite with BenchmarkDiff: an
// update_baseline=true bench_run emits both in one transaction
// (BenchmarkBaselineUpdated first, then BenchmarkDiff). PreviousBaseline
// SHA256 is empty on first-time baseline writes; the events ledger
// then has the round-trip audit shape (was-X, now-Y).
type BenchmarkBaselineUpdatedPayload struct {
	Slug                   string `json:"slug"`
	BaselineJSONPath       string `json:"baseline_json_path"`
	PreviousBaselineSHA256 string `json:"previous_baseline_sha256,omitempty"`
	NewBaselineSHA256      string `json:"new_baseline_sha256"`
}

func (BenchmarkBaselineUpdatedPayload) EventType() string { return "BenchmarkBaselineUpdated" }

// ChainAndTasksForgedPayload mirrors blueprints/events/
// ChainAndTasksForged.json. Composite event emitted by forge(chain)
// when the call lands tasks alongside the chain in one tx. Sits next
// to the per-task TaskCreated events the chain hook also emits; this
// one is the parent+children grouping signal. Mode is the analytics
// discriminator — T8's retro reports on full-object vs pipe-delimited
// adoption rates. T7 of work-batching-and-forge-templates.
type ChainAndTasksForgedPayload struct {
	ChainSlug         string   `json:"chain_slug"`
	ChainID           *int64   `json:"chain_id,omitempty"`
	TaskSlugs         []string `json:"task_slugs"`
	TaskCount         int      `json:"task_count"`
	Mode              string   `json:"mode"`
	PerTaskRationales []string `json:"per_task_rationales,omitempty"`
}

func (ChainAndTasksForgedPayload) EventType() string { return "ChainAndTasksForged" }
