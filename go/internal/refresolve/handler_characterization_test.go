package refresolve_test

// Characterization net additions for chain
// refactor-handler-parse-context-core STEP 2 (the precondition gate).
//
// These tests close the input-equivalence-class gaps the step-1
// inventory + a coverage pass surfaced in handleParseContextCore that
// the pre-existing ~13-file suite did not already pin. They pin CURRENT
// behavior exactly — they are NOT a request to change anything. The
// step-6 parity audit checks the refactor against these goldens.
//
// Gap classes pinned here (see the chain scratchpad's gap matrix):
//   - (e) in-band-error envelope: the param-unmarshal + LoadCatalogs
//     stage failures return {Error:...}, nil (never a Go error). These
//     are the repeated idiom step-1 flagged as a prime refactor target;
//     before this file only the nil-registry gate was pinned.
//   - (a) TopKPerShape<=0 substitutes the default of 5.
//   - (b) the fully-degraded deps matrix (nil Pool / Cache / trackers /
//     Classifier / KiwixFallbackSearch) is nil-safe through every phase.
//   - (d) the surfacing passes append AFTER token resolution (the fixed
//     compose order the refactor must preserve).
//
// NOTE on the two error gates NOT pinned here (Detect, Dispatch): they
// are unreachable through the public API in the current implementation
// — Detect never returns a non-nil error, and Dispatch only errors on a
// nil registry which handleParseContextCore guards before calling it.
// They are logged as dead-defensive-code findings for the step-3 audit
// (Q4), not force-covered.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"toolkit/internal/events"
	"toolkit/internal/refresolve"
	"toolkit/internal/testutil"
)

// (e) In-band-error contract — param unmarshal. A params blob whose
// message_text is the wrong JSON type fails json.Unmarshal; the handler
// must return the failure IN-BAND ({Error: "parse params: ..."}, nil),
// never as a Go error. Registry is non-nil so the earlier nil-registry
// gate doesn't short-circuit first.
func TestCharacterization_ParamUnmarshalFailureIsInBandError(t *testing.T) {
	deps := refresolve.HandlerDeps{Registry: refresolve.NewRegistry()}
	bad := json.RawMessage(`{"message_text": 12345}`) // number into a string field
	result, err := refresolve.HandleParseContext(context.Background(), deps, bad)
	if err != nil {
		t.Fatalf("in-band-error contract: want nil Go error, got %v", err)
	}
	if !strings.HasPrefix(result.Error, "parse params:") {
		t.Errorf("want Error prefixed %q, got %q", "parse params:", result.Error)
	}
	if len(result.References) != 0 {
		t.Errorf("want no references on param-parse failure, got %d", len(result.References))
	}
}

// (e) In-band-error contract — LoadCatalogs. A RepoRoot whose `skills`
// entry is a regular FILE (not a directory) makes os.ReadDir return a
// non-ENOENT error, so LoadCatalogs fails at the skills-catalog step.
// The handler must surface it in-band as {Error: "load catalogs: ..."}.
// (A *missing* skills dir is tolerated — listTOMLBasenames swallows
// ENOENT — so the fixture must be a file, not an absent path.)
func TestCharacterization_LoadCatalogsFailureIsInBandError(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, "skills"), []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	deps := refresolve.HandlerDeps{Registry: refresolve.NewRegistry(), RepoRoot: repoRoot}
	// Non-empty message so the empty-message early return doesn't fire
	// before LoadCatalogs.
	params := mustMarshalParams(t, resolveRefsParams{MessageText: "look at order-chain"})
	result, err := refresolve.HandleParseContext(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("in-band-error contract: want nil Go error, got %v", err)
	}
	if !strings.HasPrefix(result.Error, "load catalogs:") {
		t.Errorf("want Error prefixed %q, got %q", "load catalogs:", result.Error)
	}
}

