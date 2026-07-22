package construct

import (
	"sync"
	"time"
)

// Burst-nudge tuning. A session that mints forgeBurstThreshold rows via
// separate forge calls within forgeBurstWindow is in a "sequential forge"
// burst the work.batch primitive could have collapsed to one round-trip.
const (
	forgeBurstThreshold = 3
	forgeBurstWindow    = 60 * time.Second
	// forgeBurstSweepAt bounds the per-session map: once it exceeds this
	// many sessions, Record sweeps sessions whose timestamps are all stale.
	// forge is a low-frequency mutating op, so the sweep is rare and cheap.
	forgeBurstSweepAt = 256
)

// ForgeBatchNudge is the soft hint surfaced on a forge-create response once
// the calling session crosses the burst threshold. Exported so tests (and a
// future doc-string check) can assert against the exact text.
const ForgeBatchNudge = "You've created several rows via separate forge calls this session — work.batch carries N forge ops in one round-trip (forge is allowlisted). Consider collapsing the remaining creates into a single work.batch."

// ForgeBurstTracker counts standalone forge-create calls per MCP session
// within a sliding window so HandleForge can surface a one-time-per-burst
// "consider work.batch" hint (bug 887, a recurrence of 868: agents mint
// N rows via sequential forge calls instead of one batch).
//
// Only the standalone HandleForge path records here; forges inside a
// work.batch run through HandleForgeInTx, which never touches the tracker —
// so the agent that is ALREADY batching is never nudged.
//
// Nil-safe: a nil *ForgeBurstTracker records nothing and never hints, so
// wiring is optional (degraded boot / tests that don't care pass nil). The
// signal is reactive by nature — each forge call is an independent MCP
// round-trip, so the tracker observes the burst as it accumulates and nudges
// on the call that crosses the threshold; that still lands mid-burst (an
// agent forging 5 rows sees the hint on the 3rd response and can batch the
// rest). Mirrors the per-session DriftFireTracker pattern in refresolve.
type ForgeBurstTracker struct {
	mu        sync.Mutex
	window    time.Duration
	threshold int
	recent    map[string][]time.Time
	now       func() time.Time // injectable clock for tests
}

// NewForgeBurstTracker returns a tracker with the production window +
// threshold and a real clock.
func NewForgeBurstTracker() *ForgeBurstTracker {
	return &ForgeBurstTracker{
		window:    forgeBurstWindow,
		threshold: forgeBurstThreshold,
		recent:    make(map[string][]time.Time),
		now:       time.Now,
	}
}

// Record registers one standalone forge-create for sessionID at the current
// time and reports whether the session has reached the burst threshold
// within the window. An empty sessionID is ignored (returns false): without
// a stable session key the tracker can't attribute calls to a burst, and the
// safe degradation is "no hint".
func (t *ForgeBurstTracker) Record(sessionID string) bool {
	if t == nil || sessionID == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	cutoff := now.Add(-t.window)

	kept := make([]time.Time, 0, len(t.recent[sessionID])+1)
	for _, ts := range t.recent[sessionID] {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	kept = append(kept, now)
	t.recent[sessionID] = kept

	if len(t.recent) > forgeBurstSweepAt {
		t.sweepStaleLocked(cutoff)
	}
	return len(kept) >= t.threshold
}

// sweepStaleLocked drops sessions whose every recorded timestamp predates
// cutoff. Caller holds t.mu. Keeps the map from growing without bound across
// a long-lived daemon's many sessions.
func (t *ForgeBurstTracker) sweepStaleLocked(cutoff time.Time) {
	for sid, stamps := range t.recent {
		stale := true
		for _, ts := range stamps {
			if ts.After(cutoff) {
				stale = false
				break
			}
		}
		if stale {
			delete(t.recent, sid)
		}
	}
}
