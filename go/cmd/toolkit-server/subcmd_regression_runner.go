package main

// Parametric Classify regression harness. Replaces
// benchmarks/src/bin/regression_runner.rs's per-rubric dispatch with
// one Go path that loads gold scenarios from JSONL + runs them through
// rubric.Registry, recording each row with run_shape='regression'.
//
// **Workflow served:** end-to-end regression on ONE Classify rubric
// against its hand-authored gold corpus. Records per-scenario
// benchmark_results rows tagged regression + prints a verdict
// (accuracy pass-bar 0.70).
//
// **Invocation pattern:** `toolkit-server regression-runner
// --rubric <name> [--gold-dir PATH] [--db PATH] [--rubrics-dir PATH]
// [--llama-url URL] [--project-id ID]`.
//
// **chain-assessment two-pass run:** PASS A uses the scenario's
// embedded "Team context:" block (manual override); PASS B re-derives
// team context from live telemetry via measure.DeriveTeamContext.
// PASS A's accuracy is the pass-bar gate; PASS B is observational.
//
// Folded into toolkit-server as `toolkit-server regression-runner` by
// harvest-the-consolidation T4. Ported from
// benchmarks/src/bin/regression_runner.rs per chain
// rust-retirement-and-db-hardening T5 Phase 5.

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"toolkit/internal/benchmarks"
	"toolkit/internal/db"
	"toolkit/internal/inference/router"
	"toolkit/internal/measure"
	"toolkit/internal/obs"
	"toolkit/internal/qwenctx"
	"toolkit/internal/rubric"

	_ "modernc.org/sqlite"
)

const regRunnerAccuracyPassBar = 0.70

// runRegressionRunner drives the `regression-runner` subcommand.
// Returns 0 on accuracy pass-bar met; 1 otherwise; 2 on flag error.
func runRegressionRunner(args []string) int {
	fs := flag.NewFlagSet("regression-runner", flag.ContinueOnError)
	rubricSlug := fs.String("rubric", "", "rubric to regress (required)")
	dbPath := fs.String("db", defaultToolkitDBPath(), "path to toolkit.db")
	rubricsDir := fs.String("rubrics-dir", defaultRubricsDir(), "blueprints/rubrics dir")
	llamaURL := fs.String("llama-url", "http://localhost:8081", "llama.cpp server URL")
	goldDir := fs.String("gold-dir", "", "gold corpus dir (default: $HOME/dev/seed-packet/process-docs/adhoc/a1-rubric-smoke/gold)")
	projectID := fs.String("project-id", "mcp-servers", "project_id for benchmark_results scoping")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: toolkit-server regression-runner --rubric NAME [--db PATH] [--rubrics-dir PATH] [--llama-url URL] [--gold-dir PATH] [--project-id ID]")
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

	fmt.Printf("→ db:           %s\n", *dbPath)
	fmt.Printf("→ project_id:   %s\n", *projectID)
	fmt.Printf("→ rubric:       %s\n", *rubricSlug)
	fmt.Printf("→ model:        %s\n", r.ModelName())
	fmt.Printf("→ scenarios:    %d\n", len(scenarios))
	fmt.Println()

	ctx := qwenctx.WithTaskID(context.Background(), *rubricSlug)
	if *rubricSlug == "chain-assessment" {
		return regRunnerRunChainAssessment(ctx, scenarios, def, r, pool, *projectID)
	}
	return regRunnerRunStandard(ctx, *rubricSlug, scenarios, def, r, pool, *projectID)
}

// regRunnerRow holds the per-scenario verdict for the standard
// (non-chain-assessment) summary.
type regRunnerRow struct {
	slug    string
	gold    string
	verdict string
}

func regRunnerMark(g, v string) string {
	if g == v {
		return "✓"
	}
	return "✗"
}

func regRunnerGoldLabel(g benchmarks.ClassifyGold) string {
	switch g.Kind {
	case benchmarks.GoldSingleClass:
		return g.SingleLabel
	case benchmarks.GoldUnclassifiable:
		return "unclassifiable"
	case benchmarks.GoldMultiClass:
		return strings.Join(g.MultiLabel, "|")
	}
	return ""
}

func regRunnerVerdictLabel(labels []string, unclassifiable bool) string {
	if unclassifiable {
		return "unclassifiable"
	}
	if len(labels) == 0 {
		return ""
	}
	return strings.Join(labels, "|")
}

