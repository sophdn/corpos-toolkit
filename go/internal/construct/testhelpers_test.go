package construct_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"toolkit/internal/construct"
	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/forge/registry"
	"toolkit/internal/projections"
	"toolkit/internal/work"
)

// openTestPool returns a temp-path pool with the full toolkit schema applied
// via db.Open's embedded migration runner, seeded with the `mcp-servers`
// project row construct tests bind to, and the events→projections fold hook
// registered so emitted events update proj_* rows the parity tests read back.
// Mirrors work_test's helper (work tests use the same shape).
func openTestPool(t *testing.T) *db.Pool {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	pool, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if _, err := pool.DB().Exec(
		`INSERT INTO projects (id, name) VALUES ('mcp-servers', 'mcp-servers')`); err != nil {
		t.Fatalf("seed project: %v", err)
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
	return pool
}

// loadForgeRegistry locates blueprints/forge-schemas relative to the test CWD
// and loads it. Mirrors work_test's helper (the construct package can't import
// test helpers from work or forge).
func loadForgeRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	wd, _ := os.Getwd()
	var dir string
	for d := wd; d != "/" && d != ""; d = filepath.Dir(d) {
		candidate := filepath.Join(d, "blueprints", "forge-schemas")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			dir = candidate
			break
		}
	}
	if dir == "" {
		t.Skip("blueprints/forge-schemas not found relative to test CWD")
	}
	r, err := registry.Load(dir)
	if err != nil {
		t.Fatalf("Load registry: %v", err)
	}
	return r
}

// recordParams is the construct-side recordParams: marshals into the JSON the
// work.HandleRecord action expects.
func recordParams(t *testing.T, strict bool, evs ...work.RecordEvent) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(work.RecordParams{Events: evs, StrictAllOrNothing: strict})
	if err != nil {
		t.Fatalf("marshal record params: %v", err)
	}
	return b
}

// constructForgeDeps builds the (orchestration, sse-finalize, full-finalize) deps
// trio construct.HandleForgeCreate needs, mirroring main.go's wiring minus the
// SSE bus (tests assert DB rows + pointers, not bus publishes). The full variant
// wires construct.IndexUpsertNotifier so vault-note creates upsert their pointer
// (the per-schema split HandleForgeCreate applies); the covered creates pre-sync
// in construct.Create and use the sse (notifier-less) variant.
func constructForgeDeps(t *testing.T, pool *db.Pool) (deps, sse, full construct.Deps) {
	t.Helper()
	reg := loadForgeRegistry(t)
	deps = construct.Deps{Pool: pool, Schemas: reg}
	sse = construct.Deps{Pool: pool, Schemas: reg}
	full = construct.Deps{Pool: pool, Schemas: reg, OnCreate: construct.IndexUpsertNotifier(pool)}
	return deps, sse, full
}

// mustForgeMap marshals a forge-shaped param map and runs construct.HandleForgeCreate
// (the agent-facing create path that replaced forge.HandleForge — chain 311 T7
// Stage 6 P2-C.2). Used as a fixture builder + the chain+tasks fan-out runs
// natively in construct.Create. Fails on transport error OR rejection envelope.
func mustForgeMap(t *testing.T, pool *db.Pool, project string, params map[string]any) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal forge params: %v", err)
	}
	deps, sse, full := constructForgeDeps(t, pool)
	res, err := construct.HandleForgeCreate(context.Background(), deps, sse, full, project, raw)
	if err != nil {
		t.Fatalf("forge(%v): %v", params["schema_name"], err)
	}
	if res.Error != "" {
		t.Fatalf("forge(%v) rejected: %s", params["schema_name"], res.Error)
	}
}

// forgeCreateRaw runs construct.HandleForgeCreate on a raw forge-shaped envelope,
// returning the full result so callers can assert ok/error/action. Mirrors the
// production create path (replaces direct forge.HandleForge calls in the tests).
func forgeCreateRaw(t *testing.T, pool *db.Pool, project string, raw json.RawMessage) (construct.ForgeCreateResult, error) {
	t.Helper()
	deps, sse, full := constructForgeDeps(t, pool)
	return construct.HandleForgeCreate(context.Background(), deps, sse, full, project, raw)
}

// forgeEditRaw runs construct.HandleForgeEdit on a raw forge-edit envelope with
// the OnEdit knowledge-index notifier wired (mirrors main.go's editFinalizeDeps).
func forgeEditRaw(t *testing.T, pool *db.Pool, project string, raw json.RawMessage) (construct.ForgeEditResult, error) {
	t.Helper()
	reg := loadForgeRegistry(t)
	deps := construct.Deps{Pool: pool, Schemas: reg}
	editFinalize := construct.Deps{Pool: pool, Schemas: reg, OnEdit: construct.IndexUpsertOnEditNotifier(pool, reg)}
	return construct.HandleForgeEdit(context.Background(), deps, editFinalize, project, raw)
}
