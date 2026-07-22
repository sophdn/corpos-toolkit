package construct_test

import (
	"context"
	"encoding/json"
	"testing"

	"toolkit/internal/construct"
	"toolkit/internal/db"
)

// readBugRow reads the parity-relevant projection columns for a bug slug.
func readBugRow(t *testing.T, pool *db.Pool, slug string) (severity, status, title, ps string) {
	t.Helper()
	err := pool.DB().QueryRow(
		`SELECT severity, status, title, problem_statement FROM proj_current_bugs WHERE slug = ?`, slug,
	).Scan(&severity, &status, &title, &ps)
	if err != nil {
		t.Fatalf("read bug %q: %v", slug, err)
	}
	return
}

// TestCreateForgeBugParity: a bug filed through construct.Create (the
// agent-facing umbrella) lands a projection identical to forge(bug) for
// equivalent input — the umbrella runs build + dup-check + record submit +
// index-sync internally; this test asserts only the projection-row layer
// (B-F3 index parity has its own test).
func TestCreateForgeBugParity(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	const title = "Parity check title"
	const ps = "compare the forge path against the construct umbrella"

	if _, err := forgeCreateRaw(t, pool, "mcp-servers", json.RawMessage(
		`{"schema_name":"bug","slug":"parity-forge","title":"`+title+`","problem_statement":"`+ps+`"}`,
	)); err != nil {
		t.Fatalf("forge(bug): %v", err)
	}

	deps := construct.Deps{Pool: pool, Schemas: loadForgeRegistry(t)}
	res, err := construct.Create(ctx, deps, "bug", "mcp-servers", construct.Input{
		Bug: &construct.BugInput{Slug: "parity-record", Title: title, ProblemStatement: ps},
	})
	if err != nil {
		t.Fatalf("Create(bug): %v", err)
	}
	if res.EntitySlug != "parity-record" {
		t.Fatalf("CreateResult.EntitySlug=%q, want parity-record", res.EntitySlug)
	}
	if got := len(res.EventsEmitted); got != 1 {
		t.Fatalf("CreateResult.EventsEmitted len=%d, want 1", got)
	}

	fSev, fStat, fTitle, fPs := readBugRow(t, pool, "parity-forge")
	rSev, rStat, rTitle, rPs := readBugRow(t, pool, "parity-record")
	if fSev != rSev || fStat != rStat || fTitle != rTitle || fPs != rPs {
		t.Fatalf("forge vs construct bug parity mismatch:\n  forge:     sev=%q status=%q title=%q ps=%q\n  construct: sev=%q status=%q title=%q ps=%q",
			fSev, fStat, fTitle, fPs, rSev, rStat, rTitle, rPs)
	}
	if rSev != "medium" || rStat != "open" {
		t.Fatalf("construct-path bug defaults wrong: severity=%q status=%q (want medium/open)", rSev, rStat)
	}
}

func readSuggestionRow(t *testing.T, pool *db.Pool, slug string) (priority, status, title, ps string) {
	t.Helper()
	err := pool.DB().QueryRow(
		`SELECT priority, status, title, problem_statement FROM proj_current_suggestions WHERE slug = ?`, slug,
	).Scan(&priority, &status, &title, &ps)
	if err != nil {
		t.Fatalf("read suggestion %q: %v", slug, err)
	}
	return
}

