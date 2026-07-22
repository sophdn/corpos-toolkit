package work_test

import (
	"context"
	"strings"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/work"
)

// seedTrainedModel inserts a trained_model row directly via SQL. Used by
// the lifecycle tests to set up known starting states without going
// through the forge layer.
func seedTrainedModel(t *testing.T, pool *db.Pool, project, slug, task, version, status string) {
	t.Helper()
	if _, err := pool.DB().Exec(
		`INSERT INTO trained_models
			(project_id, slug, task, version, training_dataset_signature,
			 eval_metrics, status, artifact_path)
		 VALUES (?, ?, ?, ?, 'proj@2026-05-19T00:00:00Z;rows=100',
			 '{"macro_f1":0.7,"baseline_score":0.5}', ?, ?)`,
		project, slug, task, version, status, task+"/"+version+"/model.onnx"); err != nil {
		t.Fatalf("seed trained_model %q: %v", slug, err)
	}
}

// TestTrainedModelLifecycle pins the substrate's promotion-and-retirement
// round trip: forge an evaluating row, manually flip to ab_testing,
// promote, retire. Exercises every state transition the schema permits
// from outside the trained_model_promote / _retire handlers and the
// trained_model_list filter set.
func TestTrainedModelLifecycle(t *testing.T) {
	pool := openTestPool(t)

	// 1. Seed an evaluating row.
	seedTrainedModel(t, pool, "mcp-servers", "source-router-v1", "source-router", "v1", "evaluating")

	// 2. list with status=evaluating finds it.
	listResp, err := work.HandleTrainedModelList(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"status": "evaluating",
	}))
	if err != nil {
		t.Fatalf("HandleTrainedModelList (evaluating): %v", err)
	}
	if len(listResp.DefaultItems) != 1 || listResp.DefaultItems[0].Slug != "source-router-v1" {
		t.Errorf("expected one evaluating row, got %+v", listResp.DefaultItems)
	}

	// 3. Promote without force from 'evaluating' is rejected.
	promoteResp, err := work.HandleTrainedModelPromote(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "source-router-v1",
	}))
	if err != nil {
		t.Fatalf("HandleTrainedModelPromote (no force from evaluating): %v", err)
	}
	if promoteResp.Error == "" {
		t.Errorf("expected promote-without-force rejection, got %+v", promoteResp)
	}
	if !strings.Contains(promoteResp.Hint, `"force": true`) {
		t.Errorf("expected hint to mention force=true override, got hint=%q", promoteResp.Hint)
	}

	// 4. Manually flip to ab_testing (the A/B harness's responsibility in
	// production; tests bypass via SQL because §8 is T6's territory).
	if _, err := pool.DB().Exec(
		`UPDATE trained_models SET status = 'ab_testing' WHERE slug = ? AND project_id = ?`,
		"source-router-v1", "mcp-servers"); err != nil {
		t.Fatalf("manual flip to ab_testing: %v", err)
	}

	// 5. Promote from ab_testing succeeds.
	promoteResp, err = work.HandleTrainedModelPromote(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "source-router-v1",
	}))
	if err != nil {
		t.Fatalf("HandleTrainedModelPromote (from ab_testing): %v", err)
	}
	if !promoteResp.OK || promoteResp.FromStatus != "ab_testing" || promoteResp.ToStatus != "promoted" {
		t.Errorf("expected ab_testing→promoted, got %+v", promoteResp)
	}

	// 6. list with status=promoted finds it.
	listResp, err = work.HandleTrainedModelList(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"status": "promoted",
	}))
	if err != nil {
		t.Fatalf("HandleTrainedModelList (promoted): %v", err)
	}
	if len(listResp.DefaultItems) != 1 {
		t.Errorf("expected one promoted row, got %+v", listResp.DefaultItems)
	}

	// 7. Retire with reason — eval_metrics gets json_set'd.
	retireResp, err := work.HandleTrainedModelRetire(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":   "source-router-v1",
		"reason": "superseded by v2",
	}))
	if err != nil {
		t.Fatalf("HandleTrainedModelRetire: %v", err)
	}
	if !retireResp.OK || retireResp.FromStatus != "promoted" || retireResp.ToStatus != "retired" {
		t.Errorf("expected promoted→retired, got %+v", retireResp)
	}

	// 8. Verify retirement_reason was merged into eval_metrics.
	var eval string
	if err := pool.DB().QueryRow(
		`SELECT eval_metrics FROM trained_models WHERE slug = ? AND project_id = ?`,
		"source-router-v1", "mcp-servers").Scan(&eval); err != nil {
		t.Fatalf("read eval_metrics: %v", err)
	}
	if !strings.Contains(eval, "superseded by v2") {
		t.Errorf("expected retirement_reason merged into eval_metrics, got %q", eval)
	}

	// 9. Retire again is a no-op (idempotent).
	retireResp, err = work.HandleTrainedModelRetire(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "source-router-v1",
	}))
	if err != nil {
		t.Fatalf("HandleTrainedModelRetire (idempotent): %v", err)
	}
	if !retireResp.OK || retireResp.Hint == "" {
		t.Errorf("expected idempotent retire to OK with hint, got %+v", retireResp)
	}
}

