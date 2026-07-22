package projections_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/projections"
	"toolkit/internal/testutil"
)

// TestRegistry_AllProjections asserts the package's init() side
// effects: every registered projection name shows up in [All]. Drift
// here means a projection file got renamed without updating the test
// list — a quick canary against accidental de-registration. After TT3
// the three query_* read-side projections join the original write-side
// trio; after agent-substrate-crud-retirement T2 four more write-side
// projections (current_tasks / task_blockers / benchmark_results /
// current_suggestions) join too.
func TestRegistry_AllProjections(t *testing.T) {
	// Derivation-based (chain worktree-multi-agent-orchestration-support T7):
	// no hand-maintained inventory literal to merge-conflict on when two
	// agents each add a projection. Two runtime-computed invariants replace
	// the old static list:
	//   (1) projection Name()s are unique — catches a double Register().
	//   (2) the set of registered TableName()s equals the set of proj_* tables
	//       in a freshly-migrated schema — a bijection that catches BOTH a
	//       projection whose backing table was renamed/dropped AND a projection
	//       silently de-registered (its table would then be an orphan proj_).
	// Adding a projection = a new file with init()→Register + a CREATE TABLE
	// migration; neither is a shared list line.
	all := projections.All()
	if len(all) == 0 {
		t.Fatal("projections.All() is empty")
	}
	// nonProjProjectionTables: projections whose backing table is a
	// pre-existing artifact table, NOT a proj_* table. bench_harnesses (chain
	// 311 T7 Stage 6 P2-A) is an artifact table (migration 067) that became a
	// fold target when bench was event-sourced — it's excluded from the proj_*
	// bijection below and instead checked for plain existence.
	nonProjProjectionTables := map[string]bool{"bench_harnesses": true}

	// childProjectionTables: proj_* tables a projection writes as a CHILD of
	// its TableName() parent, so they have no standalone projection of their
	// own. The study_runs projection folds StudyRunRecorded into a parent
	// (proj_study_runs, its TableName()) plus a per-cell score grid
	// (proj_study_run_scores); the child is excluded from the
	// one-table-per-projection bijection below (study-run-persistence T1).
	// chain 434 (corpos-gate) T6 — the gate_runs projection folds
	// GateRunCompleted into a parent (proj_gate_runs, its TableName()) plus a
	// per-check grid (proj_gate_check_results); the child is excluded from the
	// bijection, same as proj_study_run_scores.
	childProjectionTables := map[string]bool{
		"proj_study_run_scores":   true,
		"proj_gate_check_results": true,
	}

	seen := map[string]bool{}
	regTables := make([]string, 0, len(all))
	nonProjTables := make([]string, 0, len(nonProjProjectionTables))
	for _, p := range all {
		if seen[p.Name()] {
			t.Errorf("duplicate projection Name() %q — double Register()?", p.Name())
		}
		seen[p.Name()] = true
		if nonProjProjectionTables[p.TableName()] {
			nonProjTables = append(nonProjTables, p.TableName())
			continue
		}
		regTables = append(regTables, p.TableName())
	}
	sort.Strings(regTables)

	pool := testutil.NewTestDB(t)
	schemaTables := make([]string, 0)
	for _, tbl := range projTablesInSchema(t, pool) {
		if childProjectionTables[tbl] {
			continue // owned by a parent projection; not a standalone proj_
		}
		schemaTables = append(schemaTables, tbl)
	}
	if strings.Join(regTables, ",") != strings.Join(schemaTables, ",") {
		t.Fatalf("registered projection tables != proj_* tables in the migrated schema:\n"+
			"  registered = %v\n  schema     = %v\n"+
			"(a projection's table was renamed/dropped, or a projection was de-registered leaving an orphan proj_ table)",
			regTables, schemaTables)
	}
	// The non-proj_ projection tables must still exist as real tables (their
	// CREATE lives in an earlier artifact migration, not as a proj_*).
	for _, tbl := range nonProjTables {
		if !tableExistsInSchema(t, pool, tbl) {
			t.Errorf("non-proj_ projection table %q is registered but absent from the migrated schema", tbl)
		}
	}
}

