package main

// One-shot tool that tags the 9 A1 sub-chain smoke run_ids with their
// task_id in proj_benchmark_results. Forged by chain
// mcp-servers/extract-now-rubric-foundation T6, folded into
// toolkit-server as a subcommand by harvest-the-consolidation T4.
//
// Idempotent: each UPDATE has `WHERE task_id IS NULL`. Re-running is a
// no-op once the rows are tagged.
//
// Usage:
//
//	toolkit-server backfill-a1-rubric-names [--db PATH]
//
// Defaults the DB path to ~/dev/mcp-servers/data/toolkit.db.
//
// Ported from the Rust binary benchmarks/src/bin/backfill_a1_rubric_names.rs
// per chain rust-retirement-and-db-hardening T5 Phase 2 (Option A).
// Behavior is preserved byte-for-byte modulo Go-idiomatic error handling
// and output formatting (which itself mirrors the Rust output verbatim).
//
// The mapping is sourced from the per-task handoff_outputs in the A1
// sub-chain (toolkit DB chain 548). When future per-rubric smoke runs
// land, append entries here OR write a similar one-shot per chain.

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"

	_ "modernc.org/sqlite"
)

// rubricMapping is one (run_id → task_id) row from the A1 sub-chain.
type rubricMapping struct {
	RunID  string
	TaskID string
}

// a1RunRubricMap is the 9 (run_id, task_id) pairs from the A1 sub-chain
// smoke runs. Mirrors A1_RUN_RUBRIC_MAP in the Rust source.
var a1RunRubricMap = []rubricMapping{
	{"9b88b3e8-e3f2-4776-b783-abef875061b8", "artifact-review"},
	{"fae1d6a4-bc1a-40fb-8909-49b5074d9f2a", "agentic-audit"},
	{"e3736777-a53f-49d5-a484-c67cb17bdcaa", "retirement-signal"},
	{"482eacaf-4e79-4405-9233-ccd37717bb39", "pre-commit-failure"},
	{"c084334a-b9e0-45c5-9fd5-7fe992ee5b24", "tiered-context"},
	{"a18ce9bd-af8f-417a-b127-0afd3176e99c", "refactoring-heuristics"},
	{"6873d030-244f-4dd5-a117-60beb3491f40", "session-routing"},
	{"0dd70a51-00e4-475a-97c9-af2f7f16a1e0", "chain-assessment"},
	{"1a888777-d1a3-4e89-a399-b39bedb00273", "pre-context-summarization"},
}

func defaultBackfillA1DBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/home/user/dev/mcp-servers/data/toolkit.db"
	}
	return home + "/dev/mcp-servers/data/toolkit.db"
}

// runBackfillA1 iterates the mappings and UPDATEs each row matching
// (run_id, task_id IS NULL) with the target task_id. Writes per-mapping
// progress + a final summary + per-task row counts to out. Returns nil
// on success; first error otherwise (terminates iteration). Idempotent:
// re-running against a fully-tagged DB performs zero writes.
func runBackfillA1(ctx context.Context, db *sql.DB, mappings []rubricMapping, out io.Writer) error {
	var totalUpdated int64
	for _, m := range mappings {
		res, err := db.ExecContext(ctx,
			`UPDATE proj_benchmark_results SET task_id = ?
			 WHERE run_id = ? AND task_id IS NULL`,
			m.TaskID, m.RunID,
		)
		if err != nil {
			return fmt.Errorf("update %s → %s: %w", m.RunID, m.TaskID, err)
		}
		updated, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("rows affected for %s: %w", m.RunID, err)
		}
		if updated > 0 {
			fmt.Fprintf(out, "  %s → %s: tagged %d rows\n", m.RunID, m.TaskID, updated)
		} else {
			fmt.Fprintf(out, "  %s → %s: already tagged (skipped)\n", m.RunID, m.TaskID)
		}
		totalUpdated += updated
	}
	fmt.Fprintf(out, "\nDone. Tagged %d rows total.\n", totalUpdated)

	// Per-task row counts as a sanity check (mirrors the Rust output).
	rows, err := db.QueryContext(ctx, `
		SELECT task_id, COUNT(*) FROM proj_benchmark_results
		WHERE task_id IS NOT NULL GROUP BY task_id ORDER BY task_id`)
	if err != nil {
		return fmt.Errorf("query per-task counts: %w", err)
	}
	defer rows.Close()
	fmt.Fprintf(out, "\nPer-task row counts in proj_benchmark_results:\n")
	for rows.Next() {
		var name string
		var count int64
		if err := rows.Scan(&name, &count); err != nil {
			return fmt.Errorf("scan count row: %w", err)
		}
		fmt.Fprintf(out, "  %-30s %5d\n", name, count)
	}
	return rows.Err()
}

// runBackfillA1RubricNames drives the `backfill-a1-rubric-names`
// subcommand. Returns 0 on success; 1 on any error.
func runBackfillA1RubricNames(args []string) int {
	fs := flag.NewFlagSet("backfill-a1-rubric-names", flag.ContinueOnError)
	dbPath := fs.String("db", defaultBackfillA1DBPath(), "path to toolkit.db")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: toolkit-server backfill-a1-rubric-names [--db PATH]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}
	fmt.Printf("→ db: %s\n", *dbPath)

	if _, err := os.Stat(*dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "backfill-a1-rubric-names: stat %s: %v\n", *dbPath, err)
		return 1
	}
	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backfill-a1-rubric-names: open %s: %v\n", *dbPath, err)
		return 1
	}
	defer db.Close()

	if err := runBackfillA1(context.Background(), db, a1RunRubricMap, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "backfill-a1-rubric-names: %v\n", err)
		return 1
	}
	return 0
}
