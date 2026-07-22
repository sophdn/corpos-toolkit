// Command curate-seed seeds curation_candidates from named vault notes.
// Replaces the vault-note slice of the Rust knowledge_seeder (the only
// path with real signal) and DELIBERATELY DROPS the session-deep,
// session-question, and web-conv-* paths that produced 502/558 noise
// candidates we bulk-rejected during the chain start (see bug
// knowledge-seeder-session-mining-captures-one-time-prompts).
//
// Walks ~/.claude/vault/{reference,decisions,learnings}/*.md (the
// signal-bearing subset), skips web-conv-* prefixed files (mixed
// quality), builds source material via VaultNoteBuilder (T5), scores
// via Scorer, inserts via AddCandidate with origin='session_mining',
// source_type='vault', expires_at = now+30d (fixes bug session-mining-
// candidates-have-no-expires-at).
//
// Skips files whose source_ref already has a candidate (any status —
// rejected ones DON'T get re-added per the seeder's dedup contract)
// or an active pointer.
//
// Usage:
//
//	curate-seed --project mcp-servers
//	curate-seed --project seed-packet --vault-root /custom/vault --limit 20
//	curate-seed --project mcp-servers --dry-run
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"toolkit/internal/db"
	"toolkit/internal/inference/llamacpp"
	"toolkit/internal/knowledge/curation"
	"toolkit/internal/knowledge/curation/sources"
)

const defaultDB = "/home/user/dev/mcp-servers/data/toolkit.db"
const AutoPromoteThreshold = 0.85
const VaultExpiryDays = "+30 days"

// vaultSubdirs are the subdirectories under the vault root that this
// seeder considers signal-bearing. Other subdirs (scratch, projects,
// meta, roles) are explicitly NOT walked.
var vaultSubdirs = []string{"reference", "decisions", "learnings"}

func main() {
	var (
		project   string
		vaultRoot string
		limit     int
		dryRun    bool
		dbPath    string
	)
	flag.StringVar(&project, "project", "", "project_id under which to file candidates (required)")
	flag.StringVar(&vaultRoot, "vault-root", "", "vault root path; empty = $HOME/.claude/vault")
	flag.IntVar(&limit, "limit", 0, "max new candidates this run; 0 = unlimited")
	flag.BoolVar(&dryRun, "dry-run", false, "print what would happen; no DB writes")
	flag.StringVar(&dbPath, "db", defaultDB, "path to toolkit.db")
	flag.Parse()

	if project == "" {
		fmt.Fprintln(os.Stderr, "--project is required")
		os.Exit(2)
	}
	if vaultRoot == "" {
		if home := os.Getenv("HOME"); home != "" {
			vaultRoot = filepath.Join(home, ".claude", "vault")
		}
	}

	ctx := context.Background()
	pool, err := db.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	scorer := curation.NewQwenScorer(llamacpp.NewFromEnv())
	if err := scorer.Health(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "scorer health check failed (no DB writes): %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("scorer.health ok url=%s\n", scorer.BaseURL())

	builder := sources.NewVaultNoteBuilder(vaultRoot)

	deps := RunDeps{
		Pool:      pool,
		Scorer:    scorer,
		Builder:   builder,
		Stdout:    os.Stdout,
		VaultRoot: vaultRoot,
	}
	summary, err := Run(ctx, deps, RunOpts{
		Project: project,
		Limit:   limit,
		DryRun:  dryRun,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "seed failed: %v\n", err)
		os.Exit(1)
	}
	summary.Print(os.Stdout)
}

// RunDeps bundles the dependencies Run needs. Mock-friendly.
type RunDeps struct {
	Pool      *db.Pool
	Scorer    curation.Scorer
	Builder   curation.SourceMaterialBuilder
	Stdout    io.Writer
	VaultRoot string
}

// RunOpts carries the CLI knobs.
type RunOpts struct {
	Project string
	Limit   int
	DryRun  bool
}

// Summary is the per-run tally.
type Summary struct {
	Walked       int // files enumerated
	Skipped      int // skipped due to existing candidate/pointer, or web-conv- prefix
	Added        int
	AutoPromoted int
	Errors       int
	DryRun       bool
}

// Print writes the one-line summary to w.
func (s Summary) Print(w io.Writer) {
	tag := ""
	if s.DryRun {
		tag = " (dry-run, no writes)"
	}
	fmt.Fprintf(w, "\nDone%s: walked %d, skipped %d, added %d, auto-promoted %d, errors %d.\n",
		tag, s.Walked, s.Skipped, s.Added, s.AutoPromoted, s.Errors)
}

// Run enumerates vault notes under the signal-bearing subdirs and
// scores each one, inserting new candidates for any that don't already
// have a candidate or pointer.
func Run(ctx context.Context, deps RunDeps, opts RunOpts) (Summary, error) {
	summary := Summary{DryRun: opts.DryRun}

	for _, subdir := range vaultSubdirs {
		absDir := filepath.Join(deps.VaultRoot, subdir)
		entries, err := os.ReadDir(absDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue // subdir absent on this machine; skip silently
			}
			return summary, fmt.Errorf("read %s: %w", absDir, err)
		}

		for _, e := range entries {
			if opts.Limit > 0 && summary.Added >= opts.Limit {
				return summary, nil
			}
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			if strings.HasPrefix(e.Name(), "web-conv-") {
				// Mixed-quality archived chat exports — out of scope
				// per design doc §1 non-goals.
				summary.Skipped++
				continue
			}
			summary.Walked++

			relPath := filepath.Join(".claude/vault", subdir, e.Name())
			if err := processVaultNote(ctx, deps, &summary, opts, relPath); err != nil {
				fmt.Fprintf(deps.Stdout, "  %s: %v\n", relPath, err)
				summary.Errors++
			}
		}
	}
	return summary, nil
}

