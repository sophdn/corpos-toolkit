package grounding

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/telemetry"
)

// --- moved from cmd/grounding-events-processor/main.go (grounding HTTP-ingestion refactor) ---

func transcriptTimestampToSQLite(ts string) (string, bool) {
	if ts == "" {
		return "", false
	}
	parsed, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		// Some transcripts may omit subseconds; try the no-ns form.
		parsed, err = time.Parse(time.RFC3339, ts)
		if err != nil {
			return "", false
		}
	}
	return parsed.UTC().Format("2006-01-02 15:04:05"), true
}

type fileResult struct {
	events       int
	gaps         int
	interactions int
	resolutions  int
}

// emittedEvent carries the linkage between a search-call grounding_events
// row and the prompt arc + span needed to fan out per-row click_kind
// interactions and to build the trajectory of a later terminal resolution.
type emittedEvent struct {
	DBID       int64
	PromptID   string
	SpanID     string
	SessionID  string
	SourceRefs []string
}

// interactionKey indexes the per-prompt-arc interaction-id list so the
// terminal-event resolution sweep can reconstruct query_interaction_ids
// without re-SELECTing. Shared between emitGroundingAndInteractions
// (writer) and emitResolutions (reader).
type interactionKey struct {
	PromptID  string
	SourceRef string
}

// processFile is the per-session orchestration:
//  1. parse the JSONL into events + raw entries (prompt_id threaded);
//  2. open one write tx for the whole file so detection writes are
//     atomic with the grounding_events insert they depend on;
//  3. insert each grounding_events row via InsertGroundingEventTx,
//     capture the returned id, run click_kind detectors against the
//     parsed entries, and emit per-row query_interactions;
//  4. collect terminal-event tool_use calls, emit resolved-from
//     interactions, look up write_event_ids in the events table, and
//     emit one query_resolutions row per terminal call.
//
// The whole tx is rolled back on the first hard error. The processor
// is idempotent: re-running over the same JSONL hits ON CONFLICT DO
// NOTHING on grounding_events, UPSERTs on query_interactions (the
// (span_id, source_ref, click_kind) triple), and the pre-check on
// query_resolutions skips already-resolved (entity, prompt_id) pairs.
// Result is the exported per-file count summary returned by the public entry points.
type Result struct {
	Events       int `json:"events"`
	Gaps         int `json:"gaps"`
	Interactions int `json:"interactions"`
	Resolutions  int `json:"resolutions"`
}

// Parse extracts grounding events + raw transcript entries from a session JSONL file
// (host-side; opens NO database). The binary runs this, then either writes locally
// (ProcessFile, --db fallback) or POSTs events+entries to the container's
// ingest_grounding action — the post-cutover single-writer path.
func Parse(path string) ([]ProcessedEvent, []jsonlEntry, error) { return processSession(path) }

// ProcessParsed runs the emit half — grounding-row insert + click_kind interactions +
// terminal resolutions, with the read-side projection fold — inside the caller's write
// tx. The container's ingest_grounding action calls this so parsed grounding output
// lands via the SINGLE-WRITER container, never a host direct-open of the canonical DB.
func ProcessParsed(ctx context.Context, tx *sql.Tx, projectID, parentSpanID string, preserveTranscriptTimes bool, events []ProcessedEvent, entries []jsonlEntry) (Result, error) {
	byPrompt, interactionIDs, phase1, err := emitGroundingAndInteractions(ctx, tx, projectID, parentSpanID, events, entries, preserveTranscriptTimes)
	if err != nil {
		return Result{}, err
	}
	phase2, err := emitResolutions(ctx, tx, projectID, entries, byPrompt, interactionIDs)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Events:       phase1.events,
		Gaps:         phase1.gaps,
		Interactions: phase1.interactions + phase2.interactions,
		Resolutions:  phase2.resolutions,
	}, nil
}

// ProcessFile parses path and runs the emit in one write tx — the --db / fallback path
// (single-writer only; e.g. a container-down one-shot). New deployments route through
// the container via Parse + the ingest_grounding action instead.
func ProcessFile(ctx context.Context, pool *db.Pool, path, projectID, parentSpanID string, preserveTranscriptTimes bool) (Result, error) {
	events, entries, err := Parse(path)
	if err != nil {
		return Result{}, err
	}
	var r Result
	werr := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		var e error
		r, e = ProcessParsed(ctx, tx, projectID, parentSpanID, preserveTranscriptTimes, events, entries)
		return e
	})
	return r, werr
}