// tableExistsInSchema reports whether a table of the given name exists in the
// migrated schema. Used for the non-proj_ projection-table exceptions.
func tableExistsInSchema(t *testing.T, pool *db.Pool, name string) bool {
	t.Helper()
	var got string
	err := pool.DB().QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name = ?`, name).Scan(&got)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		t.Fatalf("check table %q exists: %v", name, err)
	}
	return got == name
}

// projTablesInSchema returns the sorted proj_* table names present in a
// freshly-migrated DB. The `\_` escape keeps the LIKE `_` literal (it is
// otherwise a single-char wildcard). Used by TestRegistry_AllProjections to
// derive the projection inventory from the schema instead of a hand-list.
func projTablesInSchema(t *testing.T, pool *db.Pool) []string {
	t.Helper()
	rows, err := pool.DB().Query(
		`SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'proj\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		t.Fatalf("list proj_ tables: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan proj_ table name: %v", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

// TestRegistry_NamespacePrefixes asserts every registered projection
// name carries one of the known prefixes. Reserved prefixes for future
// chains (injection_*, offload_*) are accepted preemptively so a new
// projection registering under them does not flag this canary. The
// pre-TT3 unprefixed legacy names (current_bugs / chain_status /
// roadmap_view) are grandfathered via the legacyNames allow-list.
func TestRegistry_NamespacePrefixes(t *testing.T) {
	legacyNames := map[string]bool{
		"current_bugs": true,
		"chain_status": true,
		"roadmap_view": true,
		// agent-substrate-crud-retirement T2 additions — same write-side
		// shape as the original trio (current_*, *_status, *_view); no
		// new namespace prefix introduced.
		"current_tasks":       true,
		"task_blockers":       true,
		"benchmark_results":   true,
		"current_suggestions": true,
		// substrate-health-audit-projections T7 — write-side projection
		// folding MemoryWritten events; same (current_*) family, no new
		// namespace prefix.
		"memories": true,
		// chain 311 T7 Stage 6 P2-A — bench event-sourcing. bench_harnesses is
		// an artifact table (migration 067) that became a fold target; it keeps
		// its non-proj_ artifact name, so it's grandfathered here like the
		// other unprefixed write-side projections.
		"bench_harnesses": true,
		// study-run-persistence T1 — write-side projection folding
		// StudyRunRecorded events into proj_study_runs + proj_study_run_scores.
		// Same write-side shape as benchmark_results (parent + child rows); no
		// new namespace prefix introduced.
		"study_runs": true,
		// chain 434 (corpos-gate) T6 — write-side projection folding
		// GateRunCompleted events into proj_gate_runs + proj_gate_check_results.
		// Same parent+child shape as study_runs / benchmark_results; no new
		// namespace prefix introduced.
		"gate_runs": true,
	}
	allowedPrefixes := []string{"query_", "retrieval_", "training_", "injection_", "offload_", "inference_"}
	for _, p := range projections.All() {
		name := p.Name()
		if legacyNames[name] {
			continue
		}
		ok := false
		for _, prefix := range allowedPrefixes {
			if strings.HasPrefix(name, prefix) {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("projection %q has no known namespace prefix; allowed: %v (or legacy)", name, allowedPrefixes)
		}
	}
}

// TestRegistry_GetByName covers the Get helper used by the CLI flag
// --projection=NAME.
func TestRegistry_GetByName(t *testing.T) {
	p, ok := projections.Get("current_bugs")
	if !ok {
		t.Fatalf("Get(current_bugs) returned ok=false")
	}
	if p.TableName() != "proj_current_bugs" {
		t.Fatalf("TableName = %q, want proj_current_bugs", p.TableName())
	}
	if _, ok := projections.Get("nonexistent_proj"); ok {
		t.Fatalf("Get(nonexistent) returned ok=true")
	}
}

// TestRebuildAll_Snapshot drives a full rebuild against a DB seeded
// with rows in bugs/chains/tasks/roadmap_items and asserts each
// proj_* table receives the expected snapshot row count. The
// per-projection row contents are tested in their own _test.go files;
// here we cover the RebuildAll loop.
func TestRebuildAll_Snapshot(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	// Post-T6 (agent-substrate-crud-retirement): the retired CRUD tables
	// (bugs/chains/tasks/roadmap_items) are gone; RebuildFromEmpty replays
	// events. Seed synthetic events covering each entity kind so
	// RebuildAll produces the expected row counts.
	mustExec(t, pool, `INSERT INTO events
		(event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug, entity_project_id, payload, span_id, schema_version)
		VALUES
		('019e7b00-0001-7000-8000-000000000001', '2026-05-21 00:00:01', 'system', 'test',
		 'RoadmapUpdated', 'roadmap', 'main', 'p1',
		 '{"action_kind":"set","positions":[1],"items":[{"position":1,"ref_kind":"chain","ref_slug":"c1"}]}',
		 '019e7b00-0001-7000-8000-000000000001', 1),
		('019e7b00-0002-7000-8000-000000000002', '2026-05-21 00:00:02', 'system', 'test',
		 'BugReported', 'bug', 'b1', 'p1',
		 '{"title":"B1","problem_statement":"","severity":"high"}',
		 '019e7b00-0002-7000-8000-000000000002', 1),
		('019e7b00-0003-7000-8000-000000000003', '2026-05-21 00:00:03', 'system', 'test',
		 'BugReported', 'bug', 'b2', 'p1',
		 '{"title":"B2","problem_statement":"","severity":"low"}',
		 '019e7b00-0003-7000-8000-000000000003', 1),
		('019e7b00-0004-7000-8000-000000000004', '2026-05-21 00:00:04', 'system', 'test',
		 'ChainCreated', 'chain', 'c1', 'p1',
		 '{"output":"","design_decisions":"","completion_condition":""}',
		 '019e7b00-0004-7000-8000-000000000004', 1),
		('019e7b00-0005-7000-8000-000000000005', '2026-05-21 00:00:05', 'system', 'test',
		 'TaskCreated', 'task', 't1', 'p1',
		 '{"chain_slug":"c1","problem_statement":"p1"}',
		 '019e7b00-0005-7000-8000-000000000005', 1),
		('019e7b00-0006-7000-8000-000000000006', '2026-05-21 00:00:06', 'system', 'test',
		 'TaskCreated', 'task', 't2', 'p1',
		 '{"chain_slug":"c1","problem_statement":"p2"}',
		 '019e7b00-0006-7000-8000-000000000006', 1)`)

	results := mustRebuild(t, pool, nil)
	got := map[string]int64{}
	for _, r := range results {
		got[r.Name] = r.Rows
	}
	want := map[string]int64{
		"current_bugs":        2,
		"chain_status":        1,
		"roadmap_view":        1,
		"current_tasks":       2,
		"task_blockers":       0,
		"benchmark_results":   0,
		"current_suggestions": 0,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s rows = %d, want %d", k, got[k], v)
		}
	}
}

// TestRebuildAll_SubsetByName covers the --projection=NAME flag path.
func TestRebuildAll_SubsetByName(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	// Post-T6: seed a synthetic BugReported event so RebuildFromEmpty
	// (which replays events) produces the expected row.
	mustExec(t, pool, `INSERT INTO events
		(event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug, entity_project_id, payload, span_id, schema_version)
		VALUES ('019e7c00-0001-7000-8000-000000000001', '2026-05-21 00:00:01', 'system', 'test',
		        'BugReported', 'bug', 'b', 'p1',
		        '{"title":"B","problem_statement":"","severity":"high"}',
		        '019e7c00-0001-7000-8000-000000000001', 1)`)

	// Wipe both projection tables; rebuild only current_bugs; assert
	// chain_status stays empty.
	mustExec(t, pool, `DELETE FROM proj_current_bugs`)
	mustExec(t, pool, `DELETE FROM proj_chain_status`)
	results := mustRebuild(t, pool, []string{"current_bugs"})
	if len(results) != 1 || results[0].Name != "current_bugs" {
		t.Fatalf("subset rebuild = %+v", results)
	}
	if got := tableCount(t, pool, "proj_chain_status"); got != 0 {
		t.Errorf("chain_status was rebuilt despite not being named: got %d rows", got)
	}
	if got := tableCount(t, pool, "proj_current_bugs"); got != 1 {
		t.Errorf("current_bugs rows = %d, want 1", got)
	}
}

// TestRebuildAll_UnknownProjection asserts the error path: passing an
// unregistered name surfaces a clear error from RebuildAll without
// mutating any projection table.
func TestRebuildAll_UnknownProjection(t *testing.T) {
	pool := testutil.NewTestDB(t)
	err := pool.WithWrite(context.Background(), func(tx *sql.Tx) error {
		_, err := projections.RebuildAll(context.Background(), tx, []string{"nope"})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "unknown projection") {
		t.Fatalf("err = %v, want unknown projection error", err)
	}
}

// TestRebuildAll_IsIdempotent runs the rebuild twice and asserts every
// projection's checksum is unchanged. This is the strongest invariant
// of the rebuild contract: the second run must agree with the first
// byte-for-byte.
func TestRebuildAll_IsIdempotent(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	// Post-T6: seed synthetic events instead of CRUD rows; rebuilds
	// replay the events ledger and the byte-identical invariant holds
	// regardless of how the source was populated.
	mustExec(t, pool, `INSERT INTO events
		(event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug, entity_project_id, payload, span_id, schema_version)
		VALUES
		('019e7100-0001-7000-8000-000000000001', '2026-05-21 00:00:01', 'system', 'test',
		 'BugReported', 'bug', 'a', 'p1',
		 '{"title":"A","problem_statement":"","severity":"high"}',
		 '019e7100-0001-7000-8000-000000000001', 1),
		('019e7100-0002-7000-8000-000000000002', '2026-05-21 00:00:02', 'system', 'test',
		 'BugReported', 'bug', 'b', 'p1',
		 '{"title":"B","problem_statement":"","severity":"low"}',
		 '019e7100-0002-7000-8000-000000000002', 1),
		('019e7100-0003-7000-8000-000000000003', '2026-05-21 00:00:03', 'system', 'test',
		 'ChainCreated', 'chain', 'c1', 'p1',
		 '{"output":"","design_decisions":"","completion_condition":""}',
		 '019e7100-0003-7000-8000-000000000003', 1),
		('019e7100-0004-7000-8000-000000000004', '2026-05-21 00:00:04', 'system', 'test',
		 'TaskCreated', 'task', 't1', 'p1',
		 '{"chain_slug":"c1","problem_statement":""}',
		 '019e7100-0004-7000-8000-000000000004', 1)`)

	// One rebuild is enough now that DependentProjection (chain_status
	// declares DependsOn=["current_tasks"]) makes the fold order
	// topological. Before the dependency declaration this test needed
	// two warm-up rebuilds because chain_status sorted alphabetically
	// before current_tasks and saw an empty proj_current_tasks on
	// the first pass — the same off-by-one bug that bug
	// `proj-chain-status-counters-always-one-task-transition-behind`
	// fixed for live fold traffic. The idempotency invariant ("two
	// consecutive rebuilds of the same logical state produce
	// byte-identical projections") now holds from the FIRST rebuild.
	mustRebuild(t, pool, nil)
	pre := map[string]string{}
	for _, p := range projections.All() {
		pre[p.Name()] = tableChecksum(t, pool, p.TableName())
	}
	mustRebuild(t, pool, nil)
	for _, p := range projections.All() {
		post := tableChecksum(t, pool, p.TableName())
		if post != pre[p.Name()] {
			t.Errorf("%s checksum drift: pre=%s post=%s", p.Name(), pre[p.Name()], post)
		}
	}
}

// TestFold_IncrementalEqualsRebuild seeds CRUD, emits events via the
// real Emit path (which folds incrementally via the registered hook),
// captures the projection checksum, then TRUNCATEs the projection,
// runs RebuildAll, and asserts the checksum is identical.
//
// This is the byte-identical invariant from acceptance criteria (a)
// — exercises the live-emit fold path AND the rebuild-from-empty
// path against the same logical state.
func TestFold_IncrementalEqualsRebuild(t *testing.T) {
	pool := testutil.NewTestDB(t)
	installProjectionsFoldHook(t)
	seedProject(t, pool, "p1")

	// Post-T5-bugs: incremental fold must equal event-replay rebuild.
	// Emit both BugReported and BugResolved through events.Emit; the
	// fold inside the hook applies both. CRUD `bugs` writes are gone.
	ctx := events.WithActor(context.Background(), events.Actor{Kind: "agent", ID: "test"})
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		severity := "high"
		if _, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity:  events.NewEntityRef("bug", "b", "p1"),
			Payload: events.BugReportedPayload{Title: "B", ProblemStatement: "p", Severity: &severity},
		}); err != nil {
			return err
		}
		note := "fix landed"
		rationale := "addresses regression in handler dispatch path"
		_, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity:    events.NewEntityRef("bug", "b", "p1"),
			Payload:   events.BugResolvedPayload{Kind: "fixed", CommitSHA: strPtr("abc1234"), ResolutionNote: &note},
			Rationale: &rationale,
		})
		return err
	})
	if err != nil {
		t.Fatalf("emit + crud update: %v", err)
	}

	pre := tableChecksum(t, pool, "proj_current_bugs")
	mustExec(t, pool, `DELETE FROM proj_current_bugs`)
	mustRebuild(t, pool, []string{"current_bugs"})
	post := tableChecksum(t, pool, "proj_current_bugs")
	if pre != post {
		t.Fatalf("checksum drift: pre=%s post=%s", pre, post)
	}
}

