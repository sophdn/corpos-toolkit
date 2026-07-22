package refresolve_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/refresolve"
	"toolkit/internal/testutil"
)

// seedBugProjWS wraps [testutil.SeedBug] with the filedAt-time-based
// signature this test file uses. Wrapper survives because the
// time.Time→RFC3339 conversion is a meaningful typed-input value-add
// over the testutil string interface.
func seedBugProjWS(t *testing.T, pool *db.Pool, project, slug, title, severity, status string, filedAt time.Time) {
	t.Helper()
	ts := filedAt.Format(time.RFC3339)
	testutil.SeedBug(t, pool, project, slug, status, testutil.SeedBugOpts{
		Title:      title,
		Severity:   severity,
		ResolvedAt: ts, // ignored when status == "open" (NULL); used otherwise.
		FiledAt:    ts,
	})
}

// seedTaskProjActive wraps [testutil.SeedTask] with status='active'
// pinned and Position=1 default — the resolver-firing tests assert on
// the surfaced task set, not on ordering.
func seedTaskProjActive(t *testing.T, pool *db.Pool, chainID int64, slug, problem string) {
	t.Helper()
	testutil.SeedTask(t, pool, chainID, slug, "active", testutil.SeedTaskOpts{
		Position:         1,
		ProblemStatement: problem,
	})
}

// ResolveWorkState short-circuits on intents not in the firing set.
func TestResolveWorkState_NoFireOnExplainIntent(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedBugProjWS(t, pool, "mcp-servers", "b-explain-test", "x", "high", "open", time.Now())
	refs, tel, err := refresolve.ResolveWorkState(
		context.Background(), pool, "mcp-servers", refresolve.IntentExplain, nil, "s1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Errorf("expected 0 refs on explain intent; got %d", len(refs))
	}
	if tel.IntentShape != "" {
		t.Errorf("expected empty telemetry on no-fire; got IntentShape=%q", tel.IntentShape)
	}
}

// ResolveWorkState short-circuits gracefully on empty project (the
// no-project constraint from T6).
func TestResolveWorkState_NoFireOnEmptyProject(t *testing.T) {
	pool := testutil.NewTestDB(t)
	refs, tel, err := refresolve.ResolveWorkState(
		context.Background(), pool, "", refresolve.IntentVerify, nil, "s1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Errorf("expected 0 refs on empty project; got %d", len(refs))
	}
	if tel.IntentShape != "" {
		t.Errorf("expected empty telemetry; got IntentShape=%q", tel.IntentShape)
	}
}

// ResolveWorkState surfaces open bugs on verify intent, capped at 5.
// Cap tightened from 10 → 5 by bug 866 fix (envelope-budget retro);
// see go/internal/refresolve/work_state.go workStateMaxBugs.
func TestResolveWorkState_SurfacesOpenBugsCapped(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now()
	// 12 open bugs, mixed severity; cap is 5.
	for i := 0; i < 12; i++ {
		sev := "low"
		if i%3 == 0 {
			sev = "high"
		}
		seedBugProjWS(t, pool, "mcp-servers", fmt.Sprintf("b-cap-%d", i), fmt.Sprintf("title %d", i), sev, "open", now.Add(-time.Duration(i)*time.Hour))
	}
	refs, tel, err := refresolve.ResolveWorkState(
		context.Background(), pool, "mcp-servers", refresolve.IntentVerify, nil, "s1",
	)
	if err != nil {
		t.Fatal(err)
	}
	bugRefs := 0
	for _, r := range refs {
		if r.Shape == refresolve.ShapeBugSlug {
			bugRefs++
			if !strings.HasPrefix(r.PresentedAs, "[work-state surface]") {
				t.Errorf("ref %q missing [work-state surface] prefix: %s", r.Token, r.PresentedAs)
			}
		}
	}
	if bugRefs != 5 {
		t.Errorf("expected 5 bug refs (cap); got %d", bugRefs)
	}
	if tel.BugsCount != 5 {
		t.Errorf("telemetry BugsCount = %d, want 5", tel.BugsCount)
	}
}

