package telemetry_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/telemetry"
	"toolkit/internal/testutil"
)

// seedGroundingEvent inserts a minimal grounding_events row directly via
// the pool. Used by integration tests to anchor query_interactions FKs.
// Returns the row id.
func seedGroundingEvent(t *testing.T, pool *db.Pool, session, span, queryText string, refs []string) int64 {
	t.Helper()
	refsJSON, _ := json.Marshal(refs)
	if refs == nil {
		refsJSON = []byte("[]")
	}
	var id int64
	err := pool.WithWrite(context.Background(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(context.Background(), `
			INSERT INTO grounding_events
				(project_id, session_id, call_id, action, results_count, source_refs, span_id, query_text)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			"mcp-servers", session, span, "vault_search", len(refs), string(refsJSON), span, queryText)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		t.Fatalf("seed grounding_event: %v", err)
	}
	return id
}

// seedEvent inserts a minimal events row so query_resolutions.write_event_ids
// FK integrity check passes. Returns the event_id.
func seedEvent(t *testing.T, pool *db.Pool, eventID, span, entityKind, entitySlug, eventType string) string {
	t.Helper()
	if eventID == "" {
		eventID = "0190f8a3-7b21-7c64-9d83-" + strings.Repeat("a", 12) + time.Now().Format("050405")
	}
	err := pool.WithWrite(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(), `
			INSERT INTO events (event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug, entity_project_id, payload, span_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			eventID, time.Now().UTC().Format(time.RFC3339Nano),
			"agent", "test-actor", eventType, entityKind, entitySlug, "mcp-servers",
			"{}", span)
		return err
	})
	if err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return eventID
}

// queryInteractionCount returns the row count matching the filter.
func queryInteractionCount(t *testing.T, pool *db.Pool, where string, args ...any) int {
	t.Helper()
	var n int
	row := pool.DB().QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM query_interactions WHERE "+where, args...)
	if err := row.Scan(&n); err != nil {
		t.Fatalf("count query_interactions: %v", err)
	}
	return n
}

// TestEmitInteraction_FollowedClickKind exercises AC test (b): a search
// followed by a Read of the exact path produces a query_interactions row
// with click_kind='followed'. Production wiring: the Stop hook detects
// the follow-up Read in the transcript and calls EmitInteraction; this
// test exercises the helper end-to-end.
func TestEmitInteraction_FollowedClickKind(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ctx := context.Background()
	geID := seedGroundingEvent(t, pool, "sess-1", "span-A", "search query",
		[]string{"vault/learnings/general/x.md"})

	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := telemetry.EmitInteraction(ctx, tx, telemetry.InteractionArgs{
			GroundingEventID: geID,
			SourceRef:        "vault/learnings/general/x.md",
			ClickKind:        telemetry.ClickFollowed,
			SpanID:           "span-A",
			SessionID:        "sess-1",
			DetectedAt:       time.Now().UTC().Format(time.RFC3339Nano),
		})
		return err
	})
	if err != nil {
		t.Fatalf("emit followed: %v", err)
	}
	got := queryInteractionCount(t, pool, "click_kind = ? AND span_id = ?", "followed", "span-A")
	if got != 1 {
		t.Fatalf("want 1 followed row, got %d", got)
	}
	// Verify the default weight landed correctly.
	var weight float64
	err = pool.DB().QueryRowContext(ctx,
		"SELECT click_weight FROM query_interactions WHERE span_id = ? AND click_kind = ?",
		"span-A", "followed").Scan(&weight)
	if err != nil {
		t.Fatalf("read weight: %v", err)
	}
	if weight != 1.0 {
		t.Errorf("followed default weight = %v, want 1.0", weight)
	}
}

// TestEmitInteraction_MentionedClickKind exercises AC test (a): a search
// where the result source_ref appears in subsequent assistant text
// produces a click_kind='mentioned' row.
func TestEmitInteraction_MentionedClickKind(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ctx := context.Background()
	geID := seedGroundingEvent(t, pool, "sess-2", "span-B", "q", []string{"vault/x.md"})

	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := telemetry.EmitInteraction(ctx, tx, telemetry.InteractionArgs{
			GroundingEventID: geID,
			SourceRef:        "vault/x.md",
			ClickKind:        telemetry.ClickMentioned,
			SpanID:           "span-B",
			SessionID:        "sess-2",
		})
		return err
	})
	if err != nil {
		t.Fatalf("emit mentioned: %v", err)
	}
	if got := queryInteractionCount(t, pool, "click_kind = ?", "mentioned"); got != 1 {
		t.Fatalf("want 1 mentioned row, got %d", got)
	}
}

