package auditemit_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"toolkit/internal/auditemit"
	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/testutil"
)

// str is the *string helper the typed emitters used for optional payload
// fields; mirrored here so the parity fixtures are byte-identical.
func str(s string) *string { return &s }

// archPayload is a small-but-representative ArchitectureAuditCompleted
// payload: it exercises the nested findings array plus the optional pointer
// fields (recommended_next_phase, evidence, severity) that distinguish the
// raw-JSON path from a flat struct.
func archPayload() events.ArchitectureAuditCompletedPayload {
	return events.ArchitectureAuditCompletedPayload{
		AuditDoc:             "AUDIT.md §1",
		Summary:              "all criteria pass",
		RecommendedNextPhase: str("Phase 4"),
		Findings: []events.ArchitectureAuditFinding{
			{Item: "§1 events table", Status: "pass", Evidence: str("migration 032"), Severity: str("low")},
		},
	}
}

// readEvent pulls the columns the parity check compares for one event_id.
func readEvent(t *testing.T, pool *db.Pool, eventID string) (typ, kind, slug, payload string, project sql.NullString) {
	t.Helper()
	row := pool.DB().QueryRow(
		`SELECT type, entity_kind, entity_slug, entity_project_id, payload FROM events WHERE event_id = ?`,
		eventID)
	if err := row.Scan(&typ, &kind, &slug, &project, &payload); err != nil {
		t.Fatalf("read event %s: %v", eventID, err)
	}
	return typ, kind, slug, payload, project
}