// regRunnerDispatchClassify runs ONE Classify scenario through the
// registered rubric, records a regression row, and returns
// (verdict, latency).
func regRunnerDispatchClassify(ctx context.Context, def rubric.RubricDef, text string, r *router.Router, pool *db.Pool, project, rubricSlug string) (string, int64, error) {
	system, user := rubric.ComposeClassify(def, text)
	start := time.Now()
	genResult, err := r.Generate(ctx, user, system)
	latencyMS := time.Since(start).Milliseconds()
	if err != nil {
		return "", latencyMS, err
	}

	labels, unclassifiable := benchmarks.ParseClassifyResponse(genResult.Text, def.OutputEnum)
	verdict := regRunnerVerdictLabel(labels, unclassifiable)

	first := ""
	if len(labels) > 0 {
		first = labels[0]
	}
	dbResult := db.ClassifyResult{
		Label:        first,
		RawResponse:  genResult.Text,
		LatencyMS:    latencyMS,
		InputTokens:  genResult.InputTokens,
		OutputTokens: genResult.OutputTokens,
		InvocationOK: verdict != "",
		RunShape:     "regression",
	}
	if recordErr := db.RecordBenchmarkDispatch(ctx, pool, project, rubricSlug, r.ModelName(), dbResult); recordErr != nil {
		obs.Logger(ctx).Warn("regression-runner: record dispatch failed",
			slog.String("err", recordErr.Error()),
			slog.String("rubric", rubricSlug),
		)
	}
	return verdict, latencyMS, nil
}

func regRunnerRunStandard(ctx context.Context, rubricSlug string, scenarios []benchmarks.ClassifyScenario, def rubric.RubricDef, r *router.Router, pool *db.Pool, project string) int {
	var rows []regRunnerRow
	for _, s := range scenarios {
		gold := regRunnerGoldLabel(s.Gold)
		fmt.Printf("  %s... ", s.Slug)
		verdict, latencyMS, err := regRunnerDispatchClassify(ctx, def, s.Text, r, pool, project, rubricSlug)
		if err != nil {
			fmt.Printf("error: %v\n", err)
			continue
		}
		fmt.Printf("%-58s  gold=%-16s  %s %-16s (%d ms)\n",
			s.Slug, gold, regRunnerMark(gold, verdict), verdict, latencyMS)
		rows = append(rows, regRunnerRow{slug: s.Slug, gold: gold, verdict: verdict})
	}
	return regRunnerPrintStandardSummary(rubricSlug, rows)
}

func regRunnerPrintStandardSummary(rubricSlug string, rows []regRunnerRow) int {
	total := len(rows)
	if total == 0 {
		fmt.Println("no rows recorded")
		return 1
	}
	correct := 0
	for _, r := range rows {
		if r.gold == r.verdict {
			correct++
		}
	}
	acc := float64(correct) / float64(total)

	fmt.Println()
	fmt.Printf("── Summary (%s) ──\n", rubricSlug)
	fmt.Printf("  accuracy: %d/%d = %.1f%%  → %s\n", correct, total, acc*100, passFailString(acc >= regRunnerAccuracyPassBar))

	var honesty []regRunnerRow
	for _, r := range rows {
		if r.gold == "unclassifiable" {
			honesty = append(honesty, r)
		}
	}
	if len(honesty) > 0 {
		h := 0
		for _, r := range honesty {
			if r.verdict == "unclassifiable" {
				h++
			}
		}
		fmt.Printf("  honesty (unclassifiable): %d/%d = %.1f%%  → %s\n",
			h, len(honesty), float64(h)/float64(len(honesty))*100, passFailString(h == len(honesty)))
	}

	fmt.Println()
	var misses []regRunnerRow
	for _, r := range rows {
		if r.gold != r.verdict {
			misses = append(misses, r)
		}
	}
	if len(misses) == 0 {
		fmt.Println("  no misses.")
	} else {
		fmt.Println("── Misses ──")
		for _, r := range misses {
			fmt.Printf("  %s : gold=%s verdict=%s\n", r.slug, r.gold, r.verdict)
		}
	}

	if acc < regRunnerAccuracyPassBar {
		return 1
	}
	return 0
}

// regRunnerCARow is the chain-assessment two-pass per-scenario record.
type regRunnerCARow struct {
	slug          string
	gold          string
	verdictA      string
	verdictB      string
	manualContext string
	autoContext   string
}

