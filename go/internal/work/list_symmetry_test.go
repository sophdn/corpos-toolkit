package work_test

import (
	"context"
	"strings"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/work"
)

// These pin the list-verb symmetry hardening (chain list-verb-symmetry): no
// list verb dead-ends or silently narrows scope. task_list is a real cross-
// cutting verb, chain_find lists without a pattern, and all three compact-
// list verbs share the strict unknown-param contract. work_summary rolls the
// four counts into one read.

// seedSecondProject registers `seed-packet` so the cross-project assertions
// have two projects to span (openTestPool only seeds `mcp-servers`).
func seedSecondProject(t *testing.T, pool *db.Pool) {
	t.Helper()
	if _, err := pool.DB().Exec(
		`INSERT INTO projects (id, name) VALUES ('seed-packet', 'seed-packet')`); err != nil {
		t.Fatalf("seed second project: %v", err)
	}
}

// 1. HandleTaskList with EMPTY params returns a list without erroring —
// anti-regression for task-list-aliases-task-search-dead-ends-no-cross-
// cutting-open-task-verb. (task_search errors here; task_list must not.)
func TestTaskList_EmptyParamsListsWithoutError(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "c1")
	seedTask(t, pool, "c1", "t1", "pending")
	seedTask(t, pool, "c1", "t2", "active")

	resp, err := work.HandleTaskList(context.Background(), pool, "", nil)
	if err != nil {
		t.Fatalf("HandleTaskList(empty): %v", err)
	}
	if resp.Err != nil {
		t.Fatalf("empty params must list, got error envelope: %+v", resp.Err)
	}
	if len(resp.List) != 2 {
		t.Errorf("expected 2 tasks, got %d", len(resp.List))
	}
}

// 2. task_list / bug_list / suggestion_list with status:"open" each return
// without error AND yield cross-project results (rows from both seeded
// projects) — the anti-silent-narrowing assertion.
func TestListVerbs_OpenStatusCrossProject(t *testing.T) {
	pool := openTestPool(t)
	seedSecondProject(t, pool)

	// Tasks: one non-terminal task per project.
	seedChain(t, pool, "mcp-servers", "mc-chain")
	seedChain(t, pool, "seed-packet", "sp-chain")
	seedTask(t, pool, "mc-chain", "mc-task", "pending")
	seedTask(t, pool, "sp-chain", "sp-task", "active")

	// Bugs: one open bug per project.
	seedBug(t, pool, "mcp-servers", "mc-bug", "open")
	seedBug(t, pool, "seed-packet", "sp-bug", "open")

	// Suggestions: one open suggestion per project.
	seedSuggestion(t, pool, "mcp-servers", "mc-sugg", "open")
	seedSuggestion(t, pool, "seed-packet", "sp-sugg", "open")

	openParams := mustJSON(t, map[string]any{"status": "open"})

	taskResp, err := work.HandleTaskList(context.Background(), pool, "", openParams)
	if err != nil || taskResp.Err != nil {
		t.Fatalf("HandleTaskList: err=%v envelope=%+v", err, taskResp.Err)
	}
	assertProjectsSpanned(t, "task_list", taskSlugs(taskResp.List), "mc-task", "sp-task")

	bugResp, err := work.HandleBugList(context.Background(), pool, "", openParams)
	if err != nil {
		t.Fatalf("HandleBugList: %v", err)
	}
	assertProjectsSpanned(t, "bug_list", bugSlugs(bugResp.DefaultItems), "mc-bug", "sp-bug")

	suggResp, err := work.HandleSuggestionList(context.Background(), pool, "", openParams)
	if err != nil {
		t.Fatalf("HandleSuggestionList: %v", err)
	}
	assertProjectsSpanned(t, "suggestion_list", suggSlugs(suggResp.DefaultItems), "mc-sugg", "sp-sugg")
}

// 3. HandleChainFind with EMPTY pattern lists without erroring
// (anti-regression for suggestion #60); with a project set, results scope to
// that project.
func TestChainFind_EmptyPatternAndProjectScope(t *testing.T) {
	pool := openTestPool(t)
	seedSecondProject(t, pool)
	seedChain(t, pool, "mcp-servers", "mc-a")
	seedChain(t, pool, "mcp-servers", "mc-b")
	seedChain(t, pool, "seed-packet", "sp-a")

	// Empty pattern, cross-project → all three.
	all, err := work.HandleChainFind(context.Background(), pool, "", mustJSON(t, map[string]any{}))
	if err != nil {
		t.Fatalf("HandleChainFind(empty): %v", err)
	}
	if all.Err != nil {
		t.Fatalf("empty pattern must list, got: %+v", all.Err)
	}
	if len(all.List) != 3 {
		t.Errorf("expected 3 chains cross-project, got %d", len(all.List))
	}

	// Project-scoped list (empty pattern, project via params).
	scoped, err := work.HandleChainFind(context.Background(), pool, "",
		mustJSON(t, map[string]any{"project": "seed-packet"}))
	if err != nil {
		t.Fatalf("HandleChainFind(project): %v", err)
	}
	if len(scoped.List) != 1 || scoped.List[0].Slug != "sp-a" {
		t.Errorf("expected only seed-packet chain sp-a, got %+v", scoped.List)
	}
}