// TestFold_FailureAbortsTx registers a poison projection that returns
// error on Fold; emit an event; assert the events table stays empty
// and the originating mutation is rolled back. Acceptance criteria
// item (5): "Fold failure aborts the originating mutation — eventual
// consistency rejected".
func TestFold_FailureAbortsTx(t *testing.T) {
	pool := testutil.NewTestDB(t)
	t.Cleanup(func() { events.SetFoldHook(nil) })

	// Set a hook that always errors. The real projections aren't
	// invoked (they're called via projections.FoldAll, which we don't
	// install here).
	events.SetFoldHook(func(ctx context.Context, tx *sql.Tx, evt events.RawEvent) error {
		return fmt.Errorf("poison")
	})
	seedProject(t, pool, "p1")

	ctx := events.WithActor(context.Background(), events.Actor{Kind: "agent", ID: "test"})
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		rationale := "test rationale long enough to pass any dispatch gate"
		_, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity:    events.NewEntityRef("bug", "b", "p1"),
			Payload:   events.BugResolvedPayload{Kind: "fixed"},
			Rationale: &rationale,
		})
		return err
	})
	if err == nil {
		t.Fatalf("emit should have failed with poison fold")
	}
	if got := tableCount(t, pool, "events"); got != 0 {
		t.Errorf("events row count = %d, want 0 (fold failure must roll back)", got)
	}
}

