package main

// Production-shape exercise loop for the chain-assessment dispatch
// action. Mirrors the path that
// mcp__toolkit-server__measure.classify_chain_task_proportionality
// takes when the agent invokes it during chain-assessment skill gate-4
// evaluation.
//
// For each input task spec:
//  1. DeriveTeamContext(...)
//  2. Compose `{task_spec}\n\nTeam context:\n{prose}`
//  3. classify_chain_task_proportionality dispatch
//  4. RecordBenchmarkDispatch so the row lands in benchmark_results
//
// All four steps are encapsulated inside
// measure.HandleClassifyChainTaskProportionality, so this loop is
// thin.
//
// Folded into toolkit-server as `toolkit-server
// exercise-chain-assessment-dispatch` by harvest-the-consolidation T4.
// Ported from benchmarks/src/bin/exercise_chain_assessment_dispatch.rs
// per chain rust-retirement-and-db-hardening T5 Phase 4 (Option A).

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"toolkit/internal/db"
	"toolkit/internal/inference/router"
	"toolkit/internal/measure"
	"toolkit/internal/rubric"

	_ "modernc.org/sqlite"
)

const exerciseChainAssessmentProjectID = "mcp-servers"

// exerciseChainAssessmentTaskSpecs is the five plausible task-spec
// inputs that span the rubric label space. Sized at 5 so each
// invocation lands >= the T6 acceptance bar (>=3 production calls)
// with cushion. Verbatim from Rust source.
var exerciseChainAssessmentTaskSpecs = []string{
	"Task: 'Add a per-rubric Qwen smoke for the new agentic-audit-pillar " +
		"multi-class extension. Estimated effort: ~half-day.' Acceptance: " +
		"gold corpus, smoke binary, verdict in vault.",
	"Task: 'Refactor the dispatch tool's per-action allowlist into a " +
		"declarative table.' Estimated effort: 2 sessions. Acceptance: " +
		"all dispatch arms compile against the table; 0 regressions.",
	"Task: 'Conduct an exhaustive audit of every TOML schema across " +
		"both repos for unused fields. 60+ schemas total. Estimated " +
		"effort: 1 week.' Acceptance: per-schema compliance report.",
	"Task: 'Decide whether to consolidate the path-resolution helpers " +
		"across structure-lint, reference-checker, and forge-lib. " +
		"Acceptance: extract / leave / shared-helper verdict per site.'",
	"Task: 'Run a 30-user usability study on the dashboard radar-chart. " +
		"Estimated effort: 3 weeks.' Acceptance: aggregated findings report.",
}

// runExerciseChainAssessmentDispatch drives the
// `exercise-chain-assessment-dispatch` subcommand. Returns 0 on
// success; 1 on any error.
func runExerciseChainAssessmentDispatch(args []string) int {
	fs := flag.NewFlagSet("exercise-chain-assessment-dispatch", flag.ContinueOnError)
	dbPath := fs.String("db", defaultToolkitDBPath(), "path to toolkit.db")
	rubricsDir := fs.String("rubrics-dir", defaultRubricsDir(), "path to blueprints/rubrics dir")
	llamaURL := fs.String("llama-url", "http://localhost:8081", "llama.cpp server URL (qwen-vault-smoke port)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: toolkit-server exercise-chain-assessment-dispatch [--db PATH] [--rubrics-dir PATH] [--llama-url URL]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}

	fmt.Printf("→ db:           %s\n", *dbPath)
	fmt.Printf("→ project_id:   %s\n", exerciseChainAssessmentProjectID)
	fmt.Printf("→ rubrics_dir:  %s\n", *rubricsDir)

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

	fmt.Printf("→ task specs:   %d\n", len(exerciseChainAssessmentTaskSpecs))
	fmt.Println()

	deps := measure.ClassifyDeps{
		Pool:    pool,
		Router:  r,
		Rubrics: registry,
		Project: exerciseChainAssessmentProjectID,
	}
	ctx := context.Background()

	for i, spec := range exerciseChainAssessmentTaskSpecs {
		params, err := json.Marshal(map[string]string{"task_spec": spec})
		if err != nil {
			fmt.Fprintf(os.Stderr, "marshal params %d: %v\n", i, err)
			return 1
		}
		resp, err := measure.HandleClassifyChainTaskProportionality(ctx, deps, params)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  spec %d: dispatch error: %v\n", i, err)
			continue
		}
		if resp.Error != "" {
			fmt.Fprintf(os.Stderr, "  spec %d: response error: %s\n", i, resp.Error)
			continue
		}
		fmt.Printf("  spec %d: label=%-16s  latency=%d ms  model=%s\n",
			i, resp.Label, resp.LatencyMS, resp.ModelName)
	}

	fmt.Println()
	fmt.Printf("All %d dispatches recorded; query benchmark_results to verify.\n", len(exerciseChainAssessmentTaskSpecs))
	return 0
}