// Open bugs ordered by severity desc then filed_at desc.
func TestResolveWorkState_BugOrderingBySeverityThenFiledAt(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now()
	// 3 bugs: low (newest), high (oldest), medium (middle).
	// Expected order: high, medium, low.
	seedBugProjWS(t, pool, "mcp-servers", "b-low-newest", "low new", "low", "open", now)
	seedBugProjWS(t, pool, "mcp-servers", "b-medium-mid", "medium mid", "medium", "open", now.Add(-2*time.Hour))
	seedBugProjWS(t, pool, "mcp-servers", "b-high-oldest", "high old", "high", "open", now.Add(-4*time.Hour))

	refs, _, err := refresolve.ResolveWorkState(
		context.Background(), pool, "mcp-servers", refresolve.IntentVerify, nil, "s1",
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"b-high-oldest", "b-medium-mid", "b-low-newest"}
	got := []string{}
	for _, r := range refs {
		if r.Shape == refresolve.ShapeBugSlug {
			got = append(got, r.Token)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("got %d bug refs; want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bug[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// Active tasks surfaced, capped at 3 (tightened from 5 by bug 866 fix).
func TestResolveWorkState_SurfacesActiveTasksCapped(t *testing.T) {
	pool := testutil.NewTestDB(t)
	chainID := seedChainProj(t, pool, "mcp-servers", "active-tasks-chain", "open")
	for i := 0; i < 7; i++ {
		seedTaskProjActive(t, pool, chainID, fmt.Sprintf("T-active-%d", i), fmt.Sprintf("problem %d", i))
	}
	refs, tel, err := refresolve.ResolveWorkState(
		context.Background(), pool, "mcp-servers", refresolve.IntentImplement, nil, "s1",
	)
	if err != nil {
		t.Fatal(err)
	}
	taskRefs := 0
	for _, r := range refs {
		if r.Shape == refresolve.ShapeTaskSlug {
			taskRefs++
		}
	}
	if taskRefs != 3 {
		t.Errorf("expected 3 task refs (cap); got %d", taskRefs)
	}
	if tel.TasksCount != 3 {
		t.Errorf("telemetry TasksCount = %d, want 3", tel.TasksCount)
	}
}

// Recent open chains surfaced, capped at 2 (tightened from 3 by bug
// 866 fix), within 7-day window.
func TestResolveWorkState_RecentChainsCappedAndWindowed(t *testing.T) {
	pool := testutil.NewTestDB(t)
	// 5 open chains; the resolver caps at 2. seedChainProj uses
	// datetime('now') so all are within the 7-day window.
	for i := 0; i < 5; i++ {
		seedChainProj(t, pool, "mcp-servers", fmt.Sprintf("recent-chain-%d", i), "open")
	}
	refs, tel, err := refresolve.ResolveWorkState(
		context.Background(), pool, "mcp-servers", refresolve.IntentStatus, nil, "s1",
	)
	if err != nil {
		t.Fatal(err)
	}
	chainRefs := 0
	for _, r := range refs {
		if r.Shape == refresolve.ShapeChainSlug {
			chainRefs++
		}
	}
	if chainRefs != 2 {
		t.Errorf("expected 2 chain refs (cap); got %d", chainRefs)
	}
	if tel.ChainsCount != 2 {
		t.Errorf("telemetry ChainsCount = %d, want 2", tel.ChainsCount)
	}
}

// Cache: second call within the TTL serves from cache (telemetry
// CacheHit=true).
func TestResolveWorkState_CacheServesSecondCall(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedBugProjWS(t, pool, "mcp-servers", "b-cache-test", "x", "high", "open", time.Now())
	cache := refresolve.NewWorkStateCache()
	_, tel1, err := refresolve.ResolveWorkState(
		context.Background(), pool, "mcp-servers", refresolve.IntentVerify, cache, "s-cache",
	)
	if err != nil {
		t.Fatal(err)
	}
	if tel1.CacheHit {
		t.Error("first call: CacheHit=true, want false")
	}
	_, tel2, err := refresolve.ResolveWorkState(
		context.Background(), pool, "mcp-servers", refresolve.IntentVerify, cache, "s-cache",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !tel2.CacheHit {
		t.Errorf("second call: CacheHit=false, want true (TTL: short-5-turns)")
	}
}

// End-to-end through HandleParseContext: verify-intent prompt
// surfaces work-state Candidates alongside any token-resolved refs.
func TestHandleParseContext_VerifyIntentSurfacesWorkState(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedBugProjWS(t, pool, "mcp-servers", "b-e2e-work", "title", "high", "open", time.Now())
	seedChainProj(t, pool, "mcp-servers", "e2e-recent-chain", "open")
	registry := refresolve.NewRegistry()
	deps := refresolve.HandlerDeps{
		Pool:             pool,
		Project:          "mcp-servers",
		Registry:         registry,
		Cache:            refresolve.NewParseContextCache(),
		DriftFireTracker: refresolve.NewDriftFireTracker(),
		WorkStateCache:   refresolve.NewWorkStateCache(),
	}
	ctx := events.WithMCPSessionID(context.Background(), "work-state-e2e")
	body, _ := json.Marshal(struct {
		MessageText string `json:"message_text"`
	}{MessageText: "please verify the new feature"})
	r, err := refresolve.HandleParseContext(ctx, deps, body)
	if err != nil {
		t.Fatal(err)
	}
	foundBug := false
	foundChain := false
	for _, ref := range r.References {
		switch {
		case ref.Token == "b-e2e-work" && ref.Shape == refresolve.ShapeBugSlug:
			foundBug = true
		case ref.Token == "e2e-recent-chain" && ref.Shape == refresolve.ShapeChainSlug:
			foundChain = true
		}
	}
	if !foundBug {
		t.Error("expected open bug b-e2e-work in envelope (work-state surfacing on verify intent)")
	}
	if !foundChain {
		t.Error("expected recent chain e2e-recent-chain in envelope (work-state surfacing)")
	}
}

// Docs intent does NOT surface work-state. Confirms the §13.3 firing
// gate by negation.
func TestHandleParseContext_ExplainIntentDoesNotSurfaceWorkState(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedBugProjWS(t, pool, "mcp-servers", "b-noexplain", "title", "high", "open", time.Now())
	registry := refresolve.NewRegistry()
	deps := refresolve.HandlerDeps{
		Pool:             pool,
		Project:          "mcp-servers",
		Registry:         registry,
		Cache:            refresolve.NewParseContextCache(),
		DriftFireTracker: refresolve.NewDriftFireTracker(),
		WorkStateCache:   refresolve.NewWorkStateCache(),
	}
	ctx := events.WithMCPSessionID(context.Background(), "work-state-explain")
	body, _ := json.Marshal(struct {
		MessageText string `json:"message_text"`
	}{MessageText: "explain how the fold hook works"})
	r, err := refresolve.HandleParseContext(ctx, deps, body)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range r.References {
		if strings.HasPrefix(ref.PresentedAs, "[work-state surface]") {
			t.Errorf("explain intent should not surface work-state; got %q", ref.PresentedAs)
		}
	}
}

// Cache invalidation: a BugReported event between two parse_context
// calls drops the WorkStateCache so the second call re-resolves
// (cache miss).
func TestWorkStateCache_InvalidatesOnBugEvent(t *testing.T) {
	cache := refresolve.NewWorkStateCache()
	cache.Put("s1", "mcp-servers", refresolve.IntentVerify, refresolve.WorkStateCacheTestEntry(1, 0, 0))
	if cache.Len() != 1 {
		t.Fatalf("setup: cache len=%d, want 1", cache.Len())
	}
	// Install the fold hook (will chain in front of any existing one)
	// and emit a synthetic BugReported event through the public
	// FoldHook API.
	prevHook := events.CurrentFoldHook()
	t.Cleanup(func() { events.SetFoldHook(prevHook) })
	refresolve.InstallCacheInvalidationFoldHook(nil, cache)
	hook := events.CurrentFoldHook()
	if err := hook(context.Background(), nil, events.RawEvent{
		Type:       "BugReported",
		EntityKind: "bug",
		EntitySlug: "any-bug",
	}); err != nil {
		t.Fatalf("hook returned error: %v", err)
	}
	if cache.Len() != 0 {
		t.Errorf("after bug event: cache len=%d, want 0 (InvalidateAll)", cache.Len())
	}
}
