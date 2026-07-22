package main

// Generic per-rubric Classify smoke. Replaces the 9 Rust-side
// per-rubric smoke arms in benchmarks/src/bin/smoke_rubric.rs with a
// single Go path that dispatches scenarios by rubric slug.
//
// ## Intended use
//
// **Workflow served:** smoke-test ONE Classify rubric end-to-end
// against its hand-authored gold corpus. Records per-scenario
// benchmark_results rows + prints a verdict (accuracy / honesty
// pass-bar).
//
// **Invocation pattern:** `toolkit-server smoke-classify-rubric
// --rubric <name> [--gold-dir PATH] [--db PATH] [--rubrics-dir PATH]
// [--llama-url URL] [--project-id ID]`.
//
// Adding a new rubric port = adding one gold JSONL under
// `seed-packet/process-docs/adhoc/a1-rubric-smoke/gold/<rubric>.jsonl`
// and one line in benchmarks.KnownE4Rubrics. No new binary, no new
// dispatch arm.
//
// **Pass bar:** accuracy >= 70%; honesty (Unclassifiable gold cases) >= 90%.
//
// Folded into toolkit-server as `toolkit-server smoke-classify-rubric`
// by harvest-the-consolidation T4. Ported from
// benchmarks/src/bin/smoke_rubric.rs per chain
// rust-retirement-and-db-hardening T5 Phase 6b.

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"toolkit/internal/benchmarks"
	"toolkit/internal/db"
	"toolkit/internal/inference/router"
	"toolkit/internal/obs"
	"toolkit/internal/qwenctx"
	"toolkit/internal/rubric"

	_ "modernc.org/sqlite"
)

const (
	smokeClassifyAccuracyPassBar = 0.70
	smokeClassifyHonestyPassBar  = 0.90
)

