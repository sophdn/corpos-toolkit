package work_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"toolkit/internal/work"
)

// recordParams is a small builder for the record action's params JSON.
func recordParams(t *testing.T, strict bool, events ...work.RecordEvent) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(work.RecordParams{Events: events, StrictAllOrNothing: strict})
	if err != nil {
		t.Fatalf("marshal record params: %v", err)
	}
	return b
}

func bugReportedEvent(slug, title string) work.RecordEvent {
	return work.RecordEvent{
		Type:       "BugReported",
		EntityKind: "bug",
		EntitySlug: slug,
		Payload:    json.RawMessage(`{"title":"` + title + `","problem_statement":"something broke"}`),
		Rationale:  "filing observed friction",
	}
}

func TestRecord_SingleValidEvent_AppendsAndFolds(t *testing.T) {
	pool := openTestPool(t)
	deps := work.TableDeps{Pool: pool}
	ctx := context.Background()

	res, err := work.HandleRecord(ctx, deps, "mcp-servers", recordParams(t, false,
		bugReportedEvent("record-single", "single event create")))
	if err != nil {
		t.Fatalf("HandleRecord: %v", err)
	}
	if !res.OK || res.Recorded != 1 || res.Rejected != 0 || res.RolledBack {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Results[0].EventID == nil {
		t.Fatal("expected an event_id on the recorded event")
	}

	// The event folded into the bug projection.
	var n int
	if err := pool.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM proj_current_bugs WHERE slug = ?`, "record-single").Scan(&n); err != nil {
		t.Fatalf("query projection: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 bug projection row, got %d", n)
	}
}

func TestRecord_PartialSuccess_RejectedGhostedValidCommitted(t *testing.T) {
	pool := openTestPool(t)
	deps := work.TableDeps{Pool: pool}
	ctx := context.Background()

	// A valid event + an unknown-type event (schema rejection).
	bad := work.RecordEvent{
		Type:       "NoSuchEventType",
		EntityKind: "bug",
		EntitySlug: "bad",
		Payload:    json.RawMessage(`{}`),
	}
	res, err := work.HandleRecord(ctx, deps, "mcp-servers", recordParams(t, false,
		bugReportedEvent("record-valid", "valid one"), bad))
	if err != nil {
		t.Fatalf("HandleRecord: %v", err)
	}
	if res.Recorded != 1 || res.Rejected != 1 || res.RolledBack {
		t.Fatalf("expected 1 recorded + 1 rejected, no rollback: %+v", res)
	}
	if res.Results[1].OK || res.Results[1].RejectedReason == nil {
		t.Fatalf("expected event[1] rejected with a reason: %+v", res.Results[1])
	}
	// The valid event committed despite the sibling rejection (partial-success).
	var n int
	if err := pool.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM proj_current_bugs WHERE slug = ?`, "record-valid").Scan(&n); err != nil {
		t.Fatalf("query projection: %v", err)
	}
	if n != 1 {
		t.Fatalf("valid event should have committed; got %d rows", n)
	}
}

