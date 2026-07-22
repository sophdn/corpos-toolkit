package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"toolkit/internal/construct"
	"toolkit/internal/events"
	"toolkit/internal/forge/registry"
	"toolkit/internal/projections"
)

// loadSchemaRegistry locates blueprints/forge-schemas relative to the test
// CWD and loads it. Mirrors loadForgeRegistry from construct_test, but the
// main package can't import construct's test helpers.
func loadSchemaRegistry(t *testing.T) *registry.Registry {
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

// registerProjectionFoldHookOnce wires the events→projections fold hook so
// MemoryWritten events emitted by construct.Create update proj_memories.
// The hook is process-global; sync.Once prevents the test suite from
// trying to register it twice across multiple cases.
var registerProjectionFoldHookOnce sync.Once

func registerProjectionFoldHook(t *testing.T) {
	t.Helper()
	registerProjectionFoldHookOnce.Do(func() {
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
	})
}

// TestArcFallbackForge_MemoryRoutesThroughConstruct: a memory-shaped
// fallback forge call lands a proj_memories row + writes the vault file
// via construct.Create. Pins Stage 4 Slice 1's wiring: arcreview's
// unreviewed-fallback memory arm now goes through the record substrate.
func TestArcFallbackForge_MemoryRoutesThroughConstruct(t *testing.T) {
	pool, _ := openFreshPool(t)
	defer pool.Close()
	root := t.TempDir()
	t.Setenv("FORGE_MARKDOWN_ROOT", root)
	registerProjectionFoldHook(t)

	reg := loadSchemaRegistry(t)
	constructDeps := construct.Deps{Pool: pool, Schemas: reg}
	sseFinalize := construct.Deps{Pool: pool, Schemas: reg}
	fullFinalize := construct.Deps{Pool: pool, Schemas: reg, OnCreate: construct.IndexUpsertNotifier(pool)}

	params, err := json.Marshal(map[string]any{
		"schema_name": "memory",
		"slug":        "arc-fallback-memory-test",
		"fields": map[string]string{
			"memory_kind": "feedback",
			"name":        "arc-fallback-memory-test",
			"description": "smoke for Stage 4 Slice 1",
			"body":        "Some throwaway body content.",
			"source":      "arc-close-fallback-unreviewed",
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := arcFallbackForge(context.Background(), constructDeps, sseFinalize, fullFinalize, "mcp-servers", params); err != nil {
		t.Fatalf("arcFallbackForge(memory): %v", err)
	}

	// proj_memories row landed → confirms the MemoryWritten event was
	// folded (construct.Create's record submit).
	var kind, descr string
	var bodyLen int
	if err := pool.DB().QueryRow(
		`SELECT kind, description, body_length_bytes FROM proj_memories WHERE name = ?`,
		"arc-fallback-memory-test").Scan(&kind, &descr, &bodyLen); err != nil {
		t.Fatalf("read proj_memories: %v", err)
	}
	if kind != "feedback" || descr != "smoke for Stage 4 Slice 1" {
		t.Fatalf("proj_memories wrong: kind=%q desc=%q", kind, descr)
	}

	// File landed at the routed path.
	wantPath := filepath.Join(root, "vault", "memory", "feedback", "arc-fallback-memory-test.md")
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("memory file missing at %q: %v", wantPath, err)
	}
}

// TestArcFallbackForge_VaultNoteRoutesThroughConstruct: a vault-note fallback
// forge call lands its knowledge_pointer via construct.HandleForgeCreate's
// full-notifier path (chain 311 T7 Stage 6 P2-C.2 re-homed vault-note's
// no-event file+pointer write into construct). Pins that the fallback's
// vault-note arm writes the pointer just like an agent-issued create.
func TestArcFallbackForge_VaultNoteRoutesThroughConstruct(t *testing.T) {
	pool, _ := openFreshPool(t)
	defer pool.Close()
	root := t.TempDir()
	t.Setenv("FORGE_MARKDOWN_ROOT", root)
	registerProjectionFoldHook(t)

	reg := loadSchemaRegistry(t)
	constructDeps := construct.Deps{Pool: pool, Schemas: reg}
	sseFinalize := construct.Deps{Pool: pool, Schemas: reg}
	fullFinalize := construct.Deps{Pool: pool, Schemas: reg, OnCreate: construct.IndexUpsertNotifier(pool)}

	params, err := json.Marshal(map[string]any{
		"schema_name": "vault-note",
		"slug":        "arc-fallback-vault-note-test",
		"fields": map[string]string{
			"note_kind": "reference",
			"title":     "Stage 4 Slice 1 smoke",
			"body":      "Just a vault-note round-trip via the fallback dispatcher.",
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := arcFallbackForge(context.Background(), constructDeps, sseFinalize, fullFinalize, "mcp-servers", params); err != nil {
		t.Fatalf("arcFallbackForge(vault-note): %v", err)
	}

	// vault-note file landed via forge (no event, no projection — its
	// only artifact is the file on disk + the knowledge_pointer index).
	// vault-note's pointer uses source_type='vault' (not 'vault-note' —
	// historical naming, see migration 020 + buildVaultNotePointer's
	// source_ref encoding `{subdir}/{date}_{slug}.md`).
	var count int
	var sourceRef string
	if err := pool.DB().QueryRow(
		`SELECT COUNT(*), COALESCE(MAX(source_ref), '') FROM knowledge_pointers WHERE source_type = 'vault'`,
	).Scan(&count, &sourceRef); err != nil {
		t.Fatalf("count vault-note pointers: %v", err)
	}
	if count == 0 {
		t.Fatalf("vault-note knowledge_pointer not written — forge fall-through regressed")
	}
	if !strings.Contains(sourceRef, "arc-fallback-vault-note-test") {
		t.Fatalf("vault-note pointer source_ref doesn't reference our slug: %q", sourceRef)
	}
}
