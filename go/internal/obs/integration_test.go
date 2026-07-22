package obs_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/obs"
	"toolkit/internal/testutil"
)

// TestSpanID_PropagatesAcrossReadAndWriteHandlers exercises the load-
// bearing cross-substrate contract from chain agent-first-substrate T5's
// acceptance criteria item (d):
//
//	"The end-to-end integration test in this task MUST include a scenario
//	 where vault_search and task_complete share span_id; assert the
//	 span_id matches across the grounding_events row and the events row."
//
// The test directly exercises the contract — span_id is minted ONCE,
// stored on ctx, and downstream handlers (here represented by the
// db-level insert/emit primitives the production handlers funnel
// through) inherit it without regenerating. A regression where any
// handler calls a fresh UUID mid-request would surface here as a span
// mismatch.
//
// This is the regression gate for the sibling chain
// query-telemetry-substrate's read-write join (TT2 / TT3): if the IDs
// drift, the sibling's training-data extraction produces orphaned rows.
func TestSpanID_PropagatesAcrossReadAndWriteHandlers(t *testing.T) {
	pool := testutil.NewTestDB(t)

	// Simulate dispatch boundary: mint one root span, stamp the actor,
	// keep the resulting ctx alive across both calls. This is exactly
	// what dispatch.DispatchWithOptions does in production — wrapping
	// every handler invocation in a single span scope.
	ctx := events.WithActor(context.Background(), events.Actor{Kind: "agent", ID: "claude-opus-4-7"})
	ctx, end := obs.SpanStart(ctx, "test.dispatch")
	defer end(nil)

	rootSpan := obs.SpanFromContext(ctx)
	if rootSpan == nil {
		t.Fatal("expected obs span after SpanStart")
	}

	// Read-side: simulate vault_search emitting a grounding_events row
	// at search-exit. The production helper recordGroundingEvent in
	// go/internal/knowledge/grounding.go reads span_id from the same
	// ctx and writes a row with that span_id.
	groundingErr := db.InsertGroundingEvent(ctx, pool, db.GroundingEventInsert{
		ProjectID:    "",
		SessionID:    rootSpan.TraceID,
		CallID:       rootSpan.ID,
		Action:       "vault_search",
		ResultsCount: 3,
		SourceRefs:   []string{"decisions/x.md", "learnings/y.md"},
		SpanID:       rootSpan.ID,
	})
	if groundingErr != nil {
		t.Fatalf("insert grounding event: %v", groundingErr)
	}

	// Write-side: simulate task_complete emitting an event row. The
	// production handler funnels through events.Emit, which reads
	// span_id from the same ctx (events.SpanIDFromContext) — obs's
	// WithSpan stamps that key when SpanStart fires, so this works
	// without re-passing the id.
	rationale := "fixed: regression gate established"
	commitSHA := "deadbee"
	var eventID string
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		id, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity: events.NewEntityRef("bug", "x", "mcp-servers"),
			Payload: events.BugResolvedPayload{
				Kind:      "fixed",
				CommitSHA: &commitSHA,
			},
			Rationale: &rationale,
		})
		eventID = id
		return err
	})
	if err != nil {
		t.Fatalf("emit event: %v", err)
	}

	// Read both rows back and assert their span_id columns match.
	var groundingSpanID string
	if err := pool.DB().QueryRow(
		`SELECT span_id FROM grounding_events WHERE call_id = ?`, rootSpan.ID,
	).Scan(&groundingSpanID); err != nil {
		t.Fatalf("read grounding_events span_id: %v", err)
	}
	var eventSpanID string
	if err := pool.DB().QueryRow(
		`SELECT span_id FROM events WHERE event_id = ?`, eventID,
	).Scan(&eventSpanID); err != nil {
		t.Fatalf("read events span_id: %v", err)
	}

	if groundingSpanID != eventSpanID {
		t.Fatalf("span_id MISMATCH (regression!): grounding_events.span_id=%q, events.span_id=%q",
			groundingSpanID, eventSpanID)
	}
	if groundingSpanID != rootSpan.ID {
		t.Fatalf("span_id drift: row span_id=%q, dispatcher span.ID=%q",
			groundingSpanID, rootSpan.ID)
	}
}

