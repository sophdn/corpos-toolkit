package testutil

import (
	"fmt"
	"testing"
	"time"

	"toolkit/internal/db"
)

// Telemetry seeders for the three NOT-NULL-heavy tables
// (grounding_events, query_interactions, query_resolutions) so future
// tests don't re-derive column requirements the way ml-capability-
// substrate T6's dispatch_test.go did (closes bug 1455).
//
// Each public seeder applies sensible defaults that satisfy every NOT
// NULL + CHECK constraint and returns the inserted row's id (or
// resolution_id for query_resolutions). Functional options override
// individual fields when the test needs them.
//
// Conventions:
//
//   - Time-shaped fields (detected_at) default to time.Now() rendered
//     as RFC3339Nano; tests asserting ordering should pass WithDetectedAt
//     for determinism.
//
//   - String defaults use "test-<field>" prefixes so a join across two
//     seeded rows doesn't accidentally match on placeholder values.
//
//   - Project / session defaults match the dominant test pattern
//     ("mcp-servers" / "test-session") so seeded rows interoperate with
//     existing per-package helpers (seedBug / seedChain / seedTask in
//     internal/work, which the constraint note says we leave alone).

// GroundingEventOpts holds the per-call overrides for SeedGroundingEvent.
// Zero-valued fields fall back to defaults below.
type GroundingEventOpts struct {
	Project           string
	SessionID         string
	CallID            string
	Action            string
	ResultsCount      int
	SourceRefs        string // JSON-encoded array; default "[]"
	NextTurnHasOutput int    // 0 or 1
	SpanID            string
	PromptID          string
	ParentSpanID      string
	QuerySource       string // must satisfy the schema CHECK; default "agent_initiated"
	UserMessageID     string
	QueryText         string
	CreatedAt         string // when "", the schema default datetime('now') applies
}

// GroundingEventOption is a functional override applied to GroundingEventOpts.
type GroundingEventOption func(*GroundingEventOpts)

