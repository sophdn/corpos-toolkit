// Command curate-discover replaces benchmarks/src/bin/knowledge_curate
// (the Rust discovery binary). Two passes:
//
//	primary  — mines grounding_events WHERE results_count=0 AND
//	           next_turn_has_output=1. Reconstructs the original
//	           search query from the session JSONL (via
//	           ZeroResultGapBuilder), extracts retrieval metadata via
//	           Qwen, scores adversarially. Auto-promote candidates
//	           almost never fire on gap rows (the source material is
//	           the query itself, score is intentionally low) but the
//	           code path stays consistent for design uniformity.
//
//	secondary — walks closed tasks with handoff_output >= 50 chars
//	            that aren't yet candidates or pointers. Builds source
//	            material from problem_statement + handoff. Scores and
//	            auto-promotes >= 0.85.
//
// CRITICAL behaviors inherited from T7 (rescore pass) and the design
// doc §6:
//   - Scorer.Health() called once at start; non-zero exit + zero DB
//     writes on failure. No qwen_ok fallback path.
//   - Pre-check for existing knowledge_pointer before any
//     PromoteCandidate (bug curate-rescore-no-precheck-for-existing-
//     pointer). If a pointer for (project_id, source_type, source_ref)
//     already exists, the candidate is marked promoted in place rather
//     than failing the UNIQUE constraint.
//
// Pre-existing pending candidates are SKIPPED, not re-scored — the
// rescore pass (curate-rescore) owns that path. Discovery is for new
// sources only.
//
// Usage:
//
//	curate-discover --pass primary  --project corpos-toolkit
//	curate-discover --pass secondary --project corpos-toolkit --limit 100
//	curate-discover --pass both --project corpos-toolkit --dry-run
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

const AutoPromoteThreshold = 0.85
const SecondaryMinHandoff = 50

// defaultDBPath resolves the canonical ledger the way go/launch.sh does —
// TOOLKIT_DB, else $XDG_DATA_HOME (or ~/.local/share). It replaces a
// hardcoded path that still named the retired mcp-servers working tree, so
// the default pointed at a file that no longer exists.
func defaultDBPath() string {
	if p := os.Getenv("TOOLKIT_DB"); p != "" {
		return p
	}
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataHome, "toolkit", "data", "toolkit.db")
}

