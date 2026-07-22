package refresolve

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"toolkit/internal/db"
	"toolkit/internal/ecosystem"
)

// KnownProjects is the closed list of projects the detector
// recognizes for ShapeProjectName. New projects extend this list.
//
// Kept as a package-level var (not const) so tests can override
// without re-declaring; production callers should treat it as
// read-only.
//
// Freshened 2026-07-13 from admin.project_list ground truth (canon_resolve
// follow-on). EXCLUDES: retired names — `mcp-servers` (-> corpos-toolkit) and
// `dashboard` (-> corpos-toolkit-dashboard); resolve those via
// ecosystem.canon_resolve. Also excludes projects whose checkout is NOT
// ~/dev/<id> — `glyph-research` (~/dev/corpos-lab), `portfolio`
// (~/dev/sophdn.github.io) — since projectResolver claims the conventional
// ~/dev/<name> path, and the archived `lab-app`.
var KnownProjects = []string{
	"campaign-settings",
	"corpos-toolkit",
	"corpos-toolkit-dashboard",
	"dm-toolkit",
	"godot-rpg-engine-kit",
	"memory-courier",
	"seed-packet",
	"self-compile",
	"spirit-call",
	"voice-trainer",
	"worldkeep",
}

// LoadCatalogs reads the on-disk catalogs (skills/, action-manifests/,
// blueprints/forge-schemas/) plus the slug + library tables from the
// DB pool, and returns a Catalogs struct ready to hand to
// NewDetector.
//
// repoRoot points at the toolkit-server checkout (the directory
// containing skills/, action-manifests/, blueprints/, etc.). Tests
// can pass a temp dir with synthetic catalogs.
//
// pool may be nil; in that case the DB-backed catalogs (chains,
// tasks, bugs, library entries) are left empty. Used by tests that
// only need filesystem catalogs.
func LoadCatalogs(ctx context.Context, repoRoot string, pool *db.Pool, memoryDir string) (Catalogs, error) {
	var cat Catalogs
	cat.Projects = append(cat.Projects, KnownProjects...)

	if repoRoot != "" {
		var err error
		cat.SkillNames, err = listTOMLBasenames(filepath.Join(repoRoot, "skills"))
		if err != nil {
			return cat, fmt.Errorf("load skills catalog: %w", err)
		}
		cat.ToolNames, err = listTOMLBasenames(filepath.Join(repoRoot, "action-manifests"))
		if err != nil {
			return cat, fmt.Errorf("load action-manifests catalog: %w", err)
		}
		cat.ForgeSchemas, err = listTOMLBasenames(filepath.Join(repoRoot, "blueprints", "forge-schemas"))
		if err != nil {
			return cat, fmt.Errorf("load forge-schemas catalog: %w", err)
		}
		// reference-resolution-migration T5: skill manifest powers the
		// skill_trigger + discipline_skill resolvers. Absent manifest is
		// allowed (early-stage clones / sandbox setups) — handlers degrade.
		manifest, err := LoadSkillManifest(repoRoot)
		if err != nil {
			return cat, fmt.Errorf("load skill manifest: %w", err)
		}
		cat.SkillManifest = manifest
		cat.SkillTriggers = manifest.TriggerKeywords()
	}

	// reference-resolution-migration T10: auto-memory index for the
	// memory_entry resolver. MemoryDir is typically
	// ~/.claude/projects/<cwd-slug>/memory/; absent dir is OK
	// (resolver degrades to TierNoHit).
	if memoryDir != "" {
		memIndex, err := LoadMemoryIndex(memoryDir)
		if err != nil {
			return cat, fmt.Errorf("load memory index: %w", err)
		}
		cat.MemoryIndex = memIndex
		cat.MemoryTokens = memIndex.Tokens()
	}

	if pool != nil {
		var err error
		cat.ChainSlugs, err = querySlugs(ctx, pool.DB(), "SELECT slug FROM proj_chain_status")
		if err != nil {
			return cat, fmt.Errorf("load chain slugs: %w", err)
		}
		cat.TaskSlugs, err = querySlugs(ctx, pool.DB(), "SELECT slug FROM proj_current_tasks")
		if err != nil {
			return cat, fmt.Errorf("load task slugs: %w", err)
		}
		cat.BugSlugs, err = querySlugs(ctx, pool.DB(), "SELECT slug FROM proj_current_bugs")
		if err != nil {
			return cat, fmt.Errorf("load bug slugs: %w", err)
		}
		cat.LibrarySlugs, cat.LibraryTitles, err = queryLibrarySlugsAndTitles(ctx, pool.DB())
		if err != nil {
			return cat, fmt.Errorf("load library entries: %w", err)
		}
		// chain 435: host / service / address tokens for detectEcosystemToken.
		cat.EcosystemTokens, err = ecosystem.AllTokens(ctx, pool.DB())
		if err != nil {
			return cat, fmt.Errorf("load ecosystem tokens: %w", err)
		}
		// canon_resolve: canonical names / retired aliases / old paths+ports.
		cat.CanonTokens, err = ecosystem.CanonTokens(ctx, pool.DB())
		if err != nil {
			return cat, fmt.Errorf("load canon tokens: %w", err)
		}
	}

	// Deterministic, deduplicated order for every catalog slice.
	cat.ChainSlugs = loadCatalogsSorted(cat.ChainSlugs)
	cat.TaskSlugs = loadCatalogsSorted(cat.TaskSlugs)
	cat.BugSlugs = loadCatalogsSorted(cat.BugSlugs)
	cat.SkillNames = loadCatalogsSorted(cat.SkillNames)
	cat.ToolNames = loadCatalogsSorted(cat.ToolNames)
	cat.ForgeSchemas = loadCatalogsSorted(cat.ForgeSchemas)
	cat.LibrarySlugs = loadCatalogsSorted(cat.LibrarySlugs)
	cat.LibraryTitles = loadCatalogsSorted(cat.LibraryTitles)
	cat.Projects = loadCatalogsSorted(cat.Projects)
	cat.EcosystemTokens = loadCatalogsSorted(cat.EcosystemTokens)
	cat.CanonTokens = loadCatalogsSorted(cat.CanonTokens)

	return cat, nil
}

func listTOMLBasenames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := []string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".toml") {
			continue
		}
		base := strings.TrimSuffix(name, ".toml")
		out = append(out, base)
	}
	return out, nil
}

func querySlugs(ctx context.Context, h *sql.DB, query string) ([]string, error) {
	rows, err := h.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, err
		}
		if slug != "" {
			out = append(out, slug)
		}
	}
	return out, rows.Err()
}

// queryLibrarySlugsAndTitles returns the library catalog's lookup
// columns. The library_entries schema has no separate `slug` or
// `title` columns; the dewey number IS the slug, and the
// `establishes` short prose is the closest match-by-title surface.
//
// We populate Catalogs.LibrarySlugs from dewey only and leave
// Catalogs.LibraryTitles empty — case-insensitive substring match
// against `establishes` prose has too high a false-positive rate
// for whole-message detection. T7's trained reranker is the right
// path for library-title-style fuzzy matching.
func queryLibrarySlugsAndTitles(ctx context.Context, h *sql.DB) (slugs, titles []string, err error) {
	rows, err := h.QueryContext(ctx, "SELECT dewey FROM library_entries")
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var dewey string
		if err := rows.Scan(&dewey); err != nil {
			return nil, nil, err
		}
		if dewey != "" {
			slugs = append(slugs, dewey)
		}
	}
	return slugs, nil, rows.Err()
}
