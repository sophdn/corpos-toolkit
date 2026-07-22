package refresolve

// Internal tests for the body inliner — exercise the unexported
// applyBodyInlining + helpers directly so the budget / tier / precedence
// logic can be verified without standing up a full handler stack.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tempSkillsRoot creates a fake repo root with one skill body per
// (name, bytes) pair. Returns the root path; cleanup via t.TempDir.
func tempSkillsRoot(t *testing.T, skills map[string]int) string {
	t.Helper()
	root := t.TempDir()
	for name, size := range skills {
		dir := filepath.Join(root, "skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		body := strings.Repeat("x", size)
		bodyPath := filepath.Join(dir, "SKILL.md")
		if err := os.WriteFile(bodyPath, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", bodyPath, err)
		}
	}
	return root
}

// stubRef builds a ResolvedReference shaped like what skillTriggerResolver
// produces: ShapeSkillTrigger, TierSingleExact, PresentUseDirectly, with
// a single Candidate whose SourceRef points at the manifest body_path.
func stubRef(token, bodyPath string) ResolvedReference {
	return ResolvedReference{
		Token:             token,
		Shape:             ShapeSkillTrigger,
		ConfidenceTier:    TierSingleExact,
		RecommendedAction: PresentUseDirectly,
		TopCandidates: []Candidate{{
			ID:        token,
			SourceRef: "skill:" + bodyPath,
		}},
	}
}

func TestApplyBodyInlining_DisabledLeavesEnvelopeUntouched(t *testing.T) {
	root := tempSkillsRoot(t, map[string]int{"small": 100})
	refs := []ResolvedReference{stubRef("small", "skills/small")}
	opts := inlineBodyOpts{
		enabled:        false, // ← the toggle under test
		envelopeBudget: DefaultInlineBudgetBytes,
		repoRoot:       root,
		cache:          NewBodyCache(),
	}
	bytes, count := applyBodyInlining(refs, opts)
	if bytes != 0 || count != 0 {
		t.Errorf("disabled: want (0, 0), got (%d, %d)", bytes, count)
	}
	if refs[0].BodyInlined != "" || refs[0].BodySummary != "" || refs[0].BodyBytes != 0 {
		t.Errorf("disabled: ref should be untouched, got %+v", refs[0])
	}
}

func TestApplyBodyInlining_InlineCleanTier(t *testing.T) {
	root := tempSkillsRoot(t, map[string]int{"tiny": 500})
	refs := []ResolvedReference{stubRef("tiny", "skills/tiny")}
	opts := inlineBodyOpts{
		enabled:        true,
		envelopeBudget: DefaultInlineBudgetBytes,
		repoRoot:       root,
		cache:          NewBodyCache(),
	}
	bytes, count := applyBodyInlining(refs, opts)
	if count != 1 {
		t.Fatalf("count: %d, want 1", count)
	}
	if bytes != 500 {
		t.Errorf("bytes: %d, want 500", bytes)
	}
	r := refs[0]
	if len(r.BodyInlined) != 500 {
		t.Errorf("BodyInlined len: %d, want 500", len(r.BodyInlined))
	}
	if r.BodyTruncated {
		t.Errorf("BodyTruncated: should be false for clean-tier")
	}
	if r.BodyBytes != 500 {
		t.Errorf("BodyBytes: %d, want 500", r.BodyBytes)
	}
	if r.BodySummary != "" {
		t.Errorf("BodySummary: should be empty when BodyInlined populated, got %q", r.BodySummary[:50])
	}
}

func TestApplyBodyInlining_InlineTruncateTier_FitsUnderCap(t *testing.T) {
	// 4 KB is in inline-truncate tier (2-8 KB) and under per-skill cap (8 KB).
	root := tempSkillsRoot(t, map[string]int{"medium": 4096})
	refs := []ResolvedReference{stubRef("medium", "skills/medium")}
	opts := inlineBodyOpts{
		enabled:        true,
		envelopeBudget: DefaultInlineBudgetBytes,
		repoRoot:       root,
		cache:          NewBodyCache(),
	}
	applyBodyInlining(refs, opts)
	r := refs[0]
	if len(r.BodyInlined) != 4096 {
		t.Errorf("BodyInlined len: %d, want 4096 (whole-body inline)", len(r.BodyInlined))
	}
	if r.BodyTruncated {
		t.Errorf("BodyTruncated: should be false when whole body fits")
	}
}

func TestApplyBodyInlining_PointerOnlyTier_EmitsSummaryNotInline(t *testing.T) {
	// 10 KB is in pointer-only tier (>8 KB).
	root := tempSkillsRoot(t, map[string]int{"large": 10240})
	refs := []ResolvedReference{stubRef("large", "skills/large")}
	opts := inlineBodyOpts{
		enabled:        true,
		envelopeBudget: DefaultInlineBudgetBytes,
		repoRoot:       root,
		cache:          NewBodyCache(),
	}
	applyBodyInlining(refs, opts)
	r := refs[0]
	if r.BodyInlined != "" {
		t.Errorf("BodyInlined: should be empty for pointer-only tier, got %d bytes", len(r.BodyInlined))
	}
	if len(r.BodySummary) != BodySummaryBytes {
		t.Errorf("BodySummary len: %d, want %d", len(r.BodySummary), BodySummaryBytes)
	}
	if r.BodyBytes != 10240 {
		t.Errorf("BodyBytes: %d, want 10240", r.BodyBytes)
	}
}

func TestApplyBodyInlining_EnvelopeBudgetExceeded_DemotesLaterRefs(t *testing.T) {
	// Three 4 KB bodies; envelope budget 10 KB. With per-skill cap 8 KB,
	// first two fit (~8 KB total), third gets truncated to remaining
	// (~2 KB) or summary fallback when remaining < BodySummaryBytes.
	root := tempSkillsRoot(t, map[string]int{
		"a-first":  4096,
		"b-second": 4096,
		"c-third":  4096,
	})
	refs := []ResolvedReference{
		stubRef("a-first", "skills/a-first"),
		stubRef("b-second", "skills/b-second"),
		stubRef("c-third", "skills/c-third"),
	}
	opts := inlineBodyOpts{
		enabled:        true,
		envelopeBudget: 10240, // 10 KB total → only two 4 KB bodies fit whole
		repoRoot:       root,
		cache:          NewBodyCache(),
	}
	bytes, count := applyBodyInlining(refs, opts)
	if count != 3 {
		t.Errorf("count: %d, want 3 (all three got Body* populated)", count)
	}
	if bytes > opts.envelopeBudget+BodySummaryBytes {
		// summary fallback adds at most BodySummaryBytes beyond hard budget
		t.Errorf("bytes %d exceeded envelope budget %d", bytes, opts.envelopeBudget)
	}

	// First two refs: whole inline.
	for i, r := range refs[:2] {
		if r.BodyTruncated {
			t.Errorf("ref %d (%s): should not be truncated, got BodyTruncated=true", i, r.Token)
		}
		if len(r.BodyInlined) != 4096 {
			t.Errorf("ref %d (%s): BodyInlined len %d, want 4096", i, r.Token, len(r.BodyInlined))
		}
	}
	// Third ref: should be truncated OR summary-fallback (depends on
	// remaining room vs BodySummaryBytes threshold).
	r := refs[2]
	if r.BodyInlined != "" && !r.BodyTruncated {
		t.Errorf("third ref: if inlined, should be truncated; got BodyInlined len %d, BodyTruncated=false", len(r.BodyInlined))
	}
}

func TestApplyBodyInlining_NonUseDirectlyRefsSkipped(t *testing.T) {
	root := tempSkillsRoot(t, map[string]int{"fuzzy": 500})
	refs := []ResolvedReference{{
		Token:             "fuzzy",
		Shape:             ShapeSkillTrigger,
		ConfidenceTier:    TierFuzzyMulti, // ← not single-exact
		RecommendedAction: PresentAskUserToDisambiguate,
		TopCandidates:     []Candidate{{ID: "fuzzy", SourceRef: "skill:skills/fuzzy"}},
	}}
	opts := inlineBodyOpts{
		enabled:        true,
		envelopeBudget: DefaultInlineBudgetBytes,
		repoRoot:       root,
		cache:          NewBodyCache(),
	}
	bytes, count := applyBodyInlining(refs, opts)
	if bytes != 0 || count != 0 {
		t.Errorf("fuzzy-multi ref: want (0, 0), got (%d, %d)", bytes, count)
	}
	if refs[0].BodyInlined != "" || refs[0].BodySummary != "" {
		t.Errorf("fuzzy-multi ref: Body fields should stay empty")
	}
}

func TestApplyBodyInlining_NonSkillShapeSkipped(t *testing.T) {
	root := tempSkillsRoot(t, map[string]int{"x": 500})
	refs := []ResolvedReference{{
		Token:             "some-chain",
		Shape:             ShapeChainSlug, // ← not skill_trigger / discipline_skill
		ConfidenceTier:    TierSingleExact,
		RecommendedAction: PresentUseDirectly,
		TopCandidates:     []Candidate{{ID: "some-chain", SourceRef: "chain:some-chain"}},
	}}
	opts := inlineBodyOpts{
		enabled:        true,
		envelopeBudget: DefaultInlineBudgetBytes,
		repoRoot:       root,
		cache:          NewBodyCache(),
	}
	bytes, count := applyBodyInlining(refs, opts)
	if bytes != 0 || count != 0 {
		t.Errorf("chain-slug ref: want (0, 0), got (%d, %d)", bytes, count)
	}
}

func TestApplyBodyInlining_MissingBodyFileSkippedGracefully(t *testing.T) {
	root := t.TempDir() // skills/ dir doesn't exist
	refs := []ResolvedReference{stubRef("ghost", "skills/ghost")}
	opts := inlineBodyOpts{
		enabled:        true,
		envelopeBudget: DefaultInlineBudgetBytes,
		repoRoot:       root,
		cache:          NewBodyCache(),
	}
	bytes, count := applyBodyInlining(refs, opts)
	if bytes != 0 || count != 0 {
		t.Errorf("missing body: want (0, 0), got (%d, %d)", bytes, count)
	}
	if refs[0].BodyInlined != "" || refs[0].BodySummary != "" {
		t.Errorf("missing body: Body fields should stay empty")
	}
}

func TestBucketPrecedence(t *testing.T) {
	root := tempSkillsRoot(t, map[string]int{
		"pure":     4000,
		"condense": 4000,
		"ambient":  4000,
	})
	// Manifest: ambient gets keep-ambient bucket (highest priority);
	// condense gets condense-lazy; pure gets pure-lazy.
	manifest := &SkillManifest{
		Skills: []SkillManifestEntry{
			{Name: "pure", BodyPath: "skills/pure", Bucket: "pure-lazy"},
			{Name: "condense", BodyPath: "skills/condense", Bucket: "condense-lazy"},
			{Name: "ambient", BodyPath: "skills/ambient", Bucket: "keep-ambient"},
		},
	}
	// Order refs in REVERSE precedence to confirm the sort matters.
	refs := []ResolvedReference{
		stubRef("pure", "skills/pure"),
		stubRef("condense", "skills/condense"),
		stubRef("ambient", "skills/ambient"),
	}
	// Budget exactly fits one body; precedence decides which gets inlined.
	opts := inlineBodyOpts{
		enabled:        true,
		envelopeBudget: 4000,
		repoRoot:       root,
		manifest:       manifest,
		cache:          NewBodyCache(),
	}
	applyBodyInlining(refs, opts)

	// Find each by token, then check who got the whole body (untruncated).
	var ambient, condense, pure ResolvedReference
	for _, r := range refs {
		switch r.Token {
		case "ambient":
			ambient = r
		case "condense":
			condense = r
		case "pure":
			pure = r
		}
	}
	if len(ambient.BodyInlined) != 4000 || ambient.BodyTruncated {
		t.Errorf("keep-ambient should win precedence and get whole body, got BodyInlined len=%d truncated=%t", len(ambient.BodyInlined), ambient.BodyTruncated)
	}
	// condense + pure should NOT have whole untruncated bodies (budget exhausted).
	if len(condense.BodyInlined) == 4000 && !condense.BodyTruncated {
		t.Errorf("condense-lazy should not have won precedence over keep-ambient")
	}
	if len(pure.BodyInlined) == 4000 && !pure.BodyTruncated {
		t.Errorf("pure-lazy should not have won precedence over keep-ambient")
	}
}

// T4: the load-bearing truncator should preserve frontmatter, intro,
// and load-bearing H2 sections while dropping history/exemplar
// sections first.
func TestTruncatePreservingLoadBearing_DropsElideFirstSectionBeforeMustKeep(t *testing.T) {
	body := []byte(`---
name: example
description: cue
---

# Example Skill

Intro prose explaining what this skill is.

## Fire rule

The procedurally-load-bearing rule. Always preserve this.

## Anchors

` + strings.Repeat("anchor-content-padding ", 200) + `

## When NOT to apply

Another load-bearing section.
`)
	// Budget that fits intro + Fire rule + When NOT to apply, but NOT Anchors.
	const budget = 600
	out := truncatePreservingLoadBearing(body, budget)
	s := string(out)

	if !strings.Contains(s, "name: example") {
		t.Errorf("frontmatter dropped: %q", s)
	}
	if !strings.Contains(s, "Fire rule") {
		t.Errorf("must-keep section 'Fire rule' dropped: %q", s)
	}
	if !strings.Contains(s, "When NOT to apply") {
		t.Errorf("must-keep section 'When NOT to apply' dropped: %q", s)
	}
	if strings.Contains(s, "anchor-content-padding") {
		t.Errorf("elide-first section 'Anchors' kept despite budget pressure: len=%d", len(s))
	}
	if len(out) > budget {
		t.Errorf("output %d exceeds budget %d", len(out), budget)
	}
}

func TestTruncatePreservingLoadBearing_NoH2BodyFallsBackToHeadN(t *testing.T) {
	body := []byte("frontmatter and intro and stuff with no H2 headings, " + strings.Repeat("x", 200))
	out := truncatePreservingLoadBearing(body, 50)
	if len(out) != 50 {
		t.Errorf("expected head-N fallback to give exactly 50 bytes, got %d", len(out))
	}
	if string(out[:8]) != "frontmat" {
		t.Errorf("head-N fallback dropped the head: %q", string(out))
	}
}

func TestTruncatePreservingLoadBearing_IntroOverflowsFallsBackToHeadN(t *testing.T) {
	intro := strings.Repeat("intro-text ", 50) // ~550 B
	body := []byte(intro + "## Section\n\nbody\n")
	out := truncatePreservingLoadBearing(body, 100)
	if len(out) != 100 {
		t.Errorf("expected head-N fallback (intro too large for budget); got len=%d", len(out))
	}
}

func TestTruncatePreservingLoadBearing_UnderBudgetReturnsWhole(t *testing.T) {
	body := []byte("# Skill\n\nshort body\n## Fire rule\n\nbrief\n")
	out := truncatePreservingLoadBearing(body, 10000)
	if !bytesEqual(out, body) {
		t.Errorf("under-budget content should be returned unchanged")
	}
}

func TestClassifyHeading(t *testing.T) {
	cases := map[string]sectionPriority{
		"":                                       priorityMustKeep,
		"Fire rule":                              priorityMustKeep,
		"FIRE RULE":                              priorityMustKeep,
		"When NOT to apply":                      priorityMustKeep,
		"How to apply":                           priorityMustKeep,
		"Anti-patterns":                          priorityMustKeep,
		"TL;DR — forge-first reflex":             priorityMustKeep,
		"Anchors":                                priorityElideFirst,
		"Why this skill exists":                  priorityElideFirst,
		"Recognition signals":                    priorityElideFirst,
		"Interaction with bug-filing-discipline": priorityElideFirst,
		"Random other heading no special handling": priorityMidPriority,
	}
	for heading, want := range cases {
		got := classifyHeading(heading)
		if got != want {
			t.Errorf("classifyHeading(%q): got %d, want %d", heading, got, want)
		}
	}
}

func TestIsH2(t *testing.T) {
	cases := map[string]bool{
		"## Heading\n": true,
		"## h":         true,
		"# H1\n":       false,
		"### H3\n":     false,
		"####\n":       false,
		"":             false,
		"plain text\n": false,
		"##No space\n": false,
	}
	for line, want := range cases {
		if got := isH2([]byte(line)); got != want {
			t.Errorf("isH2(%q): got %t, want %t", line, got, want)
		}
	}
}

func TestSplitH2Sections_PreservesOriginalBytes(t *testing.T) {
	body := []byte("intro line\n## Section A\nbody A\n## Section B\nbody B\n")
	sections := splitH2Sections(body)
	if len(sections) != 3 {
		t.Fatalf("expected 3 sections (intro + 2 H2), got %d", len(sections))
	}
	var reconstructed []byte
	for _, s := range sections {
		reconstructed = append(reconstructed, s.body...)
	}
	if !bytesEqual(reconstructed, body) {
		t.Errorf("round-trip lost bytes:\n  in : %q\n  out: %q", body, reconstructed)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBodyCache_MtimeBustsStaleEntry(t *testing.T) {
	root := tempSkillsRoot(t, map[string]int{"x": 100})
	cache := NewBodyCache()
	absPath := filepath.Join(root, "skills", "x", "SKILL.md")

	first, err := cache.get(absPath)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if len(first) != 100 {
		t.Errorf("first read len: %d", len(first))
	}

	// Rewrite the file with different content and forcibly bump mtime
	// (some filesystems have 1s mtime resolution; the test runs faster
	// than that, so the rewrite alone may not change mtime).
	newContent := strings.Repeat("y", 200)
	if err := os.WriteFile(absPath, []byte(newContent), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(absPath, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	second, err := cache.get(absPath)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if string(second) != newContent {
		t.Errorf("cache returned stale content; want %q-prefix, got %q-prefix",
			newContent[:5], string(second[:5]))
	}
}
