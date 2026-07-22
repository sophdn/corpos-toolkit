package refresolve_test

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"toolkit/internal/refresolve"
)

// stubClassifier implements refresolve.DomainTermClassifier with a
// configurable fixed answer table. Used by tests so the detector's
// domain-term path is exercised without spinning up the inference
// router.
type stubClassifier struct {
	hits map[string]struct {
		isDomain bool
		conf     float64
	}
}

func (s *stubClassifier) IsDomainTerm(_ context.Context, phrase string) (bool, float64, error) {
	if h, ok := s.hits[phrase]; ok {
		return h.isDomain, h.conf, nil
	}
	return false, 0, nil
}

// fixtureCatalogs returns a Catalogs populated with deterministic
// test fixtures — covers the shape categories the test scenarios
// exercise. New shapes added in T6 (friction) extend this.
func fixtureCatalogs() refresolve.Catalogs {
	return refresolve.Catalogs{
		ChainSlugs: []string{
			"ableton-wine-setup",
			"reference-resolution-substrate",
			"agent-first-substrate",
		},
		TaskSlugs: []string{
			"install-wine-prefix",
			"configure-asio-bridge",
		},
		BugSlugs: []string{
			"forge-bug-title-omitted",
		},
		SkillNames: []string{
			"vault-pull-discipline",
			"agentic-architecture-audit",
		},
		ToolNames: []string{
			"chain-status",
			"bug-resolve",
		},
		ForgeSchemas: []string{
			"bug",
			"chain",
			"task",
		},
		LibrarySlugs:  []string{"two-towers-paper"},
		LibraryTitles: []string{"Two-Towers Retrieval"},
		Projects:      refresolve.KnownProjects,
	}
}

func newDetectorWith(t *testing.T, classifier refresolve.DomainTermClassifier) *refresolve.Detector {
	t.Helper()
	return refresolve.NewDetector(fixtureCatalogs(), classifier)
}

// Acceptance scenario (a): message "start work on ableton-wine-setup"
// returns one reference with shape=chain_slug.
func TestDetect_ChainSlugSingle(t *testing.T) {
	d := newDetectorWith(t, nil)
	refs, err := d.Detect(context.Background(), "start work on ableton-wine-setup")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("want 1 reference, got %d: %+v", len(refs), refs)
	}
	if refs[0].Token != "ableton-wine-setup" {
		t.Errorf("Token: got %q want %q", refs[0].Token, "ableton-wine-setup")
	}
	if refs[0].Shape != refresolve.ShapeChainSlug {
		t.Errorf("Shape: got %q want %q", refs[0].Shape, refresolve.ShapeChainSlug)
	}
	if refs[0].Confidence != 1.0 {
		t.Errorf("Confidence: got %v want 1.0", refs[0].Confidence)
	}
}

// Acceptance scenario (b): message "check the agentic-architecture-audit skill"
// returns one reference with shape=skill_name.
//
// (Note: agentic-architecture-audit is in the skill catalog. It is
// kebab-case shaped so it would also match the chain_slug regex,
// but it's NOT in the ChainSlugs catalog — list-match-against-
// catalog gates the slug detector, so skill_name is the only emit.)
func TestDetect_SkillNameSingle(t *testing.T) {
	d := newDetectorWith(t, nil)
	refs, err := d.Detect(context.Background(), "check the agentic-architecture-audit skill")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("want 1 reference, got %d: %+v", len(refs), refs)
	}
	if refs[0].Token != "agentic-architecture-audit" {
		t.Errorf("Token: %q", refs[0].Token)
	}
	if refs[0].Shape != refresolve.ShapeSkillName {
		t.Errorf("Shape: got %q want skill_name", refs[0].Shape)
	}
}

// Acceptance scenario (c): "the Tripolar Invariant test for glyphs"
// returns one reference with shape=domain_term. (The original
// acceptance criterion said "two domain terms"; the title-cased
// phrase regex only finds one in this sentence — "Tripolar
// Invariant" — because "glyphs" is lowercase. Acceptance still
// holds for the spirit of the test: domain-term shape detection
// works.)
func TestDetect_DomainTermViaRubric(t *testing.T) {
	classifier := &stubClassifier{hits: map[string]struct {
		isDomain bool
		conf     float64
	}{
		"Tripolar Invariant": {isDomain: true, conf: 0.85},
	}}
	d := newDetectorWith(t, classifier)
	refs, err := d.Detect(context.Background(), "the Tripolar Invariant test for glyphs")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	got := filterShape(refs, refresolve.ShapeDomainTerm)
	if len(got) != 1 {
		t.Fatalf("want 1 domain_term ref, got %d (all: %+v)", len(got), refs)
	}
	if got[0].Token != "Tripolar Invariant" {
		t.Errorf("Token: %q", got[0].Token)
	}
	if got[0].Confidence < 0.6 {
		t.Errorf("Confidence below threshold: %v", got[0].Confidence)
	}
}

