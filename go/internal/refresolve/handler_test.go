package refresolve_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"toolkit/internal/refresolve"
	"toolkit/internal/testutil"
)

// Helper to call the handler with a typed param shape so tests
// don't reach for map[string]any (which forbidigo would flag if
// this were non-test code).
type resolveRefsParams struct {
	MessageText   string `json:"message_text"`
	TopKPerShape  int    `json:"top_k_per_shape,omitempty"`
	IncludeNoHits bool   `json:"include_no_hits,omitempty"`
	TotalBudgetMs int64  `json:"total_budget_ms,omitempty"`
}

func mustMarshalParams(t *testing.T, p resolveRefsParams) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// Acceptance (a): a message with mixed shapes (one slug, one
// domain term, one tool name) gets resolved in one call with all
// three references in the result.
func TestHandleResolveReferences_MixedShapes(t *testing.T) {
	pool := testutil.NewTestDB(t)

	// Seed: one chain slug for chain_resolver to hit.
	seedChainProj(t, pool, "mcp-servers", "mix-test-chain", "open")

	// Build a stub classifier that flags "Domain Concept" as a
	// domain term. Wire it through HandlerDeps so the detector's
	// rubric path produces a domain-term reference.
	classifier := &stubClassifier{hits: map[string]struct {
		isDomain bool
		conf     float64
	}{
		"Domain Concept": {isDomain: true, conf: 0.85},
	}}

	// Build registry that has at least the chain resolver and a
	// stub domain_term resolver wired (production registry will
	// need knowledge.Deps, which tests skip).
	registry := refresolve.NewRegistry()
	registry.Register(stubResolver{
		shape: refresolve.ShapeChainSlug,
		hit:   refresolve.HitSet{Candidates: []refresolve.Candidate{{ID: "mix-test-chain", Title: "test chain", Score: 1.0, SourceRef: "chain:mix-test-chain", DebugNotes: "status=open"}}},
	})
	registry.Register(stubResolver{
		shape: refresolve.ShapeDomainTerm,
		hit:   refresolve.HitSet{Candidates: []refresolve.Candidate{{ID: "vault/note", Title: "Domain note", Score: 0.85, SourceRef: "vault:domain-note.md"}}},
	})

	deps := refresolve.HandlerDeps{
		Pool:       pool,
		Project:    "mcp-servers",
		Classifier: classifier,
		Registry:   registry,
		// RepoRoot empty: filesystem-shape detection still runs against
		// the loaded catalogs (none from filesystem because RepoRoot
		// empty); DB-backed slug detection works via pool.
	}
	params := mustMarshalParams(t, resolveRefsParams{
		MessageText: "Working on mix-test-chain and considering Domain Concept.",
	})
	result, err := refresolve.HandleResolveReferences(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleResolveReferences: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("result.Error: %s", result.Error)
	}
	if len(result.References) < 2 {
		t.Fatalf("want at least 2 references, got %d: %+v", len(result.References), result.References)
	}
	tokens := map[string]bool{}
	for _, r := range result.References {
		tokens[r.Token] = true
	}
	if !tokens["mix-test-chain"] {
		t.Errorf("missing chain slug; got %v", tokens)
	}
	if !tokens["Domain Concept"] {
		t.Errorf("missing domain term; got %v", tokens)
	}
}

// Acceptance (b): latency cap from T3's dispatcher is honored end-
// to-end. Configure a budget; the handler should report the
// elapsed time and (when exceeded) the truncated_by_budget flag.
func TestHandleResolveReferences_LatencyReported(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedChainProj(t, pool, "mcp-servers", "latency-test", "open")
	registry := refresolve.NewRegistry()
	registry.Register(stubResolver{
		shape: refresolve.ShapeChainSlug,
		hit:   refresolve.HitSet{Candidates: []refresolve.Candidate{{ID: "latency-test", Title: "x", Score: 1.0, SourceRef: "chain:latency-test"}}},
	})
	deps := refresolve.HandlerDeps{
		Pool:     pool,
		Project:  "mcp-servers",
		Registry: registry,
	}
	params := mustMarshalParams(t, resolveRefsParams{
		MessageText:   "look up latency-test",
		TotalBudgetMs: 2000,
	})
	result, err := refresolve.HandleResolveReferences(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleResolveReferences: %v", err)
	}
	if result.ResolutionTimeMs < 0 {
		t.Errorf("ResolutionTimeMs: %d", result.ResolutionTimeMs)
	}
	if result.TruncatedByBudget {
		t.Errorf("budget should not have triggered for a fast call")
	}
}

// Bug 1410: a resolver that runs past the budget must cause the
// handler to set TruncatedByBudget. The pre-fix handler compared
// ResolutionTimeMs against its local DispatchOptions copy whose
// TotalBudget could be zero (caller didn't pass total_budget_ms);
// applyDefaults() in the handler keeps the local copy in sync with
// what the dispatcher actually enforces.
func TestHandleResolveReferences_BudgetExceededSetsTruncatedFlag(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedChainProj(t, pool, "mcp-servers", "budget-trip", "open")
	registry := refresolve.NewRegistry()
	// mockResolver lives in dispatch_test.go (same _test package).
	registry.Register(&mockResolver{
		shape: refresolve.ShapeChainSlug,
		sleep: 300 * time.Millisecond,
		// Per-resolver budget = PerResolverMultiplier (4) * TypicalMs
		// (100) = 400ms. Larger than the 100ms total budget below so
		// the dispatcher's total-budget cancellation fires first — the
		// path bug 1410 cares about.
		typicalMs: 100,
		candidates: []refresolve.Candidate{{
			ID: "budget-trip", Title: "x", Score: 1.0, SourceRef: "chain:budget-trip",
		}},
	})
	deps := refresolve.HandlerDeps{Pool: pool, Project: "mcp-servers", Registry: registry}
	params := mustMarshalParams(t, resolveRefsParams{
		MessageText:   "look up budget-trip",
		TotalBudgetMs: 100, // smaller than resolver sleep
	})
	result, err := refresolve.HandleResolveReferences(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleResolveReferences: %v", err)
	}
	if !result.TruncatedByBudget {
		t.Errorf("want TruncatedByBudget=true after exceeding 50ms budget with a 150ms resolver; got result=%+v", result)
	}
}