// processVaultNote scores one note and inserts it as a candidate (if
// no candidate or pointer exists for it yet).
func processVaultNote(ctx context.Context, deps RunDeps, summary *Summary, opts RunOpts, sourceRef string) error {
	// Skip if pointer exists.
	if pid, err := curation.PointerExistsForSourceRef(ctx, deps.Pool, opts.Project, "vault", sourceRef); err != nil {
		return fmt.Errorf("pointer check: %w", err)
	} else if pid != 0 {
		summary.Skipped++
		return nil
	}
	// Skip if candidate exists in any state (the seeder's dedup contract
	// must cover rejected too so we don't re-add noise we cleaned up).
	if exists, err := curation.CandidateExistsForSourceRef(ctx, deps.Pool, "vault", sourceRef,
		"pending", "promoted", "rejected"); err != nil {
		return fmt.Errorf("candidate check: %w", err)
	} else if exists {
		summary.Skipped++
		return nil
	}

	// Build source material via the VaultNoteBuilder. The builder
	// expects a Candidate with the source_ref populated; the other
	// fields don't matter for the file-read.
	material, err := deps.Builder.Build(ctx, deps.Pool, curation.Candidate{
		ProjectID: opts.Project,
		SourceRef: sourceRef,
	})
	if err != nil {
		return fmt.Errorf("build material: %w", err)
	}

	meta, err := deps.Scorer.Extract(ctx, "vault", sourceRef, material)
	if err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	score, err := deps.Scorer.Score(ctx, meta.Question, material)
	if err != nil {
		return fmt.Errorf("score: %w", err)
	}

	display := fmt.Sprintf("%.2f", score)
	if opts.DryRun {
		summary.Added++
		verdict := "would queue"
		if score >= AutoPromoteThreshold {
			verdict = "would auto-promote"
			summary.AutoPromoted++
		}
		fmt.Fprintf(deps.Stdout, "  %s: %s (score=%s)\n", sourceRef, verdict, display)
		return nil
	}

	// expires_at is ALWAYS set (fixes bug session-mining-candidates-
	// have-no-expires-at). Below-threshold gets the 30-day TTL; even
	// promoted ones get a (moot) TTL so the column invariant holds.
	expiresAt := fmt.Sprintf("datetime('now', '%s')", VaultExpiryDays)

	id, err := curation.AddCandidate(ctx, deps.Pool, curation.CandidateInsert{
		ProjectID:    opts.Project,
		SourceType:   "vault",
		SourceRef:    sourceRef,
		Question:     meta.Question,
		InvokeWhen:   meta.InvokeWhen,
		Description:  defaultIfEmpty(meta.Description, "Vault note: "+sourceRef),
		Tags:         []string{"vault-seeder"},
		QualityScore: &score,
		Origin:       "session_mining",
		OriginRef:    nil,
		ExpiresAt:    &expiresAt,
	})
	if err != nil {
		return fmt.Errorf("add_candidate: %w", err)
	}
	summary.Added++

	if score < AutoPromoteThreshold {
		fmt.Fprintf(deps.Stdout, "  %s: queued (score=%s) → candidate %d\n", sourceRef, display, id)
		return nil
	}

	// Pre-check before promote (T8's pattern).
	if pid, err := curation.PointerExistsForSourceRef(ctx, deps.Pool, opts.Project, "vault", sourceRef); err != nil {
		return fmt.Errorf("pre-promote pointer check: %w", err)
	} else if pid != 0 {
		fmt.Fprintf(deps.Stdout, "  %s: skipped promote (pointer %d already exists)\n", sourceRef, pid)
		return nil
	}

	pointerID, err := curation.PromoteCandidate(ctx, deps.Pool, id, true)
	if err != nil {
		return fmt.Errorf("promote: %w", err)
	}
	summary.AutoPromoted++

	links, _ := curation.ProposeLinks(ctx, deps.Pool, pointerID, meta.Question, 5)
	fmt.Fprintf(deps.Stdout, "  %s: auto-promoted (score=%s) → pointer %d + %d link(s)\n",
		sourceRef, display, pointerID, links)
	return nil
}

func defaultIfEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
