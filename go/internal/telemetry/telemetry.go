package telemetry

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// FoldHook is the signature of the same-tx projection fold trigger.
// Bootstrap wires the read-side projections' fold-all via [SetFoldHook];
// emits run the hook inside their write tx after the row INSERT lands,
// and propagate any error back through Emit* so the tx rolls back. The
// hook is nil by default — tests that don't exercise projection folds
// (the telemetry package's own tests, for instance) leave it unset and
// the emits behave as if the hook didn't exist.
//
// TT3 §AC item: "Projection updates run synchronously with the
// originating telemetry emit … Fold failure aborts the emit." This
// hook is the seam that delivers that contract.
type FoldHook func(ctx context.Context, tx *sql.Tx) error

var foldHook FoldHook

// SetFoldHook installs the read-side fold trigger. Called by the
// toolkit-server bootstrap with [projections.FoldAllReadSide]. Passing
// nil clears the hook — tests that need a clean baseline use this
// shape.
func SetFoldHook(h FoldHook) { foldHook = h }

// ClickKind is the four-tier click signal enum closed by TT1.5 (see
// docs/TELEMETRY_LABEL_SPIKE.md §4). Each tier corresponds to a CHECK
// constraint value in query_interactions.click_kind.
type ClickKind string

const (
	ClickFollowed     ClickKind = "followed"      // Read/vault_read/kiwix_fetch on the exact source_ref
	ClickCited        ClickKind = "cited"         // ≥40 char quote OR markdown link / file:line reference
	ClickMentioned    ClickKind = "mentioned"     // source_ref string appears in subsequent assistant text
	ClickResolvedFrom ClickKind = "resolved-from" // terminal rationale references the source_ref
)

// DefaultClickWeights are the per-tier default weights TT1.5 confirmed.
// A per-installation override (~/.config/toolkit-server/click-weights.toml)
// may rewrite these; the values here are the embedded fallback consulted
// when no override is present.
var DefaultClickWeights = map[ClickKind]float64{
	ClickFollowed:     1.0,
	ClickCited:        0.8,
	ClickMentioned:    0.4,
	ClickResolvedFrom: 1.0,
}

// OutcomeKind is the closed set of terminal-event outcomes the
// query_resolutions.outcome_kind CHECK constraint enforces.
type OutcomeKind string

const (
	OutcomeResolved   OutcomeKind = "resolved"   // bug fixed
	OutcomeCompleted  OutcomeKind = "completed"  // task completed
	OutcomeCancelled  OutcomeKind = "cancelled"  // task cancelled
	OutcomeClosed     OutcomeKind = "closed"     // chain closed
	OutcomeSuperseded OutcomeKind = "superseded" // entity superseded by another
	OutcomeDiscarded  OutcomeKind = "discarded"  // entity discarded before resolution
)

// QuerySource is the discriminator separating agent-initiated queries
// from hook-initiated ones (TT1 §8). The CHECK constraint allows an
// 'other' fallback for future query sources without a schema migration.
type QuerySource string

const (
	SourceAgentInitiated QuerySource = "agent_initiated"
	SourceProactiveHook  QuerySource = "proactive_hook"
	SourceDashboardUser  QuerySource = "dashboard_user"
	SourceOther          QuerySource = "other"
)

// CitationKind is a sub-classifier set only when ClickKind == ClickCited.
// Names match the three citation patterns documented in TT1 §5.3.
type CitationKind string

const (
	CitationMarkdownLink CitationKind = "markdown-link"
	CitationFileLine     CitationKind = "file-line"
	CitationQuotedBlock  CitationKind = "quoted-block"
)

// InteractionArgs carries the field set for one query_interactions row.
// Unset fields land as their zero value; pointer fields land as NULL
// when nil (Go's database/sql convention).
type InteractionArgs struct {
	GroundingEventID        int64
	SourceRef               string
	Position                *int
	ClickKind               ClickKind
	ClickWeight             float64
	CitationKind            *CitationKind
	CitationQuoteChars      *int
	DwellMSEstimate         *int
	WasInjected             bool
	InjectionPosition       *int
	InjectionWasUserVisible *bool
	SpanID                  string
	PromptID                *string
	SessionID               string
	ParentSpanID            *string
	DetectedAt              string // RFC 3339; defaults to time.Now if empty
}

// ResolutionArgs carries the field set for one query_resolutions row.
// WriteEventIDs is a Go slice; the emit helper marshals to JSON and the
// schema-level trigger validates each event_id against events.event_id.
type ResolutionArgs struct {
	PromptID            string
	SessionID           string
	SpanID              string
	EntityKind          string
	EntitySlug          string
	EntityProjectID     string
	OutcomeKind         OutcomeKind
	WriteEventIDs       []string
	GroundingEventIDs   []int64
	QueryInteractionIDs []int64
	DetectedAt          string
}