// TestCreateForgeSuggestionParity: the suggestion arm of construct.Create
// lands a proj_current_suggestions row identical to forge(suggestion).
func TestCreateForgeSuggestionParity(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	const title = "Suggestion parity title"
	const ps = "compare forge(suggestion) vs the construct umbrella"

	if _, err := forgeCreateRaw(t, pool, "mcp-servers", json.RawMessage(
		`{"schema_name":"suggestion","slug":"parity-sug-forge","title":"`+title+`","problem_statement":"`+ps+`"}`,
	)); err != nil {
		t.Fatalf("forge(suggestion): %v", err)
	}

	deps := construct.Deps{Pool: pool, Schemas: loadForgeRegistry(t)}
	if _, err := construct.Create(ctx, deps, "suggestion", "mcp-servers", construct.Input{
		Suggestion: &construct.SuggestionInput{Slug: "parity-sug-record", Title: title, ProblemStatement: ps},
	}); err != nil {
		t.Fatalf("Create(suggestion): %v", err)
	}

	fPri, fStat, fTitle, fPs := readSuggestionRow(t, pool, "parity-sug-forge")
	rPri, rStat, rTitle, rPs := readSuggestionRow(t, pool, "parity-sug-record")
	if fPri != rPri || fStat != rStat || fTitle != rTitle || fPs != rPs {
		t.Fatalf("forge vs construct suggestion parity mismatch:\n  forge:     pri=%q status=%q title=%q ps=%q\n  construct: pri=%q status=%q title=%q ps=%q",
			fPri, fStat, fTitle, fPs, rPri, rStat, rTitle, rPs)
	}
	if rPri != "medium" || rStat != "open" {
		t.Fatalf("construct-path suggestion defaults wrong: priority=%q status=%q (want medium/open)", rPri, rStat)
	}
}

func readChainRow(t *testing.T, pool *db.Pool, slug string) (status, output, completionCondition string) {
	t.Helper()
	if err := pool.DB().QueryRow(
		`SELECT status, output, completion_condition FROM proj_chain_status WHERE slug = ?`, slug,
	).Scan(&status, &output, &completionCondition); err != nil {
		t.Fatalf("read chain %q: %v", slug, err)
	}
	return
}

func readTaskRow(t *testing.T, pool *db.Pool, chainSlug, taskSlug string) (position int, status, ps, ac, ctxReq, constraints, handoff string) {
	t.Helper()
	if err := pool.DB().QueryRow(
		`SELECT t.position, t.status, t.problem_statement,
		        COALESCE(t.acceptance_criteria, ''), COALESCE(t.context_required, ''),
		        COALESCE(t.constraints, ''), COALESCE(t.handoff_output, '')
		   FROM proj_current_tasks t JOIN proj_chain_status c ON c.id = t.chain_id
		  WHERE c.slug = ? AND t.slug = ?`, chainSlug, taskSlug,
	).Scan(&position, &status, &ps, &ac, &ctxReq, &constraints, &handoff); err != nil {
		t.Fatalf("read task %q/%q: %v", chainSlug, taskSlug, err)
	}
	return
}

// TestCreateForgeChainParity: a chain forged through construct.Create lands
// a proj_chain_status row identical to forge(chain).
func TestCreateForgeChainParity(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	const output = "the construct umbrella covers chain"
	const cc = "proj_chain_status row matches forge(chain) byte-for-byte"
	const dd = "preserve forge sugar, dispatch via Create"

	mustForgeMap(t, pool, "mcp-servers", map[string]any{
		"schema_name": "chain", "slug": "parity-chain-forge",
		"output": output, "design_decisions": dd, "completion_condition": cc,
	})

	deps := construct.Deps{Pool: pool, Schemas: loadForgeRegistry(t)}
	if _, err := construct.Create(ctx, deps, "chain", "mcp-servers", construct.Input{
		Chain: &construct.ChainInput{Slug: "parity-chain-record", Output: output, DesignDecisions: dd, CompletionCondition: cc},
	}); err != nil {
		t.Fatalf("Create(chain): %v", err)
	}

	fStat, fOut, fCC := readChainRow(t, pool, "parity-chain-forge")
	rStat, rOut, rCC := readChainRow(t, pool, "parity-chain-record")
	if fStat != rStat || fOut != rOut || fCC != rCC {
		t.Fatalf("forge vs construct chain parity mismatch:\n  forge:     status=%q output=%q cc=%q\n  construct: status=%q output=%q cc=%q",
			fStat, fOut, fCC, rStat, rOut, rCC)
	}
	if rStat != "open" {
		t.Fatalf("construct-path chain status=%q (want open)", rStat)
	}
}