// (a) TopKPerShape<=0 substitutes the default of 5. A resolver returns 7
// candidates for a path token; with top_k unset the handler caps the
// surfaced candidates at 5, and with an explicit top_k it caps at that.
// ShapePath + 7 candidates classifies TierFuzzyMulti (not single_exact),
// so the ref lands in References with the trimmed candidate slice.
func TestCharacterization_TopKPerShapeDefaultsToFiveWhenUnset(t *testing.T) {
	cands := make([]refresolve.Candidate, 7)
	for i := range cands {
		cands[i] = refresolve.Candidate{
			ID: fmt.Sprintf("cand-%d", i), Title: "detect.go", Score: 0.6,
			SourceRef: "path:go/internal/refresolve/detect.go",
		}
	}
	newDeps := func() refresolve.HandlerDeps {
		registry := refresolve.NewRegistry()
		registry.Register(stubResolver{shape: refresolve.ShapePath, hit: refresolve.HitSet{Candidates: cands}})
		return refresolve.HandlerDeps{Registry: registry}
	}
	pathOf := func(t *testing.T, r refresolve.ResolveReferencesResult) refresolve.ResolvedReference {
		t.Helper()
		for _, ref := range r.References {
			if ref.Shape == refresolve.ShapePath {
				return ref
			}
		}
		t.Fatalf("no path reference in envelope: %+v", r.References)
		return refresolve.ResolvedReference{}
	}

	// top_k unset (0) → default 5.
	params := mustMarshalParams(t, resolveRefsParams{MessageText: "review go/internal/refresolve/detect.go"})
	result, err := refresolve.HandleParseContext(context.Background(), newDeps(), params)
	if err != nil {
		t.Fatalf("HandleParseContext: %v", err)
	}
	if got := len(pathOf(t, result).TopCandidates); got != 5 {
		t.Errorf("top_k unset: want 5 candidates (default), got %d", got)
	}

	// explicit top_k=2 → 2.
	params2 := mustMarshalParams(t, resolveRefsParams{
		MessageText:  "review go/internal/refresolve/detect.go",
		TopKPerShape: 2,
	})
	result2, err := refresolve.HandleParseContext(context.Background(), newDeps(), params2)
	if err != nil {
		t.Fatalf("HandleParseContext: %v", err)
	}
	if got := len(pathOf(t, result2).TopCandidates); got != 2 {
		t.Errorf("top_k=2: want 2 candidates, got %d", got)
	}
}

// (b) Fully-degraded deps. Every optional dependency is nil/empty:
// Pool, Cache, Classifier, DriftFireTracker, WorkStateCache,
// DisciplineFireTracker, KiwixFallbackSearch, MemoryDir, RepoRoot. Only
// Registry is required. A verify-intent message drives every surfacing
// pass (work-state, drift, disciplines, kiwix) so the test pins that all
// of them — and the grounding-event emit — are nil-safe simultaneously.
// The refactor extracts these phases into named units and must preserve
// the guard behavior.
func TestCharacterization_FullyDegradedNilDepsDoesNotPanic(t *testing.T) {
	deps := refresolve.HandlerDeps{Registry: refresolve.NewRegistry()} // all else zero/nil
	params := mustMarshalParams(t, resolveRefsParams{
		MessageText: "verify go/internal/refresolve/detect.go behaves as expected",
	})
	result, err := refresolve.HandleParseContext(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("degraded deps: want nil Go error, got %v", err)
	}
	if result.Error != "" {
		t.Errorf("degraded deps: want empty Error, got %q", result.Error)
	}
	// Intent detection runs unconditionally (no dep), so the envelope's
	// intent field is always populated even with everything else nil.
	if result.Intent == nil {
		t.Errorf("Intent should be populated regardless of deps")
	}
}

// (d) Compose order: the surfacing passes append AFTER token resolution.
// A single-exact path token resolves as a token reference; the verify
// intent fires work-state surfacing (a seeded open bug). The token ref
// must appear in References BEFORE the work-state surface — pinning the
// fixed order (token loop, then the appended surfacing passes) the
// refactor must preserve. Work-state surfaces are distinguishable by the
// "[work-state surface]" PresentedAs prefix.
func TestCharacterization_SurfacingPassesAppendAfterTokenResolution(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedBugProjWS(t, pool, "mcp-servers", "order-bug", "title", "high", "open", time.Now())

	registry := refresolve.NewRegistry()
	registry.Register(stubResolver{
		shape: refresolve.ShapePath,
		hit: refresolve.HitSet{Candidates: []refresolve.Candidate{{
			ID: "p", Title: "detect.go", Score: 1.0,
			SourceRef: "path:go/internal/refresolve/detect.go",
		}}},
	})
	deps := refresolve.HandlerDeps{
		Pool:           pool,
		Project:        "mcp-servers",
		Registry:       registry,
		WorkStateCache: refresolve.NewWorkStateCache(),
	}
	ctx := events.WithMCPSessionID(context.Background(), "ordering-e2e")
	params := mustMarshalParams(t, resolveRefsParams{
		MessageText: "verify go/internal/refresolve/detect.go works",
	})
	result, err := refresolve.HandleParseContext(ctx, deps, params)
	if err != nil {
		t.Fatalf("HandleParseContext: %v", err)
	}

	tokenIdx, workStateIdx := -1, -1
	for i, ref := range result.References {
		isWorkState := strings.HasPrefix(ref.PresentedAs, "[work-state surface]")
		switch {
		case ref.Shape == refresolve.ShapePath && !isWorkState && tokenIdx == -1:
			tokenIdx = i
		case isWorkState && workStateIdx == -1:
			workStateIdx = i
		}
	}
	if tokenIdx == -1 {
		t.Fatalf("expected a path token reference; got %+v", result.References)
	}
	if workStateIdx == -1 {
		t.Fatalf("expected a work-state surface on verify intent; got %+v", result.References)
	}
	if tokenIdx >= workStateIdx {
		t.Errorf("token reference (idx %d) must precede work-state surface (idx %d) — surfacing passes append after token resolution",
			tokenIdx, workStateIdx)
	}
}
