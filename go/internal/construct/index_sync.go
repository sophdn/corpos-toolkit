package construct

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"toolkit/internal/db"
	"toolkit/internal/forge/registry"
	"toolkit/internal/knowledge/pointers"
	"toolkit/internal/obs"
)

// IndexUpsertNotifier returns an AfterCreateNotifier that mirrors an
// artifact's identity / content into the knowledge_pointers + FTS5 index
// so subsequent knowledge_search queries find the artifact. The
// pointers.Upsert path is DELETE-then-INSERT against the FTS5 virtual
// table, so re-running on edit refreshes the inverted-index tokens
// without leaving stale matches for the old wording.
//
// Which schemas land in the index is the per-shape Strategy.Indexed()
// capability (chain refactor-forge-shape-dispatch): a non-Indexed() shape
// is a silent no-op — the call returns without an error, so a shape opts
// into index sync by declaring Indexed()=true + a BuildPointer, not by
// being added to a switch here. Library entries have their own dedicated
// CRUD that owns its pointer rows.
//
// Vault-note re-forges follow chain `forge-vault-note-schema-rework`
// T4's policy A (auto-update on same-slug re-forge): the upsert keys
// off slug, the action verb returned to the caller becomes "updated",
// and if the scope (and thus the on-disk path) changed between forges,
// the previous file is removed. The vault root is resolved via
// FORGE_MARKDOWN_ROOT env (test override) or ~/.claude/vault default —
// matching createMarkdown's resolveMarkdownRoot logic so cleanup and
// write paths agree.
//
// The closure captures pool but not the artifact-specific data, so it's
// safe to share across all forge.Deps instances in the process.

// The per-shape KnowledgePointer builders (buildChainPointer /
// buildTaskPointer / buildBugPointer / buildVaultNotePointer /
// buildChainAnchoredDocPointer) are dispatched by each shape's
// Strategy.BuildPointer (strategy.go) — the registry replaces the prior
// buildPointer(schemaName) switch (chain refactor-forge-shape-dispatch
// T6b). Question / invoke_when fields derive from the artifact's
// most-search-relevant content; the curate-discover / curate-seed
// binaries (go/cmd/) use Qwen-enriched text when reachable, but the
// live-sync hook always uses the deterministic fallback shape (Qwen
// enrichment is a follow-up enhancement, not a parity gate).

// buildChainAnchoredDocPointer derives the knowledge_pointer for a
// retrospective or report-card doc. source_type carries the schema
// name verbatim ("retrospective" / "report-card") so vault_search /
// curate-discover filters can distinguish chain-author self-retro
// (retrospective) from fresh-sub-agent grade (report-card) from
// generic vault notes (vault).
//
// source_ref is the repo-relative doc path (the file_path the
// RetrospectiveForged / ReportCardForged event also records), so a
// reader joining the event and the pointer sees the same path on
// both surfaces.
func buildChainAnchoredDocPointer(project, slug, schemaName string, fields map[string]FieldValue) pointers.KnowledgePointer {
	chainSlug := stringField(fields, "chain_slug")
	chainSlugUpper := stringField(fields, "chain_slug_upper")
	date := stringField(fields, "date")
	if date == "" {
		date = currentDate()
	}
	suffix := "RETROSPECTIVE"
	verbForInvoke := "retrospective"
	if schemaName == "report-card" {
		suffix = "REPORT_CARD"
		verbForInvoke = "report card"
	}
	// Reconstruct the doc path the same way buildOutputPath does, so
	// the recorded pointer matches what landed on disk.
	if chainSlugUpper == "" {
		chainSlugUpper = kebabToScreamingSnake(chainSlug)
	}
	sourceRef := fmt.Sprintf("docs/%s_%s_%s.md", chainSlugUpper, suffix, date)
	title := stringField(fields, "title")
	question := title
	if question == "" {
		question = fmt.Sprintf("What did the %s for chain %q record?", verbForInvoke, chainSlug)
	}
	invokeWhen := fmt.Sprintf(
		"When looking up the %s for chain %q to load its closing context (what landed, what didn't, decisions revisited, next-chain candidates).",
		verbForInvoke, chainSlug,
	)
	// description: first 160 chars of the longest skeleton-section
	// body the caller supplied. Skips empty / missing fields so a
	// skeleton-only forge surfaces a useful description rather than
	// the empty string.
	skeleton := retrospectiveSkeletonFields
	if schemaName == "report-card" {
		skeleton = reportCardSkeletonFields
	}
	desc := ""
	for _, name := range skeleton {
		if v, ok := fields[name]; ok && !v.IsEmpty() {
			s := v.Single
			if len(s) > len(desc) {
				desc = s
			}
		}
	}
	desc = truncate(desc, 160)
	quality := fallbackQualityScore
	return pointers.KnowledgePointer{
		ProjectID:    project,
		SourceType:   schemaName,
		SourceRef:    sourceRef,
		Slug:         slug,
		Question:     truncate(question, 200),
		InvokeWhen:   invokeWhen,
		Description:  optStringPtr(desc),
		Tags:         []string{chainSlug, schemaName},
		QualityScore: &quality,
	}
}