// TestTrainedModelPromote_Force confirms the force-override path works
// from a non-ab_testing state. This is the audited bypass — production
// callers must include rationale on the work-surface envelope.
func TestTrainedModelPromote_Force(t *testing.T) {
	pool := openTestPool(t)
	seedTrainedModel(t, pool, "mcp-servers", "tagger-v1", "bug-surface-tagger", "v1", "evaluating")

	resp, err := work.HandleTrainedModelPromote(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":  "tagger-v1",
		"force": true,
	}))
	if err != nil {
		t.Fatalf("HandleTrainedModelPromote (force): %v", err)
	}
	if !resp.OK || resp.ToStatus != "promoted" || !resp.Forced {
		t.Errorf("expected forced evaluating→promoted, got %+v", resp)
	}
}

// TestTrainedModelList_TaskFilter confirms task-scoped queries return
// only the matching rows (the registry's typical lookup pattern).
func TestTrainedModelList_TaskFilter(t *testing.T) {
	pool := openTestPool(t)
	seedTrainedModel(t, pool, "mcp-servers", "router-v1", "source-router", "v1", "promoted")
	seedTrainedModel(t, pool, "mcp-servers", "router-v2", "source-router", "v2", "ab_testing")
	seedTrainedModel(t, pool, "mcp-servers", "tagger-v1", "bug-surface-tagger", "v1", "promoted")

	resp, err := work.HandleTrainedModelList(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"task": "source-router",
	}))
	if err != nil {
		t.Fatalf("HandleTrainedModelList: %v", err)
	}
	if len(resp.DefaultItems) != 2 {
		t.Errorf("expected 2 source-router rows, got %d (%+v)", len(resp.DefaultItems), resp.DefaultItems)
	}
	for _, it := range resp.DefaultItems {
		if it.Task != "source-router" {
			t.Errorf("expected task=source-router, got %q", it.Task)
		}
	}
}

// TestTrainedModelList_InvalidStatus confirms the status-enum validator
// rejects bad values at the handler seam rather than at the CHECK
// constraint.
func TestTrainedModelList_InvalidStatus(t *testing.T) {
	pool := openTestPool(t)
	resp, err := work.HandleTrainedModelList(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"status": "deployed", // not in the enum
	}))
	if err != nil {
		t.Fatalf("HandleTrainedModelList: %v", err)
	}
	if resp.Error == "" || !strings.Contains(resp.Error, "invalid status filter") {
		t.Errorf("expected invalid-status rejection, got %+v", resp)
	}
}

