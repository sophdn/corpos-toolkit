package events_test

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"sort"
	"strings"
	"testing"

	"toolkit/internal/events"
	"toolkit/internal/testutil"
)

// ── envelope + context defaults ────────────────────────────────────────────

func TestActorFromContext_DefaultsToSystemUnattributed(t *testing.T) {
	a := events.ActorFromContext(context.Background())
	if a.Kind != "system" || a.ID != "unattributed" {
		t.Fatalf("default actor = {%q, %q}, want {\"system\", \"unattributed\"}", a.Kind, a.ID)
	}
}

func TestActorFromContext_RoundTripsAttachedActor(t *testing.T) {
	want := events.Actor{Kind: "agent", ID: "claude-opus-4-7"}
	ctx := events.WithActor(context.Background(), want)
	got := events.ActorFromContext(ctx)
	if got != want {
		t.Fatalf("actor round-trip: got %+v, want %+v", got, want)
	}
}

func TestSpanIDFromContext_AttachedValueWins(t *testing.T) {
	want := "11111111-2222-4333-8444-555555555555"
	ctx := events.WithSpanID(context.Background(), want)
	got, err := events.SpanIDFromContext(ctx)
	if err != nil {
		t.Fatalf("SpanIDFromContext: %v", err)
	}
	if got != want {
		t.Fatalf("span_id round-trip: got %q, want %q", got, want)
	}
}

func TestSpanIDFromContext_GeneratesWhenAbsent(t *testing.T) {
	// Without an attached span_id, the package mints a fresh UUIDv4 — a
	// fallback that keeps Emit usable in tests and pre-T3 production
	// before dispatch wires real span minting. The orphan is queryable
	// as such (no sibling events share the id).
	got, err := events.SpanIDFromContext(context.Background())
	if err != nil {
		t.Fatalf("SpanIDFromContext: %v", err)
	}
	if !uuidv4Re.MatchString(got) {
		t.Fatalf("generated span_id %q is not a valid UUIDv4", got)
	}
}

// ── UUID generators ────────────────────────────────────────────────────────

// UUIDv7: 8-4-4-4-12 hex, version nibble 7, variant bits 10xx.
var uuidv7Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var uuidv4Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestUUIDv7_FormatAndMonotonicity(t *testing.T) {
	// Generate via Emit since the generators are unexported. Sort by
	// event_id should match insertion order — load-bearing for the
	// sibling chain's FK-ordering semantics on write_event_ids.
	pool := testutil.NewTestDB(t)
	ctx := context.Background()

	const n = 5
	ids := make([]string, 0, n)
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		for i := 0; i < n; i++ {
			id, err := emitMinimalBugReported(ctx, tx, "u7-test-bug")
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("emit loop: %v", err)
	}

	for _, id := range ids {
		if !uuidv7Re.MatchString(id) {
			t.Fatalf("event_id %q does not match UUIDv7 format", id)
		}
	}

	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for i := range ids {
		if ids[i] != sorted[i] {
			t.Fatalf("UUIDv7 not monotonic by insertion: ids=%v, sorted=%v", ids, sorted)
		}
	}
}

// ── Emit happy paths ───────────────────────────────────────────────────────

func TestEmit_InsertsRowWithAllEnvelopeFields(t *testing.T) {
	pool := testutil.NewTestDB(t)
	rationale := "fixed: title field defaulted to empty in the forge writer; verified in smoke test"
	commitSHA := "abc1234"
	ctx := events.WithActor(context.Background(), events.Actor{Kind: "agent", ID: "claude-opus-4-7"})
	ctx = events.WithSpanID(ctx, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")

	var eventID string
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		id, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity: events.NewEntityRef("bug", "forge-bug-title-omitted", "mcp-servers"),
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
		t.Fatalf("emit: %v", err)
	}

	var (
		gotID, gotType, gotEntityKind, gotEntitySlug, gotActorKind, gotActorID, gotSpan string
		gotProject, gotRationale                                                        sql.NullString
		gotPayload, gotRelated                                                          string
		gotSchemaVersion                                                                int
	)
	err = pool.DB().QueryRow(
		`SELECT event_id, type, entity_kind, entity_slug, entity_project_id,
		        actor_kind, actor_id, payload, rationale, related_entities,
		        span_id, schema_version
		 FROM events WHERE event_id = ?`, eventID,
	).Scan(&gotID, &gotType, &gotEntityKind, &gotEntitySlug, &gotProject,
		&gotActorKind, &gotActorID, &gotPayload, &gotRationale, &gotRelated,
		&gotSpan, &gotSchemaVersion)
	if err != nil {
		t.Fatalf("read back event: %v", err)
	}

	if gotID != eventID || gotType != "BugResolved" || gotEntityKind != "bug" ||
		gotEntitySlug != "forge-bug-title-omitted" || gotProject.String != "mcp-servers" ||
		gotActorKind != "agent" || gotActorID != "claude-opus-4-7" ||
		gotSpan != "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee" || gotSchemaVersion != 1 {
		t.Fatalf("row mismatch: id=%q type=%q entity=%s/%s/%s actor=%s/%s span=%q version=%d",
			gotID, gotType, gotEntityKind, gotEntitySlug, gotProject.String,
			gotActorKind, gotActorID, gotSpan, gotSchemaVersion)
	}
	if gotRationale.String != rationale {
		t.Fatalf("rationale mismatch: got %q, want %q", gotRationale.String, rationale)
	}
	if !strings.Contains(gotPayload, `"kind":"fixed"`) || !strings.Contains(gotPayload, `"commit_sha":"abc1234"`) {
		t.Fatalf("payload JSON missing expected keys: %s", gotPayload)
	}
	if gotRelated != "[]" {
		t.Fatalf("related_entities default = %q, want %q", gotRelated, "[]")
	}
}