// Acceptance (c): the handler emits the response envelope shape.
func TestHandleResolveReferences_EnvelopeShape(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedChainProj(t, pool, "mcp-servers", "env-test", "open")
	registry := refresolve.NewRegistry()
	registry.Register(stubResolver{
		shape: refresolve.ShapeChainSlug,
		hit:   refresolve.HitSet{Candidates: []refresolve.Candidate{{ID: "env-test", Title: "env-test chain", Score: 1.0, SourceRef: "chain:env-test", DebugNotes: "status=open"}}},
	})
	deps := refresolve.HandlerDeps{Pool: pool, Project: "mcp-servers", Registry: registry}
	params := mustMarshalParams(t, resolveRefsParams{MessageText: "env-test"})
	result, err := refresolve.HandleResolveReferences(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleResolveReferences: %v", err)
	}
	if len(result.References) == 0 {
		t.Fatalf("want references, got 0")
	}
	r := result.References[0]
	if r.Token != "env-test" {
		t.Errorf("Token: %q", r.Token)
	}
	if r.Shape != refresolve.ShapeChainSlug {
		t.Errorf("Shape: %s", r.Shape)
	}
	if r.ConfidenceTier != refresolve.TierSingleExact {
		t.Errorf("ConfidenceTier: %s", r.ConfidenceTier)
	}
	if r.RecommendedAction != refresolve.PresentUseDirectly {
		t.Errorf("RecommendedAction: %s", r.RecommendedAction)
	}
	if r.PresentedAs == "" {
		t.Errorf("PresentedAs empty")
	}
	if len(r.TopCandidates) != 1 {
		t.Errorf("TopCandidates length: %d", len(r.TopCandidates))
	}
}

// Acceptance (d): calling with empty message_text returns an empty
// References list and zero resolver calls.
func TestHandleResolveReferences_EmptyMessage(t *testing.T) {
	pool := testutil.NewTestDB(t)
	registry := refresolve.NewRegistry()
	deps := refresolve.HandlerDeps{Pool: pool, Project: "mcp-servers", Registry: registry}
	params := mustMarshalParams(t, resolveRefsParams{MessageText: ""})
	result, err := refresolve.HandleResolveReferences(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleResolveReferences: %v", err)
	}
	if result.Error != "" {
		t.Errorf("result.Error: %s", result.Error)
	}
	if len(result.References) != 0 {
		t.Errorf("want 0 refs, got %d", len(result.References))
	}
	if result.ResolverCallsMade != 0 {
		t.Errorf("want 0 resolver calls, got %d", result.ResolverCallsMade)
	}
}

// Acceptance (e): a sample agent-side flow — receive a user
// message, call resolve_references, get PresentedAs strings ready
// to incorporate.
func TestHandleResolveReferences_AgentSideFlow(t *testing.T) {
	pool := testutil.NewTestDB(t)
	agentFlowChainID := seedChainProj(t, pool, "mcp-servers", "agent-flow-chain", "open")
	seedTaskProj(t, pool, agentFlowChainID, "subtask-1", "pending", 1, "do thing")

	registry := refresolve.BuildProductionRegistry(refresolve.ProductionDeps{
		Pool:    pool,
		Project: "mcp-servers",
	})
	deps := refresolve.HandlerDeps{Pool: pool, Project: "mcp-servers", Registry: registry}
	params := mustMarshalParams(t, resolveRefsParams{
		MessageText: "Start work on agent-flow-chain — look at subtask-1 first.",
	})
	result, err := refresolve.HandleResolveReferences(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleResolveReferences: %v", err)
	}
	if len(result.References) < 2 {
		t.Fatalf("want at least 2 references (chain + task), got %d: %+v", len(result.References), result.References)
	}
	// Verify PresentedAs strings include source attribution.
	for _, r := range result.References {
		if r.PresentedAs == "" {
			t.Errorf("PresentedAs empty for token %q", r.Token)
		}
	}
}

// no_hit handling — include_no_hits=false (default) collapses
// no-hit references into the NoHitTokens slice rather than the
// References list.
func TestHandleResolveReferences_NoHitTokensCollapsed(t *testing.T) {
	pool := testutil.NewTestDB(t)
	registry := refresolve.NewRegistry()
	// No resolvers registered → every detected ref returns
	// no_hit + "no resolver" error.

	// Seed a chain so the detector has something to find in the
	// catalog (otherwise the kebab-token is skipped at detection).
	seedChainProj(t, pool, "mcp-servers", "no-hit-test", "open")
	deps := refresolve.HandlerDeps{Pool: pool, Project: "mcp-servers", Registry: registry}
	params := mustMarshalParams(t, resolveRefsParams{
		MessageText: "look at no-hit-test",
	})
	result, err := refresolve.HandleResolveReferences(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleResolveReferences: %v", err)
	}
	if len(result.References) != 0 {
		t.Errorf("want 0 References (no-hit collapsed), got %d", len(result.References))
	}
	if len(result.PartialFailures) == 0 {
		t.Errorf("want partial failures recorded for no-resolver case, got %+v", result)
	}
}

// include_no_hits=true keeps no-hit references in the References
// list with the AcknowledgeNoHitAndAsk recommendation.
func TestHandleResolveReferences_IncludeNoHits(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedChainProj(t, pool, "mcp-servers", "incl-test", "open")
	registry := refresolve.NewRegistry()
	deps := refresolve.HandlerDeps{Pool: pool, Project: "mcp-servers", Registry: registry}
	params := mustMarshalParams(t, resolveRefsParams{
		MessageText:   "look at incl-test",
		IncludeNoHits: true,
	})
	result, err := refresolve.HandleResolveReferences(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleResolveReferences: %v", err)
	}
	if len(result.References) == 0 {
		t.Errorf("want references with include_no_hits=true, got 0")
	}
	for _, r := range result.References {
		if r.ConfidenceTier == refresolve.TierNoHit && r.RecommendedAction != refresolve.PresentAcknowledgeNoHitAndAsk {
			t.Errorf("no_hit ref should recommend acknowledge_no_hit_and_ask, got %s", r.RecommendedAction)
		}
	}
}

