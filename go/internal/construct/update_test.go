package construct_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"toolkit/internal/construct"
	"toolkit/internal/db"
)

// readBugFull reads the parity-relevant projection columns for a bug edit
// parity check. Wider than the create-side readBugRow because edit
// updates a broader column set (surface / source / tags / AC / constraints
// land here).
func readBugFull(t *testing.T, pool *db.Pool, slug string) (severity, status, title, ps, surface, source, tags, ac, constraints string) {
	t.Helper()
	err := pool.DB().QueryRow(
		`SELECT severity, status, title, problem_statement,
		        COALESCE(surface,''), COALESCE(source,''), COALESCE(tags,''),
		        COALESCE(acceptance_criteria,''), COALESCE(constraints,'')
		   FROM proj_current_bugs WHERE slug = ?`, slug,
	).Scan(&severity, &status, &title, &ps, &surface, &source, &tags, &ac, &constraints)
	if err != nil {
		t.Fatalf("read bug %q: %v", slug, err)
	}
	return
}

func readSuggestionFull(t *testing.T, pool *db.Pool, slug string) (priority, status, title, ps, surface, source, tags, ac, constraints string) {
	t.Helper()
	err := pool.DB().QueryRow(
		`SELECT priority, status, title, problem_statement,
		        COALESCE(surface,''), COALESCE(source,''), COALESCE(tags,''),
		        COALESCE(acceptance_criteria,''), COALESCE(constraints,'')
		   FROM proj_current_suggestions WHERE slug = ?`, slug,
	).Scan(&priority, &status, &title, &ps, &surface, &source, &tags, &ac, &constraints)
	if err != nil {
		t.Fatalf("read suggestion %q: %v", slug, err)
	}
	return
}

func strPtr(s string) *string { return &s }

// TestUpdateForgeBugParity: edit a bug through forge_edit and through
// construct.Update with the same field set; the proj_current_bugs row must
// be byte-identical on the parity-relevant columns.
func TestUpdateForgeBugParity(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	schemas := loadForgeRegistry(t)

	// Two bugs, identical content at create-time.
	for _, slug := range []string{"edit-forge", "edit-record"} {
		mustForgeMap(t, pool, "mcp-servers", map[string]any{
			"schema_name":       "bug",
			"slug":              slug,
			"title":             "Original title",
			"problem_statement": "original problem statement",
			"severity":          "low",
		})
	}

	// Edit via forge_edit.
	res, err := forgeEditRaw(t, pool, "mcp-servers", json.RawMessage(
		`{"schema_name":"bug","slug":"edit-forge",
		   "fields":{"title":"Renamed title","problem_statement":"updated ps",
		             "severity":"high","surface":"forge,construct",
		             "tags":"stage-3,edit","constraints":"stay additive"}}`,
	))
	if err != nil {
		t.Fatalf("forge_edit(bug): %v", err)
	}
	if res.Error != "" {
		t.Fatalf("forge_edit rejected: %s", res.Error)
	}

	// Edit via construct.Update.
	deps := construct.Deps{Pool: pool, Schemas: schemas}
	out, err := construct.Update(ctx, deps, "bug", "mcp-servers", construct.UpdateInput{
		Bug: &construct.BugEditInput{
			Slug:             "edit-record",
			Title:            strPtr("Renamed title"),
			ProblemStatement: strPtr("updated ps"),
			Severity:         strPtr("high"),
			Surface:          strPtr("forge,construct"),
			Tags:             strPtr("stage-3,edit"),
			Constraints:      strPtr("stay additive"),
		},
	})
	if err != nil {
		t.Fatalf("construct.Update(bug): %v", err)
	}
	if out.EntitySlug != "edit-record" {
		t.Fatalf("UpdateResult.EntitySlug=%q, want edit-record", out.EntitySlug)
	}
	wantFields := []string{"constraints", "problem_statement", "severity", "surface", "tags", "title"}
	if got, want := strings.Join(out.UpdatedFields, ","), strings.Join(wantFields, ","); got != want {
		t.Fatalf("UpdatedFields=%v, want %v", out.UpdatedFields, wantFields)
	}

	fSev, fStat, fTitle, fPs, fSurf, fSrc, fTags, fAC, fCon := readBugFull(t, pool, "edit-forge")
	rSev, rStat, rTitle, rPs, rSurf, rSrc, rTags, rAC, rCon := readBugFull(t, pool, "edit-record")
	if fSev != rSev || fStat != rStat || fTitle != rTitle || fPs != rPs ||
		fSurf != rSurf || fSrc != rSrc || fTags != rTags || fAC != rAC || fCon != rCon {
		t.Fatalf("forge_edit vs construct.Update bug parity mismatch:\n  forge:     sev=%q status=%q title=%q ps=%q surface=%q source=%q tags=%q ac=%q con=%q\n  construct: sev=%q status=%q title=%q ps=%q surface=%q source=%q tags=%q ac=%q con=%q",
			fSev, fStat, fTitle, fPs, fSurf, fSrc, fTags, fAC, fCon,
			rSev, rStat, rTitle, rPs, rSurf, rSrc, rTags, rAC, rCon)
	}
	if rTitle != "Renamed title" {
		t.Fatalf("construct.Update did not apply title: got %q", rTitle)
	}
}

