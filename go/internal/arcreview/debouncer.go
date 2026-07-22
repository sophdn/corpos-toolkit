package arcreview

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"toolkit/internal/db"
)

// DefaultBackoffSeconds is the minimum seconds between two successful
// review fires for the same session_id, per design §Thresholds (B=60).
// Subsumes the 30s coalesce window: triggers within 60s of a successful
// fire are coalesced into the original fire.
const DefaultBackoffSeconds = 60

// debouncerClock is mockable so tests can drive deterministic times.
// Production uses time.Now; tests override via WithClock.
type debouncerClock func() time.Time

// DebouncerCheck is the result of one CheckAndRecordAttempt call. When
// Allowed is true, the caller should run the review and follow up with
// RecordFire on success. When Allowed is false, LastFire carries the
// timestamp of the prior fire so the caller can log the suppression
// reason.
type DebouncerCheck struct {
	Allowed  bool
	LastFire time.Time
}

// Debouncer is the per-session debounce gate. Backed by the
// arc_review_debouncer SQLite table (migration 048).
//
// Concurrent fires for the same session race past CheckAndRecordAttempt
// with a small window — both calls see no recent fire and both pass.
// This is acceptable for v1: the cost of a duplicate review is one
// extra Qwen round-trip, and the real-world fire cadence is bounded
// (debounce-after-fire prevents runaway). Stage 3 may tighten via
// INSERT-OR-FAIL claim semantics if telemetry shows the race
// materializing.
type Debouncer struct {
	pool           *db.Pool
	backoffSeconds int
	clock          debouncerClock
}

// NewDebouncer constructs a Debouncer using the given pool with the
// default backoff window. Use WithBackoffSeconds and WithClock to
// override for tests.
func NewDebouncer(pool *db.Pool) *Debouncer {
	return &Debouncer{
		pool:           pool,
		backoffSeconds: DefaultBackoffSeconds,
		clock:          time.Now,
	}
}

// WithBackoffSeconds returns a Debouncer with the given backoff window.
// Zero or negative falls back to DefaultBackoffSeconds.
func (d *Debouncer) WithBackoffSeconds(s int) *Debouncer {
	if s <= 0 {
		s = DefaultBackoffSeconds
	}
	cp := *d
	cp.backoffSeconds = s
	return &cp
}

// WithClock returns a Debouncer driven by the given clock. Test-only.
func (d *Debouncer) WithClock(c debouncerClock) *Debouncer {
	cp := *d
	cp.clock = c
	return &cp
}

// CheckAndRecordAttempt is the first call any review-attempting code
// makes for a session. It reads the prior fire/attempt timestamps,
// decides whether the call is allowed under the backoff window, and
// stamps last_fire_attempt_at regardless of the decision.
//
// Returns Allowed=true when both last_fire_at and last_fire_attempt_at
// are absent OR older than the backoff window; the caller then runs
// the review and calls RecordFire on success.
//
// Returns Allowed=false when EITHER timestamp falls inside the window;
// LastFire carries the effective (most-recent) timestamp so the caller
// can log the suppression reason. Gating on last_fire_attempt_at as
// well as last_fire_at closes bug 1476 (race where two triggers land
// during an in-flight review and both pass — last_fire_at isn't yet
// updated because RecordFire only runs after Qwen returns, but
// last_fire_attempt_at IS updated synchronously). Side effect: a
// failed Qwen call's attempt timestamp suppresses the next attempt
// for `backoffSeconds`; treat that as a feature (don't hammer a
// down Qwen).
func (d *Debouncer) CheckAndRecordAttempt(ctx context.Context, sessionID string) (DebouncerCheck, error) {
	if sessionID == "" {
		return DebouncerCheck{}, fmt.Errorf("arcreview debouncer: session_id is empty")
	}
	now := d.clock().UTC()
	nowStr := formatDebouncerTimestamp(now)

	var lastFireStr, lastAttemptStr sql.NullString
	err := d.pool.DB().QueryRowContext(ctx,
		`SELECT last_fire_at, last_fire_attempt_at FROM arc_review_debouncer WHERE session_id = ?`,
		sessionID,
	).Scan(&lastFireStr, &lastAttemptStr)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		// First trigger for this session — allow.
		if writeErr := d.stampAttempt(ctx, sessionID, nowStr); writeErr != nil {
			return DebouncerCheck{}, writeErr
		}
		return DebouncerCheck{Allowed: true}, nil
	case err != nil:
		return DebouncerCheck{}, fmt.Errorf("arcreview debouncer: read row: %w", err)
	}

	// Effective-last = the most recent of last_fire_at and
	// last_fire_attempt_at. Either being inside the backoff window
	// suppresses the call (bug 1476).
	effectiveLast, hasEffective := mostRecent(lastFireStr, lastAttemptStr)
	if !hasEffective {
		// Both columns are NULL or unparseable — treat as no prior
		// activity and allow (matches the pre-1476 behaviour for a
		// freshly-stamped row with NULL last_fire_at).
		if writeErr := d.stampAttempt(ctx, sessionID, nowStr); writeErr != nil {
			return DebouncerCheck{}, writeErr
		}
		return DebouncerCheck{Allowed: true}, nil
	}

	if now.Sub(effectiveLast) < time.Duration(d.backoffSeconds)*time.Second {
		// Inside the backoff window — suppress.
		if writeErr := d.stampAttempt(ctx, sessionID, nowStr); writeErr != nil {
			return DebouncerCheck{}, writeErr
		}
		return DebouncerCheck{Allowed: false, LastFire: effectiveLast}, nil
	}

	// Backoff elapsed — allow.
	if writeErr := d.stampAttempt(ctx, sessionID, nowStr); writeErr != nil {
		return DebouncerCheck{}, writeErr
	}
	return DebouncerCheck{Allowed: true, LastFire: effectiveLast}, nil
}