// Nil registry returns an error envelope, not a Go error.
func TestHandleResolveReferences_NilRegistry(t *testing.T) {
	deps := refresolve.HandlerDeps{Pool: nil, Registry: nil}
	params := mustMarshalParams(t, resolveRefsParams{MessageText: "anything"})
	result, err := refresolve.HandleResolveReferences(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("want nil Go error, got %v", err)
	}
	if result.Error == "" {
		t.Errorf("want result.Error populated, got empty")
	}
}

// reference-resolution-migration T5 Phase 1: parse_context is the
// canonical handler entrypoint; resolve_references is a soft alias.
// Both must produce structurally-identical responses for the same
// input until the cache + new resolvers wire in follow-on phases.
func TestHandleParseContext_AliasProducesSameShapeAsResolveReferences(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedChainProj(t, pool, "mcp-servers", "alias-shape-test", "open")
	registry := refresolve.NewRegistry()
	registry.Register(stubResolver{
		shape: refresolve.ShapeChainSlug,
		hit:   refresolve.HitSet{Candidates: []refresolve.Candidate{{ID: "alias-shape-test", Title: "test", Score: 1.0, SourceRef: "chain:alias-shape-test"}}},
	})
	deps := refresolve.HandlerDeps{
		Pool:     pool,
		Project:  "mcp-servers",
		Registry: registry,
	}
	params := mustMarshalParams(t, resolveRefsParams{MessageText: "check alias-shape-test"})

	resolveResult, err := refresolve.HandleResolveReferences(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleResolveReferences: %v", err)
	}
	parseResult, err := refresolve.HandleParseContext(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleParseContext: %v", err)
	}

	if len(resolveResult.References) != len(parseResult.References) {
		t.Fatalf("reference count differs: resolve=%d parse=%d", len(resolveResult.References), len(parseResult.References))
	}
	for i := range resolveResult.References {
		r, p := resolveResult.References[i], parseResult.References[i]
		if r.Token != p.Token || r.Shape != p.Shape || r.ConfidenceTier != p.ConfidenceTier {
			t.Errorf("reference[%d] differs: resolve=%+v parse=%+v", i, r, p)
		}
	}
	// Phase 1 invariant: cache fields stay at zero on both surfaces
	// until the filter-cache layer wires in.
	if resolveResult.CacheHits != 0 || resolveResult.CacheMisses != 0 || parseResult.CacheHits != 0 || parseResult.CacheMisses != 0 {
		t.Errorf("cache counters should be zero in Phase 1: resolve=(%d,%d) parse=(%d,%d)",
			resolveResult.CacheHits, resolveResult.CacheMisses, parseResult.CacheHits, parseResult.CacheMisses)
	}
	for i, r := range parseResult.References {
		if r.FromCache || r.CachePolicy != "" {
			t.Errorf("parse reference[%d] should not have cache fields in Phase 1: %+v", i, r)
		}
	}
}

// Bug 1426: a token detected under multiple shapes can produce a hit
// under one shape and a no-hit under another. The envelope must place
// the token in References (and dedupe NoHitTokens) so the agent gets
// exactly one verdict per token — intersect(refs_tokens, no_hit_tokens)
// must be empty.
func TestHandleParseContext_TokenInTwoShapesGoesOnlyToReferences(t *testing.T) {
	pool := testutil.NewTestDB(t)
	// Seed the same slug under both chain and task catalogs so the
	// detector emits two refs (one per shape) for the same token. The
	// detector's slug regex requires kebab-case with >= 2 segments, so
	// the slug has to be hyphenated.
	sharedChainID := seedChainProj(t, pool, "mcp-servers", "shared-name", "open")
	seedTaskProj(t, pool, sharedChainID, "shared-name", "pending", 1, "")

	registry := refresolve.NewRegistry()
	// ChainSlug resolver returns a real hit.
	registry.Register(stubResolver{
		shape: refresolve.ShapeChainSlug,
		hit: refresolve.HitSet{
			ConfidenceTier: refresolve.TierSingleExact,
			Candidates: []refresolve.Candidate{{
				ID: "shared-name", Title: "shared-name chain", Score: 1.0, SourceRef: "chain:shared-name",
			}},
		},
	})
	// TaskSlug resolver returns a no-hit with no Err — would otherwise
	// land in PartialFailures rather than NoHitTokens.
	registry.Register(stubResolver{
		shape: refresolve.ShapeTaskSlug,
		hit:   refresolve.HitSet{ConfidenceTier: refresolve.TierNoHit},
	})

	deps := refresolve.HandlerDeps{Pool: pool, Project: "mcp-servers", Registry: registry}
	params := mustMarshalParams(t, resolveRefsParams{MessageText: "look at shared-name"})
	result, err := refresolve.HandleParseContext(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleParseContext: %v", err)
	}
	refsTokens := map[string]struct{}{}
	for _, r := range result.References {
		refsTokens[r.Token] = struct{}{}
	}
	if _, ok := refsTokens["shared-name"]; !ok {
		t.Fatalf("expected `shared-name` in References; got %+v", result.References)
	}
	for _, tok := range result.NoHitTokens {
		if _, hit := refsTokens[tok]; hit {
			t.Errorf("token %q appears in both References and NoHitTokens — envelope invariant violated; refs=%+v no_hits=%+v",
				tok, result.References, result.NoHitTokens)
		}
	}
}