// emitGroundingAndInteractions runs phase 1 of processFile: per-event
// grounding insert + click_kind detection + per-row interaction emit.
// Returns the per-prompt grounding-event map and per-(prompt, source_ref)
// interaction-id map that phase 2 needs to build query_resolutions
// trajectories. fileResult aggregates per-row counts the orchestrator
// reports.
func emitGroundingAndInteractions(
	ctx context.Context, tx *sql.Tx, projectID, parentSpanID string,
	events []ProcessedEvent, entries []jsonlEntry, preserveTranscriptTimes bool,
) (map[string][]emittedEvent, map[interactionKey][]int64, fileResult, error) {
	byPrompt := map[string][]emittedEvent{}
	interactionIDs := map[interactionKey][]int64{}
	var fr fileResult

	for _, ev := range events {
		spanID := ev.CallID
		var promptIDPtr *string
		if ev.PromptID != "" {
			p := ev.PromptID
			promptIDPtr = &p
		}
		var parentSpanIDPtr *string
		switch {
		case ev.ParentSpanID != "":
			p := ev.ParentSpanID
			parentSpanIDPtr = &p
		case parentSpanID != "":
			p := parentSpanID
			parentSpanIDPtr = &p
		}
		var createdAtPtr *string
		if preserveTranscriptTimes {
			// Prefer the tool_result timestamp — that's when the
			// handler-exit online emit would have fired and is what
			// inference_invocations.created_at aligns with too. Fall back
			// to tool_use timestamp when the result wasn't paired
			// (in-flight session-end shape).
			ts := ev.ToolResultTimestamp
			if ts == "" {
				ts = ev.ToolUseTimestamp
			}
			if formatted, ok := transcriptTimestampToSQLite(ts); ok {
				createdAtPtr = &formatted
			}
		}
		// InsertGroundingEventTxBackstop: when an online-emit row
		// already covers this search (matched by action + source_refs
		// + time window), enrich it with the processor-computed
		// fields (next_turn_has_output, used, prompt_id,
		// parent_span_id) and return its id; otherwise fall through
		// to the normal insert. Collapses the steady-state duplicate
		// while keeping the processor as a backstop for online-emit-
		// failed and restart-mid-fire sessions. Closes bug
		// `grounding-events-online-emit-and-stop-hook-processor-create-duplicate-rows-per-search`.
		var queryTextPtr *string
		if ev.QueryText != "" {
			queryTextPtr = &ev.QueryText
		}
		// Per-handler runtime telemetry (Pass1/Pass2LatencyMS for vault_search;
		// QwenFellBack/KiwixHitsIn/KiwixHitsOut for kiwix_search) is deliberately
		// left unset here, so it stores NULL on a backfilled row. Those are
		// online-emit-only measurements — the live handler computes them at call
		// time and the transcript JSONL the processor reconstructs from never
		// carried them. So `pass1_latency_ms IS NULL` is the reliable
		// online-emit-vs-backfill discriminator: online emit always records it
		// (pinned by knowledge.TestHandleVaultSearch_WritesTelemetryRow + the
		// project_id='' convention), so a NULL means this row was reconstructed
		// here, not that the reranker failed to run. Latency analyses must filter
		// to `pass1_latency_ms IS NOT NULL` (the complete online subset). Closes
		// bug 959 (the NULLs are backfill-by-construction, not a write-path gap).
		id, err := db.InsertGroundingEventTxBackstop(ctx, tx, db.GroundingEventInsert{
			ProjectID:         projectID,
			SessionID:         ev.SessionID,
			CallID:            ev.CallID,
			Action:            ev.Action,
			ResultsCount:      ev.ResultsCount,
			SourceRefs:        ev.SourceRefs,
			NextTurnHasOutput: ev.NextTurnHasOutput,
			Used:              ev.Used,
			SpanID:            spanID,
			PromptID:          promptIDPtr,
			ParentSpanID:      parentSpanIDPtr,
			QueryText:         queryTextPtr,
			CreatedAt:         createdAtPtr,
		}, 0 /* default dedupe window */)
		if err != nil {
			fmt.Fprintf(os.Stderr, "insert %s: %v\n", ev.CallID, err)
			continue
		}
		fr.events++
		if ev.ResultsCount == 0 && ev.NextTurnHasOutput {
			fr.gaps++
		}
		byPrompt[ev.PromptID] = append(byPrompt[ev.PromptID], emittedEvent{
			DBID: id, PromptID: ev.PromptID, SpanID: spanID,
			SessionID: ev.SessionID, SourceRefs: ev.SourceRefs,
		})

		for _, in := range detectInteractions(ev, entries) {
			iid, err := emitInteraction(ctx, tx, ev, in, id)
			if err != nil {
				fmt.Fprintf(os.Stderr, "emit interaction %s/%s/%s: %v\n",
					spanID, in.SourceRef, in.ClickKind, err)
				continue
			}
			fr.interactions++
			k := interactionKey{ev.PromptID, in.SourceRef}
			interactionIDs[k] = append(interactionIDs[k], iid)
		}
	}
	return byPrompt, interactionIDs, fr, nil
}

