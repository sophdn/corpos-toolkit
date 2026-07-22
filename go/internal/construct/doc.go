// Package construct is the record-construction layer for forge's create
// surface: the agent-facing shim that takes forge-style inputs, runs forge's
// sugar (validation, slug-derive, defaults, file writes for the markdown/SQL
// schemas), and returns typed RecordEvent values for submission through
// internal/work's record action.
//
// ## Intended use
//
// **Workflow served:** new agents (or migrated Go callers — arcreview's
// ForgeFn, work.batch's forge ops, the work table forge action, the CLIs)
// create canonical entities (bug, suggestion, chain, task, chain+tasks
// fan-out, memory, retrospective, report-card, migration) without assembling
// forge's sugar by hand — slug-derive, severity/priority defaults, dup-reject,
// file artifact rendering, the knowledge-index sync. Construct sits between
// the caller and the record event substrate.
//
// **Invocation pattern:** Build* functions per schema (BuildBug, BuildChain,
// BuildMemory, BuildMigration, …) take a typed Input struct (BugInput,
// ChainInput, MemoryInput, …) and return a work.RecordEvent (or a slice for
// the chain+tasks fan-out). Cross-cutting guards live as siblings:
// RejectDuplicateCreate (B-D1), RejectDoubleDatedSlug (B-G2), SyncCreateIndex
// (B-F3). Chain 321 T2 lands a construct.Create umbrella that schema-name-
// dispatches and runs the full orchestration; Stage 3 of chain 311 T7 adds
// construct.Update / construct.Delete in the same shape.
//
// **Success shape:** per-Build* return is a typed work.RecordEvent (or
// []RecordEvent) the caller submits via work.HandleRecord, byte-identical in
// projection/file/knowledge_pointer to forge(schema, fields) for equivalent
// input — pinned by the per-schema parity tests in this package.
//
// **Non-goals:** does not own the record substrate itself (internal/work owns
// HandleRecord + the event append + the projections fold hook), does not
// duplicate forge's helpers (re-uses forge.SlugifyTitle / RejectDuplicateBySlug
// / IndexSyncFromProjection / WriteMemoryArtifact / WriteChainAnchoredDoc /
// WriteMigrationArtifact / CheckDoubleDatedSlug — those move into construct/
// at Stage 6 when forge archives), and does not run forge's optional
// post-create gates (e.g. retrospective's captureOrphanedFollowons is a
// documented §15 delta).
package construct