// Bug 1426: a token producing two no-hits from different shapes should
// appear at most once in NoHitTokens — the slice must be deduped.
func TestHandleParseContext_NoHitTokensDedupedAcrossShapes(t *testing.T) {
	pool := testutil.NewTestDB(t)
	sharedChainID := seedChainProj(t, pool, "mcp-servers", "shared-name", "open")
	seedTaskProj(t, pool, sharedChainID, "shared-name", "pending", 1, "")

	registry := refresolve.NewRegistry()
	// Both resolvers return no-hit; the token should appear once in
	// NoHitTokens, not twice.
	registry.Register(stubResolver{
		shape: refresolve.ShapeChainSlug,
		hit:   refresolve.HitSet{ConfidenceTier: refresolve.TierNoHit},
	})
	registry.Register(stubResolver{
		shape: refresolve.ShapeTaskSlug,
		hit:   refresolve.HitSet{ConfidenceTier: refresolve.TierNoHit},
	})

	deps := refresolve.HandlerDeps{Pool: pool, Project: "mcp-servers", Registry: registry}
	params := mustMarshalParams(t, resolveRefsParams{MessageText: "look at shared-name"})
	result, err := refresolve.HandleParseContext(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleParseContext: %v", err)
	}
	count := 0
	for _, tok := range result.NoHitTokens {
		if tok == "shared-name" {
			count++
		}
	}
	if count > 1 {
		t.Errorf("token `shared-name` appears %d times in NoHitTokens; want at most 1: %+v", count, result.NoHitTokens)
	}
}

// Bug 1451: emitGroundingEvents must stamp the per-call project on
// the grounding_events row, not the static deps.Project captured at
// startup. The build-handler wrapper (BuildResolveReferencesHandler)
// receives the dispatcher-resolved project as its string argument
// and is responsible for threading it into the handler.
//
// Acceptance: a dispatch.Handler built from deps.Project="" still
// emits grounding_events.project_id=<per-call project> when the
// dispatcher passes a non-empty project arg. Without the fix the row
// lands with project_id="" and the (project_id, source_ref) JOIN
// against knowledge_pointers zeros out the Context Pull Inspector's
// first_candidate.source_type column.
func TestBuildResolveReferencesHandler_PerCallProjectStampedOnGroundingEvent(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedChainProj(t, pool, "mcp-servers", "per-call-proj-chain", "open")

	registry := refresolve.BuildProductionRegistry(refresolve.ProductionDeps{
		Pool:    pool,
		Project: "mcp-servers",
	})
	// deps.Project deliberately empty — mirrors the production
	// stdio-session bug (toolkit-server booted without --default-project).
	deps := refresolve.HandlerDeps{Pool: pool, Project: "", Registry: registry}
	handler := refresolve.BuildResolveReferencesHandler(deps)

	params := mustMarshalParams(t, resolveRefsParams{
		MessageText: "Start work on per-call-proj-chain.",
	})
	// The dispatcher passes the resolved project as the string arg;
	// the handler must use it instead of the empty deps.Project.
	if _, err := handler(context.Background(), "mcp-servers", params); err != nil {
		t.Fatalf("handler: %v", err)
	}

	var projectID string
	err := pool.DB().QueryRow(
		`SELECT project_id FROM grounding_events
		 WHERE query_source = 'reference_resolution'
		 ORDER BY id DESC LIMIT 1`,
	).Scan(&projectID)
	if err != nil {
		t.Fatalf("read grounding_events: %v", err)
	}
	if projectID != "mcp-servers" {
		t.Errorf("project_id=%q, want %q (per-call dispatcher value should override empty deps.Project)", projectID, "mcp-servers")
	}
}

// stubResolver implements Resolver for handler tests. Distinct
// from mockResolver in dispatch_test.go (which has call-counting +
// sleep behavior). Kept simple to avoid scope creep on this file.
type stubResolver struct {
	shape refresolve.ShapeCategory
	hit   refresolve.HitSet
}

func (s stubResolver) Shape() refresolve.ShapeCategory { return s.shape }
func (s stubResolver) Cost() refresolve.ResolverCostHint {
	return refresolve.ResolverCostHint{TypicalMs: 5}
}
func (s stubResolver) Resolve(_ context.Context, _ refresolve.Reference) (refresolve.HitSet, error) {
	return s.hit, nil
}

// Chain 602: end-to-end inlining test. Sets up a temp repo with a
// skills/_manifest.toml + one skill body, drives parse_context with a
// message containing the trigger keyword, and asserts BodyInlined is
// populated when the per-request override is set.
func TestHandleParseContext_InlineSkillBody_OnUseDirectly(t *testing.T) {
	repoRoot := t.TempDir()
	mustWriteManifest(t, repoRoot, `
[[skill]]
name = "demo-skill"
body_path = "skills/demo-skill"
install_target = "skills/demo-skill"
bucket = "pure-lazy"
trigger_keywords = ["demo-trigger-keyword"]
description = "Test skill for chain 602 inline-body integration test."
origin = "test"
`)
	body := "---\nname: demo-skill\n---\n\n# Demo skill body\n\nSome content here."
	mustWriteSkillBody(t, repoRoot, "demo-skill", body)

	manifest, err := refresolve.LoadSkillManifest(repoRoot)
	if err != nil {
		t.Fatalf("LoadSkillManifest: %v", err)
	}
	registry := refresolve.NewRegistry()
	registry.Register(refresolve.NewSkillTriggerResolver(manifest))

	override := true
	deps := refresolve.HandlerDeps{
		Project:   "mcp-servers",
		Registry:  registry,
		RepoRoot:  repoRoot,
		BodyCache: refresolve.NewBodyCache(),
	}
	params, err := json.Marshal(struct {
		MessageText       string `json:"message_text"`
		InlineSkillBodies *bool  `json:"inline_skill_bodies"`
	}{
		MessageText:       "When I hit demo-trigger-keyword the skill should fire.",
		InlineSkillBodies: &override,
	})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}

	result, err := refresolve.HandleParseContext(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleParseContext: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("result.Error: %s", result.Error)
	}

	var got *refresolve.ResolvedReference
	for i := range result.References {
		r := &result.References[i]
		if r.Shape == refresolve.ShapeSkillTrigger && r.Token == "demo-trigger-keyword" {
			got = r
			break
		}
	}
	if got == nil {
		t.Fatalf("expected a skill_trigger ref for demo-trigger-keyword; refs=%+v", result.References)
	}
	if got.RecommendedAction != refresolve.PresentUseDirectly {
		t.Errorf("RecommendedAction: %s, want use_directly", got.RecommendedAction)
	}
	if got.BodyInlined == "" {
		t.Errorf("BodyInlined: empty; want the demo skill body (envelope feature flag may not have wired)")
	}
	if !strings.Contains(got.BodyInlined, "# Demo skill body") {
		t.Errorf("BodyInlined: missing expected content; got %q", got.BodyInlined)
	}
	if got.BodyBytes != len(body) {
		t.Errorf("BodyBytes: %d, want %d", got.BodyBytes, len(body))
	}
	if result.InlinedRefs != 1 {
		t.Errorf("InlinedRefs: %d, want 1", result.InlinedRefs)
	}
	if result.InlinedBytes != len(body) {
		t.Errorf("InlinedBytes: %d, want %d", result.InlinedBytes, len(body))
	}
}