// TestEmitInteraction_CitedClickKind exercises AC test (c): a search
// followed by an assistant turn quoting >=40 chars from the result
// produces a click_kind='cited' row with citation_kind populated.
func TestEmitInteraction_CitedClickKind(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ctx := context.Background()
	geID := seedGroundingEvent(t, pool, "sess-3", "span-C", "q", []string{"vault/y.md"})
	citationKind := telemetry.CitationQuotedBlock
	quoteLen := 52
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := telemetry.EmitInteraction(ctx, tx, telemetry.InteractionArgs{
			GroundingEventID:   geID,
			SourceRef:          "vault/y.md",
			ClickKind:          telemetry.ClickCited,
			CitationKind:       &citationKind,
			CitationQuoteChars: &quoteLen,
			SpanID:             "span-C",
			SessionID:          "sess-3",
		})
		return err
	})
	if err != nil {
		t.Fatalf("emit cited: %v", err)
	}
	var got struct {
		kind         string
		citationKind sql.NullString
		quoteChars   sql.NullInt64
		weight       float64
	}
	err = pool.DB().QueryRowContext(ctx, `
		SELECT click_kind, citation_kind, citation_quote_chars, click_weight
		FROM query_interactions WHERE span_id = ?`, "span-C",
	).Scan(&got.kind, &got.citationKind, &got.quoteChars, &got.weight)
	if err != nil {
		t.Fatalf("read row: %v", err)
	}
	if got.kind != "cited" || !got.citationKind.Valid || got.citationKind.String != "quoted-block" {
		t.Errorf("row mismatch: %+v", got)
	}
	if got.quoteChars.Int64 != 52 {
		t.Errorf("quote chars = %d, want 52", got.quoteChars.Int64)
	}
	if got.weight != 0.8 {
		t.Errorf("cited default weight = %v, want 0.8", got.weight)
	}
}

// TestEmitInteraction_MultipleKindsPerPair exercises AC test (g): the
// same (span, source_ref) can fire multiple click_kinds, producing one
// row each, distinguished by the unique (span_id, source_ref, click_kind)
// triple. Aggregation happens in the projection layer (TT3).
func TestEmitInteraction_MultipleKindsPerPair(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ctx := context.Background()
	geID := seedGroundingEvent(t, pool, "sess-4", "span-D", "q", []string{"vault/z.md"})

	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		for _, k := range []telemetry.ClickKind{telemetry.ClickFollowed, telemetry.ClickCited, telemetry.ClickMentioned} {
			_, err := telemetry.EmitInteraction(ctx, tx, telemetry.InteractionArgs{
				GroundingEventID: geID,
				SourceRef:        "vault/z.md",
				ClickKind:        k,
				SpanID:           "span-D",
				SessionID:        "sess-4",
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("emit multi: %v", err)
	}
	if got := queryInteractionCount(t, pool, "span_id = ?", "span-D"); got != 3 {
		t.Fatalf("want 3 rows, got %d", got)
	}
}

// TestEmitResolution_WriteEventIDsFKCheck exercises AC test (e): inserting
// a query_resolutions row with an unknown write_event_id fails. Both the
// Go-side pre-check and the SQLite trigger should catch this; the Go
// pre-check is hit first and returns ErrUnknownEventID.
func TestEmitResolution_WriteEventIDsFKCheck(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ctx := context.Background()

	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := telemetry.EmitResolution(ctx, tx, telemetry.ResolutionArgs{
			PromptID:        "prompt-1",
			SessionID:       "sess-fk",
			SpanID:          "span-fk",
			EntityKind:      "bug",
			EntitySlug:      "test-bug",
			EntityProjectID: "mcp-servers",
			OutcomeKind:     telemetry.OutcomeResolved,
			WriteEventIDs:   []string{"01abc-nonexistent-event"},
		})
		return err
	})
	if err == nil {
		t.Fatal("expected ErrUnknownEventID, got nil")
	}
	var unknown *telemetry.ErrUnknownEventID
	if !errors.As(err, &unknown) {
		t.Errorf("got %T, want *telemetry.ErrUnknownEventID: %v", err, err)
	}
}

