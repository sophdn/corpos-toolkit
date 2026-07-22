// chain-replay-verify proves the forge(chain, tasks=[full-objects])
// path is a faithful identity transform for a chain's authored content.
// It reads a production chain's row + every task row from the live DB
// (read-only), re-forges an equivalent chain via the T7 full-object
// shape into a throwaway temp DB, and compares the resulting rows
// column-by-column. Any drift in an authored-content column fails the
// run loudly, naming the divergent (entity, column) and both values.
//
// This closes the strictest reading of acceptance criterion (f) of
// chain work-batching-and-forge-templates T7: the DogFoodMirroring
// smoke (go/internal/forge/chain_with_tasks_test.go) proved the SHAPE
// on a 2-task in-test fixture; this verifies byte-identity on the full
// 9-task set against actual production data, where a later regression in
// parseFullObjectEntry or the projection fold would surface.
//
// Why a CLI and not a unit test: it depends on external, mutable state
// (the live production DB), which doesn't belong in the hermetic test
// suite — Go's test cache tracks source + tracked files, not an external
// DB read, so a cached PASS could mask a later regression. As a discrete
// precommit stage it re-runs every commit and can't be cached stale.
//
// Comparison scope — authored-content columns only:
//
//	chain (proj_chain_status):   output, completion_condition
//	task  (proj_current_tasks):  slug, position, problem_statement,
//	                             acceptance_criteria, context_required,
//	                             constraints
//
// Excluded by design (not authored-at-creation content): auto-assigned
// ids (id, chain_id), timestamps (created_at, updated_at), event
// pointers (last_event_id, last_event_ts), and mutable lifecycle state
// that necessarily differs between a closed production chain and a
// freshly-forged replay (status, closure_summary, the task-count
// columns, handoff_output, commit_sha, moved_on, originated_chain_id).
//
// Known gap (per T3 constraints): a production task forged pre-T7 has no
// recoverable per-task rationale in its event payload, and rationale is
// mandatory on the full-object shape — so the replay supplies a
// placeholder. Rationale is not a proj_current_tasks column, so it never
// reaches the row comparison.
//
// acceptance_criteria round-trips through the projection's
// acceptanceCriteriaJoined helper, which joins items with "\n- ". Since
// strings.Join(strings.Split(s, sep), sep) == s for any s, splitting the
// stored value on "\n- " and re-forging reproduces the exact bytes when
// the current join is unchanged — and a changed separator surfaces as a
// diff (the split sep here is independent of the join code).
//
// Exit codes: 0 = OK or SKIP (live DB / chain absent — keeps a fresh
// checkout green); 1 = drift detected or a hard error.
//
// Run shape:
//
//	go run ./go/cmd/chain-replay-verify
//	go run ./go/cmd/chain-replay-verify --chain some-chain --db /path/to/toolkit.db
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"toolkit/internal/construct"
	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/forge/registry"
	"toolkit/internal/projections"
)

const (
	defaultChain   = "work-batching-and-forge-templates"
	defaultProject = "corpos-toolkit"
	// acSeparator is the join string the tasks/suggestions/bugs
	// projection's acceptanceCriteriaJoined uses between list items.
	acSeparator = "\n- "
)

// defaultDBPath resolves the canonical ledger the same way go/launch.sh
// does — TOOLKIT_DB, else $XDG_DATA_HOME (or ~/.local/share) — rather than
// hardcoding an absolute path. The previous hardcoded default outlived the
// tree it named: the DB moved out of the mcp-servers working tree to the
// service-owned location in 2026-06 (finish-sophdn-repo-split T6), and this
// stage silently SKIPped on every commit from then until it was noticed.
// Deriving the path means the next relocation moves this with it.
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

// canonicalRootDir is the last-resort blueprints/forge-schemas location for
// findSchemasDir, used only when the cwd walk finds nothing (the gate runs
// from the repo root, so the walk normally wins).
func canonicalRootDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "dev", "corpos-toolkit")
}

// errSkip signals a non-failure early exit (live DB or chain absent).
var errSkip = errors.New("skip")

type chainContent struct {
	output              string
	completionCondition string
}

type taskContent struct {
	slug               string
	position           int
	problemStatement   string
	acceptanceCriteria string
	contextRequired    string
	constraints        string
}

// (Stage 4 Slice 5) The pre-construct replay used a hand-rolled forge param
// envelope (replayChainParams + replayTaskEntry). The re-forge now goes
// through construct.Create with construct.ChainWithTasksInput +
// construct.ChainTaskInput, so those local types are gone — the typed
// Input ships with the construct package.