func countEvents(t *testing.T, pool *db.Pool) int {
	t.Helper()
	var n int
	if err := pool.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

// specWith builds a one-event spec carrying the given type + payload bytes.
func specWith(typ string, payload json.RawMessage) auditemit.Spec {
	return auditemit.Spec{
		ActorID: "claude-test-agent",
		Entity:  auditemit.SpecEntity{Kind: "chain", Slug: "demo-chain", ProjectID: "mcp-servers"},
		Events:  []auditemit.SpecEvent{{Type: typ, Rationale: "test rationale long enough", Payload: payload}},
	}
}

// ── Parity: the load-bearing assertion ──────────────────────────────────────
//
// The whole refactor rests on this: an event emitted from a spec via
// auditemit.Emit (events.EmitRecord) must be indistinguishable from the same
// event emitted by the typed one-shot via events.Emit. We emit identical data
// both ways and assert the stored type, entity, and payload bytes match.
func TestEmit_ParityWithTypedEmit(t *testing.T) {
	payload := archPayload()
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	// Path A — the OLD typed path (what the one-shot binaries did).
	typedPool := testutil.NewTestDB(t)
	ctxA := events.WithActor(context.Background(), events.Actor{Kind: "agent", ID: "claude-test-agent"})
	rationale := "test rationale long enough"
	var typedID string
	if err := typedPool.WithWrite(ctxA, func(tx *sql.Tx) error {
		id, e := events.Emit(ctxA, tx, events.EmitArgs{
			Entity:    events.NewEntityRef("chain", "demo-chain", "mcp-servers"),
			Payload:   payload,
			Rationale: &rationale,
		})
		typedID = id
		return e
	}); err != nil {
		t.Fatalf("typed emit: %v", err)
	}
	typedType, typedKind, typedSlug, typedPayload, typedProj := readEvent(t, typedPool, typedID)

	// Path B — the NEW spec path (what the generic command does).
	specPool := testutil.NewTestDB(t)
	emitted, err := auditemit.Emit(context.Background(), specPool, specWith("ArchitectureAuditCompleted", payloadJSON))
	if err != nil {
		t.Fatalf("spec emit: %v", err)
	}
	if len(emitted) != 1 {
		t.Fatalf("expected 1 emitted, got %d", len(emitted))
	}
	specType, specKind, specSlug, specPayload, specProj := readEvent(t, specPool, emitted[0].EventID)

	if typedType != specType {
		t.Errorf("type mismatch: typed=%q spec=%q", typedType, specType)
	}
	if typedKind != specKind || typedSlug != specSlug || typedProj != specProj {
		t.Errorf("entity mismatch: typed=%s/%s/%v spec=%s/%s/%v", typedKind, typedSlug, typedProj, specKind, specSlug, specProj)
	}
	if typedPayload != specPayload {
		t.Errorf("payload bytes differ:\n typed=%s\n  spec=%s", typedPayload, specPayload)
	}
	// And the stored bytes are exactly the marshaled typed payload.
	if specPayload != string(payloadJSON) {
		t.Errorf("spec payload not verbatim:\n got=%s\n want=%s", specPayload, payloadJSON)
	}
}

// ── Multi-event: the bare audit-emit emitted two events under one entity ─────
func TestEmit_MultiEventSpecLandsInOrder(t *testing.T) {
	pool := testutil.NewTestDB(t)
	arch, _ := json.Marshal(archPayload())
	conv, _ := json.Marshal(events.ConventionAuditCompletedPayload{
		AuditDoc: "CONV.md",
		Summary:  "conventions honored",
		Findings: []events.ConventionAuditFinding{{Axis: "events as truth", Status: "honored"}},
	})
	spec := auditemit.Spec{
		ActorID: "claude-test-agent",
		Entity:  auditemit.SpecEntity{Kind: "chain", Slug: "two-event-chain", ProjectID: "mcp-servers"},
		Events: []auditemit.SpecEvent{
			{Type: "ArchitectureAuditCompleted", Rationale: "arch rationale long enough", Payload: arch},
			{Type: "ConventionAuditCompleted", Rationale: "conv rationale long enough", Payload: conv},
		},
	}
	emitted, err := auditemit.Emit(context.Background(), pool, spec)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if len(emitted) != 2 {
		t.Fatalf("expected 2 emitted, got %d", len(emitted))
	}
	if emitted[0].Type != "ArchitectureAuditCompleted" || emitted[1].Type != "ConventionAuditCompleted" {
		t.Errorf("order not preserved: %+v", emitted)
	}
	if n := countEvents(t, pool); n != 2 {
		t.Errorf("expected 2 rows, got %d", n)
	}
}

// ── Rejection: unknown type lands nothing ────────────────────────────────────
func TestEmit_UnknownType_NoRow(t *testing.T) {
	pool := testutil.NewTestDB(t)
	_, err := auditemit.Emit(context.Background(), pool, specWith("NotARegisteredType", json.RawMessage(`{"x":1}`)))
	if err == nil {
		t.Fatal("expected error for unknown type, got nil")
	}
	if n := countEvents(t, pool); n != 0 {
		t.Errorf("expected 0 rows after rejected emit, got %d", n)
	}
}

// ── Rejection: schema-invalid payload lands nothing ─────────────────────────
func TestEmit_SchemaInvalidPayload_NoRow(t *testing.T) {
	pool := testutil.NewTestDB(t)
	// Empty object is missing audit_doc/summary/findings required by the schema.
	_, err := auditemit.Emit(context.Background(), pool, specWith("ArchitectureAuditCompleted", json.RawMessage(`{}`)))
	if err == nil {
		t.Fatal("expected schema-validation error, got nil")
	}
	if n := countEvents(t, pool); n != 0 {
		t.Errorf("expected 0 rows after rejected emit, got %d", n)
	}
}

// ── Rejection: a valid first event commits, an invalid second one stops ──────
func TestEmit_PartialFailure_StopsAtBadEvent(t *testing.T) {
	pool := testutil.NewTestDB(t)
	arch, _ := json.Marshal(archPayload())
	spec := auditemit.Spec{
		ActorID: "claude-test-agent",
		Entity:  auditemit.SpecEntity{Kind: "chain", Slug: "partial-chain", ProjectID: "mcp-servers"},
		Events: []auditemit.SpecEvent{
			{Type: "ArchitectureAuditCompleted", Rationale: "good rationale long enough", Payload: arch},
			{Type: "ArchitectureAuditCompleted", Rationale: "bad one", Payload: json.RawMessage(`{}`)},
		},
	}
	emitted, err := auditemit.Emit(context.Background(), pool, spec)
	if err == nil {
		t.Fatal("expected error on second event, got nil")
	}
	if len(emitted) != 1 {
		t.Errorf("expected 1 event landed before failure, got %d", len(emitted))
	}
	if n := countEvents(t, pool); n != 1 {
		t.Errorf("expected 1 committed row, got %d", n)
	}
}

// ── CheckSchema: DB-free schema validation (dry-run + spec-rot guard) ────────
func TestCheckSchema(t *testing.T) {
	ctx := context.Background()
	good, _ := json.Marshal(archPayload())
	if err := auditemit.CheckSchema(ctx, specWith("ArchitectureAuditCompleted", good)); err != nil {
		t.Errorf("valid spec should pass CheckSchema, got %v", err)
	}
	if err := auditemit.CheckSchema(ctx, specWith("ArchitectureAuditCompleted", json.RawMessage(`{}`))); err == nil {
		t.Error("schema-invalid payload should fail CheckSchema")
	}
	if err := auditemit.CheckSchema(ctx, specWith("NotARegisteredType", json.RawMessage(`{}`))); err == nil {
		t.Error("unknown type should fail CheckSchema")
	}
}

// ── Structural validation (pre-emit, DB-free) ────────────────────────────────
func TestSpecValidate_Rejections(t *testing.T) {
	good := func() auditemit.Spec { return specWith("ArchitectureAuditCompleted", json.RawMessage(`{"a":1}`)) }
	cases := []struct {
		name   string
		mutate func(*auditemit.Spec)
	}{
		{"missing actor_id", func(s *auditemit.Spec) { s.ActorID = "" }},
		{"missing entity.kind", func(s *auditemit.Spec) { s.Entity.Kind = "" }},
		{"missing entity.slug", func(s *auditemit.Spec) { s.Entity.Slug = "" }},
		{"no events", func(s *auditemit.Spec) { s.Events = nil }},
		{"event missing type", func(s *auditemit.Spec) { s.Events[0].Type = "" }},
		{"event missing payload", func(s *auditemit.Spec) { s.Events[0].Payload = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := good()
			tc.mutate(&s)
			if err := s.Validate(); err == nil {
				t.Errorf("expected validation error for %q, got nil", tc.name)
			}
		})
	}
	if err := good().Validate(); err != nil {
		t.Errorf("good spec should validate, got %v", err)
	}
}

// ── Load round-trips a written spec ──────────────────────────────────────────
func TestLoad_RoundTrip(t *testing.T) {
	spec := specWith("ArchitectureAuditCompleted", json.RawMessage(`{"audit_doc":"d","summary":"s","findings":[]}`))
	raw, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "spec.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := auditemit.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.ActorID != spec.ActorID || len(got.Events) != 1 || got.Events[0].Type != spec.Events[0].Type {
		t.Errorf("round-trip mismatch: got %+v", got)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	if _, err := auditemit.Load(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}
