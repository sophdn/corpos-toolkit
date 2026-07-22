package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"toolkit/internal/db"
	"toolkit/internal/dispatch"
	"toolkit/internal/events"
)

// BatchOp is one entry in a HandleBatch params.ops list. Per the T1
// design audit (docs/WORK_BATCHING_AND_FORGE_TEMPLATES_PLAN.md §3.1),
// rationale is mandatory on every op — the per-op grain is what gets
// stamped on each cascade event's envelope rationale, so the events
// ledger stays audit-grade across batches.
type BatchOp struct {
	Op        string          `json:"op"`
	Params    json.RawMessage `json:"params"`
	Rationale string          `json:"rationale"`
}

// BatchParams is the wire shape for the work.batch action's params
// object. The envelope's own rationale (Args.Rationale on the dispatch
// envelope) is the "why this MCP call" grain; BatchRationale here is the
// optional "why these ops were grouped" grain. Both are recorded on the
// BatchExecuted payload so consumers can distinguish them.
//
// ContinueOnError defaults false. False means abort-on-first-error with
// the whole outer tx rolled back (prior ops UNDONE). True means run every
// op regardless; per-op failures land in the BatchExecuted ops[] entries
// and the outer tx commits.
type BatchParams struct {
	Ops             []BatchOp `json:"ops"`
	ContinueOnError bool      `json:"continue_on_error"`
	BatchRationale  string    `json:"batch_rationale,omitempty"`
}

// BatchResult is the work.batch action's response envelope. Mirrors
// the BatchExecutedPayload (the same shape lands both on the wire and
// in the events ledger). RolledBack=true means the outer tx aborted
// and none of the ops persisted, even those marked OK=true (their
// cascade events landed on a transaction that was rolled back).
type BatchResult struct {
	OK              bool                   `json:"ok"`
	OpCount         int                    `json:"op_count"`
	Succeeded       int                    `json:"succeeded"`
	Failed          int                    `json:"failed"`
	ContinueOnError bool                   `json:"continue_on_error"`
	RolledBack      bool                   `json:"rolled_back"`
	BatchEventID    string                 `json:"batch_event_id,omitempty"`
	Ops             []events.BatchOpResult `json:"ops"`
	Error           string                 `json:"error,omitempty"`
}

// batchAllowlist names the work-surface actions a single batch call may
// contain. v1 is the smallest superset that covers the T2 smoke fixture
// plus T3 (lifecycle_step sugar) and T7 (chain+tasks atomic create —
// though T7 may build its own atomic primitive rather than going
// through batch; the allowlist accepts forge_edit either way for the
// retro/report-card edits T8/T9 will dog-food).
//
// Expanding the allowlist is a small per-action change: add the action
// name here AND a tx-aware handler dispatch entry in batchDispatchInTx
// below. Reads outside the allowlist (chain_status, task_read,
// bug_list, etc.) are not batched in v1; their per-call latency is
// dominated by network round-trips, not handler work, and grouping
// them adds little value.
var batchAllowlist = map[string]struct{}{
	"task_complete": {},
	"task_start":    {},
	"bug_resolve":   {},
	"forge_edit":    {},
	// T1 of batch-allowlist-widening: chain_close lands the canonical
	// chain-finalize pattern (close last task + close chain) in one
	// MCP call. ChainCloseInTx variant routes the close's UPDATE +
	// ChainClosed event emit through the outer tx so a follow-on op's
	// failure correctly rolls back the close.
	"chain_close": {},
	// forge (create) scoped to schema_name in {bug, suggestion, task}.
	// The per-op validator in HandleForgeInTx enforces the schema_name
	// gate; the allowlist just declares the op as accepted. Chain
	// creation with tasks is served by forge(chain, tasks=[{…}]) directly
	// (one call, atomic), not by batching forge(chain) + forge(task).
	"forge": {},
}