// Same setup but the per-request override is false → no inlining,
// envelope stays byte-equivalent to today's output.
func TestHandleParseContext_InlineSkillBody_DisabledLeavesEnvelopeClean(t *testing.T) {
	repoRoot := t.TempDir()
	mustWriteManifest(t, repoRoot, `
[[skill]]
name = "demo-skill"
body_path = "skills/demo-skill"
install_target = "skills/demo-skill"
bucket = "pure-lazy"
trigger_keywords = ["demo-trigger-keyword"]
description = "Test skill."
origin = "test"
`)
	mustWriteSkillBody(t, repoRoot, "demo-skill", "body content")

	manifest, err := refresolve.LoadSkillManifest(repoRoot)
	if err != nil {
		t.Fatalf("LoadSkillManifest: %v", err)
	}
	registry := refresolve.NewRegistry()
	registry.Register(refresolve.NewSkillTriggerResolver(manifest))

	override := false
	deps := refresolve.HandlerDeps{
		Project:   "mcp-servers",
		Registry:  registry,
		RepoRoot:  repoRoot,
		BodyCache: refresolve.NewBodyCache(),
	}
	params, _ := json.Marshal(struct {
		MessageText       string `json:"message_text"`
		InlineSkillBodies *bool  `json:"inline_skill_bodies"`
	}{
		MessageText:       "When I hit demo-trigger-keyword.",
		InlineSkillBodies: &override,
	})
	result, err := refresolve.HandleParseContext(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleParseContext: %v", err)
	}
	for _, r := range result.References {
		if r.BodyInlined != "" || r.BodySummary != "" || r.BodyBytes != 0 || r.BodyTruncated {
			t.Errorf("ref %s/%s: feature flag off should leave Body* empty; got %+v",
				r.Shape, r.Token, r)
		}
	}
	if result.InlinedBytes != 0 || result.InlinedRefs != 0 {
		t.Errorf("InlinedBytes/InlinedRefs: want (0,0), got (%d,%d)",
			result.InlinedBytes, result.InlinedRefs)
	}
}

// Chain 602 T6 follow-up (2026-05-21): the default flipped from
// env-var-gated-OFF to default-ON. This test pins the new default:
// when no per-request override is supplied AND the env var is unset
// (the common stdio MCP child case the original rollout missed), the
// envelope MUST carry the inlined body. Without this assertion the
// stdio-dormant regression could quietly come back.
func TestHandleParseContext_InlineSkillBody_DefaultOnWithoutOverride(t *testing.T) {
	t.Setenv(refresolve.InlineBodyEnvVar, "")
	repoRoot := t.TempDir()
	mustWriteManifest(t, repoRoot, `
[[skill]]
name = "demo-skill"
body_path = "skills/demo-skill"
install_target = "skills/demo-skill"
bucket = "pure-lazy"
trigger_keywords = ["demo-trigger-keyword"]
description = "Test skill for default-on assertion."
origin = "test"
`)
	body := "---\nname: demo-skill\n---\n\n# Demo body\n\nDefault-on content."
	mustWriteSkillBody(t, repoRoot, "demo-skill", body)

	manifest, err := refresolve.LoadSkillManifest(repoRoot)
	if err != nil {
		t.Fatalf("LoadSkillManifest: %v", err)
	}
	registry := refresolve.NewRegistry()
	registry.Register(refresolve.NewSkillTriggerResolver(manifest))

	deps := refresolve.HandlerDeps{
		Project:   "mcp-servers",
		Registry:  registry,
		RepoRoot:  repoRoot,
		BodyCache: refresolve.NewBodyCache(),
	}
	// No InlineSkillBodies override — relies on the default.
	params, _ := json.Marshal(struct {
		MessageText string `json:"message_text"`
	}{
		MessageText: "When I type demo-trigger-keyword the skill should fire by default.",
	})
	result, err := refresolve.HandleParseContext(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleParseContext: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("result.Error: %s", result.Error)
	}

	var got *refresolve.ResolvedReference
	for i := range result.References {
		r := &result.References[i]
		if r.Shape == refresolve.ShapeSkillTrigger && r.Token == "demo-trigger-keyword" {
			got = r
			break
		}
	}
	if got == nil {
		t.Fatalf("expected a skill_trigger ref; refs=%+v", result.References)
	}
	if got.BodyInlined == "" {
		t.Fatalf("BodyInlined empty under default-on; the chain 602 T6 follow-up flip must have regressed (or env var leaked from outer process)")
	}
	if !strings.Contains(got.BodyInlined, "Default-on content") {
		t.Errorf("BodyInlined missing expected content; got %q", got.BodyInlined)
	}
	if result.InlinedRefs != 1 {
		t.Errorf("InlinedRefs: %d, want 1", result.InlinedRefs)
	}
	if result.InlinedBytes != len(body) {
		t.Errorf("InlinedBytes: %d, want %d", result.InlinedBytes, len(body))
	}
}