func TestRecord_StrictAllOrNothing_RejectionRollsBackAll(t *testing.T) {
	pool := openTestPool(t)
	deps := work.TableDeps{Pool: pool}
	ctx := context.Background()

	bad := work.RecordEvent{Type: "NoSuchEventType", EntityKind: "bug", EntitySlug: "bad", Payload: json.RawMessage(`{}`)}
	res, err := work.HandleRecord(ctx, deps, "mcp-servers", recordParams(t, true,
		bugReportedEvent("record-strict", "valid but doomed"), bad))
	if err != nil {
		t.Fatalf("HandleRecord: %v", err)
	}
	if !res.RolledBack {
		t.Fatalf("expected rolled_back in strict mode: %+v", res)
	}
	// The valid event's id is stripped (its INSERT was rolled back).
	if res.Results[0].EventID != nil {
		t.Fatal("expected event_id stripped on rollback")
	}
	var n int
	if err := pool.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM proj_current_bugs WHERE slug = ?`, "record-strict").Scan(&n); err != nil {
		t.Fatalf("query projection: %v", err)
	}
	if n != 0 {
		t.Fatalf("strict rollback should leave no projection row; got %d", n)
	}
}

func TestRecord_FutureTsClampedToNow(t *testing.T) {
	pool := openTestPool(t)
	deps := work.TableDeps{Pool: pool}
	ctx := context.Background()

	future := time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)
	ev := bugReportedEvent("record-ts", "future ts")
	ev.Ts = future
	res, err := work.HandleRecord(ctx, deps, "mcp-servers", recordParams(t, false, ev))
	if err != nil {
		t.Fatalf("HandleRecord: %v", err)
	}
	if !res.OK || res.Results[0].EventID == nil {
		t.Fatalf("expected the event to record: %+v", res)
	}
	var ts string
	if err := pool.DB().QueryRowContext(ctx,
		`SELECT ts FROM events WHERE event_id = ?`, *res.Results[0].EventID).Scan(&ts); err != nil {
		t.Fatalf("read ts: %v", err)
	}
	stored, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatalf("parse stored ts %q: %v", ts, err)
	}
	if stored.After(time.Now().UTC().Add(time.Minute)) {
		t.Fatalf("future ts was not clamped: stored %s", ts)
	}
}

func TestRecord_InfersEntityKindFromType(t *testing.T) {
	pool := openTestPool(t)
	deps := work.TableDeps{Pool: pool}
	ctx := context.Background()

	// BugReported WITHOUT entity_kind — record should infer kind=bug from the type.
	ev := work.RecordEvent{
		Type:       "BugReported",
		EntitySlug: "record-inferred-kind",
		Payload:    json.RawMessage(`{"title":"inferred","problem_statement":"x"}`),
		Rationale:  "inference test",
	}
	res, err := work.HandleRecord(ctx, deps, "mcp-servers", recordParams(t, false, ev))
	if err != nil {
		t.Fatalf("HandleRecord: %v", err)
	}
	if !res.OK || res.Recorded != 1 {
		t.Fatalf("expected the inferred-kind event to record: %+v", res)
	}
	var n int
	pool.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM proj_current_bugs WHERE slug=?`, "record-inferred-kind").Scan(&n)
	if n != 1 {
		t.Fatalf("inferred-kind bug should have folded into the projection; got %d", n)
	}
}

func TestRecord_NestedEntityObject(t *testing.T) {
	pool := openTestPool(t)
	deps := work.TableDeps{Pool: pool}
	ctx := context.Background()

	pid := "mcp-servers"
	ev := work.RecordEvent{
		Type:      "BugReported",
		Entity:    &work.RecordEntity{Kind: "bug", Slug: "record-nested-entity", ProjectID: &pid},
		Payload:   json.RawMessage(`{"title":"nested","problem_statement":"x"}`),
		Rationale: "nested entity test",
	}
	res, err := work.HandleRecord(ctx, deps, "mcp-servers", recordParams(t, false, ev))
	if err != nil {
		t.Fatalf("HandleRecord: %v", err)
	}
	if !res.OK || res.Recorded != 1 {
		t.Fatalf("expected the nested-entity event to record: %+v", res)
	}
}

func TestRecord_DryRun_NoWrites(t *testing.T) {
	pool := openTestPool(t)
	deps := work.TableDeps{Pool: pool}
	ctx := context.Background()

	good := bugReportedEvent("record-dryrun", "valid")
	bad := work.RecordEvent{Type: "NoSuchEventType", EntityKind: "bug", EntitySlug: "dryrun-bad", Payload: json.RawMessage(`{}`)}
	params, err := json.Marshal(work.RecordParams{Events: []work.RecordEvent{good, bad}, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	res, err := work.HandleRecord(ctx, deps, "mcp-servers", params)
	if err != nil {
		t.Fatalf("HandleRecord dry-run: %v", err)
	}
	if !res.DryRun || res.Recorded != 1 || res.Rejected != 1 {
		t.Fatalf("dry-run should report 1 would-record + 1 reject: %+v", res)
	}
	// Nothing persisted: no events for either slug, no ghosts.
	var evts, ghosts int
	pool.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE entity_slug IN ('record-dryrun','dryrun-bad')`).Scan(&evts)
	pool.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM ghosts`).Scan(&ghosts)
	if evts != 0 || ghosts != 0 {
		t.Fatalf("dry-run must write nothing: events=%d ghosts=%d", evts, ghosts)
	}
}

func TestRecord_EmptyEvents_Rejected(t *testing.T) {
	pool := openTestPool(t)
	deps := work.TableDeps{Pool: pool}
	res, err := work.HandleRecord(context.Background(), deps, "mcp-servers", recordParams(t, false))
	if err != nil {
		t.Fatalf("HandleRecord: %v", err)
	}
	if res.Error == "" {
		t.Fatal("expected an error for an empty events list")
	}
}
