// Package work's table.go assembles the dispatch.Table for the work
// meta-tool. Keeping the action wiring next to the handler code (bug.go,
// task.go, chain.go, roadmap.go) means a new action only touches the
// owning surface — the cmd/toolkit-server main wiring becomes pure
// assembly. Pairs with the parallel BuildTable functions in
// internal/admin, internal/measure, internal/knowledge.
//
// Every action wires its typed handler through dispatch.Adapt so the
// only place result types widen to `any` is the registration seam.
package work

import (
	"context"
	"database/sql"
	"encoding/json"

	"toolkit/internal/db"
	"toolkit/internal/dispatch"
	"toolkit/internal/eventbus"
	"toolkit/internal/forge/registry"
)

// TableDeps bundles everything the work dispatch table needs at build
// time. ForgeOnCreate / ForgeOnEdit are pre-composed by the caller (main
// builds the chain of notifiers — SSE bus publish + FTS5 index sync —
// per the wiring-stays-in-main split). Pool, Schemas, Bus can each be
// nil for degraded modes.
type TableDeps struct {
	Pool    *db.Pool
	Schemas *registry.Registry
	Bus     *eventbus.Bus
	// ForgeCreateInTx / ForgeEditInTx route batch's forge create/edit ops
	// through the construct umbrella on the outer batch tx. Wired from main
	// (work can't import construct — the construct→work cycle): they decode the
	// op's raw params, validate via construct.PrepareForge*, and call
	// construct.CreateInTx / UpdateInTx; they return the cascade event id (or a
	// Go error that rolls back batch's tx). REQUIRED for batch forge/forge_edit
	// ops post-archive — the forge in-tx fallback is gone, so a nil seam makes
	// those batch ops error rather than silently degrade.
	ForgeCreateInTx func(ctx context.Context, tx *sql.Tx, project string, rawParams json.RawMessage) (string, error)
	ForgeEditInTx   func(ctx context.Context, tx *sql.Tx, project string, rawParams json.RawMessage) (string, error)
}

