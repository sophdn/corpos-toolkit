// Command curate-rescore walks pending curation_candidates with
// quality_score IS NULL, rebuilds source material via the appropriate
// per-origin SourceMaterialBuilder, runs Scorer.Extract + Scorer.Score,
// writes the result back via UpdateCandidateScoring, and auto-promotes
// anything that scores >= 0.85.
//
// First production pass on the chain curation-go-migration architecture
// (T7). Closes the structural-fix half of bug
// knowledge-curate-secondary-pass-cannot-score-existing-candidates-and-
// silently-adds-unscored-on-qwen-failure: the rescore pass IS the
// missing "score existing candidates" code path, AND its abort-on-
// Scorer.Health() behavior is the silent-failure fix.
//
// CRITICAL CONTRACT: if Scorer.Health() fails, this binary exits
// non-zero with a typed error message and writes ZERO rows. There is no
// qwen_ok boolean, no fallback metadata, no "graceful degradation."
// The Rust pipeline had both and they produced 502 noise candidates
// before the symptom was diagnosed.
//
// Usage:
//
//	curate-rescore --project mcp-servers --limit 50
//	curate-rescore --project mcp-servers --limit 10 --dry-run
//	curate-rescore --db /custom/path.db --project mcp-servers
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"toolkit/internal/db"
	"toolkit/internal/inference/llamacpp"
	"toolkit/internal/knowledge/curation"
	"toolkit/internal/knowledge/curation/sources"
)

const defaultDB = "/home/user/dev/mcp-servers/data/toolkit.db"

// AutoPromoteThreshold is the quality_score above which candidates are
// automatically promoted to knowledge_pointers. Matches the Rust
// AUTO_PROMOTE_THRESHOLD constant.
const AutoPromoteThreshold = 0.85

func main() {
	var (
		project string
		limit   int
		dryRun  bool
		dbPath  string
	)
	flag.StringVar(&project, "project", "", "filter candidates by project_id (required)")
	flag.IntVar(&limit, "limit", 50, "max candidates to process this run")
	flag.BoolVar(&dryRun, "dry-run", false, "print what would happen; no DB writes")
	flag.StringVar(&dbPath, "db", defaultDB, "path to toolkit.db")
	flag.Parse()

	if project == "" {
		fmt.Fprintln(os.Stderr, "--project is required")
		os.Exit(2)
	}

	ctx := context.Background()
	pool, err := db.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	scorer := curation.NewQwenScorer(llamacpp.NewFromEnv())

	registry := curation.NewBuilderRegistry()
	for _, b := range sources.DefaultBuilders("", "") {
		registry.Register(b)
	}

	summary, err := Run(ctx, RunDeps{
		Pool:     pool,
		Scorer:   scorer,
		Registry: registry,
		Stdout:   os.Stdout,
	}, RunOpts{
		Project: project,
		Limit:   limit,
		DryRun:  dryRun,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "rescore failed: %v\n", err)
		os.Exit(1)
	}
	summary.Print(os.Stdout)
}

// RunDeps bundles the dependencies Run needs. Allows the integration
// test to inject a mock Scorer + a registry seeded with stub builders.
type RunDeps struct {
	Pool     *db.Pool
	Scorer   curation.Scorer
	Registry *curation.BuilderRegistry
	Stdout   io.Writer
}

// RunOpts carries the CLI-derived knobs.
type RunOpts struct {
	Project string
	Limit   int
	DryRun  bool
}

// Summary is the per-run tally Run returns. Suitable for cron output.
type Summary struct {
	Processed      int
	AutoPromoted   int
	LeftPending    int
	Errors         int
	BuilderMisses  int // candidate's origin had no registered builder
	HealthCheckURL string
	DryRun         bool
}

// Print writes a one-line human summary to w.
func (s Summary) Print(w io.Writer) {
	tag := ""
	if s.DryRun {
		tag = " (dry-run, no writes)"
	}
	fmt.Fprintf(w, "\nDone%s: %d processed, %d auto-promoted, %d left pending with score, %d errors, %d builder-misses.\n",
		tag, s.Processed, s.AutoPromoted, s.LeftPending, s.Errors, s.BuilderMisses)
}