// TestUpdateForgeBugParityAcceptanceCriteriaList exercises the list-shaped
// AcceptanceCriteria field: both paths must join on "\n- " identically.
func TestUpdateForgeBugParityAcceptanceCriteriaList(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	schemas := loadForgeRegistry(t)

	for _, slug := range []string{"ac-forge", "ac-record"} {
		mustForgeMap(t, pool, "mcp-servers", map[string]any{
			"schema_name": "bug", "slug": slug,
			"title": "Original", "problem_statement": "ps",
		})
	}

	if _, err := forgeEditRaw(t, pool, "mcp-servers", json.RawMessage(
		`{"schema_name":"bug","slug":"ac-forge",
		   "fields":{"acceptance_criteria":["criterion one","criterion two"]}}`,
	)); err != nil {
		t.Fatalf("forge_edit(bug, ac): %v", err)
	}

	deps := construct.Deps{Pool: pool, Schemas: schemas}
	ac := []string{"criterion one", "criterion two"}
	if _, err := construct.Update(ctx, deps, "bug", "mcp-servers", construct.UpdateInput{
		Bug: &construct.BugEditInput{Slug: "ac-record", AcceptanceCriteria: &ac},
	}); err != nil {
		t.Fatalf("construct.Update(bug, ac): %v", err)
	}

	_, _, _, _, _, _, _, fAC, _ := readBugFull(t, pool, "ac-forge")
	_, _, _, _, _, _, _, rAC, _ := readBugFull(t, pool, "ac-record")
	if fAC != rAC {
		t.Fatalf("acceptance_criteria join mismatch: forge=%q construct=%q", fAC, rAC)
	}
	if rAC != "criterion one\n- criterion two" {
		t.Fatalf("AC list join wrong: got %q want %q", rAC, "criterion one\n- criterion two")
	}
}

// TestUpdateForgeSuggestionParity: edit a suggestion through both paths;
// proj_current_suggestions row byte-identical on parity columns.
func TestUpdateForgeSuggestionParity(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	schemas := loadForgeRegistry(t)

	for _, slug := range []string{"sug-edit-forge", "sug-edit-record"} {
		mustForgeMap(t, pool, "mcp-servers", map[string]any{
			"schema_name":       "suggestion",
			"slug":              slug,
			"title":             "Original title",
			"problem_statement": "ps",
			"priority":          "low",
		})
	}

	if _, err := forgeEditRaw(t, pool, "mcp-servers", json.RawMessage(
		`{"schema_name":"suggestion","slug":"sug-edit-forge",
		   "fields":{"title":"New title","priority":"high","tags":"alpha,beta"}}`,
	)); err != nil {
		t.Fatalf("forge_edit(suggestion): %v", err)
	}

	deps := construct.Deps{Pool: pool, Schemas: schemas}
	if _, err := construct.Update(ctx, deps, "suggestion", "mcp-servers", construct.UpdateInput{
		Suggestion: &construct.SuggestionEditInput{
			Slug:  "sug-edit-record",
			Title: strPtr("New title"), Priority: strPtr("high"), Tags: strPtr("alpha,beta"),
		},
	}); err != nil {
		t.Fatalf("construct.Update(suggestion): %v", err)
	}

	fPri, fStat, fTitle, fPs, fSurf, fSrc, fTags, fAC, fCon := readSuggestionFull(t, pool, "sug-edit-forge")
	rPri, rStat, rTitle, rPs, rSurf, rSrc, rTags, rAC, rCon := readSuggestionFull(t, pool, "sug-edit-record")
	if fPri != rPri || fStat != rStat || fTitle != rTitle || fPs != rPs ||
		fSurf != rSurf || fSrc != rSrc || fTags != rTags || fAC != rAC || fCon != rCon {
		t.Fatalf("forge_edit vs construct.Update suggestion parity mismatch:\n  forge:     pri=%q title=%q tags=%q\n  construct: pri=%q title=%q tags=%q",
			fPri, fTitle, fTags, rPri, rTitle, rTags)
	}
	if rTitle != "New title" || rPri != "high" {
		t.Fatalf("construct.Update suggestion did not apply: title=%q priority=%q", rTitle, rPri)
	}
}