// regRunnerCASplit pulls task_spec + team_context_block apart at the
// "Team context:" substring. Returns (task_spec, manual_team_context).
func regRunnerCASplit(text string) (string, string) {
	const marker = "Team context:"
	idx := strings.Index(text, marker)
	if idx < 0 {
		return text, ""
	}
	return strings.TrimSpace(text[:idx]), strings.TrimSpace(text[idx+len(marker):])
}

func regRunnerCACompose(taskSpec, teamContext string) string {
	return fmt.Sprintf("%s\n\nTeam context:\n%s",
		strings.TrimSpace(taskSpec), strings.TrimSpace(teamContext))
}

func regRunnerRunChainAssessment(ctx context.Context, scenarios []benchmarks.ClassifyScenario, def rubric.RubricDef, r *router.Router, pool *db.Pool, project string) int {
	var rows []regRunnerCARow

	for _, s := range scenarios {
		gold := regRunnerGoldLabel(s.Gold)
		taskSpec, manualCtx := regRunnerCASplit(s.Text)

		fmt.Printf("  %s... ", s.Slug)

		passA, latencyA, errA := regRunnerDispatchClassify(ctx, def, regRunnerCACompose(taskSpec, manualCtx), r, pool, project, "chain-assessment")
		if errA != nil {
			passA = fmt.Sprintf("error: %v", errA)
		}

		// PASS B — derive team context from live telemetry, then re-dispatch.
		tc, deriveErr := measure.DeriveTeamContext(ctx, pool, "", project, taskSpec)
		if deriveErr != nil {
			fmt.Fprintf(os.Stderr, "derive team context for %s: %v\n", s.Slug, deriveErr)
			return 1
		}
		autoCtx := tc.Prose()
		passB, latencyB, errB := regRunnerDispatchClassify(ctx, def, regRunnerCACompose(taskSpec, autoCtx), r, pool, project, "chain-assessment")
		if errB != nil {
			passB = fmt.Sprintf("error: %v", errB)
		}

		markA := regRunnerMark(gold, passA)
		markB := regRunnerMark(gold, passB)
		fmt.Printf("  %-48s  gold=%-16s  A: %s %-16s (%d ms)   B: %s %-16s (%d ms)\n",
			s.Slug, gold, markA, passA, latencyA, markB, passB, latencyB)

		rows = append(rows, regRunnerCARow{
			slug:          s.Slug,
			gold:          gold,
			verdictA:      passA,
			verdictB:      passB,
			manualContext: manualCtx,
			autoContext:   autoCtx,
		})
	}

	return regRunnerPrintChainAssessmentSummary(rows)
}

func regRunnerPrintChainAssessmentSummary(rows []regRunnerCARow) int {
	total := len(rows)
	if total == 0 {
		fmt.Println("no rows recorded")
		return 1
	}
	accA, accB := 0, 0
	for _, r := range rows {
		if r.gold == r.verdictA {
			accA++
		}
		if r.gold == r.verdictB {
			accB++
		}
	}
	accAPct := float64(accA) / float64(total)
	accBPct := float64(accB) / float64(total)

	fmt.Println()
	fmt.Println("── Verdict (chain-assessment) ──")
	vA := "FAIL"
	if accAPct >= regRunnerAccuracyPassBar {
		vA = "PASS"
	}
	fmt.Printf("  PASS A (manual override):  %d/%d = %.1f%%  → %s\n",
		accA, total, accAPct*100, vA)
	vB := "OBSERVATIONAL (synthetic corpus has no vault overlap)"
	if accBPct >= regRunnerAccuracyPassBar {
		vB = "PASS"
	}
	fmt.Printf("  PASS B (auto-derivation):  %d/%d = %.1f%%  → %s\n",
		accB, total, accBPct*100, vB)
	fmt.Println()

	fmt.Println("── Drift summary ──")
	for _, r := range rows {
		if r.verdictA == r.verdictB {
			continue
		}
		fmt.Printf("  %s (%s): A=%s B=%s\n", r.slug, r.gold, r.verdictA, r.verdictB)
		fmt.Printf("    manual: %s\n", regRunnerFirstLine(r.manualContext))
		fmt.Printf("    auto:   %s\n", regRunnerFirstLine(r.autoContext))
	}

	if accAPct < regRunnerAccuracyPassBar {
		return 1
	}
	return 0
}

func regRunnerFirstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}