// Env var kill-switch — chain 602 T6 follow-up retained the env var
// as a way to disable inlining without a binary rebuild. Pinning the
// "0" / "false" / "off" set to disabling values so future maintainers
// don't tighten the parser and break the kill switch.
func TestHandleParseContext_InlineSkillBody_KillSwitchDisablesInlining(t *testing.T) {
	for _, val := range []string{"0", "false", "no", "off", "FALSE", "Off"} {
		t.Run("kill="+val, func(t *testing.T) {
			t.Setenv(refresolve.InlineBodyEnvVar, val)
			repoRoot := t.TempDir()
			mustWriteManifest(t, repoRoot, `
[[skill]]
name = "demo-skill"
body_path = "skills/demo-skill"
install_target = "skills/demo-skill"
bucket = "pure-lazy"
trigger_keywords = ["demo-trigger-keyword"]
description = "Test skill."
origin = "test"
`)
			mustWriteSkillBody(t, repoRoot, "demo-skill", "body content")

			manifest, err := refresolve.LoadSkillManifest(repoRoot)
			if err != nil {
				t.Fatalf("LoadSkillManifest: %v", err)
			}
			registry := refresolve.NewRegistry()
			registry.Register(refresolve.NewSkillTriggerResolver(manifest))

			deps := refresolve.HandlerDeps{
				Project:   "mcp-servers",
				Registry:  registry,
				RepoRoot:  repoRoot,
				BodyCache: refresolve.NewBodyCache(),
			}
			params, _ := json.Marshal(struct {
				MessageText string `json:"message_text"`
			}{
				MessageText: "demo-trigger-keyword should NOT inline.",
			})
			result, err := refresolve.HandleParseContext(context.Background(), deps, params)
			if err != nil {
				t.Fatalf("HandleParseContext: %v", err)
			}
			for _, r := range result.References {
				if r.BodyInlined != "" || r.BodySummary != "" || r.BodyBytes != 0 {
					t.Errorf("env=%q: ref %s/%s leaked Body fields; kill-switch broken: %+v",
						val, r.Shape, r.Token, r)
				}
			}
			if result.InlinedBytes != 0 || result.InlinedRefs != 0 {
				t.Errorf("env=%q: envelope totals non-zero; kill-switch broken: bytes=%d refs=%d",
					val, result.InlinedBytes, result.InlinedRefs)
			}
		})
	}
}

// Multi-trigger-same-skill dedup. When two trigger keywords in a
// message resolve to the SAME skill (e.g. 'golang' + 'go-test' both
// → go-conventions), only the FIRST matching ref carries
// BodyInlined; subsequent refs carry BodyInlinedFromRefIndex
// pointing back at the first. envelope inlined_bytes counts the
// body ONCE. Filed as suggestion
// `dedupe-inline-skill-bodies-when-multiple-trigger-keywords-point-at-same-skill`
// after chain 602 T6 smoke observed 2× duplicated body (3930 bytes
// for a 1965-byte skill).
func TestHandleParseContext_InlineSkillBody_DedupesMultiTriggerSameSkill(t *testing.T) {
	t.Setenv(refresolve.InlineBodyEnvVar, "")
	repoRoot := t.TempDir()
	mustWriteManifest(t, repoRoot, `
[[skill]]
name = "demo-skill"
body_path = "skills/demo-skill"
install_target = "skills/demo-skill"
bucket = "pure-lazy"
trigger_keywords = ["alpha-keyword", "beta-keyword"]
description = "Demo skill with two triggers pointing at the same body."
origin = "test"
`)
	body := "---\nname: demo-skill\n---\n\n# Demo body for dedup."
	mustWriteSkillBody(t, repoRoot, "demo-skill", body)

	manifest, err := refresolve.LoadSkillManifest(repoRoot)
	if err != nil {
		t.Fatalf("LoadSkillManifest: %v", err)
	}
	registry := refresolve.NewRegistry()
	registry.Register(refresolve.NewSkillTriggerResolver(manifest))

	deps := refresolve.HandlerDeps{
		Project:   "mcp-servers",
		Registry:  registry,
		RepoRoot:  repoRoot,
		BodyCache: refresolve.NewBodyCache(),
	}
	params, _ := json.Marshal(struct {
		MessageText string `json:"message_text"`
	}{
		MessageText: "Looking at alpha-keyword and beta-keyword in the same sentence.",
	})
	result, err := refresolve.HandleParseContext(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleParseContext: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("result.Error: %s", result.Error)
	}

	// Both refs should land — one per trigger keyword.
	var refs []*refresolve.ResolvedReference
	for i := range result.References {
		r := &result.References[i]
		if r.Shape == refresolve.ShapeSkillTrigger {
			refs = append(refs, r)
		}
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 skill_trigger refs, got %d; refs=%+v", len(refs), result.References)
	}

	// Exactly one of the two refs should carry the full BodyInlined;
	// the other should carry BodyInlinedFromRefIndex pointing at
	// the first.
	var withBody, withBackref *refresolve.ResolvedReference
	for _, r := range refs {
		if r.BodyInlined != "" {
			withBody = r
		}
		if r.BodyInlinedFromRefIndex != nil {
			withBackref = r
		}
	}
	if withBody == nil {
		t.Fatalf("no ref carries BodyInlined; envelope skipped inlining entirely")
	}
	if withBackref == nil {
		t.Fatalf("no ref carries BodyInlinedFromRefIndex; dedup did not engage — both refs would carry duplicate bodies")
	}
	if !strings.Contains(withBody.BodyInlined, "Demo body for dedup") {
		t.Errorf("BodyInlined: missing expected content; got %q", withBody.BodyInlined)
	}
	if withBackref.BodyInlined != "" {
		t.Errorf("deduped ref leaked BodyInlined: got %q", withBackref.BodyInlined)
	}
	if withBackref.BodySummary != "" {
		t.Errorf("deduped ref leaked BodySummary: got %q", withBackref.BodySummary)
	}
	// BodyBytes still set on the deduped ref (size known via the
	// resolver; agent sees the same metadata as on the original).
	if withBackref.BodyBytes != len(body) {
		t.Errorf("deduped ref BodyBytes: %d, want %d (size should still be reported)",
			withBackref.BodyBytes, len(body))
	}

	// Envelope totals: one body counted, not two.
	if result.InlinedBytes != len(body) {
		t.Errorf("InlinedBytes: %d, want %d (dedup must count body once, not 2× = %d)",
			result.InlinedBytes, len(body), 2*len(body))
	}
	if result.InlinedRefs != 1 {
		t.Errorf("InlinedRefs: %d, want 1 (one unique body inlined)", result.InlinedRefs)
	}

	// Back-ref must point at a ref that actually has the body. Locate
	// the references slice index for `withBody` and verify
	// withBackref.BodyInlinedFromRefIndex == that index.
	var withBodyIdx int = -1
	for i := range result.References {
		if &result.References[i] == withBody {
			withBodyIdx = i
			break
		}
	}
	if withBodyIdx == -1 {
		t.Fatalf("could not locate withBody in result.References (test scaffolding bug)")
	}
	if *withBackref.BodyInlinedFromRefIndex != withBodyIdx {
		t.Errorf("BodyInlinedFromRefIndex: %d, want %d (back-ref must point at ref with the body)",
			*withBackref.BodyInlinedFromRefIndex, withBodyIdx)
	}
}