// quality is the deterministic-builder default. Matches the Rust seeder's
// `if !qwen_ok` branch (0.65). When Qwen enrichment is wired into the
// live-sync path, callers can lift this to 0.8.
const fallbackQualityScore = 0.65

func buildChainPointer(project, slug string, fields map[string]FieldValue) pointers.KnowledgePointer {
	output := stringField(fields, "output")
	cc := stringField(fields, "completion_condition")

	question := truncate(output, 200)
	if question == "" {
		question = fmt.Sprintf("What did chain %q accomplish?", slug)
	}
	invoke_when := fmt.Sprintf(
		"When starting work in %s, consult chain %q for prior context.",
		project, slug,
	)
	// design_decisions retired in migration 065 (Phase 4 F2); the
	// pointer description falls back to completion_condition only.
	desc := cc
	quality := fallbackQualityScore
	// source_ref ENCODING (JOIN TRAP — see suggestion #18): `<project>::<slug>`
	// is the shared DB-entity contract here (buildTaskPointer adds a `::<chain>`
	// segment; vault-note builders use a doc path instead). This is NOT the
	// encoding grounding_events.source_refs uses (per-resolver `<type>:<rest>`,
	// built in refresolve/handler.go) — a direct JOIN is a ~100% miss. See
	// vault/reference/2026-05-23_source-ref-encoding-divergence-grounding-events-vs-knowledge-pointers.md.
	return pointers.KnowledgePointer{
		ProjectID:    project,
		SourceType:   "chain",
		SourceRef:    fmt.Sprintf("%s::%s", project, slug),
		Question:     question,
		InvokeWhen:   invoke_when,
		Description:  optStringPtr(truncate(desc, 160)),
		Tags:         []string{},
		QualityScore: &quality,
	}
}

func buildTaskPointer(project, slug string, fields map[string]FieldValue) pointers.KnowledgePointer {
	problem := stringField(fields, "problem_statement")
	handoff := stringField(fields, "handoff_output")
	chainSlug := stringField(fields, "chain_slug")

	question := truncate(problem, 200)
	if question == "" {
		question = fmt.Sprintf("What was task %q about?", slug)
	}
	invoke_when := fmt.Sprintf(
		"When doing work in %s related to the domain of task %q. "+
			"When the outcome of this task or its handoff is needed for context.",
		project, slug,
	)
	desc := truncate(handoff, 160)
	// source_ref includes chain context when available so re-creating the
	// same task slug under a different chain doesn't collide on the
	// unique (project_id, source_type, source_ref) index.
	ref := fmt.Sprintf("%s::%s", project, slug)
	if chainSlug != "" {
		ref = fmt.Sprintf("%s::%s::%s", project, chainSlug, slug)
	}
	quality := fallbackQualityScore
	return pointers.KnowledgePointer{
		ProjectID:    project,
		SourceType:   "task",
		SourceRef:    ref,
		Question:     question,
		InvokeWhen:   invoke_when,
		Description:  optStringPtr(desc),
		Tags:         []string{},
		QualityScore: &quality,
	}
}

