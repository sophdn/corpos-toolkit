package construct

import (
	"context"

	"toolkit/internal/db"
	"toolkit/internal/forge/registry"
)

// ── B-F3: knowledge-index sync for the Indexed create schemas ───────────────
//
// The record substrate folds projections but does NOT write the knowledge
// index (knowledge_pointers + FTS5). In forge that's done by the AfterCreate
// notifier (deps.OnCreate), not a projection fold — so an entity created
// through the construction layer (BuildX → record → fold) lands its
// projection row but no knowledge_pointer until SyncCreateIndex runs.

// SyncCreateIndex upserts the knowledge_pointer (B-F3) for an entity created
// through the construct → record path, reusing forge's exact pointer builder +
// read-back (IndexSyncFromProjection). Call it AFTER the create event
// has been recorded — the fold wrote the projection row this reads back — so
// the pointer matches forge(bug)/forge(chain)/forge(task)'s for equivalent
// input. No-op for non-Indexed schemas (suggestion, memory). DB-target only;
// the file schemas build their pointer at file-write time (inside
// WriteChainAnchoredDoc).
func SyncCreateIndex(ctx context.Context, pool *db.Pool, schemas *registry.Registry, schemaName, project, slug string) error {
	_, err := IndexSyncFromProjection(ctx, pool, schemas, schemaName, project, slug)
	return err
}