// runSmokeClassifyRubric drives the `smoke-classify-rubric`
// subcommand. Returns 0 on accuracy + honesty pass-bars met; 1
// otherwise; 2 on flag/setup error.
func runSmokeClassifyRubric(args []string) int {
	fs := flag.NewFlagSet("smoke-classify-rubric", flag.ContinueOnError)
	rubricSlug := fs.String("rubric", "", "rubric to smoke (required)")
	dbPath := fs.String("db", defaultToolkitDBPath(), "path to toolkit.db")
	rubricsDir := fs.String("rubrics-dir", defaultRubricsDir(), "blueprints/rubrics dir")
	llamaURL := fs.String("llama-url", "http://localhost:8081", "llama.cpp server URL")
	goldDir := fs.String("gold-dir", "", "gold corpus dir (default: $HOME/dev/seed-packet/process-docs/adhoc/a1-rubric-smoke/gold)")
	projectID := fs.String("project-id", "mcp-servers", "project_id to scope benchmark_results writes")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: toolkit-server smoke-classify-rubric --rubric NAME [--db PATH] [--rubrics-dir PATH] [--llama-url URL] [--gold-dir PATH] [--project-id ID]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}

	if *rubricSlug == "" {
		fmt.Fprintln(os.Stderr, "required: --rubric <name>")
		fmt.Fprintf(os.Stderr, "known rubrics: %v\n", benchmarks.KnownE4Rubrics)
		return 2
	}
	if !benchmarks.IsKnownE4Rubric(*rubricSlug) {
		fmt.Fprintf(os.Stderr, "unknown rubric %q; known: %v\n", *rubricSlug, benchmarks.KnownE4Rubrics)
		return 2
	}

	scenarios, err := benchmarks.LoadGoldScenarios(*rubricSlug, *goldDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load gold: %v\n", err)
		return 1
	}

	installProjectionsFoldHook()
	pool, err := db.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db %s: %v\n", *dbPath, err)
		return 1
	}
	defer pool.Close()

	r, err := router.New(*llamaURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init router: %v\n", err)
		return 1
	}
	registry, err := rubric.NewRegistry(*rubricsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load rubrics from %s: %v\n", *rubricsDir, err)
		return 1
	}
	def, ok := registry.Get(*rubricSlug)
	if !ok {
		fmt.Fprintf(os.Stderr, "rubric %q not found in registry at %s\n", *rubricSlug, *rubricsDir)
		return 1
	}

	fmt.Printf("→ rubric:        %s\n", *rubricSlug)
	fmt.Printf("→ db:            %s\n", *dbPath)
	fmt.Printf("→ project_id:    %s\n", *projectID)
	fmt.Printf("→ scenarios:     %d\n", len(scenarios))
	fmt.Printf("→ model:         %s\n", r.ModelName())
	fmt.Println()

	ctx := qwenctx.WithTaskID(context.Background(), *rubricSlug)

	passed := 0
	var accSum, honSum float64
	accN, honN := 0, 0
	var anyUnclassifiableGold bool

	for _, s := range scenarios {
		fmt.Printf("  %-50s … ", s.Slug)
		system, user := rubric.ComposeClassify(def, s.Text)
		start := time.Now()
		genResult, genErr := r.Generate(ctx, user, system)
		latencyMS := time.Since(start).Milliseconds()
		if genErr != nil {
			fmt.Printf("error: %v\n", genErr)
			continue
		}

		labels, unclassifiable := benchmarks.ParseClassifyResponse(genResult.Text, def.OutputEnum)
		result := benchmarks.GradeClassify(labels, s.Gold, unclassifiable)

		accuracy := benchmarks.GradeClassifyAccuracy(result)
		accSum += accuracy
		accN++

		var honestyMaybe *float64
		if s.Gold.Kind == benchmarks.GoldUnclassifiable {
			anyUnclassifiableGold = true
			h := benchmarks.GradeClassifyHonesty(unclassifiable, len(labels))
			honSum += h
			honN++
			honestyMaybe = &h
		}

		dispatched := smokeClassifyLabelDisplay(labels, unclassifiable)
		if result.Matched {
			fmt.Printf("✓ %-20s (%d ms)\n", dispatched, latencyMS)
			passed++
		} else {
			fmt.Printf("✗ got=%-16s gold=%s (%d ms)\n", dispatched, smokeClassifyGoldDisplay(s.Gold), latencyMS)
		}

		dbResult := db.ClassifyResult{
			Label:        smokeClassifyFirstLabel(labels),
			RawResponse:  genResult.Text,
			LatencyMS:    latencyMS,
			InputTokens:  genResult.InputTokens,
			OutputTokens: genResult.OutputTokens,
			InvocationOK: result.Matched,
			RunShape:     "smoke",
		}
		if err := db.RecordBenchmarkDispatch(ctx, pool, *projectID, *rubricSlug, r.ModelName(), dbResult); err != nil {
			obs.Logger(ctx).Warn("smoke-classify-rubric: record dispatch failed",
				slog.String("err", err.Error()),
				slog.String("slug", s.Slug),
			)
		}
		_ = honestyMaybe
	}

	total := len(scenarios)
	accMean := 0.0
	if accN > 0 {
		accMean = accSum / float64(accN)
	}

	fmt.Println()
	fmt.Println("── Verdict ──")
	fmt.Printf("  rubric:           %s\n", *rubricSlug)
	fmt.Printf("  scenarios:        %d\n", total)
	fmt.Printf("  passed:           %d\n", passed)
	fmt.Printf("  failed:           %d\n", total-passed)
	fmt.Printf("  accuracy mean:    %.3f  (pass-bar %.2f)  → %s\n",
		accMean, smokeClassifyAccuracyPassBar, passFailString(accMean >= smokeClassifyAccuracyPassBar))

	accuracyPass := accMean >= smokeClassifyAccuracyPassBar
	honestyPass := true
	if honN > 0 {
		honMean := honSum / float64(honN)
		fmt.Printf("  honesty mean:     %.3f  (pass-bar %.2f, n=%d)  → %s\n",
			honMean, smokeClassifyHonestyPassBar, honN, passFailString(honMean >= smokeClassifyHonestyPassBar))
		honestyPass = honMean >= smokeClassifyHonestyPassBar
	} else if anyUnclassifiableGold {
		fmt.Println("  honesty mean:     n/a (no Unclassifiable scenarios graded)")
	} else {
		fmt.Println("  honesty mean:     n/a (no Unclassifiable gold cases)")
	}

	if !accuracyPass || !honestyPass {
		return 1
	}
	return 0
}

func smokeClassifyLabelDisplay(labels []string, unclassifiable bool) string {
	if unclassifiable {
		return "unclassifiable"
	}
	if len(labels) == 0 {
		return ""
	}
	if len(labels) == 1 {
		return labels[0]
	}
	out := labels[0]
	for _, l := range labels[1:] {
		out += "|" + l
	}
	return out
}

func smokeClassifyGoldDisplay(g benchmarks.ClassifyGold) string {
	switch g.Kind {
	case benchmarks.GoldSingleClass:
		return g.SingleLabel
	case benchmarks.GoldUnclassifiable:
		return "unclassifiable"
	case benchmarks.GoldMultiClass:
		out := g.MultiLabel[0]
		for _, l := range g.MultiLabel[1:] {
			out += "|" + l
		}
		return out
	}
	return ""
}

func smokeClassifyFirstLabel(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	return labels[0]
}