// TestCreateForgeTaskParity: a task forged through construct.Create lands a
// proj_current_tasks row identical to forge(task) — including position=1
// (assigned at FOLD time, not construction).
func TestCreateForgeTaskParity(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	const ps = "do the thing the task describes"
	ac := []string{"first criterion", "second criterion"}
	const ctxReq = "read these files first"
	const constraints = "no new deps"

	mustForgeMap(t, pool, "mcp-servers", map[string]any{
		"schema_name": "chain", "slug": "tf-chain",
		"output": "o", "design_decisions": "dd", "completion_condition": "cc",
	})
	mustForgeMap(t, pool, "mcp-servers", map[string]any{
		"schema_name": "task", "slug": "tf-task", "chain_slug": "tf-chain",
		"problem_statement": ps, "acceptance_criteria": ac,
		"context_required": ctxReq, "constraints": constraints,
	})

	deps := construct.Deps{Pool: pool, Schemas: loadForgeRegistry(t)}
	if _, err := construct.Create(ctx, deps, "chain", "mcp-servers", construct.Input{
		Chain: &construct.ChainInput{Slug: "tr-chain", Output: "o", DesignDecisions: "dd", CompletionCondition: "cc"},
	}); err != nil {
		t.Fatalf("Create(chain): %v", err)
	}
	if _, err := construct.Create(ctx, deps, "task", "mcp-servers", construct.Input{
		Task: &construct.TaskInput{
			Slug: "tr-task", ChainSlug: "tr-chain", ProblemStatement: ps,
			AcceptanceCriteria: ac, ContextRequired: ctxReq, Constraints: constraints,
		},
	}); err != nil {
		t.Fatalf("Create(task): %v", err)
	}

	fp, fs, fps, fac, fctx, fcon, fho := readTaskRow(t, pool, "tf-chain", "tf-task")
	rp, rs, rps, rac, rctx, rcon, rho := readTaskRow(t, pool, "tr-chain", "tr-task")
	if fp != rp || fs != rs || fps != rps || fac != rac || fctx != rctx || fcon != rcon || fho != rho {
		t.Fatalf("forge vs construct task parity mismatch:\n  forge:     pos=%d status=%q ps=%q ac=%q ctx=%q con=%q ho=%q\n  construct: pos=%d status=%q ps=%q ac=%q ctx=%q con=%q ho=%q",
			fp, fs, fps, fac, fctx, fcon, fho, rp, rs, rps, rac, rctx, rcon, rho)
	}
	if rp != 1 || rs != "pending" {
		t.Fatalf("construct-path task wrong: position=%d status=%q (want 1/pending)", rp, rs)
	}
	if rac != "first criterion\n- second criterion" {
		t.Fatalf("construct-path task acceptance_criteria join wrong: %q", rac)
	}
}

