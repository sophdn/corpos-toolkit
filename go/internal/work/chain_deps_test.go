package work_test

import (
	"context"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/work"
)

func addDep(t *testing.T, pool *db.Pool, project, dependent, prerequisite, reason string) work.ChainDepResult {
	t.Helper()
	res, err := work.HandleChainDepAdd(context.Background(), pool, project, mustJSON(t, map[string]any{
		"dependent_chain": dependent, "prerequisite_chain": prerequisite, "reason": reason,
	}))
	if err != nil {
		t.Fatalf("HandleChainDepAdd: %v", err)
	}
	return res
}

func plan(t *testing.T, pool *db.Pool, project string) work.RoadmapPlanResult {
	t.Helper()
	res, err := work.HandleRoadmapPlan(context.Background(), pool, project, nil)
	if err != nil {
		t.Fatalf("HandleRoadmapPlan: %v", err)
	}
	return res
}

func planSlugs(p work.RoadmapPlanResult) []string {
	out := make([]string, len(p.Order))
	for i, e := range p.Order {
		out[i] = e.ChainSlug
	}
	return out
}

func TestChainDep_AddListRemove(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "http-api")
	seedChain(t, pool, "mcp-servers", "typed-core")

	if res := addDep(t, pool, "mcp-servers", "http-api", "typed-core", "needs the types"); !res.OK {
		t.Fatalf("add rejected: %+v", res)
	}

	// list from the dependent's perspective.
	lst, err := work.HandleChainDepList(context.Background(), pool, "", mustJSON(t, map[string]any{"chain": "http-api"}))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(lst.Prerequisites) != 1 || lst.Prerequisites[0].ChainSlug != "typed-core" {
		t.Fatalf("prerequisites: want [typed-core], got %+v", lst.Prerequisites)
	}
	if lst.Prerequisites[0].Reason != "needs the types" {
		t.Errorf("reason not carried: %+v", lst.Prerequisites[0])
	}
	// and from the prerequisite's perspective.
	lst2, _ := work.HandleChainDepList(context.Background(), pool, "", mustJSON(t, map[string]any{"chain": "typed-core"}))
	if len(lst2.Dependents) != 1 || lst2.Dependents[0].ChainSlug != "http-api" {
		t.Fatalf("dependents: want [http-api], got %+v", lst2.Dependents)
	}

	// remove.
	if res, _ := work.HandleChainDepRemove(context.Background(), pool, "", mustJSON(t, map[string]any{
		"dependent_chain": "http-api", "prerequisite_chain": "typed-core",
	})); !res.OK {
		t.Fatalf("remove rejected: %+v", res)
	}
	lst3, _ := work.HandleChainDepList(context.Background(), pool, "", mustJSON(t, map[string]any{"chain": "http-api"}))
	if len(lst3.Prerequisites) != 0 {
		t.Errorf("expected no prerequisites after remove, got %+v", lst3.Prerequisites)
	}
}