func TestEmit_RecordsCrossCuttingEntityAsNullProject(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ctx := context.Background()
	var eventID string
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		id, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity:  events.NewCrossCuttingEntityRef("benchmark_run", "regression-2026-05-16-001"),
			Payload: validBenchmarkStartedPayload(),
		})
		eventID = id
		return err
	})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}

	var project sql.NullString
	if err := pool.DB().QueryRow(`SELECT entity_project_id FROM events WHERE event_id = ?`, eventID).Scan(&project); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if project.Valid {
		t.Fatalf("cross-cutting entity project_id stored as %q, want NULL", project.String)
	}
}

// ── Emit rejection paths ───────────────────────────────────────────────────

// fakeUnregisteredPayload implements Payload with a name that has no
// blueprints/events/<name>.json file — used to verify Emit rejects
// unregistered types at runtime even though the generic-constraint
// dispatch lets the call compile.
type fakeUnregisteredPayload struct{}

func (fakeUnregisteredPayload) EventType() string { return "BugLevitatedSpontaneously" }

func TestEmit_RejectsUnknownType(t *testing.T) {
	pool := testutil.NewTestDB(t)
	err := pool.WithWrite(context.Background(), func(tx *sql.Tx) error {
		_, err := events.Emit(context.Background(), tx, events.EmitArgs{
			Entity:  events.NewEntityRef("bug", "made-up", "mcp-servers"),
			Payload: fakeUnregisteredPayload{},
		})
		return err
	})
	if err == nil {
		t.Fatal("expected ErrInvalidInput for unknown event type, got nil")
	}
	var ie *events.ErrInvalidInput
	if !errors.As(err, &ie) {
		t.Fatalf("expected *ErrInvalidInput, got %T: %v", err, err)
	}
	if ie.Field != "type" || !strings.Contains(ie.Reason, "unknown event type") {
		t.Fatalf("unexpected ErrInvalidInput: %+v", ie)
	}

	// Verify no row was inserted (the tx rolled back).
	var count int
	if err := pool.DB().QueryRow(`SELECT COUNT(*) FROM events`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 events after rejected emit, got %d", count)
	}
}

func TestEmit_RejectsPayloadFailingSchema(t *testing.T) {
	pool := testutil.NewTestDB(t)
	err := pool.WithWrite(context.Background(), func(tx *sql.Tx) error {
		// BugResolved schema requires kind; this payload's Kind is "".
		_, err := events.Emit(context.Background(), tx, events.EmitArgs{
			Entity:  events.NewEntityRef("bug", "missing-kind", "mcp-servers"),
			Payload: events.BugResolvedPayload{
				// Kind deliberately empty — fails schema's enum constraint.
			},
		})
		return err
	})
	if err == nil {
		t.Fatal("expected ErrInvalidInput for payload missing required `kind`, got nil")
	}
	var ie *events.ErrInvalidInput
	if !errors.As(err, &ie) {
		t.Fatalf("expected *ErrInvalidInput, got %T: %v", err, err)
	}
	if ie.Field != "payload" {
		t.Fatalf("expected Field=payload, got %+v", ie)
	}
}

