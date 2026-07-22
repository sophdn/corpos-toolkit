package registry_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/registry"
	"toolkit/internal/testutil"
)

// emitBug emits a BugReported then a caused-by BugResolved into pool,
// returning the two event_ids. Representative of a real cascade (a resolve
// causally following a report) so the causal tier has something to chew on.
func emitBug(t *testing.T, pool *db.Pool, slug string) (reportID, resolveID string) {
	t.Helper()
	ctx := events.WithActor(context.Background(), events.Actor{Kind: "agent", ID: "claude-opus-4-7"})
	ctx = events.WithSpanID(ctx, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	rationale := "round-trip fixture"

	if err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		id, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity:    events.NewEntityRef("bug", slug, "mcp-servers"),
			Payload:   events.BugReportedPayload{Title: "fixture bug " + slug, ProblemStatement: "something broke"},
			Rationale: &rationale,
		})
		reportID = id
		return err
	}); err != nil {
		t.Fatalf("emit BugReported: %v", err)
	}

	commitSHA := "abc1234"
	if err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		id, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity:    events.NewEntityRef("bug", slug, "mcp-servers"),
			Payload:   events.BugResolvedPayload{Kind: "fixed", CommitSHA: &commitSHA},
			Rationale: &rationale,
			Refs:      events.Refs{CausedByEventID: &reportID},
		})
		resolveID = id
		return err
	}); err != nil {
		t.Fatalf("emit BugResolved: %v", err)
	}
	return reportID, resolveID
}

func TestExportValidateVerifyDR_RoundTrip(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ctx := context.Background()
	reportID, resolveID := emitBug(t, pool, "registry-round-trip")

	dir := t.TempDir()
	n, err := registry.ExportFromDB(ctx, pool, dir)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if n != 2 {
		t.Fatalf("exported %d events, want 2", n)
	}
	for _, id := range []string{reportID, resolveID} {
		if _, err := os.Stat(filepath.Join(dir, "events", id+".json")); err != nil {
			t.Fatalf("expected event file for %s: %v", id, err)
		}
	}

	rep, err := registry.Validate(ctx, dir, registry.ValidateOptions{})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("validate reported failures: %v", rep.Failures)
	}
	if rep.Total != 2 {
		t.Fatalf("validated %d events, want 2", rep.Total)
	}

	if err := registry.VerifyDR(ctx, pool, dir); err != nil {
		t.Fatalf("verify-dr: %v", err)
	}
}

// TestValidate_GrandfathersBaseline asserts the immutable-baseline model:
// an event that fails the current schema is a hard failure when it's in the
// strict (pushed-delta) set, but is grandfathered (skipped, counted) when
// it's outside the set — the property that keeps an append-only registry of
// historical events from going permanently red when a schema later tightens.
func TestValidate_GrandfathersBaseline(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ctx := context.Background()
	reportID, _ := emitBug(t, pool, "grandfather")

	dir := t.TempDir()
	if _, err := registry.ExportFromDB(ctx, pool, dir); err != nil {
		t.Fatalf("export: %v", err)
	}

	// Hand-write a schema-INVALID event (empty required problem_statement on
	// TaskCreated) into the registry — standing in for a pre-tightening
	// historical event.
	invalid := map[string]any{
		"event_id":       "00000000-0000-7000-8000-0000000000aa",
		"ts":             "2026-01-01T00:00:00.000Z",
		"actor":          map[string]any{"kind": "system", "id": "cli-test"},
		"type":           "TaskCreated",
		"entity":         map[string]any{"kind": "task", "slug": "old-task", "project_id": "mcp-servers"},
		"payload":        map[string]any{"problem_statement": ""},
		"rationale":      nil,
		"refs":           map[string]any{"caused_by_event_id": nil, "related_entities": []any{}},
		"span_id":        "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		"schema_version": 1,
	}
	out, _ := json.MarshalIndent(invalid, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "events", "00000000-0000-7000-8000-0000000000aa.json"), out, 0o644); err != nil {
		t.Fatal(err)
	}

	// Strict set = only the genuinely-new valid event → the invalid one is
	// grandfathered → OK. (Coherence is skipped here because the empty
	// TaskCreated can't be a clean fold target; we assert the schema-tier
	// grandfathering, which is the point.)
	repGrand, err := registry.Validate(ctx, dir, registry.ValidateOptions{
		StrictSchemaEventIDs: map[string]bool{reportID: true},
	})
	if err != nil {
		t.Fatalf("validate (grandfather): %v", err)
	}
	for _, f := range repGrand.Failures {
		if f.Tier == "schema" {
			t.Fatalf("expected the invalid baseline event to be grandfathered, but got a schema failure: %v", f)
		}
	}
	if repGrand.Grandfathered == 0 {
		t.Fatal("expected at least one grandfathered event")
	}

	// Now put the invalid event IN the strict set → it must be a hard
	// schema failure.
	repStrict, err := registry.Validate(ctx, dir, registry.ValidateOptions{
		StrictSchemaEventIDs: map[string]bool{"00000000-0000-7000-8000-0000000000aa": true},
	})
	if err != nil {
		t.Fatalf("validate (strict): %v", err)
	}
	gotSchemaFail := false
	for _, f := range repStrict.Failures {
		if f.Tier == "schema" && f.EventID == "00000000-0000-7000-8000-0000000000aa" {
			gotSchemaFail = true
		}
	}
	if !gotSchemaFail {
		t.Fatalf("expected a strict schema failure for the invalid event, got: %v", repStrict.Failures)
	}
}