// Weak-boundary skill candidate emit (suggestion
// `weak-boundary-skill-candidate-emit-when-trigger-keyword-prefixes-a-kebab-slug`).
// When a trigger keyword appears as a prefix of a longer kebab token
// (e.g. "parse-context" inside "parse-context-skill-body-..."), the
// strict-boundary detector correctly rejects but the weak-boundary
// detector emits a ShapeSkillCandidate ref. Pins three contract
// properties:
//
//	(a) shape is ShapeSkillCandidate, not ShapeSkillTrigger
//	(b) recommended action is mention_as_possibly_relevant, NOT use_directly
//	(c) BodyInlined stays empty even with the default-ON inline feature
//	    (the shape is excluded from the inliner's eligibility check)
func TestHandleParseContext_WeakBoundary_EmitsSkillCandidateWithoutInlining(t *testing.T) {
	t.Setenv(refresolve.InlineBodyEnvVar, "")
	repoRoot := t.TempDir()
	// "demo-trigger" as a trigger keyword. The test message embeds it
	// inside a longer kebab token so strict boundary rejects but
	// weak boundary accepts.
	mustWriteManifest(t, repoRoot, `
[[skill]]
name = "demo-skill"
body_path = "skills/demo-skill"
install_target = "skills/demo-skill"
bucket = "pure-lazy"
trigger_keywords = ["demo-trigger"]
description = "Test skill for weak-boundary detector."
origin = "test"
`)
	mustWriteSkillBody(t, repoRoot, "demo-skill", "body content for weak-boundary test")

	manifest, err := refresolve.LoadSkillManifest(repoRoot)
	if err != nil {
		t.Fatalf("LoadSkillManifest: %v", err)
	}
	registry := refresolve.NewRegistry()
	registry.Register(refresolve.NewSkillTriggerResolver(manifest))
	registry.Register(refresolve.NewSkillCandidateResolver(manifest))

	deps := refresolve.HandlerDeps{
		Project:   "mcp-servers",
		Registry:  registry,
		RepoRoot:  repoRoot,
		BodyCache: refresolve.NewBodyCache(),
	}
	// "demo-trigger-extension-followup" has "demo-trigger" as a
	// PREFIX inside a longer kebab token. boundaryOKCatalog rejects
	// (right neighbor is "-"); boundaryOK accepts.
	params, _ := json.Marshal(struct {
		MessageText string `json:"message_text"`
	}{
		MessageText: "please work through demo-trigger-extension-followup",
	})
	result, err := refresolve.HandleParseContext(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleParseContext: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("result.Error: %s", result.Error)
	}

	// Find the weak-boundary ref. Should NOT be present as
	// ShapeSkillTrigger (strict rejected); should be present as
	// ShapeSkillCandidate.
	var strict, candidate *refresolve.ResolvedReference
	for i := range result.References {
		r := &result.References[i]
		if r.Token != "demo-trigger" {
			continue
		}
		switch r.Shape {
		case refresolve.ShapeSkillTrigger:
			strict = r
		case refresolve.ShapeSkillCandidate:
			candidate = r
		}
	}
	if strict != nil {
		t.Errorf("expected NO ShapeSkillTrigger ref for 'demo-trigger' inside a kebab token; strict boundary should have rejected. got: %+v", strict)
	}
	if candidate == nil {
		t.Fatalf("expected a ShapeSkillCandidate ref for 'demo-trigger'; refs=%+v", result.References)
	}
	if candidate.RecommendedAction != refresolve.PresentMentionAsPossiblyRelevant {
		t.Errorf("RecommendedAction: %s, want mention_as_possibly_relevant — weak-boundary path must soften the action so the agent doesn't treat it as use_directly",
			candidate.RecommendedAction)
	}
	if candidate.BodyInlined != "" || candidate.BodySummary != "" || candidate.BodyBytes != 0 {
		t.Errorf("weak-boundary candidate leaked body fields; the shape must be excluded from the inliner's eligibility filter. got: BodyInlined=%q BodySummary=%q BodyBytes=%d",
			candidate.BodyInlined, candidate.BodySummary, candidate.BodyBytes)
	}
}