// emitResolutions runs phase 2 of processFile: walk every terminal-event
// tool_use call in the session, build the (write_event_ids,
// grounding_event_ids, query_interaction_ids) trajectory, emit one
// query_resolutions row per (entity, prompt) pair plus any resolved-from
// query_interactions the terminal rationale fired. Pre-SELECT keeps the
// emit idempotent across processor reruns.
func emitResolutions(
	ctx context.Context, tx *sql.Tx, projectID string,
	entries []jsonlEntry,
	byPrompt map[string][]emittedEvent,
	interactionIDs map[interactionKey][]int64,
) (fileResult, error) {
	var fr fileResult
	terms := collectTerminalEvents(entries, projectID)
	for _, te := range terms {
		if te.PromptID == "" {
			continue
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM query_resolutions
				WHERE entity_kind = ? AND entity_slug = ?
				  AND entity_project_id = ? AND prompt_id = ?)`,
			te.EntityKind, te.EntitySlug, te.EntityProjectID, te.PromptID,
		).Scan(&exists); err != nil {
			return fr, fmt.Errorf("query_resolutions pre-check: %w", err)
		}
		if exists == 1 {
			continue
		}

		scoped := byPrompt[te.PromptID]
		var groundingIDs []int64
		var qiIDs []int64
		for _, ge := range scoped {
			groundingIDs = append(groundingIDs, ge.DBID)
			for _, in := range detectResolvedFrom(ge.SourceRefs, te.Rationale) {
				evShim := ProcessedEvent{
					SessionID: ge.SessionID,
					PromptID:  te.PromptID,
					CallID:    ge.SpanID,
				}
				iid, err := emitInteraction(ctx, tx, evShim, in, ge.DBID)
				if err != nil {
					fmt.Fprintf(os.Stderr, "emit resolved-from %s/%s: %v\n",
						ge.SpanID, in.SourceRef, err)
					continue
				}
				fr.interactions++
				k := interactionKey{te.PromptID, in.SourceRef}
				interactionIDs[k] = append(interactionIDs[k], iid)
				qiIDs = append(qiIDs, iid)
			}
			for _, ref := range ge.SourceRefs {
				qiIDs = append(qiIDs, interactionIDs[interactionKey{te.PromptID, ref}]...)
			}
		}
		qiIDs = dedupeInt64(qiIDs)

		wids, err := lookupWriteEventIDs(ctx, tx, te, 48)
		if err != nil {
			fmt.Fprintf(os.Stderr, "lookup write_event_ids for %s/%s: %v\n",
				te.EntityKind, te.EntitySlug, err)
		}

		args := telemetry.ResolutionArgs{
			PromptID:            te.PromptID,
			SessionID:           sessionIDFromScoped(scoped, te),
			SpanID:              te.ToolUseID,
			EntityKind:          te.EntityKind,
			EntitySlug:          te.EntitySlug,
			EntityProjectID:     te.EntityProjectID,
			OutcomeKind:         te.OutcomeKind,
			WriteEventIDs:       wids,
			GroundingEventIDs:   dedupeInt64(groundingIDs),
			QueryInteractionIDs: qiIDs,
		}
		if _, err := telemetry.EmitResolution(ctx, tx, args); err != nil {
			fmt.Fprintf(os.Stderr, "emit resolution %s/%s: %v\n",
				te.EntityKind, te.EntitySlug, err)
			continue
		}
		fr.resolutions++
	}
	return fr, nil
}

// emitInteraction is the thin adapter from our in-memory Interaction
// shape to the telemetry.EmitInteraction call. Centralized here so
// resolved-from and the per-event detectors share the same construction.
func emitInteraction(ctx context.Context, tx *sql.Tx, ev ProcessedEvent, in Interaction, groundingID int64) (int64, error) {
	args := telemetry.InteractionArgs{
		GroundingEventID: groundingID,
		SourceRef:        in.SourceRef,
		ClickKind:        in.ClickKind,
		SpanID:           ev.CallID,
		SessionID:        ev.SessionID,
	}
	if in.Position > 0 {
		p := in.Position
		args.Position = &p
	}
	if ev.PromptID != "" {
		p := ev.PromptID
		args.PromptID = &p
	}
	if in.CitationKind != nil {
		ck := *in.CitationKind
		args.CitationKind = &ck
	}
	if in.CitationQuoteChars != nil {
		c := *in.CitationQuoteChars
		args.CitationQuoteChars = &c
	}
	if in.DwellMS != nil {
		d := *in.DwellMS
		args.DwellMSEstimate = &d
	}
	return telemetry.EmitInteraction(ctx, tx, args)
}

// sessionIDFromScoped returns the session_id of the first scoped
// grounding event in the prompt arc, falling back to the terminal
// event's prompt_id-prefixed session-from-path when no scoped events
// exist (a terminal-without-prior-search case). The session_id column
// on query_resolutions is NOT NULL.
func sessionIDFromScoped(scoped []emittedEvent, te terminalEvent) string {
	if len(scoped) > 0 {
		return scoped[0].SessionID
	}
	return te.PromptID
}

func dedupeInt64(in []int64) []int64 {
	if len(in) <= 1 {
		return in
	}
	seen := make(map[int64]struct{}, len(in))
	out := make([]int64, 0, len(in))
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// collectFiles resolves the --session / --dir flags into a sorted file list.
// Exactly one of the two flags must be set.