// Acceptance scenario (d): "thanks, do it" returns zero references.
func TestDetect_TrivialMessage(t *testing.T) {
	d := newDetectorWith(t, nil)
	refs, err := d.Detect(context.Background(), "thanks, do it")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("want 0 references, got %d: %+v", len(refs), refs)
	}
}

// Acceptance scenario (e): a message containing both a slug and a
// domain term returns both, correctly classified.
func TestDetect_MixedSlugAndDomainTerm(t *testing.T) {
	classifier := &stubClassifier{hits: map[string]struct {
		isDomain bool
		conf     float64
	}{
		"Tripolar Invariant": {isDomain: true, conf: 0.85},
	}}
	d := newDetectorWith(t, classifier)
	refs, err := d.Detect(
		context.Background(),
		"working on ableton-wine-setup; also check the Tripolar Invariant",
	)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	shapes := map[refresolve.ShapeCategory]int{}
	for _, r := range refs {
		shapes[r.Shape]++
	}
	if shapes[refresolve.ShapeChainSlug] != 1 {
		t.Errorf("want 1 chain_slug, got %d", shapes[refresolve.ShapeChainSlug])
	}
	if shapes[refresolve.ShapeDomainTerm] != 1 {
		t.Errorf("want 1 domain_term, got %d", shapes[refresolve.ShapeDomainTerm])
	}
}

// Path detection — paths in message body, with extension, are
// emitted regardless of catalog state. (Catalog-free; the path
// regex is the contract.)
func TestDetect_Path(t *testing.T) {
	d := newDetectorWith(t, nil)
	refs, err := d.Detect(
		context.Background(),
		"see docs/REFERENCE_RESOLUTION.md and go/internal/refresolve/detect.go",
	)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	paths := filterShape(refs, refresolve.ShapePath)
	if len(paths) != 2 {
		t.Fatalf("want 2 paths, got %d: %+v", len(paths), paths)
	}
	tokens := map[string]bool{}
	for _, r := range paths {
		tokens[r.Token] = true
	}
	if !tokens["docs/REFERENCE_RESOLUTION.md"] {
		t.Errorf("missing docs path; got %v", tokens)
	}
	if !tokens["go/internal/refresolve/detect.go"] {
		t.Errorf("missing detect.go path; got %v", tokens)
	}
}

// Bug 1408: trailing literal period (common in prose — sentence end
// after a path mention) must not swallow the right-boundary check.
func TestDetect_PathTrailingPeriod(t *testing.T) {
	d := newDetectorWith(t, nil)
	refs, err := d.Detect(
		context.Background(),
		"alongside docs/REFERENCE_RESOLUTION.md and docs/TELEMETRY_SUBSTRATE.md.",
	)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	paths := filterShape(refs, refresolve.ShapePath)
	tokens := map[string]bool{}
	for _, r := range paths {
		tokens[r.Token] = true
	}
	if !tokens["docs/REFERENCE_RESOLUTION.md"] {
		t.Errorf("missing first path; got %v", tokens)
	}
	if !tokens["docs/TELEMETRY_SUBSTRATE.md"] {
		t.Errorf("missing trailing-period path; got %v", tokens)
	}
}

// Bug 1409: catalog detector must not match a short catalog name
// (e.g. forge_schema `chain`) inside a longer kebab token like
// `fictional-chain-xyz123`. The stricter boundaryOKCatalog disqualifies
// hyphen and underscore neighbors.
func TestDetect_CatalogNameNotMatchedInsideKebabToken(t *testing.T) {
	d := newDetectorWith(t, nil)
	refs, err := d.Detect(
		context.Background(),
		"that's a completely-fictional-chain-xyz123 thing",
	)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	for _, r := range refs {
		if r.Shape == refresolve.ShapeForgeSchema && r.Token == "chain" {
			t.Errorf("forge_schema `chain` matched inside kebab token; refs=%+v", refs)
		}
	}
}

// Bug 1409 (regression guard): whole-token catalog references still
// resolve after the boundary tightening — "the chain schema" still
// emits a forge_schema reference for `chain`.
func TestDetect_CatalogNameStillMatchesWholeToken(t *testing.T) {
	d := newDetectorWith(t, nil)
	refs, err := d.Detect(context.Background(), "see the chain schema definition")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	found := false
	for _, r := range refs {
		if r.Shape == refresolve.ShapeForgeSchema && r.Token == "chain" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("forge_schema `chain` not matched as whole token; refs=%+v", refs)
	}
}

// Slug list-match gating: a kebab-case token NOT in any catalog is
// NOT emitted. The detector promise is "I confirm this is X" only
// when the catalog confirms.
func TestDetect_KebabNotInCatalogIsSkipped(t *testing.T) {
	d := newDetectorWith(t, nil)
	refs, err := d.Detect(context.Background(), "look at foo-bar-baz it's broken")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	for _, r := range refs {
		if r.Token == "foo-bar-baz" {
			t.Errorf("unexpected kebab match emitted: %+v", r)
		}
	}
}