// TestExport_Idempotent asserts re-exporting an unchanged ledger writes
// byte-identical files — the property the fast-forward-only registry needs
// so an unchanged event is never a spurious diff.
func TestExport_Idempotent(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ctx := context.Background()
	emitBug(t, pool, "idempotent")

	dir := t.TempDir()
	if _, err := registry.ExportFromDB(ctx, pool, dir); err != nil {
		t.Fatalf("export 1: %v", err)
	}
	before := readAll(t, dir)
	if _, err := registry.ExportFromDB(ctx, pool, dir); err != nil {
		t.Fatalf("export 2: %v", err)
	}
	after := readAll(t, dir)

	if len(before) != len(after) {
		t.Fatalf("file count changed: %d → %d", len(before), len(after))
	}
	for name, b := range before {
		if string(after[name]) != string(b) {
			t.Fatalf("file %s not byte-identical across re-export", name)
		}
	}
}

func TestValidate_CausalFailure_NonCanonicalParent(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ctx := context.Background()
	emitBug(t, pool, "causal-fail")

	dir := t.TempDir()
	if _, err := registry.ExportFromDB(ctx, pool, dir); err != nil {
		t.Fatalf("export: %v", err)
	}

	// Repoint one event's caused_by at a valid-shaped but absent parent.
	absent := "00000000-0000-7000-8000-000000000000"
	files, _ := filepath.Glob(filepath.Join(dir, "events", "*.json"))
	tampered := false
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode %s: %v", f, err)
		}
		refs := m["refs"].(map[string]any)
		if refs["caused_by_event_id"] != nil {
			refs["caused_by_event_id"] = absent
			out, _ := json.MarshalIndent(m, "", "  ")
			if err := os.WriteFile(f, out, 0o644); err != nil {
				t.Fatalf("write tampered: %v", err)
			}
			tampered = true
			break
		}
	}
	if !tampered {
		t.Fatal("no event with a caused_by ref to tamper")
	}

	rep, err := registry.Validate(ctx, dir, registry.ValidateOptions{})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if rep.OK() {
		t.Fatal("expected a causal failure, got OK")
	}
	found := false
	for _, f := range rep.Failures {
		if f.Tier == "causal" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a causal-tier failure, got: %v", rep.Failures)
	}
}

func TestValidate_SchemaFailure_UnknownType(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "events"), 0o755); err != nil {
		t.Fatal(err)
	}
	bogus := map[string]any{
		"event_id":       "00000000-0000-7000-8000-000000000001",
		"ts":             "2026-05-27T00:00:00.000Z",
		"actor":          map[string]any{"kind": "system", "id": "cli-test"},
		"type":           "NoSuchEventTypeExists",
		"entity":         map[string]any{"kind": "bug", "slug": "x", "project_id": "mcp-servers"},
		"payload":        map[string]any{},
		"rationale":      nil,
		"refs":           map[string]any{"caused_by_event_id": nil, "related_entities": []any{}},
		"span_id":        "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		"schema_version": 1,
	}
	out, _ := json.MarshalIndent(bogus, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "events", "bogus.json"), out, 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := registry.Validate(context.Background(), dir, registry.ValidateOptions{})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if rep.OK() {
		t.Fatal("expected a schema failure for an unknown type, got OK")
	}
	if rep.Failures[0].Tier != "schema" {
		t.Fatalf("expected schema-tier failure, got: %v", rep.Failures)
	}
}

func readAll(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "events", "*.json"))
	out := make(map[string][]byte, len(files))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		out[filepath.Base(f)] = b
	}
	return out
}