// TestCreateForgeChainWithTasksFanout: construct.Create with
// Input.ChainWithTasks reproduces forge(chain, tasks=[full-object…]) — the
// chain row + every task row land identically (positions 1..N at fold time).
func TestCreateForgeChainWithTasksFanout(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	mustForgeMap(t, pool, "mcp-servers", map[string]any{
		"schema_name": "chain", "slug": "fan-forge",
		"output": "fan-out output", "design_decisions": "dd", "completion_condition": "cc",
		"tasks": []map[string]any{
			{"slug": "ft1", "problem_statement": "task one body",
				"acceptance_criteria": []string{"a1", "a2"}, "rationale": "first"},
			{"slug": "ft2", "problem_statement": "task two body",
				"acceptance_criteria": []string{"b1"}, "rationale": "second"},
		},
	})

	deps := construct.Deps{Pool: pool, Schemas: loadForgeRegistry(t)}
	res, err := construct.Create(ctx, deps, "chain", "mcp-servers", construct.Input{
		ChainWithTasks: &construct.ChainWithTasksInput{
			ChainInput: construct.ChainInput{
				Slug: "fan-record", Output: "fan-out output", DesignDecisions: "dd", CompletionCondition: "cc",
			},
			Tasks: []construct.ChainTaskInput{
				{Slug: "ft1", ProblemStatement: "task one body", AcceptanceCriteria: []string{"a1", "a2"}, Rationale: "first"},
				{Slug: "ft2", ProblemStatement: "task two body", AcceptanceCriteria: []string{"b1"}, Rationale: "second"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Create(chain+tasks): %v", err)
	}
	// 4 events: ChainCreated + 2x TaskCreated + ChainAndTasksForged.
	if got := len(res.EventsEmitted); got != 4 {
		t.Fatalf("CreateResult.EventsEmitted len=%d, want 4", got)
	}

	fStat, fOut, fCC := readChainRow(t, pool, "fan-forge")
	rStat, rOut, rCC := readChainRow(t, pool, "fan-record")
	if fStat != rStat || fOut != rOut || fCC != rCC {
		t.Fatalf("fan-out chain parity mismatch: forge=(%q,%q,%q) construct=(%q,%q,%q)", fStat, fOut, fCC, rStat, rOut, rCC)
	}
	for _, tk := range []string{"ft1", "ft2"} {
		fp, fs, fps, fac, fctx, fcon, fho := readTaskRow(t, pool, "fan-forge", tk)
		rp, rs, rps, rac, rctx, rcon, rho := readTaskRow(t, pool, "fan-record", tk)
		if fp != rp || fs != rs || fps != rps || fac != rac || fctx != rctx || fcon != rcon || fho != rho {
			t.Fatalf("fan-out task %q parity mismatch:\n  forge:     pos=%d status=%q ps=%q ac=%q\n  construct: pos=%d status=%q ps=%q ac=%q",
				tk, fp, fs, fps, fac, rp, rs, rps, rac)
		}
	}
}

// TestCreateRejectsSchemaInputMismatch proves construct.Create returns a
// clear error (not a panic) when the (schema, Input) pair doesn't agree:
// schema with the wrong field set, both Chain fields set for "chain", or an
// unknown schema.
func TestCreateRejectsSchemaInputMismatch(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	deps := construct.Deps{Pool: pool, Schemas: loadForgeRegistry(t)}

	// schema known, wrong Input field set.
	_, err := construct.Create(ctx, deps, "bug", "mcp-servers", construct.Input{
		Memory: &construct.MemoryInput{Slug: "x", Kind: "project", Description: "d", Body: "b"},
	})
	if err == nil {
		t.Fatalf("Create(bug) with Input.Memory should reject, got nil")
	}

	// schema known, no Input field set.
	_, err = construct.Create(ctx, deps, "bug", "mcp-servers", construct.Input{})
	if err == nil {
		t.Fatalf("Create(bug) with empty Input should reject, got nil")
	}

	// chain accepts either Chain or ChainWithTasks — both set is wrong.
	_, err = construct.Create(ctx, deps, "chain", "mcp-servers", construct.Input{
		Chain:          &construct.ChainInput{Slug: "x", Output: "o", DesignDecisions: "dd", CompletionCondition: "cc"},
		ChainWithTasks: &construct.ChainWithTasksInput{},
	})
	if err == nil {
		t.Fatalf("Create(chain) with both Chain + ChainWithTasks should reject, got nil")
	}

	// unknown schema.
	_, err = construct.Create(ctx, deps, "not-a-real-schema", "mcp-servers", construct.Input{
		Bug: &construct.BugInput{Title: "t", ProblemStatement: "p"},
	})
	if err == nil {
		t.Fatalf("Create(unknown schema) should reject, got nil")
	}

	// missing pool.
	emptyDeps := construct.Deps{Schemas: loadForgeRegistry(t)}
	_, err = construct.Create(ctx, emptyDeps, "bug", "mcp-servers", construct.Input{
		Bug: &construct.BugInput{Title: "t", ProblemStatement: "p"},
	})
	if err == nil {
		t.Fatalf("Create with nil Pool should reject, got nil")
	}
}

// TestCreateBugDupRejectViaUmbrella proves the umbrella runs B-D1 internally:
// a second Create("bug", ...) on the same slug rejects (the caller does not
// have to assemble RejectDuplicateCreate by hand).
func TestCreateBugDupRejectViaUmbrella(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	deps := construct.Deps{Pool: pool, Schemas: loadForgeRegistry(t)}

	if _, err := construct.Create(ctx, deps, "bug", "mcp-servers", construct.Input{
		Bug: &construct.BugInput{Slug: "dup-via-umbrella", Title: "first", ProblemStatement: "ps1"},
	}); err != nil {
		t.Fatalf("first Create(bug): %v", err)
	}
	_, err := construct.Create(ctx, deps, "bug", "mcp-servers", construct.Input{
		Bug: &construct.BugInput{Slug: "dup-via-umbrella", Title: "second", ProblemStatement: "ps2"},
	})
	if err == nil {
		t.Fatalf("second Create(bug) on same slug should reject, got nil")
	}
}

// readFullBugRow reads EVERY parity-relevant proj_current_bugs column so the
// full-field-surface parity test catches any field buildBug fails to carry
// (the gap bug construct-create-bug-suggestion-silently-drops-... documents).
func readFullBugRow(t *testing.T, pool *db.Pool, slug string) map[string]string {
	t.Helper()
	var title, ps, surface, severity, source, ac, constraints, tags, qwen, status string
	err := pool.DB().QueryRow(
		`SELECT title, problem_statement, surface, severity, source, acceptance_criteria,
		        constraints, tags, COALESCE(qwen_task_id,''), status
		   FROM proj_current_bugs WHERE slug = ?`, slug,
	).Scan(&title, &ps, &surface, &severity, &source, &ac, &constraints, &tags, &qwen, &status)
	if err != nil {
		t.Fatalf("read full bug %q: %v", slug, err)
	}
	return map[string]string{
		"title": title, "problem_statement": ps, "surface": surface, "severity": severity,
		"source": source, "acceptance_criteria": ac, "constraints": constraints,
		"tags": tags, "qwen_task_id": qwen, "status": status,
	}
}

// TestCreateForgeBugParity_FullFieldSurface drives forge(bug) and
// construct.Create("bug") with EVERY create-settable field populated
// (surface/source/tags/acceptance_criteria/constraints/qwen_task_id) and
// asserts the proj_current_bugs rows are identical column-for-column. forge(bug)
// writes the row via the BugReported FOLD (createBugInTx only emits the event),
// so identical payloads ⇒ identical rows — this guards the validated-map →
// BugInput conversion + buildBug's full payload against silent field drops.
func TestCreateForgeBugParity_FullFieldSurface(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	if _, err := forgeCreateRaw(t, pool, "mcp-servers", json.RawMessage(`{
		"schema_name":"bug","slug":"full-forge",
		"title":"Full surface bug","problem_statement":"every field set",
		"surface":"construct,forge","severity":"high","source":"agent",
		"acceptance_criteria":["a1","a2"],"constraints":"no new deps","tags":"migration,parity",
		"qwen_task_id":"qt-99"}`),
	); err != nil {
		t.Fatalf("forge(bug) full: %v", err)
	}

	deps := construct.Deps{Pool: pool, Schemas: loadForgeRegistry(t)}
	if _, err := construct.Create(ctx, deps, "bug", "mcp-servers", construct.Input{
		Bug: &construct.BugInput{
			Slug: "full-record", Title: "Full surface bug", ProblemStatement: "every field set",
			Surface: "construct,forge", Severity: "high", Source: "agent",
			AcceptanceCriteria: []string{"a1", "a2"}, Constraints: "no new deps",
			Tags: "migration,parity", QwenTaskID: "qt-99",
		},
	}); err != nil {
		t.Fatalf("Create(bug) full: %v", err)
	}

	f := readFullBugRow(t, pool, "full-forge")
	r := readFullBugRow(t, pool, "full-record")
	for _, col := range []string{"title", "problem_statement", "surface", "severity", "source", "acceptance_criteria", "constraints", "tags", "qwen_task_id", "status"} {
		if f[col] != r[col] {
			t.Errorf("full bug parity mismatch on %q: forge=%q construct=%q", col, f[col], r[col])
		}
	}
	// Sanity: the wider fields actually landed (not both-empty false-pass).
	if r["surface"] == "" || r["acceptance_criteria"] == "" || r["tags"] == "" {
		t.Fatalf("construct full bug row missing wider fields: %+v", r)
	}
}

// readFullSuggestionRow mirrors readFullBugRow for proj_current_suggestions.
func readFullSuggestionRow(t *testing.T, pool *db.Pool, slug string) map[string]string {
	t.Helper()
	var title, ps, surface, priority, source, ac, constraints, tags, status string
	err := pool.DB().QueryRow(
		`SELECT title, problem_statement, surface, priority, source, acceptance_criteria,
		        constraints, tags, status
		   FROM proj_current_suggestions WHERE slug = ?`, slug,
	).Scan(&title, &ps, &surface, &priority, &source, &ac, &constraints, &tags, &status)
	if err != nil {
		t.Fatalf("read full suggestion %q: %v", slug, err)
	}
	return map[string]string{
		"title": title, "problem_statement": ps, "surface": surface, "priority": priority,
		"source": source, "acceptance_criteria": ac, "constraints": constraints,
		"tags": tags, "status": status,
	}
}

// TestCreateForgeSuggestionParity_FullFieldSurface is the suggestion counterpart.
func TestCreateForgeSuggestionParity_FullFieldSurface(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	if _, err := forgeCreateRaw(t, pool, "mcp-servers", json.RawMessage(`{
		"schema_name":"suggestion","slug":"full-sug-forge",
		"title":"Full surface suggestion","problem_statement":"every field set",
		"surface":"docs","priority":"high","source":"agent",
		"acceptance_criteria":["s1","s2"],"constraints":"keep scope small","tags":"tooling,parity"}`),
	); err != nil {
		t.Fatalf("forge(suggestion) full: %v", err)
	}

	deps := construct.Deps{Pool: pool, Schemas: loadForgeRegistry(t)}
	if _, err := construct.Create(ctx, deps, "suggestion", "mcp-servers", construct.Input{
		Suggestion: &construct.SuggestionInput{
			Slug: "full-sug-record", Title: "Full surface suggestion", ProblemStatement: "every field set",
			Surface: "docs", Priority: "high", Source: "agent",
			AcceptanceCriteria: []string{"s1", "s2"}, Constraints: "keep scope small", Tags: "tooling,parity",
		},
	}); err != nil {
		t.Fatalf("Create(suggestion) full: %v", err)
	}

	f := readFullSuggestionRow(t, pool, "full-sug-forge")
	r := readFullSuggestionRow(t, pool, "full-sug-record")
	for _, col := range []string{"title", "problem_statement", "surface", "priority", "source", "acceptance_criteria", "constraints", "tags", "status"} {
		if f[col] != r[col] {
			t.Errorf("full suggestion parity mismatch on %q: forge=%q construct=%q", col, f[col], r[col])
		}
	}
	if r["surface"] == "" || r["acceptance_criteria"] == "" || r["tags"] == "" {
		t.Fatalf("construct full suggestion row missing wider fields: %+v", r)
	}
}