// TestWatermark_AdvanceAndRead covers ReadWatermark / WriteWatermark.
func TestWatermark_AdvanceAndRead(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ctx := context.Background()
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		if err := projections.WriteWatermark(ctx, tx, "current_bugs", "01-evt-1", "2026-05-17T00:00:00Z"); err != nil {
			return err
		}
		eid, ts, err := projections.ReadWatermark(ctx, tx, "current_bugs")
		if err != nil {
			return err
		}
		if eid != "01-evt-1" || ts != "2026-05-17T00:00:00Z" {
			t.Errorf("watermark = (%q, %q), want (01-evt-1, 2026-05-17T00:00:00Z)", eid, ts)
		}
		if err := projections.WriteWatermark(ctx, tx, "current_bugs", "02-evt-2", "2026-05-17T00:01:00Z"); err != nil {
			return err
		}
		eid, _, _ = projections.ReadWatermark(ctx, tx, "current_bugs")
		if eid != "02-evt-2" {
			t.Errorf("watermark after second write = %q, want 02-evt-2", eid)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("watermark roundtrip: %v", err)
	}
}

// ── helpers ────────────────────────────────────────────────────────────

func mustExec(t *testing.T, pool *db.Pool, sqlStr string, args ...any) {
	t.Helper()
	if _, err := pool.DB().Exec(sqlStr, args...); err != nil {
		t.Fatalf("exec %q: %v", sqlStr, err)
	}
}

