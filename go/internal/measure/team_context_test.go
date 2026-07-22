package measure_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/measure"
	"toolkit/internal/testutil"
)

// seedTasks registers a project and inserts a chain plus the given
// closure pattern. closuresLast7d closed tasks land with today's
// updated_at; weekOffsets each insert one closed task whose updated_at
// is exactly that many days ago, so each lands in a distinct week
// slot. DeriveTeamContext keys closure-week buckets off updated_at.
//
// Built on [testutil.SeedChain] and [testutil.SeedTask]; the per-task
// updated_at backdate is the measure-specific value-add and runs as a
// targeted UPDATE after the SeedTask insert.
func seedTasks(t *testing.T, pool *db.Pool, project string, closuresLast7d int, weekOffsets []int) {
	t.Helper()
	testutil.SeedProject(t, pool, project)
	chainID := testutil.SeedChain(t, pool, project, "test-chain-"+project, "open", testutil.SeedChainOpts{})
	for i := 0; i < closuresLast7d; i++ {
		testutil.SeedTask(t, pool, chainID, "recent-"+itoa(i), "closed", testutil.SeedTaskOpts{
			Position: int64(i),
		})
	}
	for j, daysAgo := range weekOffsets {
		id := testutil.SeedTask(t, pool, chainID, "old-"+itoa(j), "closed", testutil.SeedTaskOpts{
			Position: int64(100 + j),
		})
		if _, err := pool.DB().Exec(
			`UPDATE proj_current_tasks SET updated_at = datetime('now', ?) WHERE id = ?`,
			"-"+itoa(daysAgo)+" days", id,
		); err != nil {
			t.Fatalf("backdate updated_at: %v", err)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}

func TestDeriveTeamContext_ClosuresCountedInLast7Days(t *testing.T) {
	pool := testutil.NewTestDB(t)
	// 5 closures today, 1 closure 30 days ago. Only the 5 today should count.
	seedTasks(t, pool, "demo", 5, []int{30})

	tc, err := measure.DeriveTeamContext(context.Background(), pool, t.TempDir(), "demo", "anything")
	if err != nil {
		t.Fatalf("DeriveTeamContext: %v", err)
	}
	if tc.ClosuresPerWeek != 5 {
		t.Errorf("ClosuresPerWeek: want 5, got %d", tc.ClosuresPerWeek)
	}
}

func TestDeriveTeamContext_PercentileFallbackWhenSparseHistory(t *testing.T) {
	pool := testutil.NewTestDB(t)
	// Only 2 weeks of history (8d ago, 15d ago) — below MIN_WEEKS_FOR_PERCENTILE (4).
	// Fallback thresholds should kick in: p25=2, p50=4, p75=7.
	seedTasks(t, pool, "demo", 3, []int{8, 15})

	tc, err := measure.DeriveTeamContext(context.Background(), pool, t.TempDir(), "demo", "x")
	if err != nil {
		t.Fatalf("DeriveTeamContext: %v", err)
	}
	if tc.TrailingSampleWeeks != 0 {
		t.Errorf("expected fallback (sampleWeeks=0), got sampleWeeks=%d", tc.TrailingSampleWeeks)
	}
	// 3 closures vs fallback p25=2, p75=7 → nominal.
	if tc.Bandwidth != "nominal" {
		t.Errorf("bandwidth: want nominal, got %q", tc.Bandwidth)
	}
}

func TestDeriveTeamContext_ProjectScopedQuery(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedTasks(t, pool, "demo-a", 4, nil)
	seedTasks(t, pool, "demo-b", 9, nil)

	tcA, err := measure.DeriveTeamContext(context.Background(), pool, t.TempDir(), "demo-a", "x")
	if err != nil {
		t.Fatalf("DeriveTeamContext A: %v", err)
	}
	if tcA.ClosuresPerWeek != 4 {
		t.Errorf("project A closures: want 4, got %d", tcA.ClosuresPerWeek)
	}
	if tcA.ProjectScope != "demo-a" {
		t.Errorf("scope: want demo-a, got %q", tcA.ProjectScope)
	}

	tcCross, err := measure.DeriveTeamContext(context.Background(), pool, t.TempDir(), "", "x")
	if err != nil {
		t.Fatalf("DeriveTeamContext cross: %v", err)
	}
	if tcCross.ClosuresPerWeek != 13 {
		t.Errorf("cross-project closures: want 13, got %d", tcCross.ClosuresPerWeek)
	}
}

func TestDeriveTeamContext_VaultKeywordMatching(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedTasks(t, pool, "demo", 1, nil)

	// Build a hermetic vault with two matching decisions and one off-topic.
	vault := t.TempDir()
	dec := filepath.Join(vault, "decisions")
	if err := os.MkdirAll(dec, 0o755); err != nil {
		t.Fatalf("mkdir decisions: %v", err)
	}
	files := map[string]string{
		"2026-01-01_rubric-foundation.md": "title: rubric foundation\ntags: [rubric]\nbody about rubric work",
		"2026-02-01_dispatch-design.md":   "title: dispatch design\ntags: [dispatch, rubric]\nrubric dispatch decisions",
		"2026-03-01_kubernetes.md":        "title: kubernetes\nunrelated content here",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dec, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	tc, err := measure.DeriveTeamContext(context.Background(), pool, vault, "demo",
		"Implement the rubric dispatch handler for chain assessment.")
	if err != nil {
		t.Fatalf("DeriveTeamContext: %v", err)
	}

	if tc.VaultHits != 2 {
		t.Errorf("VaultHits: want 2, got %d (matched: %v)", tc.VaultHits, tc.MatchedPaths)
	}
	if tc.PriorSignal != "mid" {
		t.Errorf("PriorSignal: want mid (1-3 hits), got %q", tc.PriorSignal)
	}
	for _, want := range []string{"decisions/2026-01-01_rubric-foundation.md", "decisions/2026-02-01_dispatch-design.md"} {
		found := false
		for _, p := range tc.MatchedPaths {
			if p == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("MatchedPaths missing %q; got %v", want, tc.MatchedPaths)
		}
	}
}

// TestDeriveTeamContext_VaultBodyOnlyDoesNotMatch verifies the Rust-parity
// invariant that keywords appearing ONLY in the body (not in frontmatter,
// title, or path) do not count as hits. This guards against over-counting if
// the scanner ever regresses to whole-file substring matching.
func TestDeriveTeamContext_VaultBodyOnlyDoesNotMatch(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedTasks(t, pool, "demo", 1, nil)

	vault := t.TempDir()
	dec := filepath.Join(vault, "decisions")
	if err := os.MkdirAll(dec, 0o755); err != nil {
		t.Fatalf("mkdir decisions: %v", err)
	}

	// File whose path/title/tags/summary mentions only kafka. "kubernetes"
	// appears later in the body — past the summary line — and must NOT count
	// as a hit. This pins the Rust-parity invariant (summary = first body
	// line only; later body content is invisible to the scanner).
	body := `---
title: kafka cluster sizing
tags: [kafka, infra]
---

# kafka cluster sizing

This note discusses kafka topic partitioning and broker tuning.

Later in the body we considered kubernetes operators as an alternative
deployment surface but rejected them for this use case.
`
	if err := os.WriteFile(filepath.Join(dec, "2026-04-01_kafka-sizing.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	tc, err := measure.DeriveTeamContext(context.Background(), pool, vault, "demo",
		"investigate kubernetes operator deployment pipelines")
	if err != nil {
		t.Fatalf("DeriveTeamContext: %v", err)
	}
	if tc.VaultHits != 0 {
		t.Errorf("body-only keyword must not match; got %d hits (%v)", tc.VaultHits, tc.MatchedPaths)
	}
}

// TestDeriveTeamContext_VaultMatchesFrontmatterTags verifies a keyword that
// only appears in the frontmatter `tags:` list (not in the path or body) counts
// as a hit — this is a primary use case the Rust scanner is designed to serve.
func TestDeriveTeamContext_VaultMatchesFrontmatterTags(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedTasks(t, pool, "demo", 1, nil)

	vault := t.TempDir()
	dec := filepath.Join(vault, "decisions")
	if err := os.MkdirAll(dec, 0o755); err != nil {
		t.Fatalf("mkdir decisions: %v", err)
	}
	body := `---
title: production rollout notes
tags: [observability, retrospective]
---

# notes

Body content that does not contain the tagged keywords.
`
	if err := os.WriteFile(filepath.Join(dec, "2026-04-02_rollout.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	tc, err := measure.DeriveTeamContext(context.Background(), pool, vault, "demo",
		"design the observability dashboard for the new pipeline")
	if err != nil {
		t.Fatalf("DeriveTeamContext: %v", err)
	}
	if tc.VaultHits != 1 {
		t.Errorf("frontmatter tag match should count; got %d hits (%v)", tc.VaultHits, tc.MatchedPaths)
	}
}

func TestDeriveTeamContext_VaultMissingIsSoftFailure(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedTasks(t, pool, "demo", 1, nil)

	// Point at a directory with no decisions/ subdir.
	emptyRoot := t.TempDir()
	tc, err := measure.DeriveTeamContext(context.Background(), pool, emptyRoot, "demo", "rubric chain")
	if err != nil {
		t.Fatalf("expected soft failure, got hard error: %v", err)
	}
	if tc.VaultHits != 0 {
		t.Errorf("missing vault should yield 0 hits, got %d", tc.VaultHits)
	}
	if tc.PriorSignal != "weak" {
		t.Errorf("0 hits → weak, got %q", tc.PriorSignal)
	}
}

func TestTeamContext_ProseShape(t *testing.T) {
	tc := &measure.TeamContext{
		ClosuresPerWeek:     5,
		Bandwidth:           "nominal",
		TrailingP50:         4,
		TrailingSampleWeeks: 12,
		VaultHits:           2,
		PriorSignal:         "mid",
		Keywords:            []string{"rubric", "dispatch"},
		MatchedPaths:        []string{"decisions/foo.md", "decisions/bar.md"},
		ProjectScope:        "demo",
	}
	prose := tc.Prose()
	// Two named lines required by the rubric prose contract.
	if !strings.HasPrefix(prose, "team_bandwidth:") {
		t.Errorf("prose must lead with team_bandwidth: line; got %q", prose)
	}
	if !strings.Contains(prose, "prior_signal_strength:") {
		t.Errorf("prose missing prior_signal_strength line: %s", prose)
	}
	if !strings.Contains(prose, "project: demo") {
		t.Errorf("project scope missing from prose: %s", prose)
	}
	if !strings.Contains(prose, "trailing P50 = 4/week") {
		t.Errorf("trailing P50 missing from prose: %s", prose)
	}
	if !strings.Contains(prose, "keywords: rubric, dispatch") {
		t.Errorf("keywords missing from prose: %s", prose)
	}
	if !strings.Contains(prose, "Top matches: decisions/foo.md, decisions/bar.md") {
		t.Errorf("matched paths missing from prose: %s", prose)
	}
}

func TestTeamContext_ProseHandlesEmptyKeywords(t *testing.T) {
	tc := &measure.TeamContext{
		ClosuresPerWeek: 0,
		Bandwidth:       "low",
		PriorSignal:     "weak",
		Keywords:        nil,
		MatchedPaths:    nil,
	}
	prose := tc.Prose()
	if !strings.Contains(prose, "no keywords extracted") {
		t.Errorf("empty-keywords case must say 'no keywords extracted': %s", prose)
	}
}

// silence unused-import warnings when this file is the only one in the package
// referencing sql.
var _ = sql.ErrNoRows