// mostRecent returns the later of two debouncer timestamp strings.
// NULL or unparseable inputs are skipped. Returns (zeroTime, false)
// when both are absent.
func mostRecent(a, b sql.NullString) (time.Time, bool) {
	var ta, tb time.Time
	var haveA, haveB bool
	if a.Valid && a.String != "" {
		if parsed, err := parseDebouncerTimestamp(a.String); err == nil {
			ta, haveA = parsed, true
		}
	}
	if b.Valid && b.String != "" {
		if parsed, err := parseDebouncerTimestamp(b.String); err == nil {
			tb, haveB = parsed, true
		}
	}
	switch {
	case haveA && haveB:
		if ta.After(tb) {
			return ta, true
		}
		return tb, true
	case haveA:
		return ta, true
	case haveB:
		return tb, true
	default:
		return time.Time{}, false
	}
}

// RecordFire upserts the row to reflect that a review just fired
// successfully. Caller should invoke after the review pipeline
// returns a parsed result (regardless of whether the result is
// nothing_to_file — the fire itself counts as a fire for backoff
// purposes).
func (d *Debouncer) RecordFire(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("arcreview debouncer: session_id is empty")
	}
	now := d.clock().UTC()
	nowStr := formatDebouncerTimestamp(now)
	return d.pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO arc_review_debouncer (session_id, last_fire_at, last_fire_attempt_at, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(session_id) DO UPDATE SET
				last_fire_at = excluded.last_fire_at,
				last_fire_attempt_at = excluded.last_fire_attempt_at,
				updated_at = excluded.updated_at
		`, sessionID, nowStr, nowStr, nowStr)
		if err != nil {
			return fmt.Errorf("arcreview debouncer: upsert fire: %w", err)
		}
		return nil
	})
}

// stampAttempt records that the action was called for this session
// even when the call was suppressed by backoff. Two purposes: the
// existence of the row tells future calls that this session has been
// seen before (so first-fire telemetry is accurate), and the
// last_fire_attempt_at field surfaces suppression frequency.
func (d *Debouncer) stampAttempt(ctx context.Context, sessionID, nowStr string) error {
	return d.pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO arc_review_debouncer (session_id, last_fire_at, last_fire_attempt_at, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(session_id) DO UPDATE SET
				last_fire_attempt_at = excluded.last_fire_attempt_at,
				updated_at = excluded.updated_at
		`, sessionID, nowStr, nowStr, nowStr)
		if err != nil {
			return fmt.Errorf("arcreview debouncer: stamp attempt: %w", err)
		}
		return nil
	})
}

// debouncerTimestampLayout is RFC 3339 with millisecond precision and
// explicit UTC offset. Same shape the events table uses for
// envelope.ts so cross-table joins are simple string comparisons.
const debouncerTimestampLayout = "2006-01-02T15:04:05.000Z07:00"

func formatDebouncerTimestamp(t time.Time) string {
	return t.UTC().Format(debouncerTimestampLayout)
}

func parseDebouncerTimestamp(s string) (time.Time, error) {
	return time.Parse(debouncerTimestampLayout, s)
}