// AllowedBatchOps returns the v1 allowlist as a sorted slice. Used by
// the action-discovery surface so callers can see the accepted op
// set without trial-and-error.
func AllowedBatchOps() []string {
	out := make([]string, 0, len(batchAllowlist))
	for k := range batchAllowlist {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// HandleBatch executes a list of mutating work-surface ops inside a
// single outer write tx. Each sub-op flows through its tx-aware handler
// variant (HandleTaskCompleteInTx, HandleBugResolveInTx, etc.) so the
// whole batch commits or rolls back atomically.
//
// Two execution modes:
//
//   - ContinueOnError=false (default): abort on first error. The outer tx
//     rolls back; every previously-succeeded op is UNDONE. The response
//     records ok=true for ops that ran without error but rolled_back=true
//     on the envelope to flag the rollback.
//   - ContinueOnError=true: run every op regardless. Per-op failures
//     land in ops[].error_*; the outer tx commits. Surviving ok=true
//     entries persist normally.
//
// Per-op rationale is mandatory; the batch is rejected pre-execution
// (no DB writes) when any op omits rationale, naming the offending
// index. Unknown actions and non-allowlisted actions are rejected with
// distinct error_kinds ("UnknownAction" vs "NotAllowlisted") so
// dashboards can group them.
//
// Cross-op read-after-write within a batch: the forge(task) create path
// (createTaskInTx) routes its position = MAX+1 read through the OUTER tx,
// so a run of forge(task) ops into one chain gets sequential positions
// (each task's fold is visible to the next task's read within the tx).
// chain is not batch-creatable, so the chain lookup always resolves an
// already-committed chain; chain+tasks is served by forge(chain,
// tasks=[…]) directly. The remaining tx-aware handlers' reads
// (resolveTaskChain, fetchBugStatusAndProject, etc.) still go through
// pool.DB(); for their use cases that does not bite (task lifecycle
// handoffs and forge_edit batches operate on distinct rows). Migrate
// those read helpers to take *sql.Tx if a real-world caller needs
// read-through-tx on those paths too.
func HandleBatch(ctx context.Context, deps TableDeps, project string, params json.RawMessage) (BatchResult, error) {
	var p BatchParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return BatchResult{}, fmt.Errorf("parse batch params: %w", err)
		}
	}
	if len(p.Ops) == 0 {
		return BatchResult{Error: "batch requires a non-empty ops list"}, nil
	}

	// Pre-flight validation (runs BEFORE the outer tx opens, so a
	// rejection here costs nothing in DB state):
	//   - per-op rationale required (the chain-policy gate at the
	//     dispatch boundary only sees the envelope rationale; per-op
	//     gate lives here)
	//   - op name must be in the allowlist
	for i, op := range p.Ops {
		if strings.TrimSpace(op.Op) == "" {
			return BatchResult{Error: fmt.Sprintf("ops[%d]: missing op name", i)}, nil
		}
		if strings.TrimSpace(op.Rationale) == "" {
			return BatchResult{
				Error: fmt.Sprintf("ops[%d] (%s): per-op rationale required; the batch is rejected pre-execution so no DB writes happen", i, op.Op),
			}, nil
		}
		if _, ok := batchAllowlist[op.Op]; !ok {
			return BatchResult{
				Error: fmt.Sprintf("ops[%d]: action %q is not batch-allowlisted; accepted: %s", i, op.Op, strings.Join(AllowedBatchOps(), ", ")),
			}, nil
		}
	}

	pool := deps.Pool
	if pool == nil {
		return BatchResult{Error: "batch requires a DB pool"}, nil
	}

	results := make([]events.BatchOpResult, len(p.Ops))
	succeeded := 0
	failed := 0
	rolledBack := false

	txErr := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		for i, op := range p.Ops {
			// Each sub-op sees its own per-op rationale in ctx so its
			// cascade event's envelope.rationale records the per-op
			// grain (not the batch's envelope-level rationale).
			opCtx := events.WithRationale(ctx, op.Rationale)
			eventID, opErr := batchDispatchInTx(opCtx, tx, deps, project, op)
			res := events.BatchOpResult{
				Position:  i,
				Action:    op.Op,
				Rationale: op.Rationale,
			}
			if opErr != nil {
				res.OK = false
				kind := classifyBatchError(opErr)
				msg := opErr.Error()
				res.ErrorKind = &kind
				res.ErrorMessage = &msg
				failed++
				results[i] = res
				if !p.ContinueOnError {
					rolledBack = true
					// Return a sentinel error so WithWrite rolls back.
					// The per-op results are already populated.
					return errBatchAborted
				}
				continue
			}
			res.OK = true
			if eventID != "" {
				id := eventID
				res.EventID = &id
			}
			succeeded++
			results[i] = res
		}
		return nil
	})

	// Build the response envelope. When the outer tx rolled back, the
	// previously-stamped event_ids on succeeded ops are stripped — the
	// cascade events were rolled back along with the outer tx, so
	// returning their ids would be misleading.
	if rolledBack {
		for i := range results {
			if results[i].OK {
				results[i].EventID = nil
			}
		}
	}

	out := BatchResult{
		OK:              !rolledBack && txErr == nil,
		OpCount:         len(p.Ops),
		Succeeded:       succeeded,
		Failed:          failed,
		ContinueOnError: p.ContinueOnError,
		RolledBack:      rolledBack,
		Ops:             results,
	}

	// The BatchExecuted event ONLY emits when the outer tx committed
	// (continue_on_error=true OR no op failed). A rolled-back batch
	// produces zero events — the BatchExecuted record would be inside
	// the same rolled-back tx. The envelope shape is documented in
	// blueprints/events/BatchExecuted.json so a future synthetic-event
	// backfill could record rolled-back batches if needed.
	if !rolledBack && txErr == nil {
		var batchRationale *string
		if p.BatchRationale != "" {
			r := p.BatchRationale
			batchRationale = &r
		}
		// Emit BatchExecuted in its own tx — at this point the outer
		// tx has committed (WithWrite returned nil), so we need a fresh
		// tx for the event. The cascade events' caused_by_event_id
		// chain is therefore best-effort: the per-op event_ids are
		// recorded in BatchExecuted.ops but the per-op events
		// themselves were emitted before BatchExecuted's event_id
		// existed, so they cannot set refs.caused_by_event_id back.
		// Acceptable for v1 — the BatchExecuted payload's ops[].event_id
		// is the forward index; future T-task can add a sibling
		// "batch_id" envelope ref if reverse traversal becomes hot.
		batchPayload := events.BatchExecutedPayload{
			OpCount:         len(p.Ops),
			Succeeded:       succeeded,
			Failed:          failed,
			ContinueOnError: p.ContinueOnError,
			RolledBack:      false,
			BatchRationale:  batchRationale,
			Ops:             results,
		}
		entity := events.NewCrossCuttingEntityRef("batch", batchSlug(project, len(p.Ops)))
		_ = pool.WithWrite(ctx, func(tx *sql.Tx) error {
			id, err := events.Emit(ctx, tx, events.EmitArgs{
				Entity:  entity,
				Payload: batchPayload,
			})
			if err == nil {
				out.BatchEventID = id
			}
			return err
		})
	}

	if txErr != nil && !errors.Is(txErr, errBatchAborted) {
		// Genuine error from WithWrite (begin tx / commit failed); not
		// the sentinel we use to trigger rollback. Surface it.
		return BatchResult{}, txErr
	}
	return out, nil
}