// BuildTable returns the work surface's dispatch.Table. Forge actions
// register only when a schema registry is present; chain/task/bug/roadmap
// register only when a DB pool is present.
func BuildTable(deps TableDeps) dispatch.Table {
	table := dispatch.Table{}

	// event_schema is the record(events[]) discovery surface — it lists the
	// event-type enum + returns per-type payload schemas from the embedded
	// catalog. Read-only, needs no deps, so it registers unconditionally
	// (available even on a degraded, pool-less table).
	table["event_schema"] = dispatchAdaptEventSchema()

	// The forge / forge_edit / forge_delete / forge_schema / forge_schemas
	// actions are registered by the composition root (cmd/toolkit-server) on
	// the construct umbrella, NOT here: the construct-routed handlers need
	// construct.Deps, and work can't import construct (the construct→work import
	// cycle). forge archived in chain 311 T7 Stage 6 P2-C.2; the record-sugar
	// surface (record action) + the main-wired construct adapters are the live
	// write + introspection surface now.
	if deps.Pool == nil {
		return table
	}
	pool := deps.Pool
	bus := deps.Bus

	// Chain lifecycle.
	table["chain_status"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (ChainStatusResult, error) {
		return HandleChainStatus(ctx, pool, project, params)
	})
	table["chain_state"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (ChainStateResult, error) {
		return HandleChainState(ctx, pool, project, params)
	})
	table["chain_find"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (ChainFindResult, error) {
		return HandleChainFind(ctx, pool, project, params)
	})
	table["chain_close"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (ChainCloseResult, error) {
		out, err := HandleChainClose(ctx, pool, project, params)
		if err == nil && bus != nil && out.OK {
			bus.Publish(eventbus.TaskTransitioned(project, out.ChainSlug, "closed"))
		}
		return out, err
	})

	// Task lifecycle.
	table["task_read"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (TaskReadResult, error) {
		return HandleTaskRead(ctx, pool, project, params)
	})
	table["task_search"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (TaskListResult, error) {
		return HandleTaskSearch(ctx, pool, project, params)
	})
	table["task_list"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (TaskListResult, error) {
		// task_list is the cross-cutting "list open tasks" verb — it lists
		// across chains (empty params → all tasks) instead of dead-ending the
		// way task_search does when given neither a chain nor a pattern.
		return HandleTaskList(ctx, pool, project, params)
	})
	table["task_start"] = transitionEntry(pool, bus, HandleTaskStart, "active")
	table["task_complete"] = transitionEntry(pool, bus, HandleTaskComplete, "closed")
	table["task_cancel"] = transitionEntry(pool, bus, HandleTaskCancel, "cancelled")
	table["task_reopen"] = transitionEntry(pool, bus, HandleTaskReopen, "pending")
	// task_unstart reverts active → pending — the mid-flight pause/revert
	// path that closes bug
	// `task-lifecycle-no-clean-mid-flight-pause-or-revert` (1461).
	table["task_unstart"] = transitionEntry(pool, bus, HandleTaskUnstart, "pending")
	table["task_stamp_sha"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (ShaStampResult, error) {
		return HandleTaskStampSHA(ctx, pool, project, params)
	})
	table["task_block"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (TaskTransitionResult, error) {
		return HandleTaskBlock(ctx, pool, project, params)
	})
	table["task_unblock"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (TaskTransitionResult, error) {
		return HandleTaskUnblock(ctx, pool, project, params)
	})
	table["task_blockers"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (TaskBlockersResult, error) {
		return HandleTaskBlockers(ctx, pool, project, params)
	})
	table["task_edit"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (TaskEditResult, error) {
		return HandleTaskEdit(ctx, pool, project, params)
	})

	// Bug CRUD.
	table["bug_list"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (BugListResult, error) {
		return HandleBugList(ctx, pool, project, params)
	})
	table["bug_read"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (BugReadResult, error) {
		return HandleBugRead(ctx, pool, project, params)
	})
	table["bug_resolve"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (BugResolveResult, error) {
		out, err := HandleBugResolve(ctx, pool, project, params)
		if err == nil && bus != nil && out.OK {
			bus.Publish(eventbus.BugResolved(project, out.Slug, out.Status))
		}
		return out, err
	})
	table["bug_reopen"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (BugResolveResult, error) {
		return HandleBugReopen(ctx, pool, project, params)
	})
	table["bug_stamp_sha"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (ShaStampResult, error) {
		return HandleBugStampSHA(ctx, pool, project, params)
	})

	// Suggestion CRUD — agent-suggestion-box chain. Native vocabulary
	// (adopted / deferred / rejected) enforced in HandleSuggestionResolve;
	// no shared helper across bug_* / suggestion_* per the chain's
	// duplication-not-abstraction directive.
	table["suggestion_list"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (SuggestionListResult, error) {
		return HandleSuggestionList(ctx, pool, project, params)
	})
	table["suggestion_read"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (SuggestionReadResult, error) {
		return HandleSuggestionRead(ctx, pool, project, params)
	})
	table["suggestion_resolve"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (SuggestionResolveResult, error) {
		out, err := HandleSuggestionResolve(ctx, pool, project, params)
		if err == nil && bus != nil && out.OK {
			bus.Publish(eventbus.SuggestionResolved(project, out.Slug, out.Status))
		}
		return out, err
	})
	table["suggestion_reopen"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (SuggestionResolveResult, error) {
		return HandleSuggestionReopen(ctx, pool, project, params)
	})
	table["suggestion_stamp_sha"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (ShaStampResult, error) {
		return HandleSuggestionStampSHA(ctx, pool, project, params)
	})

	// Trained-model lifecycle (ml-capability-substrate T4).
	table["trained_model_list"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (TrainedModelListResult, error) {
		return HandleTrainedModelList(ctx, pool, project, params)
	})
	table["trained_model_promote"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (TrainedModelPromoteResult, error) {
		return HandleTrainedModelPromote(ctx, pool, project, params)
	})
	table["trained_model_retire"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (TrainedModelRetireResult, error) {
		return HandleTrainedModelRetire(ctx, pool, project, params)
	})

	// Roadmap.
	table["roadmap_list"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (RoadmapListEntries, error) {
		return HandleRoadmapList(ctx, pool, project, params)
	})
	table["roadmap_set"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (RoadmapSetResult, error) {
		return HandleRoadmapSet(ctx, pool, project, params)
	})
	table["roadmap_preview_set"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (RoadmapPreviewResult, error) {
		return HandleRoadmapPreviewSet(ctx, pool, project, params)
	})
	table["roadmap_insert"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (RoadmapInsertResult, error) {
		return HandleRoadmapInsert(ctx, pool, project, params)
	})
	table["roadmap_update"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (RoadmapUpdateResult, error) {
		return HandleRoadmapUpdate(ctx, pool, project, params)
	})
	table["roadmap_diff"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (RoadmapDiff, error) {
		return HandleRoadmapDiff(ctx, pool, project, params)
	})
	table["roadmap_mark_reassessed"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (RoadmapMarkReassessedResult, error) {
		return HandleRoadmapMarkReassessed(ctx, pool, project, params)
	})

	// Recent-activity resume briefing ("where did we leave off"). Reads the
	// events ledger as a timeline — the cross-status recency view chain_status
	// structurally can't provide. where_we_left_off is an alias (same handler).
	table["recent_activity"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (RecentActivityResult, error) {
		return HandleRecentActivity(ctx, pool, project, params)
	})
	table["where_we_left_off"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (RecentActivityResult, error) {
		return HandleRecentActivity(ctx, pool, project, params)
	})

	// Chain dependency edges + computed roadmap (dependency-driven-roadmap).
	// chain_deps is a direct-write table; roadmap_plan computes the order
	// from the edge graph (topo-sort, ready/blocked, cycle detection).
	table["chain_dep_add"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (ChainDepResult, error) {
		return HandleChainDepAdd(ctx, pool, project, params)
	})
	table["chain_dep_remove"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (ChainDepResult, error) {
		return HandleChainDepRemove(ctx, pool, project, params)
	})
	table["chain_dep_list"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (ChainDepListResult, error) {
		return HandleChainDepList(ctx, pool, project, params)
	})
	table["roadmap_plan"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (RoadmapPlanResult, error) {
		return HandleRoadmapPlan(ctx, pool, project, params)
	})

	// Work actions discovery — closes bug 1335.
	table["work_actions"] = dispatch.Adapt(HandleWorkActions)

	// One-call open-work portfolio rollup (list-verb symmetry): the four
	// list verbs' counts (bugs / chains / tasks / suggestions) in a single
	// read, optionally per-project.
	table["work_summary"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (WorkSummaryResult, error) {
		return HandleWorkSummary(ctx, pool, project, params)
	})

	// Batched-ops dispatcher. Wraps an outer write tx around N
	// allowlisted sub-ops so a single MCP round-trip can collapse the
	// most-frequent multi-call patterns (close-task + start-next,
	// resolve-bug + start-task, three-task forge_edit on retrospective
	// follow-up fields). See go/internal/work/batch.go::HandleBatch
	// for the documented semantics (per-op rationale, partial-failure
	// modes, cross-op read-after-write limitation).
	if deps.Schemas != nil {
		table["batch"] = dispatchAdaptBatch(deps)
		// lifecycle_step is sugar over batch: builds a 2-op envelope
		// (task_complete + task_start), validates the same-chain
		// invariant, dispatches HandleBatch, and emits a composite
		// TaskHandoff event after the batch commits. ZERO duplicated
		// logic vs batch per the T3 chain constraint — the file is
		// envelope construction + dispatch + composite emit only.
		table["lifecycle_step"] = dispatchAdaptLifecycleStep(deps)
	}

	// record(events[]) — the forge-v2 event-submission surface (chain
	// emit-surface-forge-v2 T3). Submits a heterogeneous array of typed
	// events to the hot local draft (the events ledger): thin-fast-local
	// validation on the shared closed enum, append + fold in one tx,
	// consolidated per-event results with partial-success. Needs only the
	// pool (guaranteed present past the early return above) — not the forge
	// schema registry, since it takes typed events directly rather than
	// forge's schema+fields sugar.
	table["record"] = dispatchAdaptRecord(deps)
	return table
}

// transitionEntry wires a task lifecycle handler into the table and
// emits a TaskTransitioned SSE event on success. The toStatus arg is
// the status the handler transitions to — duplicated here so we don't
// have to re-parse the response envelope to find it.
func transitionEntry(pool *db.Pool, bus *eventbus.Bus, h func(context.Context, *db.Pool, string, json.RawMessage) (TaskTransitionResult, error), toStatus string) dispatch.Handler {
	return dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (TaskTransitionResult, error) {
		out, err := h(ctx, pool, project, params)
		if err == nil && bus != nil && out.OK {
			bus.Publish(eventbus.TaskTransitioned(project, out.Slug, toStatus))
		}
		return out, err
	})
}
