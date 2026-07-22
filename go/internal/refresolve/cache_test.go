package refresolve_test

import (
	"context"
	"encoding/json"
	"testing"

	"toolkit/internal/events"
	"toolkit/internal/refresolve"
	"toolkit/internal/testutil"
)

// reference-resolution-migration T5 Phase 2: the filter cache holds
// per-session entries and re-uses them on subsequent parse_context
// calls within the same session. The second call to parse_context
// against the same session_id should report a cache hit on the
// previously-resolved reference, with from_cache=true on the
// returned ResolvedReference.
func TestParseContext_CacheHitOnSecondCall_SameSession(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedChainProj(t, pool, "mcp-servers", "cache-hit-chain", "open")
	registry := refresolve.NewRegistry()
	registry.Register(stubResolver{
		shape: refresolve.ShapeChainSlug,
		hit:   refresolve.HitSet{Candidates: []refresolve.Candidate{{ID: "cache-hit-chain", Title: "test", Score: 1.0, SourceRef: "chain:cache-hit-chain"}}},
	})
	cache := refresolve.NewParseContextCache()
	deps := refresolve.HandlerDeps{
		Pool:     pool,
		Project:  "mcp-servers",
		Registry: registry,
		Cache:    cache,
	}

	type params struct {
		MessageText string `json:"message_text"`
		SessionID   string `json:"session_id"`
	}
	first, _ := json.Marshal(params{MessageText: "cache-hit-chain status?", SessionID: "test-session-A"})
	second, _ := json.Marshal(params{MessageText: "and again cache-hit-chain", SessionID: "test-session-A"})

	r1, err := refresolve.HandleParseContext(context.Background(), deps, first)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if r1.CacheHits != 0 {
		t.Errorf("first call cache_hits = %d, want 0", r1.CacheHits)
	}
	if r1.CacheMisses == 0 {
		t.Errorf("first call cache_misses = %d, want >0", r1.CacheMisses)
	}

	r2, err := refresolve.HandleParseContext(context.Background(), deps, second)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if r2.CacheHits == 0 {
		t.Errorf("second call cache_hits = %d, want >0", r2.CacheHits)
	}
	foundCachedRef := false
	for _, ref := range r2.References {
		if ref.Token == "cache-hit-chain" && ref.FromCache {
			foundCachedRef = true
			if ref.CachePolicy != string(refresolve.PolicyShortFiveTurns) {
				t.Errorf("chain_slug ref cache_policy = %q, want %q",
					ref.CachePolicy, refresolve.PolicyShortFiveTurns)
			}
		}
	}
	if !foundCachedRef {
		t.Errorf("second call did not produce a from_cache ref: %+v", r2.References)
	}
}

// Cross-session isolation: cache entries scoped by session_id; a
// different session_id misses the cache even on the same token.
func TestParseContext_CacheIsolatesAcrossSessions(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedChainProj(t, pool, "mcp-servers", "iso-test-chain", "open")
	registry := refresolve.NewRegistry()
	registry.Register(stubResolver{
		shape: refresolve.ShapeChainSlug,
		hit:   refresolve.HitSet{Candidates: []refresolve.Candidate{{ID: "iso-test-chain", Title: "test", Score: 1.0, SourceRef: "chain:iso-test-chain"}}},
	})
	cache := refresolve.NewParseContextCache()
	deps := refresolve.HandlerDeps{
		Pool:     pool,
		Project:  "mcp-servers",
		Registry: registry,
		Cache:    cache,
	}

	type params struct {
		MessageText string `json:"message_text"`
		SessionID   string `json:"session_id"`
	}
	sessionA, _ := json.Marshal(params{MessageText: "iso-test-chain", SessionID: "session-A"})
	sessionB, _ := json.Marshal(params{MessageText: "iso-test-chain", SessionID: "session-B"})

	if _, err := refresolve.HandleParseContext(context.Background(), deps, sessionA); err != nil {
		t.Fatalf("session A: %v", err)
	}
	r2, err := refresolve.HandleParseContext(context.Background(), deps, sessionB)
	if err != nil {
		t.Fatalf("session B: %v", err)
	}
	if r2.CacheHits != 0 {
		t.Errorf("session B cache_hits = %d, want 0 (cache should be isolated)", r2.CacheHits)
	}
}