// errBatchAborted is the sentinel returned from the WithWrite closure
// when ContinueOnError=false and an op failed. WithWrite treats any
// non-nil error as a rollback signal; this sentinel lets HandleBatch
// distinguish "we deliberately rolled back" from "the tx machinery
// itself failed."
var errBatchAborted = errors.New("batch: aborted on first op failure")

// batchDispatchInTx routes one op to its tx-aware handler variant.
// Returns the cascade event id and any error. Only allowlisted ops
// reach here (the caller pre-validates) — the default arm therefore
// fires only on a programmer error (allowlist and dispatch out of
// sync) and emits a clear panic-equivalent error.
func batchDispatchInTx(ctx context.Context, tx *sql.Tx, deps TableDeps, project string, op BatchOp) (string, error) {
	pool := deps.Pool
	switch op.Op {
	case "task_complete":
		res, eventID, err := HandleTaskCompleteInTx(ctx, tx, pool, project, op.Params)
		if err != nil {
			return "", err
		}
		if res.Error != "" {
			return "", errors.New(res.Error)
		}
		return eventID, nil
	case "task_start":
		res, eventID, err := HandleTaskStartInTx(ctx, tx, pool, project, op.Params)
		if err != nil {
			return "", err
		}
		if res.Error != "" {
			return "", errors.New(res.Error)
		}
		return eventID, nil
	case "bug_resolve":
		res, eventID, err := HandleBugResolveInTx(ctx, tx, pool, project, op.Params)
		if err != nil {
			return "", err
		}
		if res.Error != "" {
			return "", errors.New(res.Error)
		}
		return eventID, nil
	case "forge_edit":
		if deps.Schemas == nil {
			return "", errors.New("forge_edit: schemas registry not configured")
		}
		// Route covered edits through the construct umbrella on this outer tx.
		// The forge in-tx fallback was removed when forge archived (chain 311 T7
		// Stage 6 P2-C.2) — the seam is always wired by the composition root, so
		// a nil seam is a wiring bug, not a degraded mode.
		if deps.ForgeEditInTx == nil {
			return "", errors.New("forge_edit: construct in-tx seam not wired (ForgeEditInTx is nil)")
		}
		return deps.ForgeEditInTx(ctx, tx, project, op.Params)
	case "chain_close":
		res, eventID, err := HandleChainCloseInTx(ctx, tx, pool, project, op.Params)
		if err != nil {
			return "", err
		}
		if res.Error != "" {
			return "", errors.New(res.Error)
		}
		return eventID, nil
	case "forge":
		if deps.Schemas == nil {
			return "", errors.New("forge: schemas registry not configured")
		}
		// Route covered creates through the construct umbrella on this outer tx.
		// The seam does its own knowledge-pointer sync (construct.IndexUpsertOnCreateInTx).
		// The forge in-tx fallback was removed when forge archived (chain 311 T7
		// Stage 6 P2-C.2) — the seam is always wired by the composition root.
		if deps.ForgeCreateInTx == nil {
			return "", errors.New("forge: construct in-tx seam not wired (ForgeCreateInTx is nil)")
		}
		return deps.ForgeCreateInTx(ctx, tx, project, op.Params)
	default:
		// Unreachable when the allowlist + dispatch table are in sync;
		// returning a clear error rather than panicking matches the
		// rest of the handler surface's failure ergonomics.
		return "", fmt.Errorf("batch: action %q has no tx-aware dispatch entry (allowlist/dispatch drift)", op.Op)
	}
}