// buildVaultNotePointer derives a knowledge_pointer for a vault-note
// artifact. vault-note is cross-project by design (chain
// `forge-vault-note-schema-rework`): top-level `project` carries DB
// attribution; `scope` is routing-only. The pointer's project_id
// resolves as:
//
//  1. top-level `project` (work-surface override) when non-empty
//  2. derived from kind + scope otherwise:
//     - decision / reference / learning-with-empty-scope → "vault" sentinel
//     - learning with non-empty scope → scope value
//  3. "vault" sentinel as final fallback
//
// source_ref uses the {subdir}/{date}_{slug}.md shape so the path is
// reachable when forge.Edit ports later; pointers.NormalizeVaultSourceRef
// guards against the legacy ".claude/vault/" prefix (bug 1469).
//
// invoke_when phrasing is kind-dispatched to mirror the seeder's
// retrieval-tuning shape: decisions cue on design tasks, learnings cue
// on similar-pattern encounters, reference notes cue on lookup tasks.
func buildVaultNotePointer(project, slug string, fields map[string]FieldValue) pointers.KnowledgePointer {
	kind := stringField(fields, "note_kind")
	title := stringField(fields, "title")
	body := stringField(fields, "body")
	noteScope := stringField(fields, "scope")
	tags := stringField(fields, "tags")
	subdir := stringField(fields, "subdir")
	date := stringField(fields, "date")
	if date == "" {
		// matches createMarkdown's auto-stamp
		date = currentDate()
	}

	question := truncate(title, 200)
	if question == "" {
		question = truncate(body, 200)
	}
	if question == "" {
		question = fmt.Sprintf("What did vault note %q establish?", slug)
	}

	var invokeWhen string
	switch kind {
	case "decision":
		invokeWhen = fmt.Sprintf(
			"When designing similar architecture; consult vault decision %q for prior rationale.",
			slug,
		)
	case "learning":
		invokeWhen = fmt.Sprintf(
			"When working on patterns related to %q; vault learning may apply or have caveats.",
			slug,
		)
	case "reference":
		invokeWhen = fmt.Sprintf("When looking up reference material on %q.", slug)
	default:
		invokeWhen = fmt.Sprintf("When the vault contains notes on %q.", slug)
	}

	pointerProject := resolveVaultNoteProjectID(project, kind, noteScope)

	// source_ref: relative file path under vault root (canonical bare
	// form per bug 1469; legacy ".claude/vault/" prefix gets stripped by
	// pointers.NormalizeVaultSourceRef as a belt-and-suspenders guard
	// against any caller that still emits the prefixed form).
	sourceRef := slug
	if subdir != "" {
		sourceRef = fmt.Sprintf("%s/%s_%s.md", subdir, date, slug)
	}
	sourceRef = pointers.NormalizeVaultSourceRef(sourceRef)

	desc := truncate(body, 160)
	quality := fallbackQualityScore
	tagList := []string{}
	if tags != "" {
		for _, t := range strings.Split(tags, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				tagList = append(tagList, t)
			}
		}
	}
	return pointers.KnowledgePointer{
		ProjectID:    pointerProject,
		SourceType:   "vault",
		SourceRef:    sourceRef,
		Slug:         slug,
		Question:     question,
		InvokeWhen:   invokeWhen,
		Description:  optStringPtr(desc),
		Tags:         tagList,
		QualityScore: &quality,
	}
}

// resolveVaultNoteProjectID computes the knowledge_pointers.project_id
// stamp per the chain-601 design (T2 decision) + chain-617 T1 substrate
// alignment:
//
//   - Top-level `project` (work-surface attribution) wins when set,
//     matching the convention chain/task/bug already follow.
//   - Otherwise the cross-project kinds (decision, reference) route to
//     the "vault" sentinel.
//   - Empty scope OR scope="general" for learnings route to "vault" too:
//     `learnings/general/` is the explicit cross-project bucket per the
//     SKILL `vault-filing-discipline` (chain 617 T1 alignment — the
//     prior implementation stamped project_id="general" for
//     learnings/general/* files, creating parallel pointer rows next
//     to the legacy "vault"-stamped rows and tripping projection JOINs).
//   - Otherwise (learning with a non-empty non-"general" scope) the
//     scope value becomes the project_id, matching the file's subdir.
//
// The "vault" sentinel is preserved (option 1 of the T2 decision) over
// a NULL column or borrowing the scope value.
func resolveVaultNoteProjectID(project, kind, scope string) string {
	if project != "" {
		return project
	}
	if kind == "learning" && scope != "" && scope != "general" {
		return scope
	}
	return "vault"
}

func buildBugPointer(project, slug string, fields map[string]FieldValue) pointers.KnowledgePointer {
	title := stringField(fields, "title")
	problem := stringField(fields, "problem_statement")
	surface := stringField(fields, "surface")

	question := firstNonEmpty(truncate(title, 200), truncate(problem, 200))
	if question == "" {
		question = fmt.Sprintf("What did bug %q report?", slug)
	}
	invoke_when := fmt.Sprintf(
		"When investigating issues in %s related to %q or surface %q.",
		project, slug, surface,
	)
	desc := truncate(problem, 160)
	quality := fallbackQualityScore
	return pointers.KnowledgePointer{
		ProjectID:    project,
		SourceType:   "bug",
		SourceRef:    fmt.Sprintf("%s::%s", project, slug),
		Question:     question,
		InvokeWhen:   invoke_when,
		Description:  optStringPtr(desc),
		Tags:         []string{},
		QualityScore: &quality,
	}
}