// TestTrainedModelPromote_NotFound surfaces a typed envelope rather
// than a SQL no-rows leak.
func TestTrainedModelPromote_NotFound(t *testing.T) {
	pool := openTestPool(t)
	resp, err := work.HandleTrainedModelPromote(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "ghost-v9",
	}))
	if err != nil {
		t.Fatalf("HandleTrainedModelPromote: %v", err)
	}
	if resp.Error == "" || !strings.Contains(resp.Error, "not found") {
		t.Errorf("expected not-found rejection, got %+v", resp)
	}
}

// TestTrainedModelPromote_AcceptsIDAlias pins bug 1329 parity for the
// trained_model surface: trained_model_promote accepts {id: N} as a slug
// alias so trained_model_list → trained_model_promote can stay
// id-keyed (trained_model_list's compact projection surfaces id first).
func TestTrainedModelPromote_AcceptsIDAlias(t *testing.T) {
	pool := openTestPool(t)
	seedTrainedModel(t, pool, "mcp-servers", "router-v3", "source-router", "v3", "ab_testing")
	var id int64
	if err := pool.DB().QueryRow(`SELECT id FROM trained_models WHERE slug = 'router-v3'`).Scan(&id); err != nil {
		t.Fatalf("fetch id: %v", err)
	}
	resp, err := work.HandleTrainedModelPromote(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"id": id,
	}))
	if err != nil {
		t.Fatalf("HandleTrainedModelPromote: %v", err)
	}
	if !resp.OK || resp.Slug != "router-v3" || resp.ToStatus != "promoted" {
		t.Fatalf("expected ok+slug=router-v3+promoted, got %+v", resp)
	}
}

// TestTrainedModelPromote_IDNotFoundErrors locks in the error path: an
// id that doesn't resolve surfaces 'trained_model id N not found'.
func TestTrainedModelPromote_IDNotFoundErrors(t *testing.T) {
	pool := openTestPool(t)
	resp, _ := work.HandleTrainedModelPromote(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"id": 99999,
	}))
	if resp.Error == "" || !strings.Contains(resp.Error, "99999") || !strings.Contains(resp.Error, "not found") {
		t.Errorf("expected not-found error citing id 99999, got %q", resp.Error)
	}
}

// TestTrainedModelPromote_NeitherSlugNorIDErrors keeps the
// missing-identifier path honest with the id alias added.
func TestTrainedModelPromote_NeitherSlugNorIDErrors(t *testing.T) {
	pool := openTestPool(t)
	resp, _ := work.HandleTrainedModelPromote(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{}))
	if resp.Error == "" || !strings.Contains(resp.Error, "slug") || !strings.Contains(resp.Error, "id") {
		t.Errorf("expected error naming slug AND id, got %q", resp.Error)
	}
}

// TestTrainedModelRetire_AcceptsIDAlias mirrors bug 1329 for the
// retirement action.
func TestTrainedModelRetire_AcceptsIDAlias(t *testing.T) {
	pool := openTestPool(t)
	seedTrainedModel(t, pool, "mcp-servers", "router-v4", "source-router", "v4", "promoted")
	var id int64
	pool.DB().QueryRow(`SELECT id FROM trained_models WHERE slug = 'router-v4'`).Scan(&id)
	resp, _ := work.HandleTrainedModelRetire(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"id":     id,
		"reason": "rotated",
	}))
	if !resp.OK || resp.Slug != "router-v4" || resp.ToStatus != "retired" {
		t.Fatalf("expected ok+slug=router-v4+retired, got %+v", resp)
	}
}

// TestTrainedModelRetire_IDNotFoundErrors locks in the error path.
func TestTrainedModelRetire_IDNotFoundErrors(t *testing.T) {
	pool := openTestPool(t)
	resp, _ := work.HandleTrainedModelRetire(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"id": 99999,
	}))
	if resp.Error == "" || !strings.Contains(resp.Error, "99999") || !strings.Contains(resp.Error, "not found") {
		t.Errorf("expected not-found error citing id 99999, got %q", resp.Error)
	}
}

