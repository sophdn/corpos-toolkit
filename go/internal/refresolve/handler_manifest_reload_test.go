package refresolve_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"toolkit/internal/refresolve"
	"toolkit/internal/testutil"
)

// TestParseContext_ResolvesSkillAddedToManifestAfterRegistryBuilt pins
// bug 884 (parse-context-detects-new-manifest-skills-but-cannot-resolve-
// until-restart).
//
// The asymmetry: parse_context's DETECTION reloads catalogs every call
// (handler.go LoadCatalogs), so a trigger keyword added to
// skills/_manifest.toml is picked up immediately. But RESOLUTION
// dispatched against the registry's skill_trigger / discipline_skill /
// skill_candidate resolvers, each of which held a manifest SNAPSHOT
// taken when the registry was built at boot. Result: the new skill's
// keyword DETECTED (no_hit_tokens) but never RESOLVED (TierNoHit) until
// a daemon restart rebuilt the registry.
//
// This test builds the registry from manifest V1, then mutates the
// on-disk manifest to V2 (adding beta-skill) WITHOUT rebuilding the
// registry, and asserts beta-skill RESOLVES on the next
// HandleParseContext call. Pre-fix it returns no skill_trigger hit.
func TestParseContext_ResolvesSkillAddedToManifestAfterRegistryBuilt(t *testing.T) {
	repoRoot := t.TempDir()
	skillsDir := filepath.Join(repoRoot, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatalf("mkdir skills: %v", err)
	}
	manifestPath := filepath.Join(skillsDir, "_manifest.toml")
	writeManifest := func(body string) {
		t.Helper()
		if err := os.WriteFile(manifestPath, []byte(body), 0o644); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
	}

	const v1 = `
[[skill]]
name = "alpha-skill"
body_path = "skills/alpha-skill/SKILL.md"
bucket = "test"
trigger_keywords = ["alphakw"]
`
	writeManifest(v1)

	// Build the registry from the V1 snapshot — this stands in for the
	// boot-time BuildProductionRegistry wiring (which loads the manifest
	// once and pins it into the three manifest-backed resolvers).
	v1manifest, err := refresolve.LoadSkillManifest(repoRoot)
	if err != nil {
		t.Fatalf("load V1 manifest: %v", err)
	}
	registry := refresolve.NewRegistry()
	registry.Register(refresolve.NewSkillTriggerResolver(v1manifest))
	registry.Register(refresolve.NewDisciplineSkillResolver(v1manifest))
	registry.Register(refresolve.NewSkillCandidateResolver(v1manifest))

	deps := refresolve.HandlerDeps{
		Pool:     testutil.NewTestDB(t),
		Project:  "mcp-servers",
		Registry: registry,
		RepoRoot: repoRoot,
	}

	// Precondition: V1's own skill resolves before any mutation.
	if !resolvesSkillTrigger(t, deps, "please run alphakw now", "alpha-skill") {
		t.Fatal("precondition: alpha-skill should resolve under the V1 manifest")
	}

	// V2: add beta-skill. The registry is NOT rebuilt — only the on-disk
	// manifest changes, exactly as `register a new skill` does in prod.
	writeManifest(v1 + `
[[skill]]
name = "beta-skill"
body_path = "skills/beta-skill/SKILL.md"
bucket = "test"
trigger_keywords = ["betakw"]
`)

	if !resolvesSkillTrigger(t, deps, "please run betakw now", "beta-skill") {
		t.Errorf("beta-skill added to the manifest after the registry was built did NOT resolve via skill_trigger; bug 884 regression — the registry holds a stale manifest snapshot")
	}
}

// resolvesSkillTrigger runs HandleParseContext for message and reports
// whether wantSkill surfaced as a skill_trigger Candidate.
func resolvesSkillTrigger(t *testing.T, deps refresolve.HandlerDeps, message, wantSkill string) bool {
	t.Helper()
	params, _ := json.Marshal(struct {
		MessageText string `json:"message_text"`
	}{MessageText: message})
	result, err := refresolve.HandleParseContext(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleParseContext: %v", err)
	}
	for _, ref := range result.References {
		if ref.Shape != refresolve.ShapeSkillTrigger {
			continue
		}
		for _, c := range ref.TopCandidates {
			if c.ID == wantSkill {
				return true
			}
		}
	}
	return false
}