// truncate cuts s to n characters, respecting UTF-8 rune boundaries, and
// trims trailing whitespace. Mirrors the Rust seeder's `.chars().take(n)`.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(string(r[:n]))
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func optStringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// indexDeleteForArtifact removes the knowledge_pointers + FTS5 entry for
// an artifact about to be deleted via forge_delete. The source_ref +
// source_type are derived from the shape's Strategy.BuildPointer (the same
// builder the create/edit index path uses), so the pointer is reachable by
// exactly the row that was written — collapsing the third index-sync
// enumeration into the registry (chain refactor-forge-shape-dispatch T6b).
// A non-Indexed() shape (and the generic fallback) is a no-op. Best-effort:
// callers ignore the error (the artifact row is already gone and the
// pointer cleanup is just hygiene; surfacing the error wouldn't change the
// already-committed delete).
//
// Effectively dead for the live shape set — no live schema declares the
// `delete` op, so HandleForgeDelete's supportsDelete gate never reaches
// here in production — but kept as extensibility infra (a future adopted
// deletable shape gets correct pointer cleanup for free) + characterized in
// dispatch_db_internal_test.
func indexDeleteForArtifact(ctx context.Context, pool *db.Pool, schemaName, project, slug, chainSlug string) error {
	if !indexedSchema(schemaName) {
		return nil
	}
	// chain_slug only affects the task ref (project::chain::slug); buildPointerFor
	// reads it from fields, so thread it through for the task arm.
	var fields map[string]FieldValue
	if chainSlug != "" {
		fields = map[string]FieldValue{"chain_slug": SingleValue(chainSlug)}
	}
	p := buildPointerFor(schemaName, project, slug, fields)
	return pointers.DeleteByRef(ctx, pool, project, p.SourceType, p.SourceRef)
}

// IndexDeleteForArtifact is the exported seam over indexDeleteForArtifact: a
// best-effort knowledge_pointers/FTS cleanup for a deleted artifact. construct.Delete
// calls it so a delete performs the same index cleanup forge_delete did (delete has
// no event, so there's no fold to drive the cleanup — it's an explicit call).
func IndexDeleteForArtifact(ctx context.Context, pool *db.Pool, schemaName, project, slug, chainSlug string) error {
	return indexDeleteForArtifact(ctx, pool, schemaName, project, slug, chainSlug)
}

// IndexUpsertOnEditNotifier returns an AfterEditNotifier that re-reads
// the post-update artifact state and refreshes the knowledge_pointers +
// FTS5 entry. Edit-side sync can't use only the partial fields the
// caller passed (omitted fields stayed at their previous values), so
// the notifier reads the canonical state back and rebuilds the pointer
// from there. DB-target schemas (chain, task, bug) read from the
// canonical SQL tables; markdown-target schemas (vault-note) read from
// the on-disk file and parse frontmatter + sections. Other schemas →
// silent no-op.
//
// The schemas registry is consumed only for markdown-target read-back:
// it lets the notifier look up the schema's filename_pattern + sections
// to locate and parse the artifact. Pass nil to skip the markdown
// read-back path (the DB-target schemas still resolve).

// IndexUpsertOnEditInTx is the tx-aware counterpart of the
// IndexUpsertOnEditNotifier closure: it refreshes the knowledge_pointers
// + FTS5 entry for a just-edited DB-target artifact (chain / task / bug)
// using the caller's OUTER write tx for BOTH the canonical read-back and
// the pointer upsert. work.HandleBatch's forge_edit path calls this
// instead of firing the pool-based AfterEditNotifier — the pool-based one
// re-enters db.Pool's non-reentrant write mutex (pointers.Upsert →
// pool.WithWrite) inside the batch's already-held write tx and deadlocks
// (bug forge-edit-in-batch-deadlocks-via-nested-pool-withwrite-in-onedit-notifier).
//
// Reading through tx also closes a latent correctness gap: the pool-based
// notifier read the post-edit row via pool.DB() (a separate connection),
// so under a batch's uncommitted outer tx it saw PRE-edit state and could
// rebuild the index from stale content. The tx read-back sees the pending
// edit. Markdown-target schemas (vault-note) are rejected from batch
// upstream (HandleForgeEditInTx), so only the DB-target read-back arm runs.
func IndexUpsertOnEditInTx(ctx context.Context, tx *sql.Tx, schemas *registry.Registry, schemaName, project, slug string) error {
	spanCtx, end := obs.SpanStart(ctx, "forge.index_upsert_on_edit_in_tx")
	var hookErr error
	defer func() { end(hookErr) }()
	// editedPath="" — batch forge_edit rejects markdown-target schemas
	// upstream (HandleForgeEditInTx), so the chain-anchored read-back arm is
	// never reached here; only the DB-target arms (chain/task/bug) run. A
	// non-indexed shape returns ok=false, subsuming the old buildPointer skip.
	fields, ok, err := readCanonicalFieldsFor(schemaName, spanCtx, tx, schemas, project, slug, "")
	if err != nil {
		hookErr = fmt.Errorf("index_upsert_on_edit_in_tx (%s/%s) read-back: %w", schemaName, slug, err)
		return hookErr
	}
	if !ok {
		return nil
	}
	p := buildPointerFor(schemaName, project, slug, fields)
	if _, err := pointers.UpsertInTx(spanCtx, tx, p); err != nil {
		hookErr = fmt.Errorf("index_upsert_on_edit_in_tx (%s/%s): %w", schemaName, slug, err)
		return hookErr
	}
	return nil
}

