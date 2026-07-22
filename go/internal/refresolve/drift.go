package refresolve

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"toolkit/internal/events"
	"toolkit/internal/obs"
	"toolkit/internal/stdiodrift"
)

// DriftFireTracker is per-process per-session state for the
// stdio-drift surface in parse_context. Two facts per session:
//
//   - whether the bootstrap (first-call) fire already happened — the
//     acceptance criteria's path (a) fires once regardless of intent.
//   - how many times drift has surfaced this session — after the
//     threshold, suppression kicks in to avoid spamming the agent.
//
// Thread-safe: the same MCP session may issue parse_context calls
// concurrently across spans (rare but possible) and the in-process
// daemon hosts multiple sessions.
//
// Chain parse-context-lean-orienting T9.
type DriftFireTracker struct {
	mu             sync.Mutex
	bySessionState map[string]*driftSessionState
	suppressionCap int
}

type driftSessionState struct {
	BootstrapFired bool
	FireCount      int
	LastFireAt     time.Time
}

// NewDriftFireTracker constructs a tracker with the default
// suppression cap (3 fires per session before the surface goes quiet
// until the next drift-state change, per the T9 constraint).
func NewDriftFireTracker() *DriftFireTracker {
	return &DriftFireTracker{
		bySessionState: make(map[string]*driftSessionState),
		suppressionCap: 3,
	}
}