// TestUpdateBugRejectsPlaceholderShape: B-G1 — a `{{NAME}}` whole-value
// placeholder rejects pre-emit (parity with forge_edit's default policy).
func TestUpdateBugRejectsPlaceholderShape(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	mustForgeMap(t, pool, "mcp-servers", map[string]any{
		"schema_name": "bug", "slug": "guard-placeholder",
		"title": "t", "problem_statement": "ps",
	})

	deps := construct.Deps{Pool: pool, Schemas: loadForgeRegistry(t)}
	_, err := construct.Update(ctx, deps, "bug", "mcp-servers", construct.UpdateInput{
		Bug: &construct.BugEditInput{
			Slug:             "guard-placeholder",
			ProblemStatement: strPtr("{{EXISTING_PROBLEM_STATEMENT_PLACEHOLDER}}"),
		},
	})
	if err == nil {
		t.Fatalf("expected B-G1 placeholder rejection, got nil")
	}
	if !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("expected error mentioning placeholder, got: %v", err)
	}

	// Embedded {{X}} substring is NOT a whole-value placeholder → must pass.
	if _, err := construct.Update(ctx, deps, "bug", "mcp-servers", construct.UpdateInput{
		Bug: &construct.BugEditInput{
			Slug:             "guard-placeholder",
			ProblemStatement: strPtr("see {{TEMPLATE}} in the docs"),
		},
	}); err != nil {
		t.Fatalf("embedded placeholder substring must pass: %v", err)
	}
}

// TestUpdateBugRejectsRequiredEmpty: B-ED1 — setting a required field to
// empty rejects (severity is optional so won't trip; title is required).
// Mirrors forge_edit's ValidatePartial behavior.
func TestUpdateBugRejectsRequiredEmpty(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	mustForgeMap(t, pool, "mcp-servers", map[string]any{
		"schema_name": "bug", "slug": "guard-req-empty",
		"title": "t", "problem_statement": "ps",
	})

	deps := construct.Deps{Pool: pool, Schemas: loadForgeRegistry(t)}
	empty := ""
	_, err := construct.Update(ctx, deps, "bug", "mcp-servers", construct.UpdateInput{
		Bug: &construct.BugEditInput{Slug: "guard-req-empty", Title: &empty},
	})
	if err == nil {
		t.Fatalf("expected B-ED1 empty-required rejection on title, got nil")
	}
}

// TestUpdateBugRejectsNotFound: B-ED1 — editing an unknown slug returns a
// not-found error (matches forge_edit's `not_found` envelope).
func TestUpdateBugRejectsNotFound(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	deps := construct.Deps{Pool: pool, Schemas: loadForgeRegistry(t)}
	_, err := construct.Update(ctx, deps, "bug", "mcp-servers", construct.UpdateInput{
		Bug: &construct.BugEditInput{Slug: "no-such-bug", Title: strPtr("X")},
	})
	if err == nil {
		t.Fatalf("expected not-found error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error should say not found: %v", err)
	}
}

// TestUpdateBugRejectsNoFields: an empty edit input rejects (forge_edit's
// "no field updates supplied" parity).
func TestUpdateBugRejectsNoFields(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	mustForgeMap(t, pool, "mcp-servers", map[string]any{
		"schema_name": "bug", "slug": "no-fields",
		"title": "t", "problem_statement": "ps",
	})

	deps := construct.Deps{Pool: pool, Schemas: loadForgeRegistry(t)}
	_, err := construct.Update(ctx, deps, "bug", "mcp-servers", construct.UpdateInput{
		Bug: &construct.BugEditInput{Slug: "no-fields"},
	})
	if err == nil {
		t.Fatalf("expected no-field-updates rejection, got nil")
	}
}