func main() {
	dbPath := flag.String("db", defaultDBPath(), "path to the live toolkit DB (read-only)")
	chainSlug := flag.String("chain", defaultChain, "chain slug to replay-verify")
	project := flag.String("project", defaultProject, "project the chain belongs to")
	schemasDir := flag.String("schemas-dir", "", "blueprints/forge-schemas dir; empty = search up from cwd, then the canonical path")
	flag.Parse()

	err := run(*dbPath, *chainSlug, *project, *schemasDir)
	switch {
	case errors.Is(err, errSkip):
		fmt.Printf("SKIP: %s\n", strings.TrimPrefix(err.Error(), "skip: "))
		os.Exit(0)
	case err != nil:
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	default:
		os.Exit(0)
	}
}

func run(dbPath, chainSlug, project, schemasDir string) error {
	ctx := context.Background()

	if _, statErr := os.Stat(dbPath); errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("%w: live DB not found at %s", errSkip, dbPath)
	}

	prodChain, prodTasks, err := readProduction(dbPath, project, chainSlug)
	if err != nil {
		return err
	}

	dir, err := findSchemasDir(schemasDir)
	if err != nil {
		return err
	}
	reg, err := registry.Load(dir)
	if err != nil {
		return fmt.Errorf("load registry from %s: %w", dir, err)
	}

	replayChain, replayTasks, err := reforge(ctx, reg, project, chainSlug, prodChain, prodTasks)
	if err != nil {
		return err
	}

	diffs := compare(prodChain, prodTasks, replayChain, replayTasks)
	if len(diffs) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "byte-identity replay of chain %q drifted in %d field(s):", chainSlug, len(diffs))
		for _, d := range diffs {
			b.WriteString("\n  - ")
			b.WriteString(d)
		}
		return errors.New(b.String())
	}

	fmt.Printf("OK: chain %q replay byte-identical — chain row + %d task rows match production (modulo ids/timestamps/lifecycle)\n",
		chainSlug, len(prodTasks))
	return nil
}

// readProduction opens the live DB read-only and reads the chain's
// authored-content row + its task rows ordered by position. A missing chain
// is a hard error, not a skip — see the ErrNoRows branch.
func readProduction(dbPath, project, chainSlug string) (chainContent, []taskContent, error) {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return chainContent{}, nil, fmt.Errorf("abs db path: %w", err)
	}
	// mode=ro: never migrate or write the production DB (T3 constraint).
	dsn := "file:" + abs + "?mode=ro&_pragma=busy_timeout(5000)"
	prod, err := sql.Open("sqlite", dsn)
	if err != nil {
		return chainContent{}, nil, fmt.Errorf("open production DB read-only: %w", err)
	}
	defer prod.Close()

	var pc chainContent
	err = prod.QueryRow(
		`SELECT output, completion_condition FROM proj_chain_status WHERE project_id = ? AND slug = ?`,
		project, chainSlug,
	).Scan(&pc.output, &pc.completionCondition)
	if errors.Is(err, sql.ErrNoRows) {
		// NOT a skip. Reaching here means the live DB exists (run() skips
		// before this when it doesn't), so the named chain should exist too:
		// it is closed history in an append-only ledger. Its absence means the
		// chain/project defaults have drifted off the real data — which is
		// exactly how this stage went unnoticed-inert for five weeks after the
		// mcp-servers → corpos-toolkit rename. Fail loudly instead.
		return chainContent{}, nil, fmt.Errorf(
			"chain %q not found in project %q in %s — the replay target drifted off the ledger; "+
				"pass --chain/--project, or fix the defaults in this file", chainSlug, project, abs)
	}
	if err != nil {
		return chainContent{}, nil, fmt.Errorf("read production chain row: %w", err)
	}

	rows, err := prod.Query(`
		SELECT t.slug, t.position, t.problem_statement, t.acceptance_criteria,
		       t.context_required, t.constraints
		FROM proj_current_tasks t
		JOIN proj_chain_status c ON c.id = t.chain_id
		WHERE c.project_id = ? AND c.slug = ?
		ORDER BY t.position ASC`, project, chainSlug)
	if err != nil {
		return chainContent{}, nil, fmt.Errorf("read production task rows: %w", err)
	}
	defer rows.Close()

	var tasks []taskContent
	for rows.Next() {
		var t taskContent
		if err := rows.Scan(&t.slug, &t.position, &t.problemStatement,
			&t.acceptanceCriteria, &t.contextRequired, &t.constraints); err != nil {
			return chainContent{}, nil, fmt.Errorf("scan production task row: %w", err)
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return chainContent{}, nil, fmt.Errorf("iterate production task rows: %w", err)
	}
	if len(tasks) == 0 {
		return chainContent{}, nil, fmt.Errorf("%w: chain %q has no task rows to verify", errSkip, chainSlug)
	}
	return pc, tasks, nil
}