// shouldSurface reports the per-call decision: given the current
// drift snapshot, the directive intent, and whether this is the
// first parse_context call in the session, should the substrate
// append a drift Candidate to the envelope?
//
// Returns (surface, bootstrap, suppressed). bootstrap names whether
// the decision came via the first-call path (path a in T9's
// acceptance criteria) vs the intent-conditional path (path b).
// suppressed is true when drift_detected AND the would-fire path
// matched AND the session counter exceeded suppressionCap.
//
// directiveIntent is the IntentShape T5 will plug in (chain
// parse-context-lean-orienting). Today we accept the value as an
// opaque string; T5's `intent.shape` populates it. Empty / "none"
// means "no directive intent detected" — only the bootstrap path can
// fire.
func (t *DriftFireTracker) shouldSurface(sessionID string, drift stdiodrift.State, directiveIntent string) (surface, bootstrap, suppressed bool) {
	if t == nil || sessionID == "" || !drift.DriftDetected {
		return false, false, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.bySessionState[sessionID]
	if !ok {
		st = &driftSessionState{}
		t.bySessionState[sessionID] = st
	}
	// Bootstrap path: the first parse_context call in this session
	// fires regardless of intent. Acceptance criteria (a).
	bootstrap = !st.BootstrapFired
	// Intent-conditional path: T5's directive intent gates this. T5
	// hasn't shipped yet; for now we only honor the explicit string
	// shapes the T4 design pinned. The function stays correct once T5
	// populates the value.
	intentMatch := isDriftSurfacingIntent(directiveIntent)

	if !bootstrap && !intentMatch {
		// No surfacing path applies; we don't tick the counter.
		return false, false, false
	}

	// Suppression: after the threshold the surface goes quiet UNTIL
	// the drift state changes. Pinned to the count here; the
	// state-change reset belongs in a follow-on (T9 constraint notes
	// "until the next drift-state change" but the drift-state hashing
	// needed to detect that lives in T10 work-state-surfacing).
	if st.FireCount >= t.suppressionCap {
		return false, bootstrap, true
	}

	st.FireCount++
	st.LastFireAt = time.Now().UTC()
	if bootstrap {
		st.BootstrapFired = true
	}
	return true, bootstrap, false
}

// ResetSession drops a session's drift-fire state. Used by tests; in
// production the per-process state lives the daemon's lifetime
// (which restarts often enough — see CLAUDE.md §Post-commit advisor —
// that natural turnover is sufficient).
func (t *DriftFireTracker) ResetSession(sessionID string) {
	if t == nil || sessionID == "" {
		return
	}
	t.mu.Lock()
	delete(t.bySessionState, sessionID)
	t.mu.Unlock()
}

// isDriftSurfacingIntent is the closed predicate the design's §13.3
// matrix names: intent ∈ {verify, fix, implement, audit} fires the
// stdio drift surface (path b). All other shapes (and "none") fall
// through to the bootstrap-only branch.
//
// PLACEHOLDER until T5 (T5-directive-intent-ship) lands. T5's
// implementation must thread the detected intent.shape into the
// handler so this gate fires under live load.
func isDriftSurfacingIntent(shape string) bool {
	switch shape {
	case "verify", "fix", "implement", "audit":
		return true
	default:
		return false
	}
}

// driftCandidate composes the discipline_skill-shaped ResolvedReference
// the agent reads. Token is fixed ("stdio-drift") because there's
// only one such surface per envelope; presented_as carries the
// actionable one-liner.
func driftCandidate(state stdiodrift.State) ResolvedReference {
	head := state.HeadSHA
	if head == "" {
		head = "(unknown HEAD)"
	}
	stdio := ""
	kinds := map[stdiodrift.DriftKind]bool{}
	for _, p := range state.StdioProcesses {
		if p.DriftKind == stdiodrift.DriftKindNone {
			continue
		}
		if p.ReportedGitSHA != "" {
			stdio = p.ReportedGitSHA
		}
		kinds[p.DriftKind] = true
	}
	if stdio == "" {
		stdio = "(running binary's gitSHA unknown; fd_deleted is the active signal)"
	}
	kindStr := "stdio_fd_pinned"
	if kinds[stdiodrift.DriftKindCompileTimeStale] && !kinds[stdiodrift.DriftKindStdioFDPinned] {
		kindStr = "compile_time_stale"
	}
	if kinds[stdiodrift.DriftKindBoth] || (kinds[stdiodrift.DriftKindStdioFDPinned] && kinds[stdiodrift.DriftKindCompileTimeStale]) {
		kindStr = "both"
	}
	presented := fmt.Sprintf(
		"Stdio MCP on %s, HEAD at %s (drift_kind=%s); run /mcp reconnect to pick up the new binary.",
		stdio, head, kindStr,
	)
	return ResolvedReference{
		Token:             "stdio-drift",
		Shape:             ShapeDisciplineSkill,
		ConfidenceTier:    TierSingleExact,
		PresentedAs:       presented,
		RecommendedAction: PresentUseDirectly,
		CachePolicy:       string(PolicyReEvaluatePerCall),
		TopCandidates: []Candidate{{
			ID:         "stdio-drift",
			Title:      "stdio MCP binary drift",
			Score:      1.0,
			SourceRef:  "discipline:stdio-drift",
			DebugNotes: fmt.Sprintf("kind=%s head=%s stdio=%s", kindStr, head, stdio),
		}},
	}
}

// snapshotDrift wraps stdiodrift.Snapshot with the handler's typical
// inputs. Returns a zero State on snapshot error — drift surfacing is
// best-effort and must not fail the surrounding parse_context call.
func snapshotDrift(ctx context.Context, deps HandlerDeps) stdiodrift.State {
	state, err := stdiodrift.Snapshot(ctx, stdiodrift.SnapshotInputs{
		RepoRoot:           deps.RepoRoot,
		OnDiskGitSHA:       deps.GitSHA,
		MarkerPathOverride: deps.DriftMarkerPathOverride,
		ProcRootOverride:   deps.DriftProcRootOverride,
	})
	if err != nil {
		return stdiodrift.State{}
	}
	return state
}

// emitIntentResolvedEvent fires a ParseContextIntentResolved event
// per parse_context call (chain parse-context-lean-orienting T5).
// T10's measurement consumes the event for fire-rate /
// detection-path-mix / latency dashboards. Same async-emit pattern
// as emitDriftEvent: goroutined with its own write tx so the
// request goroutine doesn't pay the tx cost.
//
// MessageHash is a 16-char SHA-256 prefix so dedup analytics don't
// retain the raw user prompt (privacy: the live envelope already
// carries the text; the events ledger doesn't need it).
func emitIntentResolvedEvent(ctx context.Context, deps HandlerDeps, sessionID, message string, intent IntentResult, latencyMs int) {
	if deps.Pool == nil || sessionID == "" {
		return
	}
	hash := messageHash16(message)
	payload := events.ParseContextIntentResolvedPayload{
		IntentShape:    string(intent.Shape),
		DetectionPath:  intent.DetectedVia,
		LatencyMs:      latencyMs,
		FallbackToNone: intent.Shape == IntentNone,
		MessageHash:    hash,
	}
	parentSpan := obs.SpanFromContext(ctx)
	asyncCtx := context.Background()
	if parentSpan != nil {
		asyncCtx = obs.WithSpan(asyncCtx, parentSpan)
	}
	go func() {
		txErr := deps.Pool.WithWrite(asyncCtx, func(tx *sql.Tx) error {
			_, emitErr := events.Emit(asyncCtx, tx, events.EmitArgs{
				Entity:  events.NewCrossCuttingEntityRef("parse_context_session", sessionID),
				Payload: payload,
			})
			return emitErr
		})
		if txErr != nil {
			obs.L().Warn("refresolve: intent resolved event emit failed",
				slog.String("err", txErr.Error()))
		}
	}()
}

// messageHash16 returns the first 16 hex chars of SHA-256(message).
// Used as the stable dedup handle on ParseContextIntentResolved
// events. Empty input maps to a deterministic constant.
func messageHash16(message string) string {
	sum := sha256.Sum256([]byte(message))
	return hex.EncodeToString(sum[:8])
}

// emitDriftEvent fires a ParseContextStdioDriftSurfaced event for
// T10's drift-rate measurement. Fires regardless of whether a
// Candidate surfaced — the drift_kind=none branch contributes the
// fire-rate denominator. Runs in a goroutine with its own write tx
// so the surrounding parse_context call doesn't pay the emit's
// tx-acquisition cost (T9 constraint: "Event emission via the async
// eventbus pathway").
//
// No-op when the pool is absent (test driver, smoke boot) or the
// session id is empty (no session scope to attribute the event to).
func emitDriftEvent(ctx context.Context, deps HandlerDeps, sessionID string, state stdiodrift.State, bootstrap, suppressed bool) {
	if deps.Pool == nil || sessionID == "" {
		return
	}
	kind := stdiodrift.DriftKindNone
	stdio := ""
	for _, p := range state.StdioProcesses {
		if p.DriftKind != stdiodrift.DriftKindNone {
			kind = p.DriftKind
			if p.ReportedGitSHA != "" {
				stdio = p.ReportedGitSHA
			}
		}
	}
	payload := events.ParseContextStdioDriftSurfacedPayload{
		HeadSHA:                state.HeadSHA,
		StdioSHA:               stdio,
		DriftKind:              string(kind),
		BootstrapPath:          bootstrap,
		SuppressedByRecentFire: suppressed,
	}
	// Detach the request ctx so the goroutine survives the response
	// being written; obs.WithSpan keeps the span lineage for tracing.
	parentSpan := obs.SpanFromContext(ctx)
	asyncCtx := context.Background()
	if parentSpan != nil {
		asyncCtx = obs.WithSpan(asyncCtx, parentSpan)
	}
	go func() {
		txErr := deps.Pool.WithWrite(asyncCtx, func(tx *sql.Tx) error {
			_, emitErr := events.Emit(asyncCtx, tx, events.EmitArgs{
				Entity:  events.NewCrossCuttingEntityRef("parse_context_session", sessionID),
				Payload: payload,
			})
			return emitErr
		})
		if txErr != nil {
			obs.L().Warn("refresolve: stdio drift event emit failed",
				slog.String("err", txErr.Error()))
		}
	}()
}