// 4. Each compact-list verb REJECTS an unknown param via the strict decoder —
// the shared strict-decode contract now covers task_list too.
func TestListVerbs_RejectUnknownParam(t *testing.T) {
	pool := openTestPool(t)
	bogus := mustJSON(t, map[string]any{"bogus": 1})

	if _, err := work.HandleTaskList(context.Background(), pool, "mcp-servers", bogus); err == nil ||
		!strings.Contains(err.Error(), "accepted") {
		t.Errorf("task_list should reject unknown param, got: %v", err)
	}
	if _, err := work.HandleBugList(context.Background(), pool, "mcp-servers", bogus); err == nil ||
		!strings.Contains(err.Error(), "accepted") {
		t.Errorf("bug_list should reject unknown param, got: %v", err)
	}
	if _, err := work.HandleSuggestionList(context.Background(), pool, "mcp-servers", bogus); err == nil ||
		!strings.Contains(err.Error(), "accepted") {
		t.Errorf("suggestion_list should reject unknown param, got: %v", err)
	}
}

// 5. HandleWorkSummary returns counts consistent with the seeded data.
func TestWorkSummary_CountsMatchSeed(t *testing.T) {
	pool := openTestPool(t)
	seedSecondProject(t, pool)

	// mcp-servers: 2 open bugs, 1 open chain w/ pending+active tasks, 1 sugg.
	seedBug(t, pool, "mcp-servers", "mc-bug1", "open")
	seedBug(t, pool, "mcp-servers", "mc-bug2", "open")
	seedBug(t, pool, "mcp-servers", "mc-bug3", "fixed") // not counted
	seedChain(t, pool, "mcp-servers", "mc-chain")
	seedTask(t, pool, "mc-chain", "mc-t1", "pending")
	seedTask(t, pool, "mc-chain", "mc-t2", "active")
	seedSuggestion(t, pool, "mcp-servers", "mc-sugg", "open")

	// seed-packet: 1 open bug, 1 open chain w/ a blocked task.
	seedBug(t, pool, "seed-packet", "sp-bug", "open")
	seedChain(t, pool, "seed-packet", "sp-chain")
	seedTask(t, pool, "sp-chain", "sp-t1", "blocked")

	// Cross-project totals.
	all, err := work.HandleWorkSummary(context.Background(), pool, "", nil)
	if err != nil {
		t.Fatalf("HandleWorkSummary: %v", err)
	}
	if all.OpenBugs != 3 {
		t.Errorf("open_bugs: want 3, got %d", all.OpenBugs)
	}
	if all.OpenChains != 2 {
		t.Errorf("open_chains: want 2, got %d", all.OpenChains)
	}
	if all.OpenSuggestions != 1 {
		t.Errorf("open_suggestions: want 1, got %d", all.OpenSuggestions)
	}
	if all.Tasks.Pending != 1 || all.Tasks.Active != 1 || all.Tasks.Blocked != 1 {
		t.Errorf("tasks: want pending=1 active=1 blocked=1, got %+v", all.Tasks)
	}

	// Project-scoped.
	mc, err := work.HandleWorkSummary(context.Background(), pool, "mcp-servers", nil)
	if err != nil {
		t.Fatalf("HandleWorkSummary(mcp-servers): %v", err)
	}
	if mc.OpenBugs != 2 {
		t.Errorf("mcp-servers open_bugs: want 2, got %d", mc.OpenBugs)
	}
	if mc.Tasks.Blocked != 0 {
		t.Errorf("mcp-servers should have no blocked tasks, got %d", mc.Tasks.Blocked)
	}

	// by_project breakdown alongside the totals.
	bp, err := work.HandleWorkSummary(context.Background(), pool, "",
		mustJSON(t, map[string]any{"by_project": true}))
	if err != nil {
		t.Fatalf("HandleWorkSummary(by_project): %v", err)
	}
	if len(bp.Projects) != 2 {
		t.Fatalf("by_project: want 2 rows, got %d (%+v)", len(bp.Projects), bp.Projects)
	}
	byName := map[string]work.WorkSummaryResult{}
	for _, r := range bp.Projects {
		byName[r.Project] = r
	}
	if byName["mcp-servers"].OpenBugs != 2 {
		t.Errorf("by_project mcp-servers open_bugs: want 2, got %d", byName["mcp-servers"].OpenBugs)
	}
	if byName["seed-packet"].Tasks.Blocked != 1 {
		t.Errorf("by_project seed-packet blocked: want 1, got %d", byName["seed-packet"].Tasks.Blocked)
	}
}

// ── small projection helpers ─────────────────────────────────────────

func taskSlugs(items []work.TaskListItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Slug)
	}
	return out
}

func bugSlugs(items []work.BugListItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Slug)
	}
	return out
}

func suggSlugs(items []work.SuggestionListItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Slug)
	}
	return out
}

func assertProjectsSpanned(t *testing.T, verb string, slugs []string, want ...string) {
	t.Helper()
	have := map[string]bool{}
	for _, s := range slugs {
		have[s] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("%s: expected cross-project row %q in results %v", verb, w, slugs)
		}
	}
}