// TestSpanTree_ParentChildLinkage exercises the in-transaction-hook
// shape: dispatch opens a root span; a child span opens for an
// in-transaction op (forge.index_upsert, chain_insert_task_skeletons,
// etc.); events emitted under the child carry the CHILD span_id, not
// the parent's. The child's parent_span_id points at the root span.
// Reads of the events table should let a dashboard render the tree.
func TestSpanTree_ParentChildLinkage(t *testing.T) {
	pool := testutil.NewTestDB(t)
	rationale := "fixed: tree-render gate"
	commitSHA := "feedbee"

	ctx := events.WithActor(context.Background(), events.Actor{Kind: "agent", ID: "claude-opus-4-7"})
	ctx, endRoot := obs.SpanStart(ctx, "work.bug_resolve")
	defer endRoot(nil)
	rootSpan := obs.SpanFromContext(ctx)

	childCtx, endChild := obs.SpanStart(ctx, "forge.index_upsert")
	defer endChild(nil)
	childSpan := obs.SpanFromContext(childCtx)

	if childSpan.ParentID != rootSpan.ID {
		t.Fatalf("child parent_span_id = %q, want root.ID = %q", childSpan.ParentID, rootSpan.ID)
	}
	if childSpan.TraceID != rootSpan.TraceID {
		t.Fatalf("child trace_id = %q != root trace_id = %q", childSpan.TraceID, rootSpan.TraceID)
	}

	// Emit one event under the child span — span_id on the row should
	// match the CHILD, not the root.
	var eventID string
	if err := pool.WithWrite(childCtx, func(tx *sql.Tx) error {
		id, err := events.Emit(childCtx, tx, events.EmitArgs{
			Entity: events.NewEntityRef("bug", "y", "mcp-servers"),
			Payload: events.BugResolvedPayload{
				Kind:      "fixed",
				CommitSHA: &commitSHA,
			},
			Rationale: &rationale,
		})
		eventID = id
		return err
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	var rowSpanID string
	if err := pool.DB().QueryRow(
		`SELECT span_id FROM events WHERE event_id = ?`, eventID,
	).Scan(&rowSpanID); err != nil {
		t.Fatalf("read back event: %v", err)
	}
	if rowSpanID != childSpan.ID {
		t.Fatalf("event row span_id = %q, want child.ID = %q (regression: events should be attributed to the active span, not the root)",
			rowSpanID, childSpan.ID)
	}
	if rowSpanID == rootSpan.ID {
		t.Fatalf("event row span_id collapsed to root — child span context not honored")
	}
}

// TestSpanSink_EmitsOneOpenAndOneCloseForFullRequest verifies the SSE
// contract that the dashboard depends on: every span open generates
// exactly one span_open SpanEvent, and every span close generates
// exactly one span_close SpanEvent. A break (missing close, double
// open) shows up here.
func TestSpanSink_EmitsOneOpenAndOneCloseForFullRequest(t *testing.T) {
	sink := &recordingSink{}
	obs.SetSpanSink(sink)
	t.Cleanup(func() { obs.SetSpanSink(nil) })

	ctx, endRoot := obs.SpanStart(context.Background(), "work.task_complete")
	childCtx, endChild := obs.SpanStart(ctx, "forge.index_upsert")
	endChild(nil)
	endRoot(nil)
	_ = childCtx

	events := sink.Snapshot()
	if len(events) != 4 {
		t.Fatalf("expected 4 events (2 open + 2 close), got %d: %+v", len(events), events)
	}
	want := []string{"span_open", "span_open", "span_close", "span_close"}
	for i, e := range events {
		if e.Type != want[i] {
			t.Errorf("event[%d].Type = %q, want %q", i, e.Type, want[i])
		}
	}
	// Closes should pair to opens by span_id.
	openIDs := map[string]bool{events[0].SpanID: true, events[1].SpanID: true}
	closeIDs := map[string]bool{events[2].SpanID: true, events[3].SpanID: true}
	for id := range openIDs {
		if !closeIDs[id] {
			t.Errorf("span_open %q has no matching span_close", id)
		}
	}
}

// TestSpanClose_OnHandlerErrorMarksErrorStatus exercises the dispatcher's
// pattern of capturing handler errors and passing them to the span's
// EndFn. The span_close event carries status="error" and the message.
func TestSpanClose_OnHandlerErrorMarksErrorStatus(t *testing.T) {
	sink := &recordingSink{}
	obs.SetSpanSink(sink)
	t.Cleanup(func() { obs.SetSpanSink(nil) })

	_, end := obs.SpanStart(context.Background(), "work.task_complete")
	end(errors.New("simulated handler failure"))

	got := sink.Snapshot()
	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %d", len(got))
	}
	closeEv := got[1]
	if closeEv.Status != "error" {
		t.Errorf("close.Status = %q, want error", closeEv.Status)
	}
	if !strings.Contains(closeEv.ErrorMsg, "simulated handler failure") {
		t.Errorf("close.ErrorMsg = %q, want substring of err.Error()", closeEv.ErrorMsg)
	}
}