func main() {
	var (
		pass    string
		project string
		limit   int
		dryRun  bool
		dbPath  string
	)
	flag.StringVar(&pass, "pass", "both", "discovery pass: primary | secondary | both")
	flag.StringVar(&project, "project", "", "project_id to discover within (required for secondary)")
	flag.IntVar(&limit, "limit", 0, "max candidates to add per pass; 0 = unlimited")
	flag.BoolVar(&dryRun, "dry-run", false, "print what would happen; no DB writes")
	flag.StringVar(&dbPath, "db", defaultDBPath(), "path to toolkit.db")
	flag.Parse()

	if pass != "primary" && pass != "secondary" && pass != "both" {
		fmt.Fprintf(os.Stderr, "--pass must be primary | secondary | both (got %q)\n", pass)
		os.Exit(2)
	}
	if (pass == "secondary" || pass == "both") && project == "" {
		fmt.Fprintln(os.Stderr, "--project is required for secondary pass")
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
	if err := scorer.Health(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "scorer health check failed (no DB writes): %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("scorer.health ok url=%s\n", scorer.BaseURL())

	registry := curation.NewBuilderRegistry()
	for _, b := range sources.DefaultBuilders("", "") {
		registry.Register(b)
	}

	deps := PassDeps{
		Pool:     pool,
		Scorer:   scorer,
		Registry: registry,
		Stdout:   os.Stdout,
	}

	totalAdded, totalPromoted, totalSkipped, totalErrors := 0, 0, 0, 0
	if pass == "primary" || pass == "both" {
		s, err := RunPrimary(ctx, deps, PassOpts{Limit: limit, DryRun: dryRun})
		if err != nil {
			fmt.Fprintf(os.Stderr, "primary pass: %v\n", err)
			os.Exit(1)
		}
		s.Print(os.Stdout, "primary")
		totalAdded += s.Added
		totalPromoted += s.AutoPromoted
		totalSkipped += s.Skipped
		totalErrors += s.Errors
	}
	if pass == "secondary" || pass == "both" {
		s, err := RunSecondary(ctx, deps, PassOpts{Project: project, Limit: limit, DryRun: dryRun})
		if err != nil {
			fmt.Fprintf(os.Stderr, "secondary pass: %v\n", err)
			os.Exit(1)
		}
		s.Print(os.Stdout, "secondary")
		totalAdded += s.Added
		totalPromoted += s.AutoPromoted
		totalSkipped += s.Skipped
		totalErrors += s.Errors
	}

	tag := ""
	if dryRun {
		tag = " (dry-run, no writes)"
	}
	fmt.Fprintf(os.Stdout, "\nTotal%s: %d added, %d auto-promoted, %d skipped, %d errors.\n",
		tag, totalAdded, totalPromoted, totalSkipped, totalErrors)
}

// PassDeps bundles dependencies the discovery passes need. Mock-friendly.
type PassDeps struct {
	Pool     *db.Pool
	Scorer   curation.Scorer
	Registry *curation.BuilderRegistry
	Stdout   io.Writer
}

// PassOpts carries the per-pass CLI-derived knobs.
type PassOpts struct {
	Project string // required for secondary; ignored by primary (project comes from grounding_events row)
	Limit   int
	DryRun  bool
}

// PassSummary is the per-pass tally.
type PassSummary struct {
	Added         int
	AutoPromoted  int
	Skipped       int
	Errors        int
	LinksProposed int
	DryRun        bool
}

// Print writes a one-line summary tagged with the pass name.
func (s PassSummary) Print(w io.Writer, passName string) {
	tag := ""
	if s.DryRun {
		tag = " (dry-run)"
	}
	fmt.Fprintf(w, "\n%s pass%s: %d added, %d auto-promoted, %d skipped, %d errors, %d see-also links proposed.\n",
		passName, tag, s.Added, s.AutoPromoted, s.Skipped, s.Errors, s.LinksProposed)
}

// secondaryTaskRow is the closed-task row shape secondary_pass walks.
type secondaryTaskRow struct {
	Slug             string
	ProjectID        string
	ProblemStatement string
	HandoffOutput    string
}

// RunSecondary walks closed tasks with handoff_output >= SecondaryMinHandoff
// chars in the given project, skipping any whose source_ref already has
// a pointer or candidate. New candidates land via AddCandidate; >=0.85
// scores auto-promote (with the duplicate-pointer pre-check from the T13
// bug).
func RunSecondary(ctx context.Context, deps PassDeps, opts PassOpts) (PassSummary, error) {
	summary := PassSummary{DryRun: opts.DryRun}

	rows, err := deps.Pool.DB().QueryContext(ctx,
		`SELECT t.slug, c.project_id, t.problem_statement, COALESCE(t.handoff_output, '')
		   FROM proj_current_tasks t
		   JOIN proj_chain_status c ON c.id = t.chain_id
		  WHERE t.status = 'closed'
		    AND c.project_id = ?
		    AND length(COALESCE(t.handoff_output, '')) >= ?
		  ORDER BY t.updated_at DESC`,
		opts.Project, SecondaryMinHandoff)
	if err != nil {
		return summary, fmt.Errorf("secondary: list tasks: %w", err)
	}
	defer rows.Close()

	var tasks []secondaryTaskRow
	for rows.Next() {
		var t secondaryTaskRow
		if err := rows.Scan(&t.Slug, &t.ProjectID, &t.ProblemStatement, &t.HandoffOutput); err != nil {
			return summary, fmt.Errorf("secondary: scan: %w", err)
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return summary, fmt.Errorf("secondary: rows: %w", err)
	}

	fmt.Fprintf(deps.Stdout, "\n─── Secondary pass: %d closed task(s) with handoff ≥%d chars in %s ───\n",
		len(tasks), SecondaryMinHandoff, opts.Project)

	processed := 0
	for _, t := range tasks {
		if opts.Limit > 0 && processed >= opts.Limit {
			break
		}
		processed++

		sourceRef := fmt.Sprintf("%s::%s", t.ProjectID, t.Slug)

		// Skip if pointer or candidate already exists.
		if pid, err := curation.PointerExistsForSourceRef(ctx, deps.Pool, t.ProjectID, "task", sourceRef); err != nil {
			fmt.Fprintf(deps.Stdout, "  %s: pointer-check error: %v\n", t.Slug, err)
			summary.Errors++
			continue
		} else if pid != 0 {
			summary.Skipped++
			continue
		}
		if exists, err := curation.CandidateExistsForSourceRef(ctx, deps.Pool, "task", sourceRef); err != nil {
			fmt.Fprintf(deps.Stdout, "  %s: candidate-check error: %v\n", t.Slug, err)
			summary.Errors++
			continue
		} else if exists {
			summary.Skipped++
			continue
		}

		// Build source material per design doc §5 task_handoff shape.
		handoffExcerpt := truncateRunes(t.HandoffOutput, curation.ExcerptChars)
		material := fmt.Sprintf("%s\n\nHandoff:\n%s", t.ProblemStatement, handoffExcerpt)

		if err := processSecondaryCandidate(ctx, deps, &summary, t, sourceRef, material, opts.DryRun); err != nil {
			fmt.Fprintf(deps.Stdout, "  %s: %v\n", t.Slug, err)
			summary.Errors++
		}
	}
	return summary, nil
}

// processSecondaryCandidate scores and inserts (and maybe promotes) one
// candidate. Split out for readability; the loop body would otherwise
// be 70+ lines of nesting.
func processSecondaryCandidate(
	ctx context.Context, deps PassDeps, summary *PassSummary,
	t secondaryTaskRow, sourceRef, material string, dryRun bool,
) error {
	meta, err := deps.Scorer.Extract(ctx, "task", sourceRef, material)
	if err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	score, err := deps.Scorer.Score(ctx, meta.Question, material)
	if err != nil {
		return fmt.Errorf("score: %w", err)
	}

	display := fmt.Sprintf("%.2f", score)
	if dryRun {
		summary.Added++
		verdict := "would queue"
		if score >= AutoPromoteThreshold {
			verdict = "would auto-promote"
			summary.AutoPromoted++
		}
		fmt.Fprintf(deps.Stdout, "  %s: %s (score=%s)\n", t.Slug, verdict, display)
		return nil
	}

	originRef := t.Slug
	var expiresAt *string
	if score < AutoPromoteThreshold {
		s := "datetime('now', '+30 days')"
		expiresAt = &s
	}

	id, err := curation.AddCandidate(ctx, deps.Pool, curation.CandidateInsert{
		ProjectID:    t.ProjectID,
		SourceType:   "task",
		SourceRef:    sourceRef,
		Question:     meta.Question,
		InvokeWhen:   meta.InvokeWhen,
		Description:  defaultIfEmpty(meta.Description, "Task handoff: "+t.Slug),
		Tags:         nil,
		QualityScore: &score,
		Origin:       "task_handoff",
		OriginRef:    &originRef,
		ExpiresAt:    expiresAt,
	})
	if err != nil {
		return fmt.Errorf("add_candidate: %w", err)
	}
	summary.Added++

	if score < AutoPromoteThreshold {
		fmt.Fprintf(deps.Stdout, "  %s: queued (score=%s) → candidate %d\n", t.Slug, display, id)
		return nil
	}

	// Pre-check before promote — the duplicate-pointer fix.
	if pid, err := curation.PointerExistsForSourceRef(ctx, deps.Pool, t.ProjectID, "task", sourceRef); err != nil {
		return fmt.Errorf("pre-promote pointer check: %w", err)
	} else if pid != 0 {
		// Already promoted in a parallel run or by an earlier insert
		// this loop body just made; skip the promote and don't error.
		fmt.Fprintf(deps.Stdout, "  %s: skipped promote (pointer %d already exists)\n", t.Slug, pid)
		return nil
	}

	pointerID, err := curation.PromoteCandidate(ctx, deps.Pool, id, true)
	if err != nil {
		return fmt.Errorf("promote: %w", err)
	}
	summary.AutoPromoted++

	links, err := curation.ProposeLinks(ctx, deps.Pool, pointerID, meta.Question, 5)
	if err != nil {
		fmt.Fprintf(deps.Stdout, "  %s: auto-promoted → pointer %d (link proposal failed: %v)\n",
			t.Slug, pointerID, err)
		return nil
	}
	summary.LinksProposed += links
	fmt.Fprintf(deps.Stdout, "  %s: auto-promoted (score=%s) → pointer %d + %d link(s)\n",
		t.Slug, display, pointerID, links)
	return nil
}

// RunPrimary mines grounding_events for zero-result gaps. Each event
// becomes a candidate via ZeroResultGapBuilder which reconstructs the
// original query from the session JSONL. Scores tend to be low (the
// source material IS the failed query) but the code path stays
// consistent with secondary.
func RunPrimary(ctx context.Context, deps PassDeps, opts PassOpts) (PassSummary, error) {
	summary := PassSummary{DryRun: opts.DryRun}

	rows, err := deps.Pool.DB().QueryContext(ctx,
		`SELECT id, project_id, session_id, call_id, action
		   FROM grounding_events
		  WHERE results_count = 0 AND next_turn_has_output = 1
		  ORDER BY created_at`)
	if err != nil {
		return summary, fmt.Errorf("primary: list grounding events: %w", err)
	}
	defer rows.Close()

	type gapEvent struct {
		ID        int64
		ProjectID string
		SessionID string
		CallID    string
		Action    string
	}
	var events []gapEvent
	for rows.Next() {
		var e gapEvent
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.SessionID, &e.CallID, &e.Action); err != nil {
			return summary, fmt.Errorf("primary: scan: %w", err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return summary, fmt.Errorf("primary: rows: %w", err)
	}

	fmt.Fprintf(deps.Stdout, "\n─── Primary pass: %d zero-result gap event(s) ───\n", len(events))

	builder, err := deps.Registry.ForOrigin("zero_result_gap")
	if err != nil {
		return summary, fmt.Errorf("primary: %w", err)
	}

	processed := 0
	for _, ev := range events {
		if opts.Limit > 0 && processed >= opts.Limit {
			break
		}
		processed++

		eventRef := fmt.Sprintf("%d", ev.ID)

		// Skip if a candidate for this gap event already exists in any non-expired status.
		var existing int64
		err := deps.Pool.DB().QueryRowContext(ctx,
			`SELECT id FROM curation_candidates
			  WHERE origin = 'zero_result_gap' AND origin_ref = ? AND status != 'expired'
			  LIMIT 1`, eventRef,
		).Scan(&existing)
		if err == nil {
			summary.Skipped++
			continue
		}

		// Build source material via the registered ZeroResultGapBuilder.
		// The builder needs a Candidate with the origin_ref + project_id
		// to do the lookup; construct a partial candidate.
		partial := curation.Candidate{
			ID:        0,
			ProjectID: ev.ProjectID,
			Origin:    "zero_result_gap",
			OriginRef: &eventRef,
		}
		material, err := builder.Build(ctx, deps.Pool, partial)
		if err != nil {
			fmt.Fprintf(deps.Stdout, "  event %d: build skipped (%v)\n", ev.ID, err)
			summary.Skipped++
			continue
		}

		sourceRef := fmt.Sprintf(".claude/vault/learnings/general/gap-%s-%d.md",
			replaceAll(ev.Action, "_", "-"), ev.ID)

		if err := processPrimaryCandidate(ctx, deps, &summary,
			ev.ID, ev.ProjectID, eventRef, sourceRef, material, opts.DryRun); err != nil {
			fmt.Fprintf(deps.Stdout, "  event %d: %v\n", ev.ID, err)
			summary.Errors++
		}
	}
	return summary, nil
}

// processPrimaryCandidate is the per-event scoring+insert helper for
// the primary pass. Symmetric to processSecondaryCandidate but with
// gap-shaped source_ref / origin / tags.
func processPrimaryCandidate(
	ctx context.Context, deps PassDeps, summary *PassSummary,
	eventID int64, projectID, eventRef, sourceRef, material string, dryRun bool,
) error {
	meta, err := deps.Scorer.Extract(ctx, "vault", sourceRef, material)
	if err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	score, err := deps.Scorer.Score(ctx, meta.Question, material)
	if err != nil {
		return fmt.Errorf("score: %w", err)
	}

	display := fmt.Sprintf("%.2f", score)
	if dryRun {
		summary.Added++
		verdict := "would queue"
		if score >= AutoPromoteThreshold {
			verdict = "would auto-promote"
			summary.AutoPromoted++
		}
		fmt.Fprintf(deps.Stdout, "  event %d: %s (score=%s)\n", eventID, verdict, display)
		return nil
	}

	var expiresAt *string
	if score < AutoPromoteThreshold {
		s := "datetime('now', '+30 days')"
		expiresAt = &s
	}

	id, err := curation.AddCandidate(ctx, deps.Pool, curation.CandidateInsert{
		ProjectID:    projectID,
		SourceType:   "vault",
		SourceRef:    sourceRef,
		Question:     meta.Question,
		InvokeWhen:   meta.InvokeWhen,
		Description:  defaultIfEmpty(meta.Description, "Zero-result-gap candidate from grounding_event "+eventRef),
		Tags:         []string{"zero-result-gap"},
		QualityScore: &score,
		Origin:       "zero_result_gap",
		OriginRef:    &eventRef,
		ExpiresAt:    expiresAt,
	})
	if err != nil {
		return fmt.Errorf("add_candidate: %w", err)
	}
	summary.Added++

	if score < AutoPromoteThreshold {
		fmt.Fprintf(deps.Stdout, "  event %d: queued (score=%s) → candidate %d\n", eventID, display, id)
		return nil
	}

	if pid, err := curation.PointerExistsForSourceRef(ctx, deps.Pool, projectID, "vault", sourceRef); err != nil {
		return fmt.Errorf("pre-promote pointer check: %w", err)
	} else if pid != 0 {
		fmt.Fprintf(deps.Stdout, "  event %d: skipped promote (pointer %d already exists)\n", eventID, pid)
		return nil
	}

	pointerID, err := curation.PromoteCandidate(ctx, deps.Pool, id, true)
	if err != nil {
		return fmt.Errorf("promote: %w", err)
	}
	summary.AutoPromoted++

	links, err := curation.ProposeLinks(ctx, deps.Pool, pointerID, meta.Question, 5)
	if err != nil {
		fmt.Fprintf(deps.Stdout, "  event %d: auto-promoted → pointer %d (link proposal failed: %v)\n",
			eventID, pointerID, err)
		return nil
	}
	summary.LinksProposed += links
	fmt.Fprintf(deps.Stdout, "  event %d: auto-promoted (score=%s) → pointer %d + %d link(s)\n",
		eventID, display, pointerID, links)
	return nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func defaultIfEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func replaceAll(s, old, new string) string {
	return strings.ReplaceAll(s, old, new)
}