// IndexUpsertOnCreateInTx is the tx-aware counterpart of the
// IndexUpsertNotifier closure for the batch CREATE path. It mirrors a
// just-created DB-target artifact into knowledge_pointers + FTS5 using the
// caller's OUTER write tx, instead of firing the pool-based
// AfterCreateNotifier — the pool-based one calls pointers.Upsert →
// pool.WithWrite, which would re-enter db.Pool's non-reentrant write mutex
// inside the batch's already-held tx and deadlock (same shape the edit
// path's IndexUpsertOnEditInTx avoids; bug
// `forge-create-in-batch-skips-knowledge-pointer-onecreate-not-wired`,
// sibling of `forge-edit-in-batch-deadlocks-via-nested-pool-withwrite-in-onedit-notifier`).
//
// Unlike the edit variant, no read-back is needed: the just-validated
// create fields ARE the canonical post-create content, so the pointer is
// built directly from them (matching how the non-batch IndexUpsertNotifier
// builds from the validated fields). A non-Indexed() shape (e.g. suggestion,
// which has no pointer builder on any path) is a silent no-op, identical to
// the non-batch notifier.
func IndexUpsertOnCreateInTx(ctx context.Context, tx *sql.Tx, schemaName, project, slug string, fields map[string]FieldValue) error {
	spanCtx, end := obs.SpanStart(ctx, "forge.index_upsert_on_create_in_tx")
	var hookErr error
	defer func() { end(hookErr) }()
	if !indexedSchema(schemaName) {
		return nil
	}
	p := buildPointerFor(schemaName, project, slug, fields)
	if _, err := pointers.UpsertInTx(spanCtx, tx, p); err != nil {
		hookErr = fmt.Errorf("index_upsert_on_create_in_tx (%s/%s): %w", schemaName, slug, err)
		return hookErr
	}
	return nil
}

// IndexSyncFromProjection mirrors a just-created DB-target artifact (chain /
// task / bug) into knowledge_pointers + FTS5 by reading its canonical fields
// back from the projection the create event just folded, building the pointer
// via the shape's Strategy.BuildPointer, and upserting it (pool-based). It is
// the re-home seam (T7 §15 / chain record-layer-stage2-additive-remainder) the
// record construction layer calls AFTER a record(create) emit commits — the
// fold wrote the projection row this reads — so an entity created through
// record lands the SAME knowledge_pointer forge's AfterCreate notifier writes
// for an equivalent create (same BuildPointer over the same content). This
// closes the B-F3 index gap: the record substrate folds projections but does
// NOT write the knowledge index (forge does that via the notifier, not a fold).
//
// Reuses the read-back machinery the edit notifier already uses
// (Strategy.ReadCanonicalFields → readChainFieldsForIndex / readTaskFieldsForIndex
// / readBugFieldsForIndex), so it can't drift from forge's pointer content. A
// non-Indexed() shape (suggestion, memory) returns ok=false from the read-back
// → no-op (action "", nil), exactly like the notifier. DB-target only: the
// markdown shapes (retrospective / report-card / vault-note) build their pointer
// at file-write time and are served by the file-schema builders, not here.
// IndexUpsertNotifier returns an AfterCreateNotifier that mirrors a just-created
// artifact's identity/content into knowledge_pointers + FTS5 (pool-based). Port
// of forge's IndexUpsertNotifier (chain 311 T7 Stage 6 P2-C.2): the retired
// Strategy dispatch (strategyFor(name).Indexed()/.BuildPointer()) is replaced by
// the name-keyed indexedSchema/buildPointerFor helpers. The dispatch host wires
// this as the FULL AfterCreate notifier for the vault-note create path (the lone
// covered create whose construct.Create arm writes the file but does NOT pre-sync
// the pointer — every other covered create pre-syncs in construct.Create, so they
// use an SSE-only notifier to avoid a double-write). The pointer is built from the
// validated create `fields` (the same map createVaultNote mutated with {subdir}/
// {date}), matching forge's build-from-fields semantics — and the returned action
// verb is "created"/"updated" so a vault-note same-slug re-forge reports "updated".
func IndexUpsertNotifier(pool *db.Pool) AfterCreateNotifier {
	return func(ctx context.Context, schemaName, project, slug string, _ CreatePersistResult, fields map[string]FieldValue) (string, error) {
		if !indexedSchema(schemaName) {
			return "", nil
		}
		p := buildPointerFor(schemaName, project, slug, fields)
		spanCtx, end := obs.SpanStart(ctx, "forge.index_upsert")
		var hookErr error
		defer func() { end(hookErr) }()
		res, err := pointers.Upsert(spanCtx, pool, p)
		if err != nil {
			hookErr = fmt.Errorf("index_upsert (%s/%s): %w", schemaName, slug, err)
			return "", hookErr
		}
		// Scope-change re-forge cleanup: when a vault-note's slug-keyed upsert
		// detects a source_ref change, the previous file on disk is orphaned.
		// Remove it (missing-file is not an error). DELIBERATE vault-note-specific
		// side effect — it's the only shape that auto-relocates its file on
		// same-slug re-forge (chain forge-vault-note-schema-rework policy A).
		if schemaName == "vault-note" && res.Action == "updated" && res.PreviousSourceRef != "" {
			root := vaultRootForCleanup()
			if root != "" {
				oldPath := root + string(os.PathSeparator) + res.PreviousSourceRef
				if err := os.Remove(oldPath); err != nil && !os.IsNotExist(err) {
					hookErr = fmt.Errorf("index_upsert (%s/%s): remove orphan %s: %w", schemaName, slug, oldPath, err)
					return res.Action, hookErr
				}
			}
		}
		return res.Action, nil
	}
}