// TestUpdateRejectsSchemaInputMismatch: union discipline — wrong / empty /
// double-set / unknown-schema → clear error, no panic.
func TestUpdateRejectsSchemaInputMismatch(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	deps := construct.Deps{Pool: pool, Schemas: loadForgeRegistry(t)}

	if _, err := construct.Update(ctx, deps, "bug", "mcp-servers", construct.UpdateInput{
		Suggestion: &construct.SuggestionEditInput{Slug: "x", Title: strPtr("y")},
	}); err == nil {
		t.Fatalf("Update(bug) with Suggestion input must reject")
	}
	if _, err := construct.Update(ctx, deps, "bug", "mcp-servers", construct.UpdateInput{}); err == nil {
		t.Fatalf("Update(bug) with empty input must reject")
	}
	if _, err := construct.Update(ctx, deps, "bug", "mcp-servers", construct.UpdateInput{
		Bug:        &construct.BugEditInput{Slug: "x", Title: strPtr("y")},
		Suggestion: &construct.SuggestionEditInput{Slug: "x", Title: strPtr("y")},
	}); err == nil {
		t.Fatalf("Update(bug) with both Bug + Suggestion must reject")
	}
	if _, err := construct.Update(ctx, deps, "not-a-real-schema", "mcp-servers", construct.UpdateInput{
		Bug: &construct.BugEditInput{Slug: "x", Title: strPtr("y")},
	}); err == nil {
		t.Fatalf("Update(unknown schema) must reject")
	}
	emptyDeps := construct.Deps{Schemas: loadForgeRegistry(t)}
	if _, err := construct.Update(ctx, emptyDeps, "bug", "mcp-servers", construct.UpdateInput{
		Bug: &construct.BugEditInput{Slug: "x", Title: strPtr("y")},
	}); err == nil {
		t.Fatalf("Update with nil Pool must reject")
	}
}

// TestUpdateBugIndexSyncRefreshesPointer: B-F3 parity — after a title edit,
// the knowledge_pointer's title (description fallback) reflects the new
// title, matching forge_edit's OnEdit notifier behavior.
func TestUpdateBugIndexSyncRefreshesPointer(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	schemas := loadForgeRegistry(t)

	// Create the bug + its initial pointer via forge.
	mustForgeMap(t, pool, "mcp-servers", map[string]any{
		"schema_name": "bug", "slug": "idx-edit",
		"title": "Initial title", "problem_statement": "ps",
	})
	// Seed the pointer (Stage 2 SyncCreateIndex runs inside construct.Create;
	// the mustForgeMap path doesn't wire OnCreate, so seed it here).
	if _, err := construct.IndexSyncFromProjection(ctx, pool, schemas, "bug", "mcp-servers", "idx-edit"); err != nil {
		t.Fatalf("seed pointer: %v", err)
	}

	deps := construct.Deps{Pool: pool, Schemas: schemas}
	if _, err := construct.Update(ctx, deps, "bug", "mcp-servers", construct.UpdateInput{
		Bug: &construct.BugEditInput{Slug: "idx-edit", Title: strPtr("Refreshed title")},
	}); err != nil {
		t.Fatalf("Update(bug): %v", err)
	}

	// The bug pointer's Question is the title (truncated to 200 chars); after
	// edit, B-F3's read-back-from-projection + Upsert refreshes it.
	var question string
	err := pool.DB().QueryRow(
		`SELECT question FROM knowledge_pointers WHERE source_type = 'bug' AND source_ref = 'mcp-servers::idx-edit'`,
	).Scan(&question)
	if err != nil {
		t.Fatalf("read pointer: %v", err)
	}
	if question != "Refreshed title" {
		t.Fatalf("pointer question=%q, want Refreshed title (B-F3 index sync regression)", question)
	}
}

// ── Slice 2: chain + task parity ────────────────────────────────────────────

func readChainFull(t *testing.T, pool *db.Pool, slug string) (status, output, cc string) {
	t.Helper()
	if err := pool.DB().QueryRow(
		`SELECT status, output, completion_condition
		   FROM proj_chain_status WHERE slug = ?`, slug,
	).Scan(&status, &output, &cc); err != nil {
		t.Fatalf("read chain %q: %v", slug, err)
	}
	return
}