// classifyBatchError returns a short error_kind classifier for the
// BatchOpResult. Best-effort string match; the classifications are for
// dashboard grouping, not for programmatic flow control (the verbatim
// error_message field carries the authoritative detail).
func classifyBatchError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not found") || strings.Contains(msg, "no rows"):
		return "NotFound"
	case strings.Contains(msg, "requires") || strings.Contains(msg, "missing"):
		return "MissingParam"
	case strings.Contains(msg, "invalid transition") || strings.Contains(msg, "cannot transition"):
		return "InvalidTransition"
	case strings.Contains(msg, "schema_not_found") || strings.Contains(msg, "unknown param"):
		return "SchemaError"
	default:
		return "HandlerError"
	}
}

// batchSlug derives a stable synthetic slug for the BatchExecuted
// event's entity. Per the BatchExecuted.json doc the slug is
// observability-only (the events table indexes on event_id, not slug);
// kind='batch' + a span-derived suffix is enough to disambiguate.
func batchSlug(project string, opCount int) string {
	if project == "" {
		project = "unscoped"
	}
	return fmt.Sprintf("%s-batch-%dops", project, opCount)
}

// dispatchAdapt is the wiring shim used by BuildTable to register
// HandleBatch through dispatch.Adapt. Kept here so the deps capture is
// local and the table.go entry stays one-line.
func dispatchAdaptBatch(deps TableDeps) dispatch.Handler {
	return dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (BatchResult, error) {
		return HandleBatch(ctx, deps, project, params)
	})
}

// Silence unused-import for db when it's referenced only transitively.
var _ *db.Pool