// cache_policy_override='fresh' bypasses the cache entirely.
func TestParseContext_CachePolicyOverrideFreshBypasses(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedChainProj(t, pool, "mcp-servers", "fresh-override-chain", "open")
	registry := refresolve.NewRegistry()
	registry.Register(stubResolver{
		shape: refresolve.ShapeChainSlug,
		hit:   refresolve.HitSet{Candidates: []refresolve.Candidate{{ID: "fresh-override-chain", Title: "test", Score: 1.0, SourceRef: "chain:fresh-override-chain"}}},
	})
	cache := refresolve.NewParseContextCache()
	deps := refresolve.HandlerDeps{
		Pool:     pool,
		Project:  "mcp-servers",
		Registry: registry,
		Cache:    cache,
	}

	type params struct {
		MessageText         string `json:"message_text"`
		SessionID           string `json:"session_id"`
		CachePolicyOverride string `json:"cache_policy_override"`
	}
	warm, _ := json.Marshal(params{MessageText: "fresh-override-chain", SessionID: "freshs"})
	if _, err := refresolve.HandleParseContext(context.Background(), deps, warm); err != nil {
		t.Fatalf("warm: %v", err)
	}
	bypass, _ := json.Marshal(params{MessageText: "fresh-override-chain", SessionID: "freshs", CachePolicyOverride: "fresh"})
	r, err := refresolve.HandleParseContext(context.Background(), deps, bypass)
	if err != nil {
		t.Fatalf("bypass: %v", err)
	}
	if r.CacheHits != 0 {
		t.Errorf("cache_policy_override=fresh should bypass: cache_hits = %d", r.CacheHits)
	}
}

// friction_shape is policy=never; no cache write on first call,
// no cache hit on second.
func TestParseContext_FrictionShapeIsPolicyNever(t *testing.T) {
	// Cache policy itself is tested directly — the resolver isn't
	// registered in this test (the friction detector emits the shape
	// without a registry hit; resolution path returns no_hit but
	// the policy decision happens at cache lookup/put which doesn't
	// require a resolver).
	cache := refresolve.NewParseContextCache()
	cache.Put("s1", "paper-cut", refresolve.ShapeFrictionShape, refresolve.HitSet{})
	if _, _, ok := cache.Get("s1", "paper-cut", refresolve.ShapeFrictionShape); ok {
		t.Errorf("friction_shape should never cache: Get returned hit")
	}
}

// Chain parse-context-lean-orienting T1: production-shape repro.
// Agent callers don't pass session_id explicitly; the handler must
// derive session identity from events.MCPSessionIDFromContext(ctx) so
// the cache key matches across calls within one MCP session. The
// pre-fix path fell through to obs.SpanFromContext(ctx).TraceID,
// which the dispatcher mints fresh on every call — same MCP session,
// different TraceID, cache always missed (cache_hits=0 in the T13
// report card observation that motivated this task).
//
// This test pins the fix: HandleParseContext receives a ctx carrying a
// stable MCP session id (no params session_id), calls twice, and the
// second envelope must report cache_hits > 0.
func TestParseContext_CacheHitOnSecondCall_FromMCPSessionContext(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedChainProj(t, pool, "mcp-servers", "mcp-session-chain", "open")
	registry := refresolve.NewRegistry()
	registry.Register(stubResolver{
		shape: refresolve.ShapeChainSlug,
		hit:   refresolve.HitSet{Candidates: []refresolve.Candidate{{ID: "mcp-session-chain", Title: "test", Score: 1.0, SourceRef: "chain:mcp-session-chain"}}},
	})
	cache := refresolve.NewParseContextCache()
	deps := refresolve.HandlerDeps{
		Pool:     pool,
		Project:  "mcp-servers",
		Registry: registry,
		Cache:    cache,
	}

	// Stable MCP-session id on ctx — what the toolkit-server's
	// stampMCPSessionID wrapper attaches in production. No session_id
	// param on the body; the handler must read MCP session id from ctx.
	ctx := events.WithMCPSessionID(context.Background(), "stdio-0xfeedface")

	type params struct {
		MessageText string `json:"message_text"`
	}
	first, _ := json.Marshal(params{MessageText: "mcp-session-chain status?"})
	second, _ := json.Marshal(params{MessageText: "and again mcp-session-chain"})

	r1, err := refresolve.HandleParseContext(ctx, deps, first)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if r1.CacheHits != 0 {
		t.Errorf("first call cache_hits = %d, want 0", r1.CacheHits)
	}
	if r1.CacheMisses == 0 {
		t.Errorf("first call cache_misses = %d, want >0", r1.CacheMisses)
	}

	r2, err := refresolve.HandleParseContext(ctx, deps, second)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if r2.CacheHits == 0 {
		t.Errorf("second call cache_hits = %d, want >0 (MCP-session id should have keyed the cache)", r2.CacheHits)
	}
	if r2.CacheMisses > 0 {
		t.Errorf("second call cache_misses = %d, want 0 for re-resolved tokens", r2.CacheMisses)
	}
	foundCachedRef := false
	for _, ref := range r2.References {
		if ref.Token == "mcp-session-chain" && ref.FromCache {
			foundCachedRef = true
		}
	}
	if !foundCachedRef {
		t.Errorf("second call did not produce a from_cache ref: %+v", r2.References)
	}
}