// SeedGroundingEvent inserts one grounding_events row with all NOT NULL
// columns populated and returns the inserted id. Test code reaches for
// this when it needs a parent grounding_event_id to hang query_interactions
// off, or when the test simply needs "some row" to exist for an
// aggregation/projection query.
//
// Example:
//
//	geID := testutil.SeedGroundingEvent(t, pool,
//	    testutil.WithGroundingSpan("span-x"),
//	    testutil.WithGroundingQuerySource("reference_resolution"))
//
// The schema's CHECK constraint on query_source accepts the closed set
// {agent_initiated, proactive_hook, dashboard_user, reference_resolution,
// harness_reminder_interception, other}; passing a different value
// fails the INSERT (the test's job, not the seeder's).
func SeedGroundingEvent(t *testing.T, pool *db.Pool, opts ...GroundingEventOption) int64 {
	t.Helper()
	o := GroundingEventOpts{
		Project:           "mcp-servers",
		SessionID:         "test-session",
		Action:            "test_action",
		ResultsCount:      0,
		SourceRefs:        "[]",
		NextTurnHasOutput: 0,
		QuerySource:       "agent_initiated",
	}
	for _, fn := range opts {
		fn(&o)
	}
	// Auto-generate slot-shaped IDs when the caller didn't supply one.
	// `t.Name()` keeps the value scoped to the test so cross-test
	// uniqueness holds without the caller doing the work.
	if o.CallID == "" {
		o.CallID = fmt.Sprintf("call-%s-%d", t.Name(), time.Now().UnixNano())
	}
	if o.SpanID == "" {
		o.SpanID = fmt.Sprintf("span-%s-%d", t.Name(), time.Now().UnixNano())
	}

	args := db.NewArgs().
		AddString(o.Project).
		AddString(o.SessionID).
		AddString(o.CallID).
		AddString(o.Action).
		AddInt64(int64(o.ResultsCount)).
		AddString(o.SourceRefs).
		AddInt64(int64(o.NextTurnHasOutput)).
		AddString(o.SpanID).
		AddNullableString(stringPtrOrNil(o.PromptID)).
		AddNullableString(stringPtrOrNil(o.ParentSpanID)).
		AddString(o.QuerySource).
		AddNullableString(stringPtrOrNil(o.UserMessageID)).
		AddNullableString(stringPtrOrNil(o.QueryText))

	// created_at is appended only when the caller set it; otherwise the
	// column is omitted so the schema's datetime('now') default applies —
	// preserving the ~now behavior every existing caller relies on.
	cols := "project_id, session_id, call_id, action," +
		" results_count, source_refs, next_turn_has_output," +
		" span_id, prompt_id, parent_span_id," +
		" query_source, user_message_id, query_text"
	placeholders := "?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?"
	if o.CreatedAt != "" {
		args.AddString(o.CreatedAt)
		cols += ", created_at"
		placeholders += ", ?"
	}

	res, err := pool.DB().Exec(
		"INSERT INTO grounding_events ("+cols+") VALUES ("+placeholders+")",
		args.Slice()...)
	if err != nil {
		t.Fatalf("SeedGroundingEvent: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("SeedGroundingEvent LastInsertId: %v", err)
	}
	return id
}

// WithGroundingProject overrides project_id.
func WithGroundingProject(p string) GroundingEventOption {
	return func(o *GroundingEventOpts) { o.Project = p }
}

// WithGroundingSession overrides session_id.
func WithGroundingSession(s string) GroundingEventOption {
	return func(o *GroundingEventOpts) { o.SessionID = s }
}

// WithGroundingCallID overrides call_id.
func WithGroundingCallID(c string) GroundingEventOption {
	return func(o *GroundingEventOpts) { o.CallID = c }
}

// WithGroundingAction overrides action.
func WithGroundingAction(a string) GroundingEventOption {
	return func(o *GroundingEventOpts) { o.Action = a }
}

// WithGroundingSpan overrides span_id.
func WithGroundingSpan(s string) GroundingEventOption {
	return func(o *GroundingEventOpts) { o.SpanID = s }
}

// WithGroundingQuerySource overrides query_source. Must satisfy the
// schema's CHECK enum.
func WithGroundingQuerySource(q string) GroundingEventOption {
	return func(o *GroundingEventOpts) { o.QuerySource = q }
}

// WithGroundingQueryText overrides query_text.
func WithGroundingQueryText(q string) GroundingEventOption {
	return func(o *GroundingEventOpts) { o.QueryText = q }
}

// WithGroundingPromptID overrides prompt_id (per-user-input arc key).
func WithGroundingPromptID(p string) GroundingEventOption {
	return func(o *GroundingEventOpts) { o.PromptID = p }
}

// WithGroundingResultsCount overrides results_count.
func WithGroundingResultsCount(n int) GroundingEventOption {
	return func(o *GroundingEventOpts) { o.ResultsCount = n }
}

// WithGroundingSourceRefs overrides source_refs (JSON-encoded array string).
func WithGroundingSourceRefs(refs string) GroundingEventOption {
	return func(o *GroundingEventOpts) { o.SourceRefs = refs }
}

// WithGroundingCreatedAt overrides created_at. Tests pinning a proximity
// window (e.g. the vault-rerank success predicate's latency-scaled
// tolerance) pass this so the grounding row sits a known delta from the
// inference row; the format must be one SQLite's strftime parses
// (RFC3339 or 'YYYY-MM-DD HH:MM:SS').
func WithGroundingCreatedAt(ts string) GroundingEventOption {
	return func(o *GroundingEventOpts) { o.CreatedAt = ts }
}

// QueryInteractionOpts holds the per-call overrides for SeedQueryInteraction.
type QueryInteractionOpts struct {
	SourceRef               string
	Position                *int
	ClickKind               string // {followed, cited, mentioned, resolved-from}
	ClickWeight             float64
	CitationKind            string
	CitationQuoteChars      *int
	DwellMSEstimate         *int
	WasInjected             int
	InjectionPosition       *int
	InjectionWasUserVisible *int
	SpanID                  string
	PromptID                string
	SessionID               string
	ParentSpanID            string
	DetectedAt              string
}

// QueryInteractionOption is a functional override applied to QueryInteractionOpts.
type QueryInteractionOption func(*QueryInteractionOpts)

// SeedQueryInteraction inserts one query_interactions row tied to the
// supplied grounding_event_id (must already exist; the schema enforces
// the FK). Returns the inserted id. The unique index
// (span_id, source_ref, click_kind) means callers seeding multiple
// rows against the same span must vary at least one of the three —
// the defaults below auto-vary span_id so back-to-back calls don't
// collide without the caller opting into a specific span.
func SeedQueryInteraction(t *testing.T, pool *db.Pool, groundingEventID int64, opts ...QueryInteractionOption) int64 {
	t.Helper()
	o := QueryInteractionOpts{
		SourceRef:   "test/source-ref",
		ClickKind:   "cited",
		ClickWeight: 1.0,
		WasInjected: 0,
		SessionID:   "test-session",
	}
	for _, fn := range opts {
		fn(&o)
	}
	if o.SpanID == "" {
		o.SpanID = fmt.Sprintf("span-qi-%s-%d", t.Name(), time.Now().UnixNano())
	}
	if o.DetectedAt == "" {
		o.DetectedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}

	args := db.NewArgs().
		AddInt64(groundingEventID).
		AddString(o.SourceRef)
	if o.Position != nil {
		args.AddInt64(int64(*o.Position))
	} else {
		args.AddNullableString(nil)
	}
	args.AddString(o.ClickKind).
		AddFloat(o.ClickWeight).
		AddNullableString(stringPtrOrNil(o.CitationKind))
	if o.CitationQuoteChars != nil {
		args.AddInt64(int64(*o.CitationQuoteChars))
	} else {
		args.AddNullableString(nil)
	}
	if o.DwellMSEstimate != nil {
		args.AddInt64(int64(*o.DwellMSEstimate))
	} else {
		args.AddNullableString(nil)
	}
	args.AddInt64(int64(o.WasInjected))
	if o.InjectionPosition != nil {
		args.AddInt64(int64(*o.InjectionPosition))
	} else {
		args.AddNullableString(nil)
	}
	if o.InjectionWasUserVisible != nil {
		args.AddInt64(int64(*o.InjectionWasUserVisible))
	} else {
		args.AddNullableString(nil)
	}
	args.AddString(o.SpanID).
		AddNullableString(stringPtrOrNil(o.PromptID)).
		AddString(o.SessionID).
		AddNullableString(stringPtrOrNil(o.ParentSpanID)).
		AddString(o.DetectedAt)

	res, err := pool.DB().Exec(`
		INSERT INTO query_interactions
			(grounding_event_id, source_ref, position,
			 click_kind, click_weight,
			 citation_kind, citation_quote_chars, dwell_ms_estimate,
			 was_injected, injection_position, injection_was_user_visible,
			 span_id, prompt_id, session_id, parent_span_id, detected_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, args.Slice()...)
	if err != nil {
		t.Fatalf("SeedQueryInteraction: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("SeedQueryInteraction LastInsertId: %v", err)
	}
	return id
}

// WithQISourceRef overrides source_ref.
func WithQISourceRef(s string) QueryInteractionOption {
	return func(o *QueryInteractionOpts) { o.SourceRef = s }
}

// WithQIClickKind overrides click_kind. Must satisfy the schema's CHECK
// enum: {followed, cited, mentioned, resolved-from}.
func WithQIClickKind(k string) QueryInteractionOption {
	return func(o *QueryInteractionOpts) { o.ClickKind = k }
}

// WithQIClickWeight overrides click_weight.
func WithQIClickWeight(w float64) QueryInteractionOption {
	return func(o *QueryInteractionOpts) { o.ClickWeight = w }
}

// WithQISpan overrides span_id. Pair with WithQISourceRef when seeding
// multiple rows for the same span to avoid the unique-index collision
// on (span_id, source_ref, click_kind).
func WithQISpan(s string) QueryInteractionOption {
	return func(o *QueryInteractionOpts) { o.SpanID = s }
}

// WithQISession overrides session_id (defaults to "test-session" to
// match SeedGroundingEvent's default for cheap join).
func WithQISession(s string) QueryInteractionOption {
	return func(o *QueryInteractionOpts) { o.SessionID = s }
}

// WithQIPromptID overrides prompt_id.
func WithQIPromptID(p string) QueryInteractionOption {
	return func(o *QueryInteractionOpts) { o.PromptID = p }
}

// WithQIPosition overrides position (1-indexed rank in the original result list).
func WithQIPosition(n int) QueryInteractionOption {
	return func(o *QueryInteractionOpts) { o.Position = &n }
}

// WithQIDetectedAt overrides detected_at (RFC3339Nano string). Use this
// for deterministic ordering in tests that assert order-by-detected_at.
func WithQIDetectedAt(ts string) QueryInteractionOption {
	return func(o *QueryInteractionOpts) { o.DetectedAt = ts }
}

// WithQIWasInjected sets the was_injected flag (and lets the caller
// also set the related injection_position / injection_was_user_visible
// via the other With* options).
func WithQIWasInjected(b bool) QueryInteractionOption {
	return func(o *QueryInteractionOpts) {
		if b {
			o.WasInjected = 1
		} else {
			o.WasInjected = 0
		}
	}
}

// stringPtrOrNil returns nil for empty strings, &s otherwise, so the
// pointer-shaped sql.NullString equivalent (a *string) lands as SQL NULL
// when the caller didn't supply a value.
func stringPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