// Multi-candidate weak-boundary case: when a trigger keyword is
// shared by multiple skills (the live "parse-context" keyword is
// declared by both parse-context-first-call and reference-resolution),
// the skillCandidateResolver returns TierFuzzyMulti. The
// formatResolved special-case for ShapeSkillCandidate MUST still
// produce PresentMentionAsPossiblyRelevant — not
// PresentAskUserToDisambiguate (which would be louder than the
// weak-boundary signal warrants). The smoke test on 2026-05-21
// caught this gap: the first ShapeSkillCandidate implementation
// only special-cased TierSingleExact, so multi-candidate matches
// fell through to the fuzzy-multi branch.
func TestHandleParseContext_WeakBoundary_MultiCandidateStillSoftensAction(t *testing.T) {
	t.Setenv(refresolve.InlineBodyEnvVar, "")
	repoRoot := t.TempDir()
	// Two skills sharing the same trigger keyword.
	mustWriteManifest(t, repoRoot, `
[[skill]]
name = "skill-a"
body_path = "skills/skill-a"
install_target = "skills/skill-a"
bucket = "pure-lazy"
trigger_keywords = ["shared-trigger"]
description = "First skill claiming the shared trigger."
origin = "test"

[[skill]]
name = "skill-b"
body_path = "skills/skill-b"
install_target = "skills/skill-b"
bucket = "pure-lazy"
trigger_keywords = ["shared-trigger"]
description = "Second skill claiming the shared trigger."
origin = "test"
`)
	mustWriteSkillBody(t, repoRoot, "skill-a", "body of skill-a")
	mustWriteSkillBody(t, repoRoot, "skill-b", "body of skill-b")

	manifest, err := refresolve.LoadSkillManifest(repoRoot)
	if err != nil {
		t.Fatalf("LoadSkillManifest: %v", err)
	}
	registry := refresolve.NewRegistry()
	registry.Register(refresolve.NewSkillTriggerResolver(manifest))
	registry.Register(refresolve.NewSkillCandidateResolver(manifest))

	deps := refresolve.HandlerDeps{
		Project:   "mcp-servers",
		Registry:  registry,
		RepoRoot:  repoRoot,
		BodyCache: refresolve.NewBodyCache(),
	}
	// Embed "shared-trigger" inside a kebab token so the weak-
	// boundary detector fires.
	params, _ := json.Marshal(struct {
		MessageText string `json:"message_text"`
	}{
		MessageText: "look at the shared-trigger-followup-chain",
	})
	result, err := refresolve.HandleParseContext(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleParseContext: %v", err)
	}

	var candidate *refresolve.ResolvedReference
	for i := range result.References {
		r := &result.References[i]
		if r.Shape == refresolve.ShapeSkillCandidate && r.Token == "shared-trigger" {
			candidate = r
			break
		}
	}
	if candidate == nil {
		t.Fatalf("expected ShapeSkillCandidate ref for 'shared-trigger'; refs=%+v", result.References)
	}
	if candidate.ConfidenceTier != refresolve.TierFuzzyMulti {
		t.Errorf("ConfidenceTier: %s, want fuzzy_multi (two skills claim the trigger)", candidate.ConfidenceTier)
	}
	if candidate.RecommendedAction != refresolve.PresentMentionAsPossiblyRelevant {
		t.Errorf("RecommendedAction: %s, want mention_as_possibly_relevant — multi-candidate weak-boundary must NOT fall into ask_user_to_disambiguate (that's louder than the weak-boundary signal)",
			candidate.RecommendedAction)
	}
	if len(candidate.TopCandidates) != 2 {
		t.Errorf("TopCandidates count: %d, want 2 (both skills surface for the agent to pick from)", len(candidate.TopCandidates))
	}
	if candidate.BodyInlined != "" || candidate.BodySummary != "" || candidate.BodyBytes != 0 {
		t.Errorf("multi-candidate weak-boundary leaked body fields; got BodyInlined=%q BodySummary=%q BodyBytes=%d",
			candidate.BodyInlined, candidate.BodySummary, candidate.BodyBytes)
	}
}

// Negative case: when the trigger keyword appears as a STANDALONE
// word (strict boundary passes), only the strict ShapeSkillTrigger
// ref emits — no double-emit on ShapeSkillCandidate. The weak
// detector's mutual-exclusion-by-construction must hold.
func TestHandleParseContext_WeakBoundary_DoesNotDoubleEmitOnStandaloneKeyword(t *testing.T) {
	t.Setenv(refresolve.InlineBodyEnvVar, "")
	repoRoot := t.TempDir()
	mustWriteManifest(t, repoRoot, `
[[skill]]
name = "demo-skill"
body_path = "skills/demo-skill"
install_target = "skills/demo-skill"
bucket = "pure-lazy"
trigger_keywords = ["demo-trigger"]
description = "Test skill."
origin = "test"
`)
	mustWriteSkillBody(t, repoRoot, "demo-skill", "body content")

	manifest, err := refresolve.LoadSkillManifest(repoRoot)
	if err != nil {
		t.Fatalf("LoadSkillManifest: %v", err)
	}
	registry := refresolve.NewRegistry()
	registry.Register(refresolve.NewSkillTriggerResolver(manifest))
	registry.Register(refresolve.NewSkillCandidateResolver(manifest))

	deps := refresolve.HandlerDeps{
		Project:   "mcp-servers",
		Registry:  registry,
		RepoRoot:  repoRoot,
		BodyCache: refresolve.NewBodyCache(),
	}
	// Standalone "demo-trigger" — strict boundary passes (both
	// neighbors are spaces).
	params, _ := json.Marshal(struct {
		MessageText string `json:"message_text"`
	}{
		MessageText: "looking at demo-trigger today",
	})
	result, err := refresolve.HandleParseContext(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleParseContext: %v", err)
	}

	var strictCount, candidateCount int
	for _, r := range result.References {
		if r.Token != "demo-trigger" {
			continue
		}
		switch r.Shape {
		case refresolve.ShapeSkillTrigger:
			strictCount++
		case refresolve.ShapeSkillCandidate:
			candidateCount++
		}
	}
	if strictCount != 1 {
		t.Errorf("expected exactly 1 ShapeSkillTrigger ref, got %d", strictCount)
	}
	if candidateCount != 0 {
		t.Errorf("expected 0 ShapeSkillCandidate refs (the standalone keyword hits the strict path; weak-boundary detector skips strict-accepted matches by construction); got %d", candidateCount)
	}
}

func mustWriteManifest(t *testing.T, repoRoot, body string) {
	t.Helper()
	dir := filepath.Join(repoRoot, "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "_manifest.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func mustWriteSkillBody(t *testing.T, repoRoot, name, body string) {
	t.Helper()
	dir := filepath.Join(repoRoot, "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
}