// Different MCP-session ids isolate the cache: same token, different
// stable session id → second call misses.
func TestParseContext_CacheIsolatesAcrossMCPSessions(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedChainProj(t, pool, "mcp-servers", "iso-mcp-chain", "open")
	registry := refresolve.NewRegistry()
	registry.Register(stubResolver{
		shape: refresolve.ShapeChainSlug,
		hit:   refresolve.HitSet{Candidates: []refresolve.Candidate{{ID: "iso-mcp-chain", Title: "test", Score: 1.0, SourceRef: "chain:iso-mcp-chain"}}},
	})
	cache := refresolve.NewParseContextCache()
	deps := refresolve.HandlerDeps{
		Pool:     pool,
		Project:  "mcp-servers",
		Registry: registry,
		Cache:    cache,
	}

	ctxA := events.WithMCPSessionID(context.Background(), "stdio-A")
	ctxB := events.WithMCPSessionID(context.Background(), "stdio-B")
	body, _ := json.Marshal(struct {
		MessageText string `json:"message_text"`
	}{MessageText: "iso-mcp-chain"})

	if _, err := refresolve.HandleParseContext(ctxA, deps, body); err != nil {
		t.Fatalf("session A: %v", err)
	}
	r, err := refresolve.HandleParseContext(ctxB, deps, body)
	if err != nil {
		t.Fatalf("session B: %v", err)
	}
	if r.CacheHits != 0 {
		t.Errorf("session B cache_hits = %d, want 0 (different MCP session ids should isolate)", r.CacheHits)
	}
}

// Direct-cache-API: InvalidateToken drops every entry whose token
// matches across all sessions.
func TestParseContextCache_InvalidateTokenSweepsAcrossSessions(t *testing.T) {
	cache := refresolve.NewParseContextCache()
	hs := refresolve.HitSet{Candidates: []refresolve.Candidate{{ID: "x"}}}
	cache.Put("s1", "shared-chain", refresolve.ShapeChainSlug, hs)
	cache.Put("s2", "shared-chain", refresolve.ShapeChainSlug, hs)
	cache.Put("s1", "other-chain", refresolve.ShapeChainSlug, hs)

	cache.InvalidateToken("shared-chain")

	if _, _, ok := cache.Get("s1", "shared-chain", refresolve.ShapeChainSlug); ok {
		t.Error("session s1 entry for shared-chain should be invalidated")
	}
	if _, _, ok := cache.Get("s2", "shared-chain", refresolve.ShapeChainSlug); ok {
		t.Error("session s2 entry for shared-chain should be invalidated (cross-session sweep)")
	}
	if _, _, ok := cache.Get("s1", "other-chain", refresolve.ShapeChainSlug); !ok {
		t.Error("unrelated token other-chain should survive InvalidateToken")
	}
}