func TestChainDep_AddValidation(t *testing.T) {
	pool := openTestPool(t)
	if _, err := pool.DB().Exec(`INSERT INTO projects (id, name) VALUES ('other','other')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	seedChain(t, pool, "mcp-servers", "alpha")
	seedChain(t, pool, "mcp-servers", "beta")
	seedChain(t, pool, "other", "gamma")

	// self-edge
	if res := addDep(t, pool, "mcp-servers", "alpha", "alpha", ""); res.OK || res.Error == "" {
		t.Errorf("self-edge should be rejected, got %+v", res)
	}
	// unknown chain
	if res := addDep(t, pool, "mcp-servers", "alpha", "nope", ""); res.OK || res.Error == "" {
		t.Errorf("unknown prerequisite should be rejected, got %+v", res)
	}
	// cross-project
	if res := addDep(t, pool, "", "alpha", "gamma", ""); res.OK || res.Error == "" {
		t.Errorf("cross-project edge should be rejected, got %+v", res)
	}
	// duplicate
	if res := addDep(t, pool, "mcp-servers", "alpha", "beta", ""); !res.OK {
		t.Fatalf("first add should succeed: %+v", res)
	}
	if res := addDep(t, pool, "mcp-servers", "alpha", "beta", ""); res.OK || res.Error == "" {
		t.Errorf("duplicate edge should be rejected, got %+v", res)
	}
}

func TestRoadmapPlan_LinearTopoOrder(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "c-third")
	seedChain(t, pool, "mcp-servers", "b-second")
	seedChain(t, pool, "mcp-servers", "a-first")
	// c depends on b, b depends on a → order a, b, c.
	addDep(t, pool, "mcp-servers", "c-third", "b-second", "after b")
	addDep(t, pool, "mcp-servers", "b-second", "a-first", "after a")

	p := plan(t, pool, "mcp-servers")
	if got := planSlugs(p); len(got) != 3 || got[0] != "a-first" || got[1] != "b-second" || got[2] != "c-third" {
		t.Fatalf("topo order: got %v", got)
	}
	// a-first is ready (no open prereqs); b/c blocked with reasons.
	if p.Order[0].Status != "ready" || len(p.Order[0].DependsOn) != 0 {
		t.Errorf("a-first should be ready with no deps: %+v", p.Order[0])
	}
	if p.Order[1].Status != "blocked" || len(p.Order[1].DependsOn) != 1 || p.Order[1].DependsOn[0].ChainSlug != "a-first" {
		t.Errorf("b-second should be blocked on a-first: %+v", p.Order[1])
	}
	if p.Order[1].DependsOn[0].Reason != "after a" {
		t.Errorf("why-reason not carried: %+v", p.Order[1].DependsOn[0])
	}
}

func TestRoadmapPlan_Diamond(t *testing.T) {
	pool := openTestPool(t)
	for _, s := range []string{"d-top", "b-mid", "c-mid", "a-base"} {
		seedChain(t, pool, "mcp-servers", s)
	}
	// b,c depend on a; d depends on b,c.
	addDep(t, pool, "mcp-servers", "b-mid", "a-base", "")
	addDep(t, pool, "mcp-servers", "c-mid", "a-base", "")
	addDep(t, pool, "mcp-servers", "d-top", "b-mid", "")
	addDep(t, pool, "mcp-servers", "d-top", "c-mid", "")

	p := plan(t, pool, "mcp-servers")
	got := planSlugs(p)
	if len(got) != 4 {
		t.Fatalf("want 4 placed, got %v", got)
	}
	pos := map[string]int{}
	for i, s := range got {
		pos[s] = i
	}
	if pos["a-base"] != 0 {
		t.Errorf("a-base must be first, got order %v", got)
	}
	if pos["d-top"] != 3 {
		t.Errorf("d-top must be last, got order %v", got)
	}
	if pos["b-mid"] < pos["a-base"] || pos["c-mid"] < pos["a-base"] {
		t.Errorf("b/c must follow a, got %v", got)
	}
	// d-top blocked on both b and c.
	if d := p.Order[3]; len(d.DependsOn) != 2 {
		t.Errorf("d-top should depend on 2, got %+v", d.DependsOn)
	}
}

func TestRoadmapPlan_CycleReported(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "loop-a")
	seedChain(t, pool, "mcp-servers", "loop-b")
	addDep(t, pool, "mcp-servers", "loop-a", "loop-b", "")
	addDep(t, pool, "mcp-servers", "loop-b", "loop-a", "")

	p := plan(t, pool, "mcp-servers")
	if p.Error == "" {
		t.Fatal("expected a cycle error")
	}
	if len(p.Cycle) != 2 {
		t.Fatalf("expected 2 chains in cycle, got %v", p.Cycle)
	}
	// neither cycle member is placed in the order.
	if len(p.Order) != 0 {
		t.Errorf("cycle members should not be placed, got order %v", planSlugs(p))
	}
}

func TestRoadmapPlan_PositionTiebreak(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "zeta") // alphabetically last
	seedChain(t, pool, "mcp-servers", "alpha")
	// Both are ready (no deps); slug tiebreak would put alpha first.
	// A manual roadmap position on zeta (1) must override that.
	if res, err := work.HandleRoadmapSet(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"items": []map[string]any{{"ref_kind": "chain", "ref_slug": "zeta", "position": 1}},
	})); err != nil || !res.OK {
		t.Fatalf("roadmap_set: %+v %v", res, err)
	}

	p := plan(t, pool, "mcp-servers")
	got := planSlugs(p)
	if len(got) != 2 || got[0] != "zeta" {
		t.Fatalf("manual position should win the ready tiebreak: got %v", got)
	}
}

func TestRoadmapPlan_ClosedPrereqSatisfied(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "consumer")
	seedChain(t, pool, "mcp-servers", "foundation")
	addDep(t, pool, "mcp-servers", "consumer", "foundation", "needs it")

	// Close the prerequisite via the lifecycle event so the projection updates.
	if res, err := work.HandleChainClose(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "foundation", "summary": "done",
	})); err != nil || !res.OK {
		t.Fatalf("chain_close: %+v %v", res, err)
	}

	p := plan(t, pool, "mcp-servers")
	// foundation is closed → dropped from the plan; consumer is now ready.
	if got := planSlugs(p); len(got) != 1 || got[0] != "consumer" {
		t.Fatalf("want [consumer] only, got %v", got)
	}
	if p.Order[0].Status != "ready" {
		t.Errorf("consumer should be ready once prereq closed: %+v", p.Order[0])
	}
}
