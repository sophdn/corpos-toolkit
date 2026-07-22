package main

import (
	"context"
	"database/sql"
	"os"

	"toolkit/internal/events"
	"toolkit/internal/projections"
)

// defaultToolkitDBPath returns the canonical local toolkit.db path.
// The subcommand-fold-into-toolkit-server CLIs (smoke-classify-rubric
// et al.) used to each carry their own copy of this helper.
func defaultToolkitDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "data/toolkit.db"
	}
	return home + "/dev/mcp-servers/data/toolkit.db"
}

// defaultRubricsDir returns the canonical blueprints/rubrics path.
func defaultRubricsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "blueprints/rubrics"
	}
	return home + "/dev/mcp-servers/blueprints/rubrics"
}

// passFailString returns "PASS" / "FAIL" — used in the verdict
// blocks of the smoke / regression subcommands.
func passFailString(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

// installProjectionsFoldHook wires events.Emit → projections.FoldAll
// inside the emit tx. Without this, events emitted by subcommand paths
// land in the events table but the proj_* projections stay stale.
//
// The closure adapts events.RawEvent → projections.RawEvent
// (field-by-field identical; the seam is the dependency reversal, not
// a shape transformation). Hook fires inside every Emit's tx;
// projection refresh failure rolls the originating mutation back.
//
// Pre-harvest-the-consolidation T4 each standalone-CLI binary
// duplicated this scaffolding (smoke-classify-rubric, regression-runner,
// regression-knowledge-search, exercise-chain-assessment-dispatch,
// qwen-vault-smoke-backfill). T4 folded those CLIs into
// `toolkit-server <subcommand>`; the shared helper lives here.
//
// Server-mode (top-level main()) wires the same hook inline so the
// declaration order in main.go stays self-evident; subcommand code
// calls this helper.
func installProjectionsFoldHook() {
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