// ErrInvalidInput is returned when the args fail Go-side validation
// before any DB write. The struct mirrors events.ErrInvalidInput so
// callers can branch on (*ErrInvalidInput) uniformly across substrates.
type ErrInvalidInput struct {
	Field  string
	Reason string
}

func (e *ErrInvalidInput) Error() string {
	if e.Field == "" {
		return "telemetry: invalid input: " + e.Reason
	}
	return "telemetry: invalid input on field " + e.Field + ": " + e.Reason
}

// ErrUnknownEventID is returned when EmitResolution's pre-check finds
// a write_event_id that doesn't exist in events. The SQLite trigger
// would catch this at INSERT time anyway; this helper provides a
// typed error before the round-trip.
type ErrUnknownEventID struct {
	EventID string
}

func (e *ErrUnknownEventID) Error() string {
	return "telemetry: write_event_id not present in events: " + e.EventID
}

func isValidClickKind(k ClickKind) bool {
	switch k {
	case ClickFollowed, ClickCited, ClickMentioned, ClickResolvedFrom:
		return true
	}
	return false
}

func isValidOutcomeKind(k OutcomeKind) bool {
	switch k {
	case OutcomeResolved, OutcomeCompleted, OutcomeCancelled, OutcomeClosed, OutcomeSuperseded, OutcomeDiscarded:
		return true
	}
	return false
}

func isValidCitationKind(k CitationKind) bool {
	switch k {
	case CitationMarkdownLink, CitationFileLine, CitationQuotedBlock:
		return true
	}
	return false
}