// TestEmitResolution_Happy exercises the cross-substrate happy path:
// seed an events row, emit a query_resolutions row that FKs it, verify
// the row landed with the expected arrays.
func TestEmitResolution_Happy(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ctx := context.Background()
	eventID := seedEvent(t, pool, "0190f8a3-7b21-7c64-9d83-aaaaaaaaaaaa", "span-res", "bug", "happy-bug", "BugResolved")
	geID := seedGroundingEvent(t, pool, "sess-res", "span-res-search", "q", []string{"vault/a.md"})

	var resID string
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		var err error
		resID, err = telemetry.EmitResolution(ctx, tx, telemetry.ResolutionArgs{
			PromptID:          "prompt-happy",
			SessionID:         "sess-res",
			SpanID:            "span-res",
			EntityKind:        "bug",
			EntitySlug:        "happy-bug",
			EntityProjectID:   "mcp-servers",
			OutcomeKind:       telemetry.OutcomeResolved,
			WriteEventIDs:     []string{eventID},
			GroundingEventIDs: []int64{geID},
		})
		return err
	})
	if err != nil {
		t.Fatalf("emit resolution: %v", err)
	}
	if resID == "" {
		t.Fatal("emit resolution returned empty id")
	}
	var got struct {
		writeIDs    string
		groundIDs   string
		outcomeKind string
	}
	err = pool.DB().QueryRowContext(ctx, `
		SELECT write_event_ids, grounding_event_ids, outcome_kind
		FROM query_resolutions WHERE resolution_id = ?`, resID,
	).Scan(&got.writeIDs, &got.groundIDs, &got.outcomeKind)
	if err != nil {
		t.Fatalf("read row: %v", err)
	}
	if !strings.Contains(got.writeIDs, eventID) {
		t.Errorf("write_event_ids missing seeded event: %s", got.writeIDs)
	}
	if got.outcomeKind != "resolved" {
		t.Errorf("outcome_kind = %s, want resolved", got.outcomeKind)
	}
}

// TestEmitResolution_NoUpdateNoDelete exercises AC test (f): UPDATE/DELETE
// on query_resolutions fails per the trigger.
func TestEmitResolution_NoUpdateNoDelete(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ctx := context.Background()
	eventID := seedEvent(t, pool, "0190f8a3-7b21-7c64-9d83-bbbbbbbbbbbb", "span-noupdate", "bug", "noupdate-bug", "BugResolved")

	var resID string
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		var err error
		resID, err = telemetry.EmitResolution(ctx, tx, telemetry.ResolutionArgs{
			PromptID:        "prompt-nu",
			SessionID:       "sess-nu",
			SpanID:          "span-noupdate",
			EntityKind:      "bug",
			EntitySlug:      "noupdate-bug",
			EntityProjectID: "mcp-servers",
			OutcomeKind:     telemetry.OutcomeResolved,
			WriteEventIDs:   []string{eventID},
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed resolution: %v", err)
	}

	// UPDATE must ABORT.
	err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE query_resolutions SET outcome_kind = 'cancelled' WHERE resolution_id = ?`, resID)
		return err
	})
	if err == nil {
		t.Fatal("UPDATE should have ABORTed; nothing happened")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("unexpected UPDATE error: %v", err)
	}

	// DELETE must ABORT.
	err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM query_resolutions WHERE resolution_id = ?`, resID)
		return err
	})
	if err == nil {
		t.Fatal("DELETE should have ABORTed; nothing happened")
	}
	if !strings.Contains(err.Error(), "deletion is not supported") {
		t.Errorf("unexpected DELETE error: %v", err)
	}
}

