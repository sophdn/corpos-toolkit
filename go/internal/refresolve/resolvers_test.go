package refresolve_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/refresolve"
	"toolkit/internal/testutil"
)

// chainResolver / taskResolver / bugResolver are unexported in
// refresolve; tests exercise them indirectly via the
// BuildProductionRegistry path with a real test DB.

func TestProductionRegistry_ChainResolver(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ctx := context.Background()

	// Seed a chain so the resolver has a row to find.
	chainID := seedChainProj(t, pool, "mcp-servers", "test-chain", "open")
	seedTaskProj(t, pool, chainID, "task-one", "pending", 1, "test task")

	registry := refresolve.BuildProductionRegistry(refresolve.ProductionDeps{Pool: pool})
	res, ok := registry.Get(refresolve.ShapeChainSlug)
	if !ok {
		t.Fatalf("chain resolver not registered")
	}
	ref := refresolve.Reference{Token: "test-chain", Shape: refresolve.ShapeChainSlug, Confidence: 1.0}
	hs, err := res.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(hs.Candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(hs.Candidates))
	}
	c := hs.Candidates[0]
	if c.SourceRef != "chain:test-chain" {
		t.Errorf("SourceRef: %q", c.SourceRef)
	}
	if c.Score != 1.0 {
		t.Errorf("Score: %v", c.Score)
	}
}

func TestProductionRegistry_TaskResolver(t *testing.T) {
	pool := testutil.NewTestDB(t)
	chainAID := seedChainProj(t, pool, "mcp-servers", "chainA", "open")
	seedTaskProj(t, pool, chainAID, "taskA", "pending", 1, "A")

	registry := refresolve.BuildProductionRegistry(refresolve.ProductionDeps{Pool: pool})
	res, ok := registry.Get(refresolve.ShapeTaskSlug)
	if !ok {
		t.Fatalf("task resolver not registered")
	}
	ref := refresolve.Reference{Token: "taskA", Shape: refresolve.ShapeTaskSlug, Confidence: 1.0}
	hs, err := res.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(hs.Candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d (%+v)", len(hs.Candidates), hs)
	}
	if hs.Candidates[0].SourceRef != "task:chainA/taskA" {
		t.Errorf("SourceRef: %q", hs.Candidates[0].SourceRef)
	}
	// DebugNotes must surface BOTH the task's own status AND the
	// parent chain's status so the agent can scan-check whether the
	// chain is still active without a second round-trip (bug
	// task-chain-bug-state-not-glanceable-from-id-or-slug-in-conversation-prose).
	notes := hs.Candidates[0].DebugNotes
	for _, want := range []string{"status=pending", "position=1", "chain=chainA:open"} {
		if !contains(notes, want) {
			t.Errorf("DebugNotes %q missing %q", notes, want)
		}
	}
}

func TestProductionRegistry_TaskResolver_SurfacesClosedChainStatus(t *testing.T) {
	pool := testutil.NewTestDB(t)
	chainClosedID := seedChainProj(t, pool, "mcp-servers", "chainClosed", "closed")
	seedTaskProj(t, pool, chainClosedID, "taskInClosedChain", "closed", 3, "done")

	registry := refresolve.BuildProductionRegistry(refresolve.ProductionDeps{Pool: pool})
	res, _ := registry.Get(refresolve.ShapeTaskSlug)
	ref := refresolve.Reference{Token: "taskInClosedChain", Shape: refresolve.ShapeTaskSlug, Confidence: 1.0}
	hs, err := res.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	notes := hs.Candidates[0].DebugNotes
	if !contains(notes, "chain=chainClosed:closed") {
		t.Errorf("expected closed chain status in DebugNotes; got %q", notes)
	}
}

func TestProductionRegistry_BugResolver(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedBugProj(t, pool, "mcp-servers", "paper-cut", "tiny issue", "observed once", "open", "medium", "", "", "", "")

	registry := refresolve.BuildProductionRegistry(refresolve.ProductionDeps{Pool: pool})
	res, ok := registry.Get(refresolve.ShapeBugSlug)
	if !ok {
		t.Fatalf("bug resolver not registered")
	}
	ref := refresolve.Reference{Token: "paper-cut", Shape: refresolve.ShapeBugSlug, Confidence: 1.0}
	hs, err := res.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(hs.Candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(hs.Candidates))
	}
	// Open bug: compact form, no resolution detail.
	notes := hs.Candidates[0].DebugNotes
	if notes != "status=open severity=medium" {
		t.Errorf("open bug DebugNotes should be compact; got %q", notes)
	}
}

func TestProductionRegistry_BugResolver_SurfacesFixedBugCommit(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedBugProj(t, pool, "mcp-servers", "fixed-bug", "was a bug", "observed", "fixed", "medium", "fixed", "", "", "abc1234def5678")
	registry := refresolve.BuildProductionRegistry(refresolve.ProductionDeps{Pool: pool})
	res, _ := registry.Get(refresolve.ShapeBugSlug)
	ref := refresolve.Reference{Token: "fixed-bug", Shape: refresolve.ShapeBugSlug, Confidence: 1.0}
	hs, _ := res.Resolve(context.Background(), ref)
	notes := hs.Candidates[0].DebugNotes
	for _, want := range []string{"status=fixed", "kind=fixed", "sha=abc1234"} {
		if !contains(notes, want) {
			t.Errorf("DebugNotes %q missing %q", notes, want)
		}
	}
}

