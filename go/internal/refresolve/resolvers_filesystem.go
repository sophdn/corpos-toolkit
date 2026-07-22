package refresolve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// pathResolver verifies the path exists on the filesystem (via
// os.Stat) and returns a Candidate with size + modification time
// metadata. The detector emits Path References for tokens that
// LOOK like file paths (extension regex); the resolver confirms
// they actually exist.
//
// Resolves both absolute paths and repo-relative paths; the
// repoRoot is consulted when the token is not absolute and not
// already a $HOME-relative path.
type pathResolver struct{ repoRoot string }

func (pathResolver) Shape() ShapeCategory   { return ShapePath }
func (pathResolver) Cost() ResolverCostHint { return ResolverCostHint{TypicalMs: 2} }

func (r pathResolver) Resolve(_ context.Context, ref Reference) (HitSet, error) {
	tok := ref.Token
	var candidates []string
	switch {
	case strings.HasPrefix(tok, "/"):
		candidates = []string{tok}
	case strings.HasPrefix(tok, "~/"):
		home, _ := os.UserHomeDir()
		candidates = []string{filepath.Join(home, tok[2:])}
	case strings.HasPrefix(tok, "./"), strings.HasPrefix(tok, "../"):
		if r.repoRoot != "" {
			candidates = []string{filepath.Join(r.repoRoot, tok)}
		} else {
			candidates = []string{tok}
		}
	default:
		// Repo-relative — try repoRoot/tok and fall back to bare tok.
		if r.repoRoot != "" {
			candidates = []string{filepath.Join(r.repoRoot, tok), tok}
		} else {
			candidates = []string{tok}
		}
	}
	for _, p := range candidates {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		return HitSet{Candidates: []Candidate{{
			ID:         p,
			Title:      filepath.Base(p),
			Score:      1.0,
			SourceRef:  "path:" + p,
			DebugNotes: fmt.Sprintf("size=%d mtime=%s isDir=%t", st.Size(), st.ModTime().UTC().Format("2006-01-02T15:04Z"), st.IsDir()),
		}}}, nil
	}
	// All candidate paths failed to stat — no hit, no error
	// (the file may have been renamed or deleted; the detector's
	// regex is shape-only, not existence-checking).
	return HitSet{}, nil
}

// skillResolver returns a Candidate pointing at the skill file in
// skills/. Detector confirmed the catalog match; resolver just
// builds the canonical path.
type skillResolver struct{ repoRoot string }

func (skillResolver) Shape() ShapeCategory   { return ShapeSkillName }
func (skillResolver) Cost() ResolverCostHint { return ResolverCostHint{TypicalMs: 1} }

func (r skillResolver) Resolve(_ context.Context, ref Reference) (HitSet, error) {
	return resolveCatalogFile(r.repoRoot, "skills", ref.Token, ".toml", "skill", "skill")
}

// toolResolver returns a Candidate pointing at the action manifest
// in action-manifests/.
type toolResolver struct{ repoRoot string }

func (toolResolver) Shape() ShapeCategory   { return ShapeToolName }
func (toolResolver) Cost() ResolverCostHint { return ResolverCostHint{TypicalMs: 1} }

func (r toolResolver) Resolve(_ context.Context, ref Reference) (HitSet, error) {
	return resolveCatalogFile(r.repoRoot, "action-manifests", ref.Token, ".toml", "tool", "action-manifest")
}

// schemaResolver returns a Candidate pointing at the forge schema
// in blueprints/forge-schemas/.
type schemaResolver struct{ repoRoot string }

func (schemaResolver) Shape() ShapeCategory   { return ShapeForgeSchema }
func (schemaResolver) Cost() ResolverCostHint { return ResolverCostHint{TypicalMs: 1} }

func (r schemaResolver) Resolve(_ context.Context, ref Reference) (HitSet, error) {
	return resolveCatalogFile(r.repoRoot, filepath.Join("blueprints", "forge-schemas"), ref.Token, ".toml", "schema", "forge-schema")
}

// projectResolver returns a Candidate naming the project's
// well-known checkout path. Projects don't have files this way;
// the SourceRef is "project:<name>" and the DebugNotes is the
// canonical checkout dir if known.
type projectResolver struct{}

func (projectResolver) Shape() ShapeCategory   { return ShapeProjectName }
func (projectResolver) Cost() ResolverCostHint { return ResolverCostHint{TypicalMs: 1} }

func (projectResolver) Resolve(_ context.Context, ref Reference) (HitSet, error) {
	for _, p := range KnownProjects {
		if p == ref.Token {
			return HitSet{Candidates: []Candidate{{
				ID:         ref.Token,
				Title:      ref.Token,
				Score:      1.0,
				SourceRef:  "project:" + ref.Token,
				DebugNotes: fmt.Sprintf("~/dev/%s (conventional)", ref.Token),
			}}}, nil
		}
	}
	return HitSet{}, nil
}

// resolveCatalogFile is the shared resolver for skills, tools, and
// schemas — confirms a file with the expected name exists under
// the named subdir of repoRoot and returns a Candidate pointing at
// it.
func resolveCatalogFile(repoRoot, subdir, name, suffix, sourcePrefix, kindLabel string) (HitSet, error) {
	if repoRoot == "" {
		return HitSet{ConfidenceTier: TierNoHit, Err: errors.New("repo root not configured")}, nil
	}
	rel := filepath.Join(subdir, name+suffix)
	abs := filepath.Join(repoRoot, rel)
	st, err := os.Stat(abs)
	if err != nil {
		return HitSet{}, nil
	}
	return HitSet{Candidates: []Candidate{{
		ID:         name,
		Title:      fmt.Sprintf("%s %s", kindLabel, name),
		Score:      1.0,
		SourceRef:  sourcePrefix + ":" + rel,
		DebugNotes: fmt.Sprintf("path=%s size=%d", abs, st.Size()),
	}}}, nil
}