func readTaskFull(t *testing.T, pool *db.Pool, chainSlug, taskSlug string) (status, ps, ac, ctxReq, constraints, handoff string) {
	t.Helper()
	if err := pool.DB().QueryRow(
		`SELECT t.status, t.problem_statement,
		        COALESCE(t.acceptance_criteria, ''), COALESCE(t.context_required, ''),
		        COALESCE(t.constraints, ''), COALESCE(t.handoff_output, '')
		   FROM proj_current_tasks t JOIN proj_chain_status c ON c.id = t.chain_id
		  WHERE c.slug = ? AND t.slug = ?`, chainSlug, taskSlug,
	).Scan(&status, &ps, &ac, &ctxReq, &constraints, &handoff); err != nil {
		t.Fatalf("read task %q/%q: %v", chainSlug, taskSlug, err)
	}
	return
}

// TestUpdateForgeChainParity: edit a chain through forge_edit and through
// construct.Update; proj_chain_status row byte-identical on the folded
// columns. (design_decisions stays on the event payload but no longer
// folds — migration 065 — so it doesn't appear in the projection compare.)
func TestUpdateForgeChainParity(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	schemas := loadForgeRegistry(t)

	for _, slug := range []string{"chain-edit-forge", "chain-edit-record"} {
		mustForgeMap(t, pool, "mcp-servers", map[string]any{
			"schema_name": "chain", "slug": slug,
			"output": "original output", "design_decisions": "dd-original",
			"completion_condition": "cc-original",
		})
	}

	if _, err := forgeEditRaw(t, pool, "mcp-servers", json.RawMessage(
		`{"schema_name":"chain","slug":"chain-edit-forge",
		   "fields":{"output":"updated output","completion_condition":"updated cc"}}`,
	)); err != nil {
		t.Fatalf("forge_edit(chain): %v", err)
	}

	deps := construct.Deps{Pool: pool, Schemas: schemas}
	if _, err := construct.Update(ctx, deps, "chain", "mcp-servers", construct.UpdateInput{
		Chain: &construct.ChainEditInput{
			Slug:                "chain-edit-record",
			Output:              strPtr("updated output"),
			CompletionCondition: strPtr("updated cc"),
		},
	}); err != nil {
		t.Fatalf("construct.Update(chain): %v", err)
	}

	fStat, fOut, fCC := readChainFull(t, pool, "chain-edit-forge")
	rStat, rOut, rCC := readChainFull(t, pool, "chain-edit-record")
	if fStat != rStat || fOut != rOut || fCC != rCC {
		t.Fatalf("forge vs construct chain edit parity mismatch:\n  forge:     status=%q output=%q cc=%q\n  construct: status=%q output=%q cc=%q",
			fStat, fOut, fCC, rStat, rOut, rCC)
	}
	if rOut != "updated output" || rCC != "updated cc" {
		t.Fatalf("construct.Update chain did not apply: output=%q cc=%q", rOut, rCC)
	}
}