// readTrainedModelRow reads back every authored column for a (project, slug) row.
// Used by the create-parity test to pin HandleTrainedModelCreate's column set
// against the canonical trained_models row shape.
func readTrainedModelRow(t *testing.T, pool *db.Pool, project, slug string) (task, version, sig, evalMetrics, status, artifact, createdAt, updatedAt string) {
	t.Helper()
	err := pool.DB().QueryRow(
		`SELECT task, version, training_dataset_signature, eval_metrics, status,
			artifact_path, created_at, updated_at
		 FROM trained_models WHERE project_id = ? AND slug = ?`,
		project, slug).Scan(&task, &version, &sig, &evalMetrics, &status, &artifact, &createdAt, &updatedAt)
	if err != nil {
		t.Fatalf("read trained_models row %q: %v", slug, err)
	}
	return
}

// TestHandleTrainedModelCreate_RowParity pins the chain 311 T7 Stage 6 P2-C.1
// minimal sever: trained_model create routes to work.HandleTrainedModelCreate
// (a direct INSERT) instead of forge.GenericStrategy. It asserts the handler
// writes the canonical trained_models row shape — the same column set
// forge.GenericStrategy.Create produced (forge side pinned independently by
// forge/strategy_test.go TestGenericStrategy_CreateParity). Archive-safe: this
// test asserts explicit canonical values rather than importing the soon-archived
// forge.GenericStrategy, so it survives the P2-C.2 archive untouched.
func TestHandleTrainedModelCreate_RowParity(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	const (
		project = "mcp-servers"
		sig     = "proj_training_data@2026-05-19T00:00:00Z;rows=841"
		metrics = `{"macro_f1":0.71,"baseline_score":0.58}`
	)

	// Status OMITTED → DB default 'training' (the handler applies the same
	// default the migration-043 column DEFAULT would, byte-identical either way).
	if err := work.HandleTrainedModelCreate(ctx, pool, project, "source-router-v1",
		"source-router", "v1", sig, metrics, "", "source-router/v1/model.onnx"); err != nil {
		t.Fatalf("HandleTrainedModelCreate (status omitted): %v", err)
	}
	task, version, gotSig, gotMetrics, status, artifact, createdAt, updatedAt := readTrainedModelRow(t, pool, project, "source-router-v1")
	if task != "source-router" || version != "v1" || gotSig != sig ||
		gotMetrics != metrics || artifact != "source-router/v1/model.onnx" {
		t.Errorf("authored columns mismatch: task=%q version=%q sig=%q metrics=%q artifact=%q",
			task, version, gotSig, gotMetrics, artifact)
	}
	if status != "training" {
		t.Errorf("omitted status: want default 'training', got %q", status)
	}
	if createdAt == "" || updatedAt == "" {
		t.Errorf("timestamps not stamped: created_at=%q updated_at=%q", createdAt, updatedAt)
	}

	// Status PROVIDED → persisted verbatim (within the CHECK enum).
	if err := work.HandleTrainedModelCreate(ctx, pool, project, "curation-classifier-v2",
		"curation-classifier", "v2", sig, metrics, "ab_testing", "curation-classifier/v2/model.onnx"); err != nil {
		t.Fatalf("HandleTrainedModelCreate (status provided): %v", err)
	}
	if _, _, _, _, status, _, _, _ = readTrainedModelRow(t, pool, project, "curation-classifier-v2"); status != "ab_testing" {
		t.Errorf("provided status: want 'ab_testing', got %q", status)
	}

	// Duplicate (project_id, slug) → UNIQUE-constraint error (mirrors
	// GenericStrategy's bare INSERT; no once-only dup envelope for generic shapes).
	if err := work.HandleTrainedModelCreate(ctx, pool, project, "source-router-v1",
		"source-router", "v1", sig, metrics, "", "source-router/v1/model.onnx"); err == nil {
		t.Error("expected duplicate (project_id, slug) create to error, got nil")
	}
}