// End-to-end invalidation through HandleParseContext: cache warms on
// first call; a direct InvalidateToken (simulating a TaskStarted event
// that the fold-hook invalidator translates to InvalidateToken) drops
// the entry; second call re-resolves freshly. The "TaskStarted event
// invalidates affected entries" acceptance criterion in T1 — covered
// at the cache layer because the fold-hook → InvalidateToken seam is
// trivial pass-through (cache_invalidate.go invalidateFromEvent).
func TestParseContext_TaskStartedBetweenCallsClearsCache(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedChainProj(t, pool, "mcp-servers", "invalidation-chain", "open")
	registry := refresolve.NewRegistry()
	registry.Register(stubResolver{
		shape: refresolve.ShapeChainSlug,
		hit:   refresolve.HitSet{Candidates: []refresolve.Candidate{{ID: "invalidation-chain", Title: "test", Score: 1.0, SourceRef: "chain:invalidation-chain"}}},
	})
	cache := refresolve.NewParseContextCache()
	deps := refresolve.HandlerDeps{
		Pool:     pool,
		Project:  "mcp-servers",
		Registry: registry,
		Cache:    cache,
	}
	ctx := events.WithMCPSessionID(context.Background(), "stdio-invalidation")
	body, _ := json.Marshal(struct {
		MessageText string `json:"message_text"`
	}{MessageText: "invalidation-chain"})

	// Warm.
	r1, err := refresolve.HandleParseContext(ctx, deps, body)
	if err != nil || r1.CacheHits != 0 || r1.CacheMisses == 0 {
		t.Fatalf("warm: err=%v hits=%d misses=%d", err, r1.CacheHits, r1.CacheMisses)
	}
	// Confirm warming actually populated the cache (sanity).
	r2, err := refresolve.HandleParseContext(ctx, deps, body)
	if err != nil || r2.CacheHits == 0 {
		t.Fatalf("post-warm sanity: err=%v hits=%d", err, r2.CacheHits)
	}
	// Simulate the fold-hook invalidator firing on a TaskStarted-type
	// event whose chain affiliation matches the cached slug. The
	// production fold hook calls cache.InvalidateToken(slug); this
	// test exercises the same observable contract directly.
	cache.InvalidateToken("invalidation-chain")

	r3, err := refresolve.HandleParseContext(ctx, deps, body)
	if err != nil {
		t.Fatalf("post-invalidate: %v", err)
	}
	if r3.CacheHits != 0 {
		t.Errorf("post-invalidate cache_hits = %d, want 0 (entry should have been swept)", r3.CacheHits)
	}
	if r3.CacheMisses == 0 {
		t.Errorf("post-invalidate cache_misses = %d, want >0 (re-resolution should fire)", r3.CacheMisses)
	}
}

// Direct-cache-API: policies match the design doc §3.3 table.
func TestPolicyForShape_MatchesDesignTable(t *testing.T) {
	cases := []struct {
		shape  refresolve.ShapeCategory
		policy refresolve.CachePolicy
	}{
		{refresolve.ShapeChainSlug, refresolve.PolicyShortFiveTurns},
		{refresolve.ShapeTaskSlug, refresolve.PolicyShortFiveTurns},
		{refresolve.ShapeBugSlug, refresolve.PolicyShortFiveTurns},
		{refresolve.ShapePath, refresolve.PolicyIndefiniteWithinSession},
		{refresolve.ShapeSkillName, refresolve.PolicyIndefiniteWithinSession},
		{refresolve.ShapeDomainTerm, refresolve.PolicyIndefiniteWithinSession},
		{refresolve.ShapeFrictionShape, refresolve.PolicyNever},
		{refresolve.ShapeSkillTrigger, refresolve.PolicyIndefiniteWithinSession},
		{refresolve.ShapeMemoryEntry, refresolve.PolicyIndefiniteWithinSession},
		{refresolve.ShapeVaultCandidate, refresolve.PolicyIndefiniteWithinSession},
		{refresolve.ShapeKiwixBridge, refresolve.PolicyIndefiniteWithinSession},
		{refresolve.ShapeDisciplineSkill, refresolve.PolicyReEvaluatePerCall},
	}
	for _, tc := range cases {
		if got := refresolve.PolicyForShape(tc.shape); got != tc.policy {
			t.Errorf("PolicyForShape(%q) = %q, want %q", tc.shape, got, tc.policy)
		}
	}
}