func mustExecRet(t *testing.T, pool *db.Pool, sqlStr string, args ...any) int64 {
	t.Helper()
	res, err := pool.DB().Exec(sqlStr, args...)
	if err != nil {
		t.Fatalf("exec %q: %v", sqlStr, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func seedProject(t *testing.T, pool *db.Pool, id string) {
	t.Helper()
	mustExec(t, pool, `INSERT OR IGNORE INTO projects (id, name) VALUES (?, ?)`, id, id)
}

func tableCount(t *testing.T, pool *db.Pool, table string) int64 {
	t.Helper()
	var n int64
	if err := pool.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// tableColumns returns the table's column names in cid (declaration) order
// via PRAGMA table_info. Used to derive a deterministic checksum ordering
// without a hand-maintained per-table column map.
func tableColumns(t *testing.T, pool *db.Pool, table string) []string {
	t.Helper()
	rows, err := pool.DB().Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan column name: %v", err)
		}
		cols = append(cols, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return cols
}

// tableChecksum returns a SHA-256 over every row of the table, with a
// deterministic row order so a before/after comparison on the same table is
// stable. The order is DERIVED from the table's own columns (PRAGMA
// table_info) rather than a hand-maintained per-table ORDER BY map — adding a
// projection no longer means editing a shared map that parallel agents
// merge-conflict on (chain worktree-multi-agent-orchestration-support T7).
//
// The volatile last_event_id / last_event_ts columns are excluded from BOTH
// the order and the hash: they legitimately differ between two convergent
// paths (one stamps from event emit, the other leaves the snapshot sentinel),
// so ordering by them would make row order path-dependent. Ordering by every
// remaining (domain) column makes the order a pure function of content, which
// is what the convergence comparison needs.
func tableChecksum(t *testing.T, pool *db.Pool, table string) string {
	t.Helper()
	orderCols := make([]string, 0)
	for _, c := range tableColumns(t, pool, table) {
		if c == "last_event_id" || c == "last_event_ts" {
			continue
		}
		orderCols = append(orderCols, `"`+c+`"`)
	}
	orderBy := "rowid"
	if len(orderCols) > 0 {
		orderBy = strings.Join(orderCols, ", ")
	}
	rows, err := pool.DB().Query("SELECT * FROM " + table + " ORDER BY " + orderBy)
	if err != nil {
		t.Fatalf("query %s: %v", table, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	h := sha256.New()
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		// last_event_id / last_event_ts may differ between paths (one
		// stamps from event emit, the other leaves the snapshot
		// sentinel ''). Skip them in the checksum so the test asserts
		// domain-column equivalence.
		for i, name := range cols {
			if name == "last_event_id" || name == "last_event_ts" {
				continue
			}
			b, _ := json.Marshal(vals[i])
			h.Write([]byte(name + "="))
			h.Write(b)
			h.Write([]byte{'\n'})
		}
		h.Write([]byte{'|'})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func mustRebuild(t *testing.T, pool *db.Pool, names []string) []projections.RebuildResult {
	t.Helper()
	var results []projections.RebuildResult
	err := pool.WithWrite(context.Background(), func(tx *sql.Tx) error {
		var err error
		results, err = projections.RebuildAll(context.Background(), tx, names)
		return err
	})
	if err != nil {
		t.Fatalf("RebuildAll: %v", err)
	}
	return results
}

// installProjectionsFoldHook wires the production fold hook for tests
// that need real projection updates during Emit. Restores the prior
// hook via t.Cleanup so tests don't pollute each other.
func installProjectionsFoldHook(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { events.SetFoldHook(nil) })
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
}

func strPtr(s string) *string { return &s }