// vaultRootForCleanup resolves the on-disk vault root the IndexUpsertNotifier
// uses to remove an orphaned file after a scope-change re-forge. Matches
// resolveMarkdownRoot's vault branch: FORGE_MARKDOWN_ROOT overrides (tests),
// else ~/.claude/vault. Empty string ⇒ skip cleanup. (Port of forge's.)
func vaultRootForCleanup() string {
	if root := os.Getenv("FORGE_MARKDOWN_ROOT"); root != "" {
		return root + string(os.PathSeparator) + "vault"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home + string(os.PathSeparator) + ".claude" + string(os.PathSeparator) + "vault"
}

// IndexUpsertOnEditNotifier returns an AfterEditNotifier that refreshes a
// just-edited artifact's knowledge_pointer + FTS5 entry by reading the canonical
// post-edit state back (DB-target from SQL; markdown-target from disk) and
// rebuilding the pointer. Port of forge's IndexUpsertOnEditNotifier (P2-C.2):
// strategyFor(name).ReadCanonicalFields/.BuildPointer → readCanonicalFieldsFor/
// buildPointerFor. The dispatch host wires this as the OnEdit notifier for the
// (single) construct.Update edit path; it runs idempotently after the covered
// edits' own IndexSyncFromProjection AND serves the vault-note delta survivor's
// file read-back. A non-indexed shape (bench/trained_model/suggestion/memory)
// returns ok=false → no-op.
func IndexUpsertOnEditNotifier(pool *db.Pool, schemas *registry.Registry) AfterEditNotifier {
	return func(ctx context.Context, schemaName, project, slug string, result EditResult, _ map[string]FieldValue) error {
		spanCtx, end := obs.SpanStart(ctx, "forge.index_upsert_on_edit")
		var hookErr error
		defer func() { end(hookErr) }()
		// result.ArtifactPath is the post-edit on-disk path (chain-anchored docs
		// read it back from there; vault-note locates by slug). A non-indexed
		// shape returns ok=false from the read-back → skip.
		fields, ok, err := readCanonicalFieldsFor(schemaName, spanCtx, pool.DB(), schemas, project, slug, result.ArtifactPath)
		if err != nil {
			hookErr = fmt.Errorf("index_upsert_on_edit (%s/%s) read-back: %w", schemaName, slug, err)
			return hookErr
		}
		if !ok {
			return nil
		}
		p := buildPointerFor(schemaName, project, slug, fields)
		if _, err := pointers.Upsert(spanCtx, pool, p); err != nil {
			hookErr = fmt.Errorf("index_upsert_on_edit (%s/%s): %w", schemaName, slug, err)
			return hookErr
		}
		return nil
	}
}

func IndexSyncFromProjection(ctx context.Context, pool *db.Pool, schemas *registry.Registry, schemaName, project, slug string) (string, error) {
	spanCtx, end := obs.SpanStart(ctx, "forge.index_sync_from_projection")
	var hookErr error
	defer func() { end(hookErr) }()
	fields, ok, err := readCanonicalFieldsFor(schemaName, spanCtx, pool.DB(), schemas, project, slug, "")
	if err != nil {
		hookErr = fmt.Errorf("index_sync_from_projection (%s/%s) read-back: %w", schemaName, slug, err)
		return "", hookErr
	}
	if !ok {
		return "", nil
	}
	p := buildPointerFor(schemaName, project, slug, fields)
	res, err := pointers.Upsert(spanCtx, pool, p)
	if err != nil {
		hookErr = fmt.Errorf("index_sync_from_projection (%s/%s): %w", schemaName, slug, err)
		return "", hookErr
	}
	return res.Action, nil
}

// The per-shape canonical read-back functions below (readChainFieldsForIndex /
// readTaskFieldsForIndex / readBugFieldsForIndex for db shapes;
// readVaultNoteForIndex / readChainAnchoredDocForIndex for markdown shapes)
// are dispatched by each shape's Strategy.ReadCanonicalFields (strategy.go) —
// the registry replaces the prior readArtifactFieldsForIndex(schemaName)
// switch (chain refactor-forge-shape-dispatch T6b). Each returns
// (fields, true, nil) when the source exists; (nil, false, nil) when not
// found (e.g. the artifact was deleted between Edit and notifier fire —
// harmless to skip); (nil, false, err) on other errors.
//
// editedPath (chain-anchored docs only) is the post-edit on-disk path threaded
// from EditResult.ArtifactPath by the non-batch edit notifier. Retrospective /
// report-card have no {slug} in their filename_pattern (identity is
// chain_slug), so they can't be located by slug the way vault-note is — the
// just-written path is the authoritative read-back source.

// readChainFieldsForIndex reads a chain's index-relevant content from
// proj_chain_status. design_decisions retired in migration 065 (Phase 4 F2);
// pointer rebuilds for chains no longer source it from the projection cache —
// buildChainPointer falls back to completion_condition for the description.
func readChainFieldsForIndex(ctx context.Context, q db.Queryer, project, slug string) (map[string]FieldValue, bool, error) {
	var output, cc string
	err := q.QueryRowContext(ctx,
		`SELECT output, completion_condition
		   FROM proj_chain_status WHERE project_id = ? AND slug = ?`,
		project, slug).Scan(&output, &cc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return map[string]FieldValue{
		"output":               SingleValue(output),
		"completion_condition": SingleValue(cc),
	}, true, nil
}

// readTaskFieldsForIndex reads a task's index-relevant content from
// proj_current_tasks (joined to proj_chain_status for the chain slug).
func readTaskFieldsForIndex(ctx context.Context, q db.Queryer, project, slug string) (map[string]FieldValue, bool, error) {
	var chainSlug, problem, handoff string
	err := q.QueryRowContext(ctx,
		`SELECT c.slug, t.problem_statement, t.handoff_output
		   FROM proj_current_tasks t JOIN proj_chain_status c ON t.chain_id = c.id
		  WHERE t.slug = ? AND c.project_id = ?`,
		slug, project).Scan(&chainSlug, &problem, &handoff)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return map[string]FieldValue{
		"chain_slug":        SingleValue(chainSlug),
		"problem_statement": SingleValue(problem),
		"handoff_output":    SingleValue(handoff),
	}, true, nil
}

// readBugFieldsForIndex reads a bug's index-relevant content from
// proj_current_bugs.
func readBugFieldsForIndex(ctx context.Context, q db.Queryer, project, slug string) (map[string]FieldValue, bool, error) {
	var title, problem, surface string
	err := q.QueryRowContext(ctx,
		`SELECT title, problem_statement, surface
		   FROM proj_current_bugs WHERE project_id = ? AND slug = ?`,
		project, slug).Scan(&title, &problem, &surface)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return map[string]FieldValue{
		"title":             SingleValue(title),
		"problem_statement": SingleValue(problem),
		"surface":           SingleValue(surface),
	}, true, nil
}

// readChainAnchoredDocForIndex reads a just-edited retrospective /
// report-card doc back from disk and parses its frontmatter + sections so
// the edit-path pointer refresh reflects the post-edit content (bug 926).
//
// Unlike vault-note (located by slug via findMarkdownPath), chain-anchored
// docs have no {slug} in their filename_pattern — identity is chain_slug,
// the filename is {chain_slug_upper}_{KIND}_{date}.md — so they can't be
// located by slug. The edit just wrote the file, so the caller threads in
// EditResult.ArtifactPath rather than re-deriving the path.
// buildChainAnchoredDocPointer reconstructs source_ref from the parsed
// chain_slug + date (frontmatter), deriving chain_slug_upper from
// chain_slug when absent, so the refreshed pointer keeps the SAME
// source_ref and updates the existing row in place (no orphan). Returns
// (nil, false, nil) when editedPath is empty or the file is gone — the
// notifier degrades to a no-op rather than blocking the edit envelope.
func readChainAnchoredDocForIndex(schemas *registry.Registry, schemaName, editedPath string) (map[string]FieldValue, bool, error) {
	if schemas == nil || editedPath == "" {
		return nil, false, nil
	}
	schema, ok := schemas.Get(schemaName)
	if !ok {
		return nil, false, nil
	}
	raw, err := os.ReadFile(editedPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	fields, _, _, err := parseMarkdownDoc(string(raw), schema)
	if err != nil {
		return nil, false, err
	}
	return fields, true, nil
}

// readVaultNoteForIndex locates a vault-note .md file by slug, parses
// frontmatter + sections back into a FieldValue map, and re-derives the
// {subdir} routing field so buildVaultNotePointer's source_ref matches
// the post-edit on-disk path. Returns (nil, false, nil) when the
// schema registry isn't available or the file isn't found — the
// notifier degrades silently rather than blocking the edit envelope.
func readVaultNoteForIndex(schemas *registry.Registry, project, slug string) (map[string]FieldValue, bool, error) {
	if schemas == nil {
		return nil, false, nil
	}
	schema, ok := schemas.Get("vault-note")
	if !ok {
		return nil, false, nil
	}
	// vault-note schemas hit the vault-branch of resolveMarkdownRoot
	// (output_dir contains "vault"), so pool+project aren't consulted;
	// passing nil/"" is correct here.
	root := resolveMarkdownRoot(context.Background(), nil, "", schema)
	storage := schema.ResolvedStorage()
	outputDir := storage.OutputDir
	pattern := storage.FilenamePattern
	if storage.Markdown != nil {
		if storage.Markdown.OutputDir != "" {
			outputDir = storage.Markdown.OutputDir
		}
		if storage.Markdown.FilenamePattern != "" {
			pattern = storage.Markdown.FilenamePattern
		}
	}
	guard := joinForRoot(root, outputDir)
	path, err := findMarkdownPath(guard, pattern, slug)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	fields, _, _, err := parseMarkdownDoc(string(raw), schema)
	if err != nil {
		return nil, false, err
	}
	// Re-derive subdir + note_kind from the file's path: the on-disk
	// location is the source of truth for routing, regardless of what
	// the frontmatter says. (The schema declares render_as="frontmatter"
	// for note_kind so new notes do carry it in frontmatter, but a
	// kind change without a corresponding file move would leave the
	// frontmatter stale — see edit_markdown.go for the relocate path.)
	subdir, kind := subdirAndKindFromPath(path, guard)
	fields["subdir"] = SingleValue(subdir)
	if _, has := fields["note_kind"]; !has && kind != "" {
		fields["note_kind"] = SingleValue(kind)
	}
	// Re-derive `scope` from the path's learnings/<X>/ component (chain
	// `forge-vault-note-schema-rework`). Decisions and reference notes
	// don't carry a scope. The "general" bucket is the explicit cross-
	// project sentinel; treat it as empty scope (== cross-project).
	if _, has := fields["scope"]; !has {
		if pathScope := scopeFromSubdir(subdir, kind); pathScope != "" {
			fields["scope"] = SingleValue(pathScope)
		}
	}
	_ = project // top-level project is DB-attribution-only for vault-note; ignored here
	return fields, true, nil
}

// scopeFromSubdir extracts the routing scope from a path-derived subdir.
// Only learnings carry a scope; "learnings/general" maps to empty (the
// explicit cross-project bucket).
func scopeFromSubdir(subdir, kind string) string {
	if kind != "learning" {
		return ""
	}
	const prefix = "learnings/"
	if !strings.HasPrefix(subdir, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(subdir, prefix)
	if rest == "" || rest == "general" {
		return ""
	}
	return rest
}

// joinForRoot mirrors filepath.Join without importing it twice; matches
// the join semantics editMarkdown uses to construct its rootGuard.
func joinForRoot(root, outputDir string) string {
	if outputDir == "" {
		return root
	}
	return root + string(os.PathSeparator) + outputDir
}

// subdirAndKindFromPath recovers the routing subdir + note_kind from
// the file's relative path under guard. vault-note layout is:
//
//	decisions/<date>_<slug>.md          → kind=decision
//	learnings/<project>/<date>_<slug>.md → kind=learning
//	reference/<date>_<slug>.md          → kind=reference
//
// Returns the directory component (e.g. "learnings/mcp-servers") and the
// inferred kind. Falls back to "" / "" on a path that doesn't match the
// expected layout.
func subdirAndKindFromPath(absPath, guard string) (string, string) {
	rel, err := relUnder(absPath, guard)
	if err != nil {
		return "", ""
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	if len(parts) < 2 {
		return "", ""
	}
	switch parts[0] {
	case "decisions":
		return "decisions", "decision"
	case "reference":
		return "reference", "reference"
	case "learnings":
		if len(parts) >= 3 {
			return "learnings/" + parts[1], "learning"
		}
		return "learnings", "learning"
	}
	return strings.Join(parts[:len(parts)-1], "/"), ""
}

// relUnder returns the path of absPath relative to guard, or an error
// when absPath isn't under guard.
func relUnder(absPath, guard string) (string, error) {
	abs := absPath
	g := guard
	if !strings.HasPrefix(abs, g) {
		return "", fmt.Errorf("path %q not under guard %q", absPath, guard)
	}
	rel := strings.TrimPrefix(abs, g)
	rel = strings.TrimPrefix(rel, string(os.PathSeparator))
	return rel, nil
}