// TestEmitInteraction_ResolvedFromClickKind exercises AC test (d): the
// resolved-from click_kind fires when a terminal event's rationale
// mentions the source_ref. Hook-side production code detects this; the
// test exercises the in-process emit path that the hook uses.
func TestEmitInteraction_ResolvedFromClickKind(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ctx := context.Background()
	eventID := seedEvent(t, pool, "0190f8a3-7b21-7c64-9d83-cccccccccccc", "span-resfrom", "bug", "res-bug", "BugResolved")
	geID := seedGroundingEvent(t, pool, "sess-resfrom", "span-resfrom-search", "q",
		[]string{"vault/r.md"})

	var resID string
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		// Stop hook would: (1) emit query_interactions(resolved-from) for
		// each source_ref the rationale references, (2) emit
		// query_resolutions linking the event_id and grounding_event_id.
		_, err := telemetry.EmitInteraction(ctx, tx, telemetry.InteractionArgs{
			GroundingEventID: geID,
			SourceRef:        "vault/r.md",
			ClickKind:        telemetry.ClickResolvedFrom,
			SpanID:           "span-resfrom-search",
			SessionID:        "sess-resfrom",
		})
		if err != nil {
			return err
		}
		resID, err = telemetry.EmitResolution(ctx, tx, telemetry.ResolutionArgs{
			PromptID:          "prompt-resfrom",
			SessionID:         "sess-resfrom",
			SpanID:            "span-resfrom",
			EntityKind:        "bug",
			EntitySlug:        "res-bug",
			EntityProjectID:   "mcp-servers",
			OutcomeKind:       telemetry.OutcomeResolved,
			WriteEventIDs:     []string{eventID},
			GroundingEventIDs: []int64{geID},
		})
		return err
	})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if resID == "" {
		t.Fatal("empty resolution_id")
	}
	if got := queryInteractionCount(t, pool, "click_kind = ?", "resolved-from"); got != 1 {
		t.Errorf("want 1 resolved-from row, got %d", got)
	}
}

// TestEmitInteraction_Idempotent exercises the UPSERT-on-conflict
// behavior: emitting the same (span_id, source_ref, click_kind) triple
// twice produces one row, not an error. Lets the Stop hook re-walk a
// session idempotently.
func TestEmitInteraction_Idempotent(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ctx := context.Background()
	geID := seedGroundingEvent(t, pool, "sess-idem", "span-idem", "q", []string{"vault/i.md"})

	args := telemetry.InteractionArgs{
		GroundingEventID: geID,
		SourceRef:        "vault/i.md",
		ClickKind:        telemetry.ClickFollowed,
		SpanID:           "span-idem",
		SessionID:        "sess-idem",
	}
	var id1, id2 int64
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		var err error
		id1, err = telemetry.EmitInteraction(ctx, tx, args)
		if err != nil {
			return err
		}
		id2, err = telemetry.EmitInteraction(ctx, tx, args)
		return err
	})
	if err != nil {
		t.Fatalf("idempotent emit: %v", err)
	}
	if id1 != id2 {
		t.Errorf("idempotent emit returned different ids: %d vs %d", id1, id2)
	}
	if got := queryInteractionCount(t, pool, "span_id = ?", "span-idem"); got != 1 {
		t.Errorf("want 1 row after idempotent emit, got %d", got)
	}
}

// TestQuerySourceContext exercises the ctx-based propagation of
// query_source through the new telemetry-context API. Validates the
// 'agent_initiated' default and an override.
func TestQuerySourceContext(t *testing.T) {
	ctx := context.Background()
	if got := telemetry.QuerySourceFromContext(ctx); got != telemetry.SourceAgentInitiated {
		t.Errorf("default = %q, want %q", got, telemetry.SourceAgentInitiated)
	}
	ctx2 := telemetry.WithQuerySource(ctx, telemetry.SourceProactiveHook)
	if got := telemetry.QuerySourceFromContext(ctx2); got != telemetry.SourceProactiveHook {
		t.Errorf("override = %q, want %q", got, telemetry.SourceProactiveHook)
	}
	// Original context unchanged.
	if got := telemetry.QuerySourceFromContext(ctx); got != telemetry.SourceAgentInitiated {
		t.Errorf("immutability broken: %q", got)
	}
}

// TestInvalidClickKindRejected confirms the Go-side validation rejects
// unknown click_kinds before the SQL CHECK would.
func TestInvalidClickKindRejected(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ctx := context.Background()
	geID := seedGroundingEvent(t, pool, "sess-iv", "span-iv", "q", []string{"vault/iv.md"})
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := telemetry.EmitInteraction(ctx, tx, telemetry.InteractionArgs{
			GroundingEventID: geID,
			SourceRef:        "vault/iv.md",
			ClickKind:        "drive-by",
			SpanID:           "span-iv",
			SessionID:        "sess-iv",
		})
		return err
	})
	if err == nil {
		t.Fatal("expected ErrInvalidInput, got nil")
	}
	var ie *telemetry.ErrInvalidInput
	if !errors.As(err, &ie) || ie.Field != "ClickKind" {
		t.Errorf("got %T (field=%v) want *ErrInvalidInput{Field:ClickKind}: %v",
			err, fieldOf(err), err)
	}
}

func fieldOf(err error) string {
	var ie *telemetry.ErrInvalidInput
	if errors.As(err, &ie) {
		return ie.Field
	}
	return ""
}
