package refresolve

import (
	"sync"
	"time"
)

// CachePolicy enumerates how long a resolver's output stays valid in
// the parse_context filter cache. Policies live alongside resolvers
// in the per-shape table (see PolicyForShape below). The agent never
// names a CachePolicy directly; the substrate picks based on the
// detected reference's shape.
//
// Design: docs/PARSE_CONTEXT.md §3.3 + §4.2.
type CachePolicy string

const (
	// PolicyIndefiniteWithinSession holds cached entries until the
	// session ends (or the server restarts). Right for resolvers
	// over data that doesn't mutate mid-conversation in normal use:
	// filesystem walks, schema lookups, vault content.
	PolicyIndefiniteWithinSession CachePolicy = "indefinite-within-session"
	// PolicyShortFiveTurns holds entries briefly; the conversational
	// proxy is ~5 turns / 5 minutes wall-clock. Used for slugs
	// (chain/task/bug) whose state mutates over the chain's
	// lifetime. Event-bus invalidation is a future refinement
	// (PARSE_CONTEXT §4.3); the TTL is the conservative fallback.
	PolicyShortFiveTurns CachePolicy = "short-5-turns"
	// PolicyNever skips the cache entirely. Used for friction_shape:
	// every observation is a candidate filing moment; caching would
	// drop signal.
	PolicyNever CachePolicy = "never"
	// PolicyReEvaluatePerCall doesn't cache; the underlying
	// resolver's work is cheap enough (shape-match re-evaluation)
	// that caching adds no benefit. Used for discipline_skill.
	PolicyReEvaluatePerCall CachePolicy = "re-evaluate-per-call"
	// PolicyShortThreeTurns is the advisory policy on low-confidence
	// kiwix fallback refs (chain parse-context-lean-orienting T8).
	// Shorter than PolicyShortFiveTurns because orientation pointers
	// are session-context-dependent — what kiwix returns for "configure
	// GoLand project structure" stays valid for the immediate clarifying
	// turn but quickly grows stale as the user reshapes their request.
	// Not wired into PolicyForShape: T8 refs are handler-built (not
	// dispatched), so the per-shape cache table doesn't touch them; the
	// constant is the agent-facing hint stamped onto ResolvedReference.
	PolicyShortThreeTurns CachePolicy = "short-3-turns"
)

// shortFiveTurnsTTL is the wall-clock proxy for the "5 turns"
// promise. Tunable if observed conversational pace diverges.
const shortFiveTurnsTTL = 5 * time.Minute

// PolicyForShape returns the cache policy for a given reference
// shape per the design's §3.3 matrix. New shapes default to
// PolicyIndefiniteWithinSession — adding a shape that mutates
// state should explicitly opt into PolicyShortFiveTurns here.
func PolicyForShape(shape ShapeCategory) CachePolicy {
	switch shape {
	case ShapeChainSlug, ShapeTaskSlug, ShapeBugSlug:
		return PolicyShortFiveTurns
	case ShapeFrictionShape:
		return PolicyNever
	default:
		// Filesystem shapes (path, skill_name, tool_name,
		// forge_schema), project catalog, library, domain term,
		// external_technical, and every parse_context-only shape
		// (skill_trigger, memory_entry, vault_candidate,
		// kiwix_bridge) fit the indefinite policy. discipline_skill
		// overrides below.
		if shape == ShapeDisciplineSkill {
			return PolicyReEvaluatePerCall
		}
		return PolicyIndefiniteWithinSession
	}
}

// cacheKey is the composite key the parse_context cache stores
// against. shape disambiguates the same token across resolvers
// (e.g. "rust" as a domain_term vs as a skill_trigger).
type cacheKey struct {
	sessionID string
	token     string
	shape     ShapeCategory
}

// cacheEntry is one stored resolver output plus the metadata the
// invalidation logic needs.
type cacheEntry struct {
	hitset   HitSet
	policy   CachePolicy
	cachedAt time.Time
}

// ParseContextCache is the per-process filter layer for
// parse_context. Constructed once at server startup and shared
// across all parse_context handler calls. Thread-safe — the
// substrate may dispatch concurrent parse_context calls when
// multiple agents share the daemon.
type ParseContextCache struct {
	mu    sync.RWMutex
	byKey map[cacheKey]cacheEntry
}

