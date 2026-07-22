package arcreview

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"toolkit/internal/db"
)

// DefaultArcHashCacheTTLSeconds is the look-back window for the per-arc
// content-hash dedupe. 600 seconds (10 minutes) per design candidate
// (1) of bug `arc-close-filing-review-fires-multiply-on-overlapping-arc-within-single-session`
// (1482): wide enough to catch the few-minute-spaced cross-trigger
// fires the bug observed (commit_landed → task_completed cascade), tight
// enough that genuinely distinct arcs hours apart get their own review.
const DefaultArcHashCacheTTLSeconds = 600

// arcHashFromSnapshot returns the stable content fingerprint of the
// snapshot. SHA-256 over a "ROLE: content\n" concatenation matches the
// renderSnapshot shape so the same conversation produces the same hash
// regardless of which trigger surfaced first. Truncation flag is NOT
// included — a fire on a truncated arc and a follow-up fire on the
// same arc with the same truncated tail should collide.
func arcHashFromSnapshot(snap Snapshot) string {
	var b strings.Builder
	for _, m := range snap.Messages {
		b.WriteString(strings.ToUpper(m.Role))
		b.WriteString(": ")
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// ArcHashCacheEntry is the row the dedupe layer reads on hit. Carries
// the prior fire's event_id so the dedupe surface can name the canonical
// decision when it short-circuits.
type ArcHashCacheEntry struct {
	PriorEventID string
	FiredAt      time.Time
}

// ArcHashCache wraps the arc_review_hash_cache SQLite table (migration
// 053). Read returns a hit when a row exists for (session_id, arc_hash)
// AND fired_at is inside the TTL window. Record upserts a fresh row.
type ArcHashCache struct {
	pool       *db.Pool
	ttlSeconds int
	clock      debouncerClock
}

// NewArcHashCache constructs an ArcHashCache backed by the given pool
// with the default TTL window.
func NewArcHashCache(pool *db.Pool) *ArcHashCache {
	return &ArcHashCache{
		pool:       pool,
		ttlSeconds: DefaultArcHashCacheTTLSeconds,
		clock:      time.Now,
	}
}

// WithTTLSeconds returns a cache with the given TTL window. Zero or
// negative falls back to DefaultArcHashCacheTTLSeconds.
func (c *ArcHashCache) WithTTLSeconds(s int) *ArcHashCache {
	if s <= 0 {
		s = DefaultArcHashCacheTTLSeconds
	}
	cp := *c
	cp.ttlSeconds = s
	return &cp
}

// WithClock returns a cache driven by the given clock. Test-only.
func (c *ArcHashCache) WithClock(clk debouncerClock) *ArcHashCache {
	cp := *c
	cp.clock = clk
	return &cp
}

// Lookup returns (entry, true) when a row exists for the (session_id,
// arc_hash) key AND fired_at is within the TTL window of "now". Returns
// (zero, false) on miss or expired hit. Errors propagate; SQL ErrNoRows
// resolves to a miss.
func (c *ArcHashCache) Lookup(ctx context.Context, sessionID, arcHash string) (ArcHashCacheEntry, bool, error) {
	if sessionID == "" || arcHash == "" {
		return ArcHashCacheEntry{}, false, nil
	}
	var priorEventID, firedAtStr string
	err := c.pool.DB().QueryRowContext(ctx, `
		SELECT prior_event_id, fired_at
		FROM arc_review_hash_cache
		WHERE session_id = ? AND arc_hash = ?
	`, sessionID, arcHash).Scan(&priorEventID, &firedAtStr)
	if errors.Is(err, sql.ErrNoRows) {
		return ArcHashCacheEntry{}, false, nil
	}
	if err != nil {
		return ArcHashCacheEntry{}, false, fmt.Errorf("arc hash cache: read: %w", err)
	}
	firedAt, parseErr := parseDebouncerTimestamp(firedAtStr)
	if parseErr != nil {
		// Stale-shape row — treat as miss; the next Record will overwrite.
		return ArcHashCacheEntry{}, false, nil
	}
	now := c.clock().UTC()
	if now.Sub(firedAt) > time.Duration(c.ttlSeconds)*time.Second {
		return ArcHashCacheEntry{}, false, nil
	}
	return ArcHashCacheEntry{PriorEventID: priorEventID, FiredAt: firedAt}, true, nil
}

// Record upserts a row stamping the just-fired ArcCloseFilingReviewed
// event_id under the (session_id, arc_hash) key. Subsequent Lookups
// within the TTL window hit this row.
func (c *ArcHashCache) Record(ctx context.Context, sessionID, arcHash, priorEventID string) error {
	if sessionID == "" || arcHash == "" || priorEventID == "" {
		return nil
	}
	now := c.clock().UTC()
	nowStr := formatDebouncerTimestamp(now)
	return c.pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO arc_review_hash_cache (session_id, arc_hash, prior_event_id, fired_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(session_id, arc_hash) DO UPDATE SET
				prior_event_id = excluded.prior_event_id,
				fired_at = excluded.fired_at,
				updated_at = excluded.updated_at
		`, sessionID, arcHash, priorEventID, nowStr, nowStr)
		if err != nil {
			return fmt.Errorf("arc hash cache: upsert: %w", err)
		}
		return nil
	})
}