func TestEmit_RejectsMissingRequiredFields(t *testing.T) {
	pool := testutil.NewTestDB(t)
	validPayload := events.BugReportedPayload{Title: "x", ProblemStatement: "y"}
	cases := []struct {
		name      string
		args      events.EmitArgs
		wantField string
	}{
		{
			name:      "nil payload",
			args:      events.EmitArgs{Entity: events.NewEntityRef("bug", "x", "p"), Payload: nil},
			wantField: "payload",
		},
		{
			name:      "missing entity.kind",
			args:      events.EmitArgs{Entity: events.EntityRef{Slug: "x"}, Payload: validPayload},
			wantField: "entity.kind",
		},
		{
			name:      "missing entity.slug",
			args:      events.EmitArgs{Entity: events.EntityRef{Kind: "bug"}, Payload: validPayload},
			wantField: "entity.slug",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := pool.WithWrite(context.Background(), func(tx *sql.Tx) error {
				_, err := events.Emit(context.Background(), tx, tc.args)
				return err
			})
			if err == nil {
				t.Fatal("expected ErrInvalidInput, got nil")
			}
			var ie *events.ErrInvalidInput
			if !errors.As(err, &ie) {
				t.Fatalf("expected *ErrInvalidInput, got %T: %v", err, err)
			}
			if ie.Field != tc.wantField {
				t.Fatalf("Field=%q, want %q", ie.Field, tc.wantField)
			}
		})
	}
}

func TestEmit_RejectsNilTransaction(t *testing.T) {
	_, err := events.Emit(context.Background(), nil, events.EmitArgs{
		Entity:  events.NewEntityRef("bug", "x", "p"),
		Payload: events.BugReportedPayload{Title: "x", ProblemStatement: "y"},
	})
	if err == nil {
		t.Fatal("expected ErrInvalidInput for nil tx, got nil")
	}
	var ie *events.ErrInvalidInput
	if !errors.As(err, &ie) || ie.Field != "tx" {
		t.Fatalf("expected ErrInvalidInput{Field:tx}, got %T %+v", err, err)
	}
}

// ── Append-only trigger enforcement (SQL-level) ────────────────────────────