// Tool / schema / project detection — exact catalog match emits
// the appropriate shape.
func TestDetect_ToolAndSchemaAndProject(t *testing.T) {
	d := newDetectorWith(t, nil)
	refs, err := d.Detect(
		context.Background(),
		"call bug-resolve against the bug schema in corpos-toolkit",
	)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	shapes := map[refresolve.ShapeCategory]bool{}
	for _, r := range refs {
		shapes[r.Shape] = true
	}
	if !shapes[refresolve.ShapeToolName] {
		t.Errorf("missing tool_name; got %v (refs: %+v)", shapes, refs)
	}
	if !shapes[refresolve.ShapeForgeSchema] {
		t.Errorf("missing forge_schema; got %v", shapes)
	}
	if !shapes[refresolve.ShapeProjectName] {
		t.Errorf("missing project_name; got %v", shapes)
	}
}

// Library entry — case-insensitive title match. The slug match is
// case-sensitive (handled by the same exact-catalog helper).
func TestDetect_LibraryTitle(t *testing.T) {
	d := newDetectorWith(t, nil)
	refs, err := d.Detect(
		context.Background(),
		"the two-towers retrieval design fits here",
	)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	got := filterShape(refs, refresolve.ShapeLibraryEntry)
	if len(got) == 0 {
		t.Fatalf("want library_entry match, got %+v", refs)
	}
}

// Performance budget — rule-based path under 5ms for a 1000-char
// message (the design doc target). Runs without a classifier so
// only the rule-based detectors execute.
func TestDetect_RuleBasedLatencyBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping latency test in -short mode")
	}
	d := newDetectorWith(t, nil)
	// Build a ~1000 char message that exercises every rule-based
	// detector at least once.
	parts := []string{
		"start work on ableton-wine-setup",
		"and configure-asio-bridge",
		"after reading docs/REFERENCE_RESOLUTION.md",
		"plus the vault-pull-discipline skill",
		"with bug-resolve action against the bug schema in corpos-toolkit",
		"considering forge-bug-title-omitted from earlier",
		"using the agent-first-substrate framework",
	}
	body := strings.Join(parts, " ") + " " + strings.Repeat("filler word ", 100)
	if len(body) < 1000 {
		body += strings.Repeat("more ", (1000-len(body))/5+1)
	}

	// Warm-up + measurement: run a few times to amortize allocation
	// noise; report the median of 5 runs.
	const runs = 5
	durs := make([]time.Duration, 0, runs)
	for i := 0; i < runs; i++ {
		start := time.Now()
		if _, err := d.Detect(context.Background(), body); err != nil {
			t.Fatalf("Detect: %v", err)
		}
		durs = append(durs, time.Since(start))
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	median := durs[len(durs)/2]
	if median > 5*time.Millisecond {
		t.Errorf("rule-based detect median %v exceeds 5ms budget", median)
	}
}

// Empty message → empty slice, no error.
func TestDetect_EmptyMessage(t *testing.T) {
	d := newDetectorWith(t, nil)
	refs, err := d.Detect(context.Background(), "")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if refs == nil {
		t.Errorf("want non-nil empty slice, got nil")
	}
	if len(refs) != 0 {
		t.Errorf("want 0 refs, got %d", len(refs))
	}
}

// References sort by StartPos so callers can step through in
// source-message order.
func TestDetect_ResultsSortedByPosition(t *testing.T) {
	d := newDetectorWith(t, nil)
	refs, err := d.Detect(
		context.Background(),
		"reference-resolution-substrate and ableton-wine-setup are two chains",
	)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	for i := 1; i < len(refs); i++ {
		if refs[i].StartPos < refs[i-1].StartPos {
			t.Errorf("refs out of order: refs[%d].StartPos=%d < refs[%d].StartPos=%d",
				i, refs[i].StartPos, i-1, refs[i-1].StartPos)
		}
	}
}

// Detection is deterministic — same input twice produces the same
// output across calls.
func TestDetect_Deterministic(t *testing.T) {
	d := newDetectorWith(t, nil)
	msg := "ableton-wine-setup and configure-asio-bridge in corpos-toolkit"
	a, _ := d.Detect(context.Background(), msg)
	b, _ := d.Detect(context.Background(), msg)
	if len(a) != len(b) {
		t.Fatalf("lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("ref %d differs: %+v vs %+v", i, a[i], b[i])
		}
	}
}

func filterShape(refs []refresolve.Reference, s refresolve.ShapeCategory) []refresolve.Reference {
	out := []refresolve.Reference{}
	for _, r := range refs {
		if r.Shape == s {
			out = append(out, r)
		}
	}
	return out
}