func TestProductionRegistry_BugResolver_SurfacesRoutedTarget(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedBugProj(t, pool, "mcp-servers", "routed-bug", "needs a chain", "observed", "routed", "medium", "routed", "some-chain", "some-task", "")
	registry := refresolve.BuildProductionRegistry(refresolve.ProductionDeps{Pool: pool})
	res, _ := registry.Get(refresolve.ShapeBugSlug)
	ref := refresolve.Reference{Token: "routed-bug", Shape: refresolve.ShapeBugSlug, Confidence: 1.0}
	hs, _ := res.Resolve(context.Background(), ref)
	notes := hs.Candidates[0].DebugNotes
	for _, want := range []string{"status=routed", "kind=routed", "routed_chain=some-chain/some-task"} {
		if !contains(notes, want) {
			t.Errorf("DebugNotes %q missing %q", notes, want)
		}
	}
}

// contains is a tiny test helper to keep DebugNotes assertions
// readable without pulling in strings.Contains inline at each call.
func contains(s, sub string) bool { return len(s) >= len(sub) && len(sub) > 0 && indexOf(s, sub) >= 0 }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Path resolver — stat-based; works without DB or repo-root.
func TestProductionRegistry_PathResolver(t *testing.T) {
	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "test.md")
	if err := os.WriteFile(target, []byte("hi"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	registry := refresolve.BuildProductionRegistry(refresolve.ProductionDeps{RepoRoot: tmpDir})
	res, ok := registry.Get(refresolve.ShapePath)
	if !ok {
		t.Fatalf("path resolver not registered")
	}
	ref := refresolve.Reference{Token: "test.md", Shape: refresolve.ShapePath, Confidence: 1.0}
	hs, err := res.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(hs.Candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(hs.Candidates))
	}
}

// Project resolver — closed list match.
func TestProductionRegistry_ProjectResolver(t *testing.T) {
	registry := refresolve.BuildProductionRegistry(refresolve.ProductionDeps{})
	res, ok := registry.Get(refresolve.ShapeProjectName)
	if !ok {
		t.Fatalf("project resolver not registered")
	}
	ref := refresolve.Reference{Token: "corpos-toolkit", Shape: refresolve.ShapeProjectName, Confidence: 1.0}
	hs, err := res.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(hs.Candidates) != 1 {
		t.Errorf("want 1 candidate, got %d", len(hs.Candidates))
	}
}

// Partial dependency — only Pool wired; the production registry
// installs DB-backed resolvers and projectResolver, but skips
// filesystem and knowledge resolvers.
func TestBuildProductionRegistry_PartialDeps(t *testing.T) {
	pool := testutil.NewTestDB(t)
	registry := refresolve.BuildProductionRegistry(refresolve.ProductionDeps{Pool: pool})
	shapes := registry.Shapes()
	wantShapes := map[refresolve.ShapeCategory]bool{
		refresolve.ShapeChainSlug:     true,
		refresolve.ShapeTaskSlug:      true,
		refresolve.ShapeBugSlug:       true,
		refresolve.ShapeLibraryEntry:  true,
		refresolve.ShapeProjectName:   true,
		refresolve.ShapeFrictionShape: true,
		// chain 435: ecosystem_token resolver is Pool-backed (the local-
		// ecosystem store), so it registers with only Pool wired.
		refresolve.ShapeEcosystemToken: true,
		// canon_resolve: canon_token resolver is Pool-backed too.
		refresolve.ShapeCanonToken: true,
		// reference-resolution-migration T5 Phase 5: memory_entry is
		// shell-registered unconditionally (returns TierNoHit until
		// T10 wires the MEMORY.md lookup).
		refresolve.ShapeMemoryEntry: true,
	}
	for _, s := range shapes {
		if !wantShapes[s] {
			t.Errorf("unexpected shape registered without RepoRoot/Knowledge: %s", s)
		}
		delete(wantShapes, s)
	}
	for s := range wantShapes {
		t.Errorf("missing expected shape: %s", s)
	}
}

func mustExec(t *testing.T, pool *db.Pool, q string, args ...any) {
	t.Helper()
	if _, err := pool.DB().ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// seedChainProj wraps [testutil.SeedChain] returning the chain id so
// downstream task seeds can link via chain_id. The wrapper survives
// because the typed return is the package-specific value-add.
func seedChainProj(t *testing.T, pool *db.Pool, project, slug, status string) int64 {
	t.Helper()
	return testutil.SeedChain(t, pool, project, slug, status, testutil.SeedChainOpts{})
}

// seedTaskProj wraps [testutil.SeedTask]. refresolve tests don't
// refresh parent-chain counters (no assertion reads them), so this
// wrapper deliberately drops the refresh step that observehttp's
// equivalent includes.
func seedTaskProj(t *testing.T, pool *db.Pool, chainID int64, slug, status string, position int64, problemStatement string) {
	t.Helper()
	testutil.SeedTask(t, pool, chainID, slug, status, testutil.SeedTaskOpts{
		Position:         position,
		ProblemStatement: problemStatement,
	})
}

// seedBugProj wraps [testutil.SeedBug] with the long-form positional
// signature this package's tests had converged on. Routed-chain /
// routed-task / resolved-commit-sha pass through verbatim; the helper
// in testutil/ handles the empty-string→NULL distinction the
// bugResolver expects.
func seedBugProj(t *testing.T, pool *db.Pool, project, slug, title, problem, status, severity, resolutionKind, routedChain, routedTask, resolvedSHA string) {
	t.Helper()
	testutil.SeedBug(t, pool, project, slug, status, testutil.SeedBugOpts{
		Title:             title,
		ProblemStatement:  problem,
		Severity:          severity,
		ResolutionKind:    resolutionKind,
		RoutedChainSlug:   routedChain,
		RoutedTaskSlug:    routedTask,
		ResolvedCommitSHA: resolvedSHA,
	})
}