// reforge spins up a throwaway temp DB, wires the events→projections
// fold hook (without it, forge's events never reach proj_*), and
// re-creates the chain via the full-object tasks shape from the
// production content. It then reads back the replayed rows.
func reforge(ctx context.Context, reg *registry.Registry, project, chainSlug string,
	prodChain chainContent, prodTasks []taskContent) (chainContent, []taskContent, error) {

	tmpDir, err := os.MkdirTemp("", "chain-replay-verify-")
	if err != nil {
		return chainContent{}, nil, fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	pool, err := db.Open(filepath.Join(tmpDir, "replay.db"))
	if err != nil {
		return chainContent{}, nil, fmt.Errorf("open temp DB: %w", err)
	}
	defer pool.Close()

	if _, err := pool.DB().Exec(`INSERT INTO projects (id, name) VALUES (?, ?)`, project, project); err != nil {
		return chainContent{}, nil, fmt.Errorf("seed project row: %w", err)
	}

	events.SetFoldHook(func(ctx context.Context, tx *sql.Tx, evt events.RawEvent) error {
		return projections.FoldAll(ctx, tx, projections.RawEvent{
			EventID:         evt.EventID,
			Ts:              evt.Ts,
			ActorKind:       evt.ActorKind,
			ActorID:         evt.ActorID,
			Type:            evt.Type,
			EntityKind:      evt.EntityKind,
			EntitySlug:      evt.EntitySlug,
			EntityProjectID: evt.EntityProjectID,
			Payload:         evt.Payload,
			Rationale:       evt.Rationale,
			CausedByEventID: evt.CausedByEventID,
			RelatedEntities: evt.RelatedEntities,
			SpanID:          evt.SpanID,
			SchemaVersion:   evt.SchemaVersion,
		})
	})

	// Chain 311 T7 Stage 4 Slice 5: replay through construct.Create's
	// ChainWithTasks fan-out (was forge.HandleForge with the
	// {schema_name, tasks:[...]} envelope). Same proj_chain_status +
	// proj_current_tasks rows land — construct.Create dispatches the
	// ChainCreated + N TaskCreated event sequence through record, the fold
	// hook materialises the rows. Behavior-preserving: the same byte-
	// identical replay guarantee.
	taskInputs := make([]construct.ChainTaskInput, 0, len(prodTasks))
	for _, pt := range prodTasks {
		entry := construct.ChainTaskInput{
			Slug:             pt.slug,
			ProblemStatement: pt.problemStatement,
			ContextRequired:  pt.contextRequired,
			Constraints:      pt.constraints,
			// rationale is mandatory on the full-object shape but is not
			// recoverable from pre-T7 production events and is not a
			// proj_current_tasks column — placeholder, never compared.
			Rationale: "chain-replay-verify fixture — original per-task rationale not recoverable from pre-T7 event history",
		}
		if pt.acceptanceCriteria != "" {
			entry.AcceptanceCriteria = strings.Split(pt.acceptanceCriteria, acSeparator)
		}
		taskInputs = append(taskInputs, entry)
	}

	deps := construct.Deps{Pool: pool, Schemas: reg}
	if _, err := construct.Create(ctx, deps, "chain", project, construct.Input{
		ChainWithTasks: &construct.ChainWithTasksInput{
			ChainInput: construct.ChainInput{
				Slug:                chainSlug,
				Output:              prodChain.output,
				CompletionCondition: prodChain.completionCondition,
				// design_decisions is event-only (migration 065) — not a
				// projection column, so a placeholder is faithful for the
				// row comparison.
				DesignDecisions: "chain-replay-verify fixture — design_decisions is event-only (not a projection column); not reproduced",
			},
			Tasks: taskInputs,
		},
	}); err != nil {
		return chainContent{}, nil, fmt.Errorf("re-forge chain via construct.Create: %w", err)
	}

	var rc chainContent
	if err := pool.DB().QueryRow(
		`SELECT output, completion_condition FROM proj_chain_status WHERE project_id = ? AND slug = ?`,
		project, chainSlug,
	).Scan(&rc.output, &rc.completionCondition); err != nil {
		return chainContent{}, nil, fmt.Errorf("read replayed chain row: %w", err)
	}

	rows, err := pool.DB().Query(`
		SELECT t.slug, t.position, t.problem_statement, t.acceptance_criteria,
		       t.context_required, t.constraints
		FROM proj_current_tasks t
		JOIN proj_chain_status c ON c.id = t.chain_id
		WHERE c.project_id = ? AND c.slug = ?
		ORDER BY t.position ASC`, project, chainSlug)
	if err != nil {
		return chainContent{}, nil, fmt.Errorf("read replayed task rows: %w", err)
	}
	defer rows.Close()

	var replayed []taskContent
	for rows.Next() {
		var t taskContent
		if err := rows.Scan(&t.slug, &t.position, &t.problemStatement,
			&t.acceptanceCriteria, &t.contextRequired, &t.constraints); err != nil {
			return chainContent{}, nil, fmt.Errorf("scan replayed task row: %w", err)
		}
		replayed = append(replayed, t)
	}
	if err := rows.Err(); err != nil {
		return chainContent{}, nil, fmt.Errorf("iterate replayed task rows: %w", err)
	}
	return rc, replayed, nil
}

// compare collects a per-field diff list. Empty == byte-identical on the
// authored-content columns.
func compare(prodChain chainContent, prodTasks []taskContent,
	replayChain chainContent, replayTasks []taskContent) []string {

	var diffs []string
	addDiff := func(label, prod, replay string) {
		diffs = append(diffs, fmt.Sprintf("%s: prod=%s replay=%s", label, truncQuote(prod), truncQuote(replay)))
	}

	if prodChain.output != replayChain.output {
		addDiff("chain.output", prodChain.output, replayChain.output)
	}
	if prodChain.completionCondition != replayChain.completionCondition {
		addDiff("chain.completion_condition", prodChain.completionCondition, replayChain.completionCondition)
	}

	if len(prodTasks) != len(replayTasks) {
		diffs = append(diffs, fmt.Sprintf("task count: prod=%d replay=%d", len(prodTasks), len(replayTasks)))
		return diffs
	}

	for i := range prodTasks {
		p, r := prodTasks[i], replayTasks[i]
		tag := fmt.Sprintf("task[%d] %q", i, p.slug)
		if p.slug != r.slug {
			addDiff(tag+".slug", p.slug, r.slug)
		}
		if p.position != r.position {
			addDiff(tag+".position", fmt.Sprintf("%d", p.position), fmt.Sprintf("%d", r.position))
		}
		if p.problemStatement != r.problemStatement {
			addDiff(tag+".problem_statement", p.problemStatement, r.problemStatement)
		}
		if p.acceptanceCriteria != r.acceptanceCriteria {
			addDiff(tag+".acceptance_criteria", p.acceptanceCriteria, r.acceptanceCriteria)
		}
		if p.contextRequired != r.contextRequired {
			addDiff(tag+".context_required", p.contextRequired, r.contextRequired)
		}
		if p.constraints != r.constraints {
			addDiff(tag+".constraints", p.constraints, r.constraints)
		}
	}
	return diffs
}

// truncQuote renders a value for the diff display, capping long bodies so
// a multi-paragraph problem_statement doesn't flood the output. The
// comparison itself uses the full untruncated strings.
func truncQuote(s string) string {
	const max = 160
	if len(s) <= max {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%q…(+%d bytes)", s[:max], len(s)-max)
}

// findSchemasDir resolves the blueprints/forge-schemas directory: the
// explicit flag wins; else walk up from cwd; else the canonical repo
// path. Returns errSkip when none exists.
func findSchemasDir(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if wd, err := os.Getwd(); err == nil {
		for d := wd; d != "/" && d != ""; d = filepath.Dir(d) {
			candidate := filepath.Join(d, "blueprints", "forge-schemas")
			if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
				return candidate, nil
			}
		}
	}
	canonical := filepath.Join(canonicalRootDir(), "blueprints", "forge-schemas")
	if info, err := os.Stat(canonical); err == nil && info.IsDir() {
		return canonical, nil
	}
	return "", fmt.Errorf("%w: blueprints/forge-schemas not found (cwd search + %s)", errSkip, canonical)
}
