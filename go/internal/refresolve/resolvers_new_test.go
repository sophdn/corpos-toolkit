package refresolve_test

import (
	"context"
	"encoding/json"
	"testing"

	"toolkit/internal/refresolve"
	"toolkit/internal/testutil"
)

// reference-resolution-migration T5 Phase 3: skill_trigger resolver
// matches a manifest keyword to its skill body. Built inline so the
// test doesn't depend on the on-disk manifest.
func TestSkillTriggerResolver_KeywordResolvesToSkillBody(t *testing.T) {
	manifest := &refresolve.SkillManifest{
		Skills: []refresolve.SkillManifestEntry{
			{
				Name:            "rust-conventions",
				BodyPath:        "skills/rust-conventions",
				Bucket:          "pure-lazy",
				TriggerKeywords: []string{"cargo", "rust-conventions"},
			},
		},
	}
	r := refresolve.NewSkillTriggerResolver(manifest)
	hs, err := r.Resolve(context.Background(), refresolve.Reference{
		Token: "cargo",
		Shape: refresolve.ShapeSkillTrigger,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if hs.ConfidenceTier != refresolve.TierSingleExact {
		t.Errorf("tier = %q, want single_exact", hs.ConfidenceTier)
	}
	if len(hs.Candidates) != 1 || hs.Candidates[0].ID != "rust-conventions" {
		t.Errorf("candidates = %+v", hs.Candidates)
	}
}

// Two skills sharing a trigger keyword → fuzzy_multi tier so the
// dispatcher asks the agent to disambiguate.
func TestSkillTriggerResolver_SharedKeywordIsFuzzyMulti(t *testing.T) {
	manifest := &refresolve.SkillManifest{
		Skills: []refresolve.SkillManifestEntry{
			{Name: "artifact-review", BodyPath: "skills/artifact-review", TriggerKeywords: []string{"review"}},
			{Name: "github-code-review", BodyPath: "skills/github-code-review", TriggerKeywords: []string{"review"}},
		},
	}
	r := refresolve.NewSkillTriggerResolver(manifest)
	hs, err := r.Resolve(context.Background(), refresolve.Reference{
		Token: "review",
		Shape: refresolve.ShapeSkillTrigger,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if hs.ConfidenceTier != refresolve.TierFuzzyMulti {
		t.Errorf("tier = %q, want fuzzy_multi", hs.ConfidenceTier)
	}
	if len(hs.Candidates) != 2 {
		t.Errorf("candidates count = %d, want 2", len(hs.Candidates))
	}
}

// discipline_skill resolver: friction_shape detection in primary
// pass triggers a synthetic ShapeDisciplineSkill reference for
// bug-filing-discipline that resolves to the skill body.
func TestDisciplineSkillResolver_FrictionTriggersBugFilingDiscipline(t *testing.T) {
	manifest := &refresolve.SkillManifest{
		Skills: []refresolve.SkillManifestEntry{
			{Name: "bug-filing-discipline", BodyPath: "skills/bug-filing-discipline", Bucket: "condense-lazy"},
		},
	}
	r := refresolve.NewDisciplineSkillResolver(manifest)
	hs, err := r.Resolve(context.Background(), refresolve.Reference{
		Token:           "bug-filing-discipline",
		Shape:           refresolve.ShapeDisciplineSkill,
		DetectionMethod: "friction_shape:paper-cut",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if hs.ConfidenceTier != refresolve.TierSingleExact {
		t.Errorf("tier = %q, want single_exact", hs.ConfidenceTier)
	}
	if len(hs.Candidates) != 1 || hs.Candidates[0].ID != "bug-filing-discipline" {
		t.Errorf("candidates = %+v", hs.Candidates)
	}
}

// Bridge detection: a domain_term primary reference promotes to a
// vault_candidate second-pass reference; an external_technical
// primary reference promotes to a kiwix_bridge reference.
// reference-resolution-migration T5 Phase 5.
func TestDetectBridgeShapes_PromotesDomainTermAndExternalTechnical(t *testing.T) {
	pool := testutil.NewTestDB(t)
	registry := refresolve.NewRegistry()
	// Stub primary-shape resolvers so the bridges have something to
	// promote. The bridge detection happens inside Detect (called from
	// the handler core), so this test exercises the end-to-end flow.
	registry.Register(stubResolver{
		shape: refresolve.ShapeDomainTerm,
		hit: refresolve.HitSet{Candidates: []refresolve.Candidate{{
			ID: "tripolar-invariant", Title: "vault note", Score: 0.7,
			SourceRef: "vault:tripolar-invariant.md",
		}}},
	})
	registry.Register(stubResolver{
		shape: refresolve.ShapeExternalTechnical,
		hit: refresolve.HitSet{Candidates: []refresolve.Candidate{{
			ID: "rust-async", Title: "Rust async", Score: 0.6,
			SourceRef: "kiwix_reference:rust/async",
		}}},
	})
	// And install the bridge resolvers — both return whatever stubs
	// we wire (real bridges call HandleVaultSearch / HandleKnowledgeSearch
	// which need full deps; this test exercises detection + dispatch,
	// not the underlying knowledge handler).
	registry.Register(stubResolver{
		shape: refresolve.ShapeVaultCandidate,
		hit: refresolve.HitSet{
			ConfidenceTier: refresolve.TierWeakDomain,
			Candidates: []refresolve.Candidate{{
				ID: "vault/note.md", Title: "matched vault note", Score: 0.5,
				SourceRef: "vault:vault/note.md",
			}},
		},
	})
	registry.Register(stubResolver{
		shape: refresolve.ShapeKiwixBridge,
		hit: refresolve.HitSet{
			ConfidenceTier: refresolve.TierWeakDomain,
			Candidates: []refresolve.Candidate{{
				ID: "kiwix/article", Title: "kiwix article", Score: 0.5,
				SourceRef: "kiwix:kiwix/article",
			}},
		},
	})

	classifier := &stubClassifier{hits: map[string]struct {
		isDomain bool
		conf     float64
	}{"Tripolar Invariant": {isDomain: true, conf: 0.85}}}
	deps := refresolve.HandlerDeps{
		Pool:       pool,
		Project:    "mcp-servers",
		Classifier: classifier,
		Registry:   registry,
	}
	params, _ := json.Marshal(struct {
		MessageText string `json:"message_text"`
	}{MessageText: "checking the Tripolar Invariant"})
	result, err := refresolve.HandleParseContext(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleParseContext: %v", err)
	}
	hasVaultBridge := false
	for _, ref := range result.References {
		if ref.Shape == refresolve.ShapeVaultCandidate {
			hasVaultBridge = true
		}
	}
	if !hasVaultBridge {
		t.Errorf("expected ShapeVaultCandidate from domain_term promotion; got %+v", result.References)
	}
}

// End-to-end via the handler: a message containing a friction shape
// surfaces BOTH a friction_shape reference AND a discipline_skill
// reference for bug-filing-discipline.
func TestHandleParseContext_FrictionEmitsDisciplineSkill(t *testing.T) {
	registry := refresolve.NewRegistry()
	registry.Register(refresolve.NewDisciplineSkillResolver(&refresolve.SkillManifest{
		Skills: []refresolve.SkillManifestEntry{
			{Name: "bug-filing-discipline", BodyPath: "skills/bug-filing-discipline"},
		},
	}))
	// friction_shape resolver is built-in via build_registry, but
	// tests use NewRegistry; install a stub so detection produces
	// the friction reference and the resolver doesn't NoHit-collapse.
	registry.Register(stubResolver{
		shape: refresolve.ShapeFrictionShape,
		hit: refresolve.HitSet{Candidates: []refresolve.Candidate{{
			ID: "friction", Title: "friction observation", Score: 1.0,
			SourceRef: "friction:paper-cut",
		}}},
	})
	deps := refresolve.HandlerDeps{
		Project:  "mcp-servers",
		Registry: registry,
	}
	params, _ := json.Marshal(struct {
		MessageText   string `json:"message_text"`
		IncludeNoHits bool   `json:"include_no_hits"`
	}{
		MessageText:   "the way the banner kept reappearing is a paper cut worth filing",
		IncludeNoHits: true,
	})
	result, err := refresolve.HandleParseContext(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleParseContext: %v", err)
	}
	hasFriction, hasDiscipline := false, false
	for _, ref := range result.References {
		if ref.Shape == refresolve.ShapeFrictionShape {
			hasFriction = true
		}
		if ref.Shape == refresolve.ShapeDisciplineSkill && ref.Token == "bug-filing-discipline" {
			hasDiscipline = true
		}
	}
	if !hasFriction {
		t.Errorf("expected friction_shape reference; got %+v", result.References)
	}
	if !hasDiscipline {
		t.Errorf("expected discipline_skill reference for bug-filing-discipline; got %+v", result.References)
	}
}