// EmitInteraction inserts one row into query_interactions. Click weight
// defaults from DefaultClickWeights when args.ClickWeight is zero; pass
// a non-zero value to override (per-installation tuning). On UNIQUE
// constraint violation (the same (span_id, source_ref, click_kind)
// triple emitted twice), the second call returns the existing row's id
// via SELECT — letting the Stop hook re-walk a session idempotently.
func EmitInteraction(ctx context.Context, tx *sql.Tx, args InteractionArgs) (int64, error) {
	if args.GroundingEventID == 0 {
		return 0, &ErrInvalidInput{Field: "GroundingEventID", Reason: "required"}
	}
	if strings.TrimSpace(args.SourceRef) == "" {
		return 0, &ErrInvalidInput{Field: "SourceRef", Reason: "required"}
	}
	if !isValidClickKind(args.ClickKind) {
		return 0, &ErrInvalidInput{Field: "ClickKind", Reason: "unknown kind: " + string(args.ClickKind)}
	}
	if args.CitationKind != nil && !isValidCitationKind(*args.CitationKind) {
		return 0, &ErrInvalidInput{Field: "CitationKind", Reason: "unknown kind: " + string(*args.CitationKind)}
	}
	if args.SpanID == "" {
		return 0, &ErrInvalidInput{Field: "SpanID", Reason: "required"}
	}
	if args.SessionID == "" {
		return 0, &ErrInvalidInput{Field: "SessionID", Reason: "required"}
	}
	weight := args.ClickWeight
	if weight == 0 {
		if w, ok := DefaultClickWeights[args.ClickKind]; ok {
			weight = w
		}
	}
	detected := args.DetectedAt
	if detected == "" {
		detected = time.Now().UTC().Format(time.RFC3339Nano)
	}

	var citationKindStr *string
	if args.CitationKind != nil {
		s := string(*args.CitationKind)
		citationKindStr = &s
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO query_interactions (
			grounding_event_id, source_ref, position, click_kind, click_weight,
			citation_kind, citation_quote_chars, dwell_ms_estimate,
			was_injected, injection_position, injection_was_user_visible,
			span_id, prompt_id, session_id, parent_span_id, detected_at
		)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (span_id, source_ref, click_kind) DO UPDATE SET
			click_weight   = excluded.click_weight,
			citation_kind  = excluded.citation_kind,
			dwell_ms_estimate = excluded.dwell_ms_estimate,
			was_injected   = excluded.was_injected
	`,
		args.GroundingEventID, args.SourceRef, args.Position, string(args.ClickKind), weight,
		citationKindStr, args.CitationQuoteChars, args.DwellMSEstimate,
		boolToInt(args.WasInjected), args.InjectionPosition, boolPtrToIntPtr(args.InjectionWasUserVisible),
		args.SpanID, args.PromptID, args.SessionID, args.ParentSpanID, detected,
	)
	if err != nil {
		return 0, fmt.Errorf("insert query_interactions: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read inserted id: %w", err)
	}
	// LastInsertId returns 0 on the UPSERT-no-op path; resolve via SELECT.
	if id == 0 {
		row := tx.QueryRowContext(ctx,
			`SELECT id FROM query_interactions WHERE span_id = ? AND source_ref = ? AND click_kind = ?`,
			args.SpanID, args.SourceRef, string(args.ClickKind))
		if err := row.Scan(&id); err != nil {
			return 0, fmt.Errorf("re-fetch upserted id: %w", err)
		}
	}
	if foldHook != nil {
		if err := foldHook(ctx, tx); err != nil {
			return 0, fmt.Errorf("read-side fold: %w", err)
		}
	}
	return id, nil
}

// EmitResolution inserts one row into query_resolutions, minting a
// fresh UUIDv7 resolution_id. The pre-check validates every write_event_id
// exists in events; absent IDs surface as *ErrUnknownEventID before
// the INSERT touches the table. The schema-level FK trigger is the
// defense-in-depth structural enforcement — this Go-side check makes
// the error typed and avoids a wasted round-trip.
func EmitResolution(ctx context.Context, tx *sql.Tx, args ResolutionArgs) (string, error) {
	if args.PromptID == "" {
		return "", &ErrInvalidInput{Field: "PromptID", Reason: "required"}
	}
	if args.SessionID == "" {
		return "", &ErrInvalidInput{Field: "SessionID", Reason: "required"}
	}
	if args.SpanID == "" {
		return "", &ErrInvalidInput{Field: "SpanID", Reason: "required"}
	}
	if args.EntityKind == "" || args.EntitySlug == "" || args.EntityProjectID == "" {
		return "", &ErrInvalidInput{Field: "Entity", Reason: "kind, slug, project_id all required"}
	}
	switch args.EntityKind {
	case "bug", "task", "chain":
	default:
		return "", &ErrInvalidInput{Field: "EntityKind", Reason: "unknown kind: " + args.EntityKind}
	}
	if !isValidOutcomeKind(args.OutcomeKind) {
		return "", &ErrInvalidInput{Field: "OutcomeKind", Reason: "unknown kind: " + string(args.OutcomeKind)}
	}

	// Validate write_event_ids exist in events. Empty array is fine.
	for _, eid := range args.WriteEventIDs {
		var exists int
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE event_id = ?)`, eid).Scan(&exists)
		if err != nil {
			return "", fmt.Errorf("validate event_id %s: %w", eid, err)
		}
		if exists == 0 {
			return "", &ErrUnknownEventID{EventID: eid}
		}
	}

	writeJSON, err := jsonArray(args.WriteEventIDs)
	if err != nil {
		return "", fmt.Errorf("marshal write_event_ids: %w", err)
	}
	groundJSON, err := jsonInt64Array(args.GroundingEventIDs)
	if err != nil {
		return "", fmt.Errorf("marshal grounding_event_ids: %w", err)
	}
	interJSON, err := jsonInt64Array(args.QueryInteractionIDs)
	if err != nil {
		return "", fmt.Errorf("marshal query_interaction_ids: %w", err)
	}

	resID, err := newUUIDv7()
	if err != nil {
		return "", fmt.Errorf("mint resolution_id: %w", err)
	}
	detected := args.DetectedAt
	if detected == "" {
		detected = time.Now().UTC().Format(time.RFC3339Nano)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO query_resolutions (
			resolution_id, prompt_id, session_id, span_id,
			entity_kind, entity_slug, entity_project_id, outcome_kind,
			write_event_ids, grounding_event_ids, query_interaction_ids,
			detected_at
		)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		resID, args.PromptID, args.SessionID, args.SpanID,
		args.EntityKind, args.EntitySlug, args.EntityProjectID, string(args.OutcomeKind),
		writeJSON, groundJSON, interJSON,
		detected,
	)
	if err != nil {
		return "", fmt.Errorf("insert query_resolutions: %w", err)
	}
	if foldHook != nil {
		if err := foldHook(ctx, tx); err != nil {
			return "", fmt.Errorf("read-side fold: %w", err)
		}
	}
	return resID, nil
}

// jsonArray marshals a string slice to a JSON array. An empty slice
// produces "[]" (the table's DEFAULT) so the FK trigger's
// json_array_length check passes vacuously.
func jsonArray(s []string) (string, error) {
	if len(s) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func jsonInt64Array(ns []int64) (string, error) {
	if len(ns) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(ns)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func boolPtrToIntPtr(b *bool) *int {
	if b == nil {
		return nil
	}
	v := 0
	if *b {
		v = 1
	}
	return &v
}

// newUUIDv7 mints a fresh UUIDv7 — 48-bit Unix-ms timestamp followed
// by 74 bits of randomness, with version (7) and variant (RFC 4122)
// markers in the standard positions. The internal events package has
// a fuller version with intra-ms monotonicity (RFC 9562 §6.2 Method 1);
// for query_resolutions.resolution_id we don't need cross-process
// ordering, so this lighter version is sufficient. If collisions
// surface in practice, switch to events.NewUUIDv7 (TODO: export it).
func newUUIDv7() (string, error) {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	binary.BigEndian.PutUint64(b[0:8], ms<<16)
	if _, err := rand.Read(b[6:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0F) | 0x70 // version 7
	b[8] = (b[8] & 0x3F) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	), nil
}

var _ = errors.New // keep errors import in case future helpers add typed errors