// TestUpdateForgeTaskParity: edit a task through both paths; proj_current_tasks
// row byte-identical on parity columns.
func TestUpdateForgeTaskParity(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	schemas := loadForgeRegistry(t)

	for _, chainSlug := range []string{"tedit-chain-forge", "tedit-chain-record"} {
		mustForgeMap(t, pool, "mcp-servers", map[string]any{
			"schema_name": "chain", "slug": chainSlug,
			"output": "o", "design_decisions": "dd", "completion_condition": "cc",
		})
		mustForgeMap(t, pool, "mcp-servers", map[string]any{
			"schema_name": "task", "slug": "tedit-task",
			"chain_slug":        chainSlug,
			"problem_statement": "original ps", "context_required": "original ctx",
		})
	}

	if _, err := forgeEditRaw(t, pool, "mcp-servers", json.RawMessage(
		`{"schema_name":"task","slug":"tedit-task","chain_slug":"tedit-chain-forge",
		   "fields":{"problem_statement":"updated ps","context_required":"updated ctx",
		             "acceptance_criteria":["a1","a2"],"constraints":"updated con","handoff_output":"updated ho"}}`,
	)); err != nil {
		t.Fatalf("forge_edit(task): %v", err)
	}

	deps := construct.Deps{Pool: pool, Schemas: schemas}
	ac := []string{"a1", "a2"}
	if _, err := construct.Update(ctx, deps, "task", "mcp-servers", construct.UpdateInput{
		Task: &construct.TaskEditInput{
			Slug:               "tedit-task",
			ChainSlug:          "tedit-chain-record",
			ProblemStatement:   strPtr("updated ps"),
			ContextRequired:    strPtr("updated ctx"),
			AcceptanceCriteria: &ac,
			Constraints:        strPtr("updated con"),
			HandoffOutput:      strPtr("updated ho"),
		},
	}); err != nil {
		t.Fatalf("construct.Update(task): %v", err)
	}

	fStat, fPs, fAC, fCtx, fCon, fHo := readTaskFull(t, pool, "tedit-chain-forge", "tedit-task")
	rStat, rPs, rAC, rCtx, rCon, rHo := readTaskFull(t, pool, "tedit-chain-record", "tedit-task")
	if fStat != rStat || fPs != rPs || fAC != rAC || fCtx != rCtx || fCon != rCon || fHo != rHo {
		t.Fatalf("forge vs construct task edit parity mismatch:\n  forge:     status=%q ps=%q ac=%q ctx=%q con=%q ho=%q\n  construct: status=%q ps=%q ac=%q ctx=%q con=%q ho=%q",
			fStat, fPs, fAC, fCtx, fCon, fHo, rStat, rPs, rAC, rCtx, rCon, rHo)
	}
	if rPs != "updated ps" || rAC != "a1\n- a2" {
		t.Fatalf("construct.Update task fields wrong: ps=%q ac=%q", rPs, rAC)
	}
}

// TestUpdateTaskRequiresChainSlug: task edit without chain_slug rejects
// (anti-fanout discipline — TaskEditedPayload.ChainSlug is load-bearing).
func TestUpdateTaskRequiresChainSlug(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	deps := construct.Deps{Pool: pool, Schemas: loadForgeRegistry(t)}

	mustForgeMap(t, pool, "mcp-servers", map[string]any{
		"schema_name": "chain", "slug": "no-cs-chain",
		"output": "o", "design_decisions": "dd", "completion_condition": "cc",
	})
	mustForgeMap(t, pool, "mcp-servers", map[string]any{
		"schema_name": "task", "slug": "no-cs-task",
		"chain_slug": "no-cs-chain", "problem_statement": "ps",
	})

	_, err := construct.Update(ctx, deps, "task", "mcp-servers", construct.UpdateInput{
		Task: &construct.TaskEditInput{
			Slug:             "no-cs-task",
			ProblemStatement: strPtr("changed"),
		},
	})
	if err == nil {
		t.Fatalf("task edit without ChainSlug must reject")
	}
}

// TestUpdateChainIndexSyncRefreshesPointer: B-F3 on chain — edit the
// completion_condition; the knowledge_pointer description (fallback when
// design_decisions is empty per buildChainPointer) reflects the new value.
func TestUpdateChainIndexSyncRefreshesPointer(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	schemas := loadForgeRegistry(t)

	mustForgeMap(t, pool, "mcp-servers", map[string]any{
		"schema_name": "chain", "slug": "chain-idx-edit",
		"output": "starting output", "design_decisions": "dd",
		"completion_condition": "starting cc",
	})
	if _, err := construct.IndexSyncFromProjection(ctx, pool, schemas, "chain", "mcp-servers", "chain-idx-edit"); err != nil {
		t.Fatalf("seed chain pointer: %v", err)
	}

	deps := construct.Deps{Pool: pool, Schemas: schemas}
	if _, err := construct.Update(ctx, deps, "chain", "mcp-servers", construct.UpdateInput{
		Chain: &construct.ChainEditInput{
			Slug:   "chain-idx-edit",
			Output: strPtr("refreshed output"),
		},
	}); err != nil {
		t.Fatalf("Update(chain): %v", err)
	}

	var question string
	if err := pool.DB().QueryRow(
		`SELECT question FROM knowledge_pointers WHERE source_type = 'chain' AND source_ref = 'mcp-servers::chain-idx-edit'`,
	).Scan(&question); err != nil {
		t.Fatalf("read chain pointer: %v", err)
	}
	if question != "refreshed output" {
		t.Fatalf("chain pointer question=%q, want refreshed output", question)
	}
}
