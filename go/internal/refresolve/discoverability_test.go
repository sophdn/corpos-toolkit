package refresolve_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"testing"

	"toolkit/internal/refresolve"
	"toolkit/internal/testutil"
)

// reference-resolution-migration T3 (skill-body-paring-and-registry-
// population) acceptance: representative discoverability tests. A
// fresh parse_context call against the live manifest should surface
// the right skill for each representative message shape.
//
// Loads the actual mcp-servers/skills/_manifest.toml so this test
// also catches regressions in trigger-keyword coverage over time.
func TestT3_RepresentativeDiscoverability(t *testing.T) {
	// Walk relative-path arithmetic up from this test file to the
	// repo root, the same trick internal/actiondocs/param_tag_gate_test
	// uses. Anchors on the runtime.Caller(0) path so the test runs
	// regardless of where `go test` is invoked from.
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))

	pool := testutil.NewTestDB(t)
	cats, err := refresolve.LoadCatalogs(context.Background(), repoRoot, pool, "")
	if err != nil {
		t.Fatalf("LoadCatalogs: %v", err)
	}
	if cats.SkillManifest == nil {
		t.Skip("skill manifest not present at repoRoot; skipping discoverability check")
	}
	if len(cats.SkillTriggers) == 0 {
		t.Fatal("manifest declares zero trigger keywords; expected at least the parse-context-first-call set")
	}

	registry := refresolve.NewRegistry()
	registry.Register(refresolve.NewSkillTriggerResolver(cats.SkillManifest))
	registry.Register(refresolve.NewDisciplineSkillResolver(cats.SkillManifest))
	// stubFrictionResolver lets the friction_shape ref produce a hit
	// so detectDisciplineSkill can promote it to a discipline_skill
	// reference for bug-filing-discipline.
	registry.Register(stubResolver{
		shape: refresolve.ShapeFrictionShape,
		hit: refresolve.HitSet{Candidates: []refresolve.Candidate{{
			ID: "friction", Title: "friction observation", Score: 1.0,
			SourceRef: "friction:noted",
		}}},
	})

	deps := refresolve.HandlerDeps{
		Pool:     pool,
		Project:  "mcp-servers",
		Registry: registry,
		// Don't pass RepoRoot — we already loaded catalogs above and
		// the handler reloads them per call; we exercise the existing
		// reload path by routing through HandleParseContext.
		RepoRoot: repoRoot,
	}

	cases := []struct {
		name          string
		message       string
		wantSkillName string // expected as a Candidate ID in the response
		wantShape     refresolve.ShapeCategory
	}{
		{
			name:          "rust-conventions on cargo mention",
			message:       "I need to write a new Rust crate using cargo for X",
			wantSkillName: "rust-conventions",
			wantShape:     refresolve.ShapeSkillTrigger,
		},
		{
			name:          "vault-pull-discipline on vault_search mention",
			message:       "should I vault_search for prior art on agent-loop refactoring?",
			wantSkillName: "vault-pull-discipline",
			wantShape:     refresolve.ShapeSkillTrigger,
		},
		{
			name:          "bug-filing-discipline on friction observation",
			message:       "the way the banner reappears is a paper cut worth filing",
			wantSkillName: "bug-filing-discipline",
			wantShape:     refresolve.ShapeDisciplineSkill,
		},
		{
			name:          "refactoring-discipline on refactor mention",
			message:       "I want to refactor this module and restructure it",
			wantSkillName: "refactoring-discipline",
			wantShape:     refresolve.ShapeSkillTrigger,
		},
		{
			name:          "code-migration-discipline on port mention",
			message:       "I want to port from Rust to Go without regressions",
			wantSkillName: "code-migration-discipline",
			wantShape:     refresolve.ShapeSkillTrigger,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params, _ := json.Marshal(struct {
				MessageText   string `json:"message_text"`
				IncludeNoHits bool   `json:"include_no_hits"`
			}{MessageText: tc.message, IncludeNoHits: true})
			result, err := refresolve.HandleParseContext(context.Background(), deps, params)
			if err != nil {
				t.Fatalf("HandleParseContext: %v", err)
			}
			found := false
			for _, ref := range result.References {
				if ref.Shape != tc.wantShape {
					continue
				}
				for _, c := range ref.TopCandidates {
					if c.ID == tc.wantSkillName {
						found = true
					}
				}
			}
			if !found {
				t.Errorf("message %q did not surface %s/%q. References: %+v",
					tc.message, tc.wantShape, tc.wantSkillName, result.References)
			}
		})
	}
}
