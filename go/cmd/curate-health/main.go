// Command curate-health is the one-step diagnostic for the curation
// inference path. Replaces benchmarks/src/bin/inference_health.rs (T11
// of chain curation-go-migration).
//
// Two stages:
//
//	Stage 1 — pure connectivity. Print base URL, call Scorer.Health(),
//	make one round-trip Score("test text"). Catches the silent
//	port-drift class of failure that bit on 2026-05-17 in seconds.
//
//	Stage 2 — real-candidate smoke. Pull N pending task_handoff
//	candidates from the live DB, run Scorer.Extract + Scorer.Score
//	against each, print results. NO DB writes — proves the production
//	pipeline end-to-end on actual data without modifying the backlog.
//
// Usage:
//
//	curate-health --project mcp-servers
//	curate-health --project mcp-servers --limit 5
//	curate-health --project mcp-servers --stage1-only
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"toolkit/internal/db"
	"toolkit/internal/inference/llamacpp"
	"toolkit/internal/knowledge/curation"
	"toolkit/internal/knowledge/curation/sources"
)

const defaultDB = "/home/user/dev/mcp-servers/data/toolkit.db"

func main() {
	var (
		project    string
		limit      int
		stage1Only bool
		dbPath     string
	)
	flag.StringVar(&project, "project", "", "filter Stage 2 candidates by project_id (required unless --stage1-only)")
	flag.IntVar(&limit, "limit", 3, "Stage 2: max candidates to probe (default 3)")
	flag.BoolVar(&stage1Only, "stage1-only", false, "skip Stage 2 (the candidate-scoring probe)")
	flag.StringVar(&dbPath, "db", defaultDB, "path to toolkit.db")
	flag.Parse()

	if !stage1Only && project == "" {
		fmt.Fprintln(os.Stderr, "--project is required (or pass --stage1-only)")
		os.Exit(2)
	}

	ctx := context.Background()
	client := llamacpp.NewFromEnv()
	scorer := curation.NewQwenScorer(client)

	if err := runStage1(ctx, os.Stdout, scorer); err != nil {
		fmt.Fprintf(os.Stderr, "stage 1 failed: %v\n", err)
		os.Exit(1)
	}

	if stage1Only {
		return
	}

	pool, err := db.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	registry := curation.NewBuilderRegistry()
	for _, b := range sources.DefaultBuilders("", "") {
		registry.Register(b)
	}

	if err := runStage2(ctx, os.Stdout, pool, scorer, registry, project, limit); err != nil {
		fmt.Fprintf(os.Stderr, "stage 2 failed: %v\n", err)
		os.Exit(1)
	}
}

// runStage1 probes connectivity. Mirrors the Rust diagnostic's stage 1
// shape (base URL, health, one-shot generate).
func runStage1(ctx context.Context, w writeable, scorer *curation.QwenScorer) error {
	fmt.Fprintln(w, "─── Stage 1: connectivity ───")
	fmt.Fprintf(w, "local_base_url = %s\n", scorer.BaseURL())

	if err := scorer.Health(ctx); err != nil {
		return fmt.Errorf("Health(): %w", err)
	}
	fmt.Fprintln(w, "Health() = ok")

	// One round-trip Extract on canned content proves /v1/chat/completions
	// works under this client's config — not just /health.
	meta, err := scorer.Extract(ctx, "test", "diagnostic::echo",
		"The diagnostic is verifying the chat-completion path is reachable and produces parseable output.")
	if err != nil {
		return fmt.Errorf("round-trip Extract(): %w", err)
	}
	fmt.Fprintf(w, "round-trip Extract() ok: %q\n", meta.Question)
	return nil
}

// runStage2 walks N pending task_handoff candidates, builds source
// material, runs Extract + Score against each, prints results. No DB
// writes.
func runStage2(
	ctx context.Context, w writeable, pool *db.Pool, scorer curation.Scorer,
	registry *curation.BuilderRegistry, project string, limit int,
) error {
	fmt.Fprintln(w)
	fmt.Fprintf(w, "─── Stage 2: scoring smoke on %d pending task_handoff (no DB writes) ───\n", limit)

	cands, err := curation.ListPending(ctx, pool, curation.ListFilter{
		ProjectID:    project,
		Origin:       "task_handoff",
		UnscoredOnly: true,
		Limit:        limit,
	})
	if err != nil {
		return fmt.Errorf("list pending: %w", err)
	}
	if len(cands) == 0 {
		fmt.Fprintf(w, "(no unscored task_handoff candidates in %s — nothing to probe)\n", project)
		return nil
	}

	for i, cand := range cands {
		fmt.Fprintf(w, "\n• [%d/%d] candidate id=%d  source_ref=%s\n",
			i+1, len(cands), cand.ID, cand.SourceRef)
		fmt.Fprintf(w, "  current question: %q\n", cand.Question)

		builder, err := registry.ForOrigin(cand.Origin)
		if err != nil {
			fmt.Fprintf(w, "  ⚠ builder-miss: %v\n", err)
			continue
		}
		material, err := builder.Build(ctx, pool, cand)
		if err != nil {
			fmt.Fprintf(w, "  ⚠ build: %v\n", err)
			continue
		}

		meta, err := scorer.Extract(ctx, cand.SourceType, cand.SourceRef, material)
		if err != nil {
			fmt.Fprintf(w, "  ⚠ extract: %v\n", err)
			continue
		}
		fmt.Fprintf(w, "  extracted question: %s\n", meta.Question)

		score, err := scorer.Score(ctx, meta.Question, material)
		if err != nil {
			fmt.Fprintf(w, "  ⚠ score: %v\n", err)
			continue
		}
		verdict := "would leave pending with score"
		if score >= 0.85 {
			verdict = "would auto-promote"
		}
		fmt.Fprintf(w, "  score: %.2f  (%s)\n", score, verdict)
	}
	return nil
}

// writeable is the minimum io.Writer surface — declared so tests don't
// reach for an "any" placeholder while keeping the signature explicit.
type writeable interface {
	Write(p []byte) (int, error)
}