func TestEvents_UpdateForbiddenByTrigger(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ctx := context.Background()
	var eventID string
	if err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		id, err := emitMinimalBugReported(ctx, tx, "trigger-update-test")
		eventID = id
		return err
	}); err != nil {
		t.Fatalf("seed emit: %v", err)
	}

	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE events SET rationale = 'tampered' WHERE event_id = ?`, eventID)
		return err
	})
	if err == nil {
		t.Fatal("expected trigger to reject UPDATE, got nil")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("trigger error did not mention append-only: %v", err)
	}

	// Row still has the original rationale.
	var rationale sql.NullString
	if err := pool.DB().QueryRow(`SELECT rationale FROM events WHERE event_id = ?`, eventID).Scan(&rationale); err != nil {
		t.Fatalf("readback: %v", err)
	}
	if rationale.String == "tampered" {
		t.Fatal("trigger did not prevent UPDATE — rationale was overwritten")
	}
}

func TestEvents_DeleteForbiddenByTrigger(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ctx := context.Background()
	var eventID string
	if err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		id, err := emitMinimalBugReported(ctx, tx, "trigger-delete-test")
		eventID = id
		return err
	}); err != nil {
		t.Fatalf("seed emit: %v", err)
	}

	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM events WHERE event_id = ?`, eventID)
		return err
	})
	if err == nil {
		t.Fatal("expected trigger to reject DELETE, got nil")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("trigger error did not mention append-only: %v", err)
	}

	var count int
	if err := pool.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE event_id = ?`, eventID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("DELETE rolled the row away: count=%d", count)
	}
}

// ── IsRegisteredType ───────────────────────────────────────────────────────

func TestIsRegisteredType(t *testing.T) {
	// Derivation-based, no hand-maintained literal list (chain
	// worktree-multi-agent-orchestration-support T7). The catalog is the
	// embedded schemas/*.json set; RegisteredTypes() is its single source of
	// truth. Adding an event type means adding a schema FILE — there is no
	// shared list here for two parallel agents to merge-conflict on. The
	// "does a specific type exist" coverage now lives where it belongs: the
	// fold/emit tests that exercise each event type end to end.
	types := events.RegisteredTypes()
	if len(types) == 0 {
		t.Fatal("RegisteredTypes() returned empty — embedded schema catalog failed to load?")
	}
	for _, typ := range types {
		if typ == "_envelope" {
			t.Error("RegisteredTypes() leaked the _envelope schema as an event type")
		}
		if !events.IsRegisteredType(typ) {
			t.Errorf("IsRegisteredType(%q) = false, but it is in RegisteredTypes()", typ)
		}
	}
	if events.IsRegisteredType("DefinitelyNotARealType") {
		t.Error("IsRegisteredType returned true for a fake type")
	}
}

// TestNewEventTypes_HappyPathEmit covers the six event types added by
// T2's catalog extension (TaskTransitioned, TaskEdited, BugEdited,
// ChainEdited, BugStamped, TaskStamped). Each subtest emits a minimal
// valid payload and asserts the row landed — i.e. the schema validator
// accepts the Go struct's JSON shape end-to-end. Negative-path validation
// (missing required fields, wrong enum values) is covered by the schema
// validator itself + the generic TestEmit_RejectsUnknownType /
// TestEmit_RejectsInvalidPayload tests; per-type negative tests would be
// duplicative.
func TestNewEventTypes_HappyPathEmit(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ctx := context.Background()

	cases := []struct {
		name    string
		entity  events.EntityRef
		payload events.Payload
	}{
		{
			name:   "TaskTransitioned",
			entity: events.NewEntityRef("task", "task-tx", "mcp-servers"),
			payload: events.TaskTransitionedPayload{
				FromStatus: "pending",
				ToStatus:   "active",
			},
		},
		{
			name:   "TaskTransitioned_with_blocker",
			entity: events.NewEntityRef("task", "task-tx-blocked", "mcp-servers"),
			payload: events.TaskTransitionedPayload{
				FromStatus:  "active",
				ToStatus:    "blocked",
				BlockerSlug: ptr("blocker-task"),
			},
		},
		{
			name:   "TaskEdited",
			entity: events.NewEntityRef("task", "task-ed", "mcp-servers"),
			payload: events.TaskEditedPayload{
				UpdatedFields: []string{"problem_statement", "constraints"},
			},
		},
		{
			name:   "BugEdited",
			entity: events.NewEntityRef("bug", "bug-ed", "mcp-servers"),
			payload: events.BugEditedPayload{
				UpdatedFields: []string{"title", "problem_statement"},
			},
		},
		{
			name:   "SuggestionEdited",
			entity: events.NewEntityRef("suggestion", "sug-ed", "mcp-servers"),
			payload: events.SuggestionEditedPayload{
				UpdatedFields: []string{"problem_statement", "priority"},
			},
		},
		{
			name:    "ChainEdited",
			entity:  events.NewEntityRef("chain", "chain-ed", "mcp-servers"),
			payload: events.ChainEditedPayload{UpdatedFields: []string{"completion_condition"}},
		},
		{
			name:    "BugStamped",
			entity:  events.NewEntityRef("bug", "bug-st", "mcp-servers"),
			payload: events.BugStampedPayload{CommitSHA: "abc1234"},
		},
		{
			name:    "TaskStamped",
			entity:  events.NewEntityRef("task", "task-st", "mcp-servers"),
			payload: events.TaskStampedPayload{CommitSHA: "unversioned"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
				_, err := events.Emit(ctx, tx, events.EmitArgs{
					Entity:  c.entity,
					Payload: c.payload,
				})
				return err
			})
			if err != nil {
				t.Fatalf("Emit %s: %v", c.name, err)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// ── helpers ────────────────────────────────────────────────────────────────

// emitMinimalBugReported emits a valid BugReported event for the given
// bug slug. Used to seed events in tests that need a row to UPDATE /
// DELETE / select from.
func emitMinimalBugReported(ctx context.Context, tx *sql.Tx, slug string) (string, error) {
	return events.Emit(ctx, tx, events.EmitArgs{
		Entity: events.NewEntityRef("bug", slug, "mcp-servers"),
		Payload: events.BugReportedPayload{
			Title:            "test bug for events package",
			ProblemStatement: "seeded by events_test.go",
		},
	})
}

// validBenchmarkStartedPayload returns a fully-populated BenchmarkRunStarted
// payload — every provenance subfield set. Used by cross-cutting-entity
// tests that don't care about the values but need the payload to validate.
func validBenchmarkStartedPayload() events.BenchmarkRunStartedPayload {
	return events.BenchmarkRunStartedPayload{
		ScenarioID: "scen-1",
		Provenance: events.BenchmarkProvenance{
			ModelID:             "claude-opus-4-7",
			ModelVersion:        "claude-opus-4-7-20260301",
			PromptTemplateHash:  "deadbeef",
			CorpusHash:          "cafebabe",
			RetrieverVersion:    "qwen-rerank-2.5-32b-v3",
			RetrieverConfigHash: "f00dface",
			Seed:                42,
			EnvHash:             "abad1dea",
		},
	}
}
