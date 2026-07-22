package refresolve_test

// Surface wiring for the execute intent (chain parse-context-directive-
// intent-extension T4; docs/PARSE_CONTEXT.md §14.4): execute fires
// work-state, surfaces scratchpad-discipline, and dedups a named chain
// that slug detection already surfaced so it isn't double-listed.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"toolkit/internal/events"
	"toolkit/internal/refresolve"
	"toolkit/internal/testutil"
)

// Execute is in the work-state firing set: a no-slug execute prompt
// ("clear the backlog") surfaces the open-bug work-state — the open bugs
// ARE the work.
func TestResolveWorkState_FiresOnExecuteIntent(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedBugProjWS(t, pool, "mcp-servers", "b-exec-1", "an open bug", "high", "open", time.Now())
	seedBugProjWS(t, pool, "mcp-servers", "b-exec-2", "another open bug", "medium", "open", time.Now())
	refs, tel, err := refresolve.ResolveWorkState(
		context.Background(), pool, "mcp-servers", refresolve.IntentExecute, nil, "s1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if tel.IntentShape != "execute" {
		t.Errorf("expected telemetry IntentShape=execute (fired); got %q", tel.IntentShape)
	}
	bugRefs := 0
	for _, r := range refs {
		if r.Shape == refresolve.ShapeBugSlug {
			bugRefs++
		}
	}
	if bugRefs != 2 {
		t.Errorf("expected 2 open-bug work-state refs on execute intent; got %d", bugRefs)
	}
}

// Execute surfaces scratchpad-discipline (single entry) — the chain-
// execution reflex. NOT coding-philosophy / lang-conventions.
func TestResolveIntentDisciplines_ExecuteSurfacesScratchpad(t *testing.T) {
	manifest := buildTestManifest()
	refs, tel := refresolve.ResolveIntentDisciplines(
		context.Background(), manifest, refresolve.IntentExecute,
		"please work through reference-resolution-substrate-frontend",
		"s-exec-disc", map[string]bool{}, refresolve.NewDisciplineFireTracker(),
	)
	if len(refs) != 1 {
		t.Fatalf("expected exactly 1 discipline ref for execute; got %d (%v)", len(refs), tel.Surfaced)
	}
	if refs[0].Token != "scratchpad-discipline" {
		t.Errorf("execute discipline = %q, want scratchpad-discipline", refs[0].Token)
	}
	if refs[0].Shape != refresolve.ShapeDisciplineSkill {
		t.Errorf("discipline ref shape = %q, want discipline_skill", refs[0].Shape)
	}
}

// The motivating case (§14.4): an execute prompt that NAMES a chain which
// is also a recent open chain must surface that chain EXACTLY ONCE — slug
// detection wins, the work-state pass dedups its duplicate.
func TestHandleParseContext_ExecuteNamedChainNotDoubleListed(t *testing.T) {
	pool := testutil.NewTestDB(t)
	const chainSlug = "reference-resolution-substrate-frontend"
	seedChainProj(t, pool, "mcp-servers", chainSlug, "open") // recent open chain → work-state would surface it

	registry := refresolve.NewRegistry()
	registry.Register(stubResolver{
		shape: refresolve.ShapeChainSlug,
		hit: refresolve.HitSet{Candidates: []refresolve.Candidate{
			{ID: chainSlug, Title: "the chain", Score: 1.0, SourceRef: "chain:" + chainSlug},
		}},
	})
	deps := refresolve.HandlerDeps{
		Pool:             pool,
		Project:          "mcp-servers",
		Registry:         registry,
		Cache:            refresolve.NewParseContextCache(),
		WorkStateCache:   refresolve.NewWorkStateCache(),
		DriftFireTracker: refresolve.NewDriftFireTracker(),
	}
	ctx := events.WithMCPSessionID(context.Background(), "exec-dedup-test")
	body, _ := json.Marshal(struct {
		MessageText string `json:"message_text"`
	}{MessageText: "please work through " + chainSlug})
	r, err := refresolve.HandleParseContext(ctx, deps, body)
	if err != nil {
		t.Fatal(err)
	}
	if r.Intent == nil || r.Intent.Shape != "execute" {
		t.Fatalf("expected Intent.Shape=execute; got %+v", r.Intent)
	}
	count := 0
	for _, ref := range r.References {
		if ref.Token == chainSlug {
			count++
		}
	}
	if count != 1 {
		t.Errorf("chain %q appears %d times in references (want 1 — work-state dedup failed); refs=%+v", chainSlug, count, r.References)
	}
}

// A no-slug execute prompt still surfaces work-state through the handler
// (the open bugs ARE the work, nothing to dedup against).
func TestHandleParseContext_ExecuteNoSlugSurfacesWorkState(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedBugProjWS(t, pool, "mcp-servers", "b-backlog-1", "backlog bug", "high", "open", time.Now())
	deps := refresolve.HandlerDeps{
		Pool:             pool,
		Project:          "mcp-servers",
		Registry:         refresolve.NewRegistry(),
		Cache:            refresolve.NewParseContextCache(),
		WorkStateCache:   refresolve.NewWorkStateCache(),
		DriftFireTracker: refresolve.NewDriftFireTracker(),
	}
	ctx := events.WithMCPSessionID(context.Background(), "exec-noslug-test")
	body, _ := json.Marshal(struct {
		MessageText string `json:"message_text"`
	}{MessageText: "please pick up the next task in order that is ready"}) // execute, no slug named
	r, err := refresolve.HandleParseContext(ctx, deps, body)
	if err != nil {
		t.Fatal(err)
	}
	if r.Intent == nil || r.Intent.Shape != "execute" {
		t.Fatalf("expected Intent.Shape=execute; got %+v", r.Intent)
	}
	found := false
	for _, ref := range r.References {
		if ref.Token == "b-backlog-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("backlog-drain prompt did not surface the open bug via work-state; intent=%+v refs=%+v", r.Intent, r.References)
	}
}