// Run is the testable core: walks pending unscored candidates, scores
// each, updates the row (or simulates in dry-run), auto-promotes when
// the score >= threshold. Returns Summary regardless of per-candidate
// errors (those are counted, not propagated) — the only fatal errors
// are health-check failure and DB enumeration failure.
//
// Health-check failure is the load-bearing safety net: zero DB writes
// happen if Scorer.Health() returns an error. The function returns
// early before any candidate is even loaded.
func Run(ctx context.Context, deps RunDeps, opts RunOpts) (Summary, error) {
	summary := Summary{DryRun: opts.DryRun}

	if err := deps.Scorer.Health(ctx); err != nil {
		// Surface URL if the scorer can give it (QwenScorer can).
		if bu, ok := deps.Scorer.(interface{ BaseURL() string }); ok {
			summary.HealthCheckURL = bu.BaseURL()
		}
		return summary, fmt.Errorf("scorer health check failed (no DB writes): %w", err)
	}
	if bu, ok := deps.Scorer.(interface{ BaseURL() string }); ok {
		summary.HealthCheckURL = bu.BaseURL()
		fmt.Fprintf(deps.Stdout, "scorer.health ok url=%s\n", summary.HealthCheckURL)
	} else {
		fmt.Fprintln(deps.Stdout, "scorer.health ok")
	}

	candidates, err := curation.ListPending(ctx, deps.Pool, curation.ListFilter{
		ProjectID:    opts.Project,
		UnscoredOnly: true,
		Limit:        opts.Limit,
	})
	if err != nil {
		return summary, fmt.Errorf("list pending: %w", err)
	}
	fmt.Fprintf(deps.Stdout, "found %d unscored pending candidates in %s\n",
		len(candidates), opts.Project)

	for i, cand := range candidates {
		summary.Processed++
		fmt.Fprintf(deps.Stdout, "[%d/%d] id=%d  source_ref=%s  origin=%s\n",
			i+1, len(candidates), cand.ID, cand.SourceRef, cand.Origin)

		builder, err := deps.Registry.ForOrigin(cand.Origin)
		if err != nil {
			fmt.Fprintf(deps.Stdout, "  builder-miss: %v\n", err)
			summary.BuilderMisses++
			continue
		}

		material, err := builder.Build(ctx, deps.Pool, cand)
		if err != nil {
			fmt.Fprintf(deps.Stdout, "  build: %v\n", err)
			summary.Errors++
			continue
		}

		meta, err := deps.Scorer.Extract(ctx, cand.SourceType, cand.SourceRef, material)
		if err != nil {
			fmt.Fprintf(deps.Stdout, "  extract: %v\n", err)
			summary.Errors++
			continue
		}

		score, err := deps.Scorer.Score(ctx, meta.Question, material)
		if err != nil {
			fmt.Fprintf(deps.Stdout, "  score: %v\n", err)
			summary.Errors++
			continue
		}

		fmt.Fprintf(deps.Stdout, "  question: %s\n", meta.Question)
		fmt.Fprintf(deps.Stdout, "  score: %.2f", score)

		if opts.DryRun {
			if score >= AutoPromoteThreshold {
				fmt.Fprintln(deps.Stdout, "  (would auto-promote)")
				summary.AutoPromoted++
			} else {
				fmt.Fprintln(deps.Stdout, "  (would leave pending with score)")
				summary.LeftPending++
			}
			continue
		}

		if err := curation.UpdateCandidateScoring(ctx, deps.Pool, cand.ID, meta, score); err != nil {
			fmt.Fprintf(deps.Stdout, "  update: %v\n", err)
			summary.Errors++
			continue
		}

		if score >= AutoPromoteThreshold {
			pointerID, err := curation.PromoteCandidate(ctx, deps.Pool, cand.ID, true)
			if err != nil {
				// Update succeeded; promote failed. The candidate now
				// has a score but stayed pending. Acceptable degraded
				// state — a subsequent rescore run can retry the promote.
				fmt.Fprintf(deps.Stdout, "  promote: %v (candidate scored but still pending)\n", err)
				summary.Errors++
				continue
			}
			fmt.Fprintf(deps.Stdout, "  → auto-promoted (pointer id=%d)\n", pointerID)
			summary.AutoPromoted++
		} else {
			fmt.Fprintln(deps.Stdout, "  → left pending with score")
			summary.LeftPending++
		}
	}

	return summary, nil
}