// NewParseContextCache returns an empty cache ready for use.
func NewParseContextCache() *ParseContextCache {
	return &ParseContextCache{byKey: make(map[cacheKey]cacheEntry)}
}

// Get returns the cached HitSet for (sessionID, token, shape) if
// one exists and the entry's policy still admits it. Returns
// (zero HitSet, "", false) on miss or stale entry. Stale entries
// stay in the map until next Put or explicit eviction; the
// freshness check is the load-bearing guard.
func (c *ParseContextCache) Get(sessionID, token string, shape ShapeCategory) (HitSet, CachePolicy, bool) {
	if c == nil || sessionID == "" {
		return HitSet{}, "", false
	}
	policy := PolicyForShape(shape)
	if policy == PolicyNever || policy == PolicyReEvaluatePerCall {
		// These shapes never read from cache by design.
		return HitSet{}, policy, false
	}
	c.mu.RLock()
	entry, ok := c.byKey[cacheKey{sessionID: sessionID, token: token, shape: shape}]
	c.mu.RUnlock()
	if !ok {
		return HitSet{}, policy, false
	}
	if !entry.fresh(time.Now()) {
		return HitSet{}, policy, false
	}
	return entry.hitset, entry.policy, true
}

// Put stores a fresh resolver output for (sessionID, token, shape).
// No-op when sessionID is empty (no session → no cache scope) or
// when the shape's policy is no-cache (PolicyNever /
// PolicyReEvaluatePerCall).
func (c *ParseContextCache) Put(sessionID, token string, shape ShapeCategory, hs HitSet) {
	if c == nil || sessionID == "" {
		return
	}
	policy := PolicyForShape(shape)
	if policy == PolicyNever || policy == PolicyReEvaluatePerCall {
		return
	}
	c.mu.Lock()
	c.byKey[cacheKey{sessionID: sessionID, token: token, shape: shape}] = cacheEntry{
		hitset:   hs,
		policy:   policy,
		cachedAt: time.Now(),
	}
	c.mu.Unlock()
}

// InvalidateSession removes every entry for a session. Called by
// the periodic sweeper or on Stop-hook signal; not yet wired in
// Phase 2 — sessions accumulate in memory until process restart,
// which on this daemon happens often enough that growth is
// bounded. A follow-up sweeper hook is tracked in
// docs/PARSE_CONTEXT.md §4.4.
func (c *ParseContextCache) InvalidateSession(sessionID string) {
	if c == nil || sessionID == "" {
		return
	}
	c.mu.Lock()
	for k := range c.byKey {
		if k.sessionID == sessionID {
			delete(c.byKey, k)
		}
	}
	c.mu.Unlock()
}

// InvalidateToken drops every cache entry whose token matches across
// all sessions and shapes. The event-bus observer in
// cmd/toolkit-server calls this when a state-changing event (TaskStarted,
// TaskCompleted, BugResolved, CommitLanded, etc.) lands so a
// subsequent parse_context call resolves the slug freshly instead of
// returning the pre-mutation candidate set.
//
// Cross-session sweep is deliberate: the same chain slug may be
// cached under multiple agent sessions, all of which became stale on
// the mutation. The token comparison is exact (no fuzzing) — the
// observer passes the EntitySlug verbatim from the emitted event.
//
// No-op when token is empty (avoids accidentally invalidating
// everything if an emit lands with a blank slug, which shouldn't
// happen but is cheaper to guard against than to debug).
func (c *ParseContextCache) InvalidateToken(token string) {
	if c == nil || token == "" {
		return
	}
	c.mu.Lock()
	for k := range c.byKey {
		if k.token == token {
			delete(c.byKey, k)
		}
	}
	c.mu.Unlock()
}

// fresh reports whether the entry still satisfies its policy at
// the supplied wall-clock instant. PolicyIndefiniteWithinSession
// entries never go stale (until session-wide invalidation). The
// short-5-turns TTL uses wall-clock as a proxy for the "5 turns"
// promise.
func (e cacheEntry) fresh(now time.Time) bool {
	switch e.policy {
	case PolicyIndefiniteWithinSession:
		return true
	case PolicyShortFiveTurns:
		return now.Sub(e.cachedAt) < shortFiveTurnsTTL
	default:
		// PolicyNever and PolicyReEvaluatePerCall entries shouldn't
		// be in the map; if they are, treat them as stale.
		return false
	}
}
