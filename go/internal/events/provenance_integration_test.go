package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"toolkit/internal/events"
)

// TestValidateRecordArgs_provenanceGuard proves the guard fires through the REAL
// write path (ValidateRecordArgs → prepareRecord), AFTER the schema validation —
// i.e. a structurally-valid event is still rejected when it launders a scope
// decision as the user's. ValidateRecordArgs is the no-DB dry-run twin of
// EmitRecord, so this exercises the exact same prepareRecord chain a live
// record() call runs, without standing up a database.
func TestValidateRecordArgs_provenanceGuard(t *testing.T) {
	ctx := events.WithActor(context.Background(), events.Actor{Kind: "agent", ID: "claude"})
	ctx = events.WithSpanID(ctx, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")

	mk := func(problem string) events.RecordArgs {
		payload, err := json.Marshal(events.BugReportedPayload{Title: "x", ProblemStatement: problem})
		if err != nil {
			t.Fatal(err)
		}
		return events.RecordArgs{
			Type:    "BugReported",
			Payload: payload,
			Entity:  events.NewEntityRef("bug", "prov-guard-test", "mcp-servers"),
		}
	}

	// Schema-valid (Title + ProblemStatement present) but launders an agent
	// scope decision as the user's → rejected by the provenance guard.
	err := events.ValidateRecordArgs(ctx, mk("closed per the user's 2026-06-26 scope decision to defer the full port"))
	if err == nil {
		t.Fatal("expected provenance rejection for a laundered problem_statement")
	}
	var inv *events.ErrInvalidInput
	if !errors.As(err, &inv) || !strings.Contains(inv.Reason, "provenance") {
		t.Fatalf("want *ErrInvalidInput citing provenance, got %v", err)
	}

	// The same attribution, but CITED → passes the guard (and the schema).
	if err := events.ValidateRecordArgs(ctx, mk(`the user chose minimal sessions — you said: "minimal real now"`)); err != nil {
		t.Fatalf("a cited attribution should validate, got: %v", err)
	}

	// An ordinary bug problem_statement → passes untouched.
	if err := events.ValidateRecordArgs(ctx, mk("clicking Sessions shows an unknown-entity-kind error")); err != nil {
		t.Fatalf("a benign problem_statement should validate, got: %v", err)
	}
}