// ── Action-doc descriptor (registry, action_doc.go) ────────
//
// `ops` Type is left empty (derived). BatchParams.Ops is []BatchOp, so the
// derived type is "object[]" — the single enumerated delta the contract landed
// in T4 (the former actionSpecs declared "object"; see
// docs/ACTION_DOC_CONTRACT.md and TestExtract_BatchOpsLandedDelta).
var batchDoc = ActionDoc{
	Purpose: "Execute a list of allowlisted mutating work-surface ops inside a single outer write tx. Per-op rationale required (rejected pre-execution if missing). Default mode aborts on first error and rolls back the whole batch; pass `continue_on_error: true` to commit each op independently and report per-op outcomes. v1 allowlist: task_complete, task_start, bug_resolve, forge_edit. Cross-op read-after-write within a batch flows through the outer tx for the tx-aware reads (resolveTaskChain, lookupChainProject, taskHasStructuralBlockers).",
	Params: []DocParam{
		{Name: "ops", Required: true, Description: "List of {op, params, rationale} triples. op is the work-surface action name; params is the action's normal params object; rationale is the per-op 'why'. All three are required on every entry."},
		{Name: "continue_on_error", Required: false, Description: "Default false (abort-on-first-error with outer-tx rollback). True runs every op regardless; failures land in per-op status without rollback."},
		{Name: "batch_rationale", Required: false, Description: "Optional 'why these ops were grouped' note, recorded on the BatchExecuted event payload distinct from the envelope-level rationale ('why this MCP call')."},
	},
	Example: `{"ops":[{"op":"task_complete","params":{"task_id":123,"commit_sha":"abc1234"},"rationale":"T1 deliverable landed"},{"op":"task_start","params":{"task_id":124},"rationale":"T2 picked up"}],"continue_on_error":false}`,
	Notes: "Current allowlist: task_complete, task_start, bug_resolve, forge_edit, chain_close, forge. Other mutating actions reject pre-execution with NotAllowlisted error_kind. Expanding the allowlist requires adding the action to batchAllowlist + a tx-aware dispatch entry in batchDispatchInTx (go/internal/work/batch.go). chain_close enables the canonical chain-finalize (batch([task_complete, chain_close])). forge (create) is scoped to schema_name in {bug, suggestion, task}; other schemas reject pre-dispatch with a scope error.\n\n" +
		"Creating a chain WITH its tasks: do NOT batch forge(chain) + forge(task) ops — chain is not batch-creatable, so a forge(chain) op rejects and (abort-on-first-error) rolls back the whole batch. Use forge(chain, tasks=[{full-object task entries}]) directly, which creates the chain and all its tasks atomically in one call. work.batch's forge(task) support is for ADDING tasks to an already-existing chain in bulk; createTaskInTx computes position = MAX+1 through the outer tx, so a run of batched tasks gets sequential positions rather than all colliding on the same slot.\n\n" +
		"Rollback semantics: default abort-on-first-error uses the outer write tx — prior ops UNDONE on rollback. Reads in tx-aware handlers (resolveTaskChain, lookupChainProject, taskHasStructuralBlockers) flow through the outer tx via the queryer interface so op2's reads see op1's pending writes; this closes the auto-clear-blocked-then-start path that bit the first refactor pass.\n\n" +
		"Event emit: each sub-op emits its existing event type unchanged (TaskCompleted, TaskStarted, BugResolved, etc.) with the per-op rationale on envelope.rationale. One additional BatchExecuted event emits per batch carrying the per-op outcomes; rolled-back batches produce zero events (the BatchExecuted record would be inside the same rolled-back tx).",
	EnvelopeRequirements: []ActionEnvelopeReq{{
		Field:               "rationale",
		Required:            true,
		Reason:              "Dispatcher policy gate (action-manifests/dispatch-policy.toml). Envelope-level rationale is the 'why this MCP call' grain — separate from each op's per-op rationale (enforced by the HandleBatch validator). Both grains record on the BatchExecuted event payload.",
		AppliesToActorKinds: []string{"agent"},
	}},
	Returns: &ActionReturn{Shape: "BatchResult", Description: "Per-op outcomes plus the batch-level envelope: ok, op_count, succeeded, failed, continue_on_error, rolled_back, batch_event_id, ops[]. Each ops[] entry carries position, action, ok, rationale, and either event_id (on commit) or error_kind+error_message (on failure). When rolled_back=true, ok=true entries lose their event_id (the cascade events were rolled back with the outer tx)."},
}
