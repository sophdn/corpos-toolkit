package construct

import (
	"context"
	"database/sql"
	"fmt"

	"toolkit/internal/forge/registry"
)

// ── construct.Delete — the agent-facing delete umbrella ────────────────────
//
// Mirrors forge_delete: schema-name dispatched, B-DEL1 rejects bug / chain /
// task / suggestion by default (no hard delete — terminal state is owned by
// the lifecycle action: bug_resolve / task_cancel / chain_close), and a
// supported-ops opt-in schema routes through a generic (project_id, slug)
// DELETE.
//
// Today no live schema declares `delete` in its TOML — every project-scoped
// schema's lifecycle action covers terminal state — so in practice this
// umbrella's surface is the rejection-policy layer that names the correct
// action. The opt-in DELETE arm exists because forge_delete's tests + the
// extensibility infra exercise the synthetic-schema path; keeping the parity
// shape lets Stage 4 callers route forge_delete calls through here without
// special-casing.

// DeleteResult is the umbrella's return shape. Matches DeleteResult
// (NotFound when the schema supports delete but no row matched).
type DeleteResult struct {
	Schema     string
	EntitySlug string
	NotFound   bool
}

// Delete takes a schema name + project + slug and either rejects per B-DEL1
// (when the schema's terminal state belongs to a lifecycle action) or runs a
// generic (project_id, slug) DELETE for a schema that declares
// `supported_ops = [..., "delete"]`. chainSlug is accepted for parity with
// forge_delete but ignored — no live deletable schema is task-shaped, and
// the (chain_id, slug) keyed path would land at Stage 4 when an actual
// caller surfaces.
//
// Error vs. NotFound distinction (mirrors forge_delete):
//   - Schema declares no delete + terminal-state lifecycle action → return an
//     error pointing at the action (B-DEL1, hard reject).
//   - Schema supports delete + row exists → DELETE, return DeleteResult{}.
//   - Schema supports delete + row missing → DeleteResult{NotFound:true}, nil.
func Delete(ctx context.Context, deps Deps, schema, project, slug string) (DeleteResult, error) {
	if deps.Pool == nil {
		return DeleteResult{}, fmt.Errorf("construct.Delete: Deps.Pool is required")
	}
	if deps.Schemas == nil {
		return DeleteResult{}, fmt.Errorf("construct.Delete: Deps.Schemas is required")
	}
	if schema == "" {
		return DeleteResult{}, fmt.Errorf("construct.Delete: schema is required")
	}
	if slug == "" {
		return DeleteResult{}, fmt.Errorf("construct.Delete: slug is required")
	}

	s, ok := deps.Schemas.Get(schema)
	if !ok {
		return DeleteResult{}, fmt.Errorf("construct.Delete: unknown schema %q (no registry entry)", schema)
	}

	// B-DEL1: reject when the schema's supported_ops omits delete, naming
	// the lifecycle action (soft_delete_action) where one is declared.
	if !schemaSupportsDelete(s) {
		if alt := s.Lifecycle.SoftDeleteAction; alt != "" {
			return DeleteResult{}, fmt.Errorf(
				"schema %q does not support deletion; use action=%q for soft-cancellation. "+
					"call %s instead — this schema's state transitions are owned by lifecycle actions, not construct.Delete",
				schema, alt, alt)
		}
		return DeleteResult{}, fmt.Errorf("schema %q does not support deletion", schema)
	}

	// Opt-in delete: generic (project_id, slug) DELETE matching
	// GenericStrategy.Delete's body. No event fires — forge_delete
	// has no associated event type for the schemas that declare delete.
	table := deletableTable(s)
	if table == "" {
		return DeleteResult{}, fmt.Errorf("construct.Delete: schema %q declares delete but has no resolvable storage table", schema)
	}
	notFound := false
	err := deps.Pool.WithWrite(ctx, func(tx *sql.Tx) error {
		res, dErr := tx.ExecContext(ctx,
			fmt.Sprintf("DELETE FROM %s WHERE project_id = ? AND slug = ?", table),
			project, slug,
		)
		if dErr != nil {
			return fmt.Errorf("delete on %s failed: %w", table, dErr)
		}
		if affected, _ := res.RowsAffected(); affected == 0 {
			notFound = true
		}
		return nil
	})
	if err != nil {
		return DeleteResult{}, err
	}
	// Best-effort knowledge_pointers/FTS cleanup, matching forge_delete: delete
	// has no event (no fold to drive the cleanup), so it's an explicit call on
	// both paths. The DB row is already gone even if this fails. Skipped on
	// NotFound (nothing was deleted). chainSlug is "" — no deletable schema is
	// task-shaped (see Delete's doc).
	if !notFound {
		_ = IndexDeleteForArtifact(ctx, deps.Pool, schema, project, slug, "")
	}
	return DeleteResult{Schema: schema, EntitySlug: slug, NotFound: notFound}, nil
}

// schemaSupportsDelete reports whether the schema declares `delete` in its
// supported_ops. Mirrors supportsDelete's body — copied (not re-homed)
// because supportsDelete is unexported and the rule is one line; if a
// future schema toggle adds nuance we'll export it then.
func schemaSupportsDelete(s registry.Schema) bool {
	for _, op := range s.SupportedOps {
		if op == "delete" {
			return true
		}
	}
	return false
}

// deletableTable resolves the SQL table the generic delete targets, mirroring
// GenericStrategy.Delete: storage.Table on db shapes; storage.DB.Table
// on dual shapes (markdown-target schemas don't reach here because no
// markdown schema declares delete).
func deletableTable(s registry.Schema) string {
	storage := s.ResolvedStorage()
	if storage.DB != nil {
		return storage.DB.Table
	}
	return storage.Table
}
