package construct

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/forge/registry"
	"toolkit/internal/knowledge/pointers"
)

// ResolvedChainAnchor is the exported shape ChainAnchor returns. Carries the
// DB-resolved chain identity (id + project) chain-anchored docs need so the
// *Forged event can stamp chain_id (and the construct edit path can re-anchor
// without re-implementing the project-scoped + ambiguity-resolving lookup).
type ResolvedChainAnchor struct {
	ChainID   int64
	ProjectID string
}

// ChainAnchor looks up a chain by (project, slug) and returns the resolved
// identity (chain_id + project_id). Exported (chain 311 T7 Stage 3 Slice 4)
// so the construct edit path for retrospective + report-card re-homes the
// resolve without re-implementing the project-scoped lookup + the
// cross-project-ambiguity error wording. Wraps resolveChainAnchor for
// external callers; the internal forge path keeps using resolveChainAnchor.
func ChainAnchor(ctx context.Context, q db.Queryer, project, slug string) (ResolvedChainAnchor, error) {
	r, err := resolveChainAnchor(ctx, q, project, slug)
	if err != nil {
		return ResolvedChainAnchor{}, err
	}
	return ResolvedChainAnchor{ChainID: r.ChainID, ProjectID: r.ProjectID}, nil
}

// RepoRelativePath returns the doc path relative to the repo's markdown root
// — the form the *Forged event's file_path field uses. Exported so the
// construct edit path matches forge's create-time repo-relative form
// byte-for-byte. See repoRelativePath for the fallback semantics.
func RepoRelativePath(outputPath, root string, schema registry.Schema) string {
	return repoRelativePath(outputPath, root, schema)
}

// ResolveMarkdownRoot returns the absolute markdown root for the given
// schema + project (env override → project paths → CWD fallback). Exported
// so the construct edit path can compute RepoRelativePath without poking
// the env directly.
func ResolveMarkdownRoot(ctx context.Context, q db.Queryer, project string, schema registry.Schema) string {
	return resolveMarkdownRoot(ctx, q, project, schema)
}

// RetrospectiveSkeletonFields names the skeleton sections forge counts when
// computing RetrospectiveForged.SectionCount. Exported so the construct edit
// path can compute the same count from the post-edit merged map.
func RetrospectiveSkeletonFields() []string {
	out := make([]string, len(retrospectiveSkeletonFields))
	copy(out, retrospectiveSkeletonFields)
	return out
}

// ReportCardSkeletonFields names the skeleton sections forge counts when
// computing ReportCardForged.SectionCount.
func ReportCardSkeletonFields() []string {
	out := make([]string, len(reportCardSkeletonFields))
	copy(out, reportCardSkeletonFields)
	return out
}

// BuildChainAnchoredDocPointer builds the knowledge_pointer the AfterCreate /
// AfterEdit notifier upserts for a chain-anchored doc. Exported so the
// construct edit path refreshes the pointer with the same content forge
// would, after the file edit lands. See buildChainAnchoredDocPointer (the
// internal body).
func BuildChainAnchoredDocPointer(project, slug, kind string, fields map[string]FieldValue) pointers.KnowledgePointer {
	return buildChainAnchoredDocPointer(project, slug, kind, fields)
}

// UpsertChainAnchoredDocPointer refreshes the knowledge_pointer for a
// chain-anchored doc (retrospective / report-card) so the construct edit
// path keeps the index in sync with the post-edit file, matching forge_edit's
// AfterEditNotifier behavior. Built from the same field map the edit
// rewrote — keeps the pointer byte-identical to forge's. Pool-based:
// non-transactional, called after the record submit lands.
func UpsertChainAnchoredDocPointer(ctx context.Context, pool *db.Pool, project, slug, kind string, fields map[string]FieldValue) error {
	p := buildChainAnchoredDocPointer(project, slug, kind, fields)
	_, err := pointers.Upsert(ctx, pool, p)
	return err
}

// chainAnchorResolution carries the DB-resolved chain identity that
// retrospective + report-card schemas need on the create path. Populated
// by resolveChainAnchor before the file write so a missing chain rejects
// pre-write rather than after.
type chainAnchorResolution struct {
	ChainID   int64
	ProjectID string
}

// resolveChainAnchor looks up a chain by slug + (when supplied) project
// scope, returning the (chain_id, project_id) pair for downstream
// frontmatter + event-emit use. Rejects with a clear error when the
// chain isn't found — chain-anchored docs MUST point at a real chain.
//
// When the caller passes an empty project (cross-project sentinel), the
// lookup falls back to slug-only resolution; if the slug appears in
// multiple projects the error names them so the caller can re-call
// with explicit project scope.
func resolveChainAnchor(ctx context.Context, q db.Queryer, project, slug string) (chainAnchorResolution, error) {
	if project != "" {
		var id int64
		err := q.QueryRowContext(ctx,
			`SELECT id FROM proj_chain_status WHERE project_id = ? AND slug = ?`,
			project, slug).Scan(&id)
		if err == nil {
			return chainAnchorResolution{ChainID: id, ProjectID: project}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return chainAnchorResolution{}, fmt.Errorf("chain lookup: %w", err)
		}
		return chainAnchorResolution{}, fmt.Errorf("chain_not_found: no chain with slug %q in project %q", slug, project)
	}
	rows, err := q.QueryContext(ctx,
		`SELECT id, project_id FROM proj_chain_status WHERE slug = ? ORDER BY project_id`, slug)
	if err != nil {
		return chainAnchorResolution{}, fmt.Errorf("chain lookup: %w", err)
	}
	defer rows.Close()
	type candidate struct {
		id      int64
		project string
	}
	var all []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.project); err != nil {
			return chainAnchorResolution{}, err
		}
		all = append(all, c)
	}
	switch len(all) {
	case 0:
		return chainAnchorResolution{}, fmt.Errorf("chain_not_found: no chain with slug %q in any project", slug)
	case 1:
		return chainAnchorResolution{ChainID: all[0].id, ProjectID: all[0].project}, nil
	default:
		projects := make([]string, 0, len(all))
		for _, c := range all {
			projects = append(projects, c.project)
		}
		return chainAnchorResolution{}, fmt.Errorf("chain slug %q is ambiguous across projects: %s. Re-call with explicit top-level project param", slug, strings.Join(projects, ", "))
	}
}

// kebabToScreamingSnake converts a kebab-case slug (work-batching-
// and-forge-templates) to SCREAMING_SNAKE_CASE
// (WORK_BATCHING_AND_FORGE_TEMPLATES). Used to derive the filename
// stem for retrospective + report-card docs from chain_slug. Idempotent
// on already-upper input; trims any extra hyphens / underscores.
func kebabToScreamingSnake(s string) string {
	out := strings.ReplaceAll(s, "-", "_")
	return strings.ToUpper(out)
}

// repoRelativePath returns the doc path relative to the repo root for
// use in the event payload's file_path field. Strips the
// resolveMarkdownRoot prefix so the recorded path is portable across
// machines (the on-disk root differs per workstation; the repo-relative
// form survives clone).
func repoRelativePath(outputPath, root string, schema registry.Schema) string {
	rel, err := filepath.Rel(root, outputPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		// Fallback to the full path if relativization fails; better to
		// record something than nothing.
		return outputPath
	}
	return rel
}

// retrospectiveSkeletonFields is the ordered list of skeleton section
// field names declared in blueprints/forge-schemas/retrospective.toml.
// Used to count populated sections for the RetrospectiveForged event's
// section_count. Mirrors the schema's [[fields]] order; if the schema
// adds / removes a skeleton field, update this list to match.
var retrospectiveSkeletonFields = []string{
	"what_landed",
	"token_budget_delta",
	"what_didnt_land",
	"surprises",
	"next_chain_candidates",
	"closing_event",
	"acceptance_criteria_status",
}

// reportCardSkeletonFields is the ordered list of skeleton section
// field names declared in blueprints/forge-schemas/report-card.toml.
var reportCardSkeletonFields = []string{
	"per_task_grades",
	"independent_reproduction",
	"overall_verdict",
	"measurement",
}

// renderMemoryMarkdown builds the legacy-compatible auto-memory file
// body. Frontmatter shape per ~/.claude/CLAUDE.md §auto-memory:
//
//	---
//	name: <kebab-slug>
//	description: <one-line>
//	metadata:
//	  type: <kind>
//	  project: <optional>
//	  source: <optional>
//	  observed_first: <optional ISO date>
//	  recurrence_count: <optional int>
//	---
//	<body markdown>
//
// The harness's auto-load reads this exact shape; T3's session-start
// materialization hook copies it verbatim into the project's harness
// dir. The materialization hook uses metadata.project (when set) to
// route project-scoped entries (feedback/project/reference kinds) only
// into the project they were forged in; user-kind entries fan out
// regardless. Don't add a `date:` or `slug:` line — the harness doesn't
// expect them and they'd diverge from the documented contract.
func renderMemoryMarkdown(project, slug string, fields map[string]FieldValue) string {
	var out strings.Builder
	out.WriteString("---\n")
	out.WriteString(fmt.Sprintf("name: %s\n", slug))
	if desc := stringField(fields, "description"); desc != "" {
		out.WriteString(fmt.Sprintf("description: %s\n", desc))
	}
	out.WriteString("metadata:\n")
	out.WriteString(fmt.Sprintf("  type: %s\n", stringField(fields, "memory_kind")))
	if project != "" {
		out.WriteString(fmt.Sprintf("  project: %s\n", project))
	}
	if src := stringField(fields, "source"); src != "" {
		out.WriteString(fmt.Sprintf("  source: %s\n", src))
	}
	if obs := stringField(fields, "observed_first"); obs != "" {
		out.WriteString(fmt.Sprintf("  observed_first: %s\n", obs))
	}
	if rc := stringField(fields, "recurrence_count"); rc != "" {
		out.WriteString(fmt.Sprintf("  recurrence_count: %s\n", rc))
	}
	out.WriteString("---\n\n")
	out.WriteString(stringField(fields, "body"))
	if !strings.HasSuffix(out.String(), "\n") {
		out.WriteString("\n")
	}
	return out.String()
}

// WriteMemoryArtifact renders + atomically writes the auto-memory FILE exactly
// as forge(memory) does and returns the on-disk path. It is the re-home seam
// (T7 §15) the record construction layer (go/internal/work) calls so a memory
// filed through record produces a BYTE-IDENTICAL file to forge's — the file is
// the primary artifact for the markdown schemas (the MemoryWritten event is
// fail-open telemetry). Reuses forge's renderMemoryMarkdown / resolveMarkdownRoot
// / buildOutputPath / atomicWrite verbatim, so it can't drift from forge's
// rendering or its markdown-root resolution (incl. the FORGE_MARKDOWN_ROOT
// override + the vault→~/.claude branch).
//
// Parameterized on db.Queryer (not *sql.Tx) because the file write is
// non-transactional and the only DB touch is resolveMarkdownRoot's projects.path
// lookup — which a *sql.DB serves fine (vault schemas like memory don't even
// reach it). Forge's own memoryStrategy.Create is unchanged.
// Returns the on-disk path AND the routing note byte-identical to forge(memory)'s
// (memoryStrategy.DeriveRoutingFields) so the construct create result can carry
// it into the agent-facing response without re-deriving / drifting.
func WriteMemoryArtifact(ctx context.Context, q db.Queryer, schema registry.Schema, project, slug, memoryKind, description, body, source, observedFirst, recurrenceCount string) (path, routingNote string, err error) {
	fields := map[string]FieldValue{
		"memory_kind": SingleValue(memoryKind),
		"kind":        SingleValue(memoryKind), // {kind} filename token (memoryStrategy.DeriveRoutingFields)
		"description": SingleValue(description),
		"body":        SingleValue(body),
	}
	if source != "" {
		fields["source"] = SingleValue(source)
	}
	if observedFirst != "" {
		fields["observed_first"] = SingleValue(observedFirst)
	}
	if recurrenceCount != "" {
		fields["recurrence_count"] = SingleValue(recurrenceCount)
	}
	bodyMD := renderMemoryMarkdown(project, slug, fields)
	root := resolveMarkdownRoot(ctx, q, project, schema)
	path = buildOutputPath(root, schema, slug, "", fields)
	storage := schema.ResolvedStorage()
	guard := filepath.Join(root, firstNonEmptyStr(storage.OutputDir, markdownOutputDirFromStorage(storage)))
	if err := atomicWrite(path, guard, []byte(bodyMD)); err != nil {
		return "", "", err
	}
	// Byte-identical to memoryStrategy.DeriveRoutingFields' note.
	routingNote = fmt.Sprintf("routed to memory/%s (memory_kind=%q)", memoryKind, memoryKind)
	return path, routingNote, nil
}

// ChainAnchoredDocResult carries what the record construction layer needs after
// WriteChainAnchoredDoc writes a retrospective / report-card file: the
// repo-relative path + resolved chain id for the *Forged event payload, and the
// populated-section count. The knowledge_pointer is already upserted.
type ChainAnchoredDocResult struct {
	FilePath     string // repo-relative (matches the event's file_path)
	ChainID      int64  // resolved chain anchor id (event's chain_id)
	SectionCount int
	RoutingNote  string // byte-identical to the strategy's "routed to docs/…" note
}

// WriteChainAnchoredDoc renders + atomically writes a retrospective or
// report-card markdown file exactly as forge(retrospective)/forge(report-card)
// does, upserts its knowledge_pointer, and returns the data the record
// construction layer needs to emit the fail-open RetrospectiveForged /
// ReportCardForged event through record. name is "retrospective" or
// "report-card"; sections maps the schema's section field names → verbatim
// markdown body (what_landed, surprises, per_task_grades, …); empty/missing
// entries are skipped (skeleton-only docs are valid). title defaults to
// "<chainSlug> — Retrospective"/"Report Card" when empty.
//
// Re-homes forge.createChainAnchoredDoc's file path (resolveChainAnchor →
// defaults → renderMarkdown → write under the guard → buildChainAnchoredDocPointer
// → pointers.Upsert), reusing forge's helpers verbatim so the file + pointer
// can't drift. Two DELIBERATE differences vs createChainAnchoredDoc:
//   - the *Forged EVENT is NOT emitted here — the caller routes it through
//     record (so the emit goes through the record substrate, not events.Emit);
//   - the retrospective next-chain-candidate capture gate
//     (captureOrphanedFollowons, which auto-files a suggestion per uncaptured
//     candidate) is NOT run — it is a DOCUMENTED DELTA of the record path for
//     now (forge still runs it when invoked directly).
//
// Rejects chain_not_found when chainSlug doesn't resolve (parity with forge).
// pool-based: the file write + pointer upsert are non-transactional.
func WriteChainAnchoredDoc(ctx context.Context, pool *db.Pool, schema registry.Schema, name, project, slug, chainSlug, title string, sections map[string]string) (ChainAnchoredDocResult, error) {
	if strings.TrimSpace(chainSlug) == "" {
		return ChainAnchoredDocResult{}, fmt.Errorf("forge(%s): chain_slug is required", name)
	}
	anchor, err := resolveChainAnchor(ctx, pool.DB(), project, chainSlug)
	if err != nil {
		return ChainAnchoredDocResult{}, err
	}
	fields := map[string]FieldValue{
		"chain_slug":       SingleValue(chainSlug),
		"chain_slug_upper": SingleValue(kebabToScreamingSnake(chainSlug)),
	}
	for k, v := range sections {
		if v != "" {
			fields[k] = SingleValue(v)
		}
	}
	date := ensureDate(fields)
	fields["created"] = SingleValue(date)
	if title == "" {
		label := "Retrospective"
		if name == "report-card" {
			label = "Report Card"
		}
		title = fmt.Sprintf("%s — %s", chainSlug, label)
	}
	fields["title"] = SingleValue(title)
	fields["kind"] = SingleValue(name)

	body := renderMarkdown(schema, fields, nil, slug, date)
	root := resolveMarkdownRoot(ctx, pool.DB(), project, schema)
	path := buildOutputPath(root, schema, slug, date, fields)
	storage := schema.ResolvedStorage()
	guard := filepath.Join(root, firstNonEmptyStr(storage.OutputDir, markdownOutputDirFromStorage(storage)))
	if err := atomicWrite(path, guard, []byte(body)); err != nil {
		return ChainAnchoredDocResult{}, err
	}

	skeleton := retrospectiveSkeletonFields
	if name == "report-card" {
		skeleton = reportCardSkeletonFields
	}
	sectionCount := 0
	for _, fn := range skeleton {
		if v, ok := fields[fn]; ok && !v.IsEmpty() {
			sectionCount++
		}
	}

	// knowledge_pointer (Indexed) — built from the same fields forge's
	// AfterCreate notifier uses (buildChainAnchoredDocPointer), so the row is
	// byte-identical to forge's.
	p := buildChainAnchoredDocPointer(project, slug, name, fields)
	if _, err := pointers.Upsert(ctx, pool, p); err != nil {
		return ChainAnchoredDocResult{}, fmt.Errorf("forge(%s): index upsert: %w", name, err)
	}

	// Byte-identical to the chain-anchored-doc strategy's routing note.
	routingNote := fmt.Sprintf("routed to docs/%s (chain_id=%d resolved from chain_slug=%q)",
		fmt.Sprintf("%s_%s_%s.md", kebabToScreamingSnake(chainSlug), strings.ToUpper(strings.ReplaceAll(name, "-", "_")), date),
		anchor.ChainID, chainSlug)
	return ChainAnchoredDocResult{
		FilePath:     repoRelativePath(path, root, schema),
		ChainID:      anchor.ChainID,
		SectionCount: sectionCount,
		RoutingNote:  routingNote,
	}, nil
}

// vaultNoteSubdir derives the vault subdirectory for a vault-note from
// its kind enum and (optional) scope routing input, matching the existing
// ~/.claude/vault/ layout:
//
//	decision  → "decisions"
//	learning  → "learnings/<scope>" (or "learnings/general" when empty)
//	reference → "reference"
//
// Returns the singular "reference" / plural "decisions+learnings" mix
// that matches the established vault convention. Unknown kinds fall
// back to "scratch" so a misconfigured forge call doesn't end up at
// the vault root.
func vaultNoteSubdir(kind, scope string) string {
	switch kind {
	case "decision":
		return "decisions"
	case "learning":
		if scope == "" {
			return "learnings/general"
		}
		return "learnings/" + scope
	case "reference":
		return "reference"
	}
	return "scratch"
}

// vaultNoteRoutingNote constructs the one-line routing summary surfaced
// on every vault-note forge / forge_edit response (bug 1433). Subdir is
// the resolved path under vault/; kind and scope are the inputs that
// produced it. Empty kind yields an empty note — the caller is editing
// an existing entry without changing its kind, so there's no routing
// decision to surface. Mirrors the SKILL `vault-filing-discipline`
// "Response shape" subsection's `routing_note` description.
func vaultNoteRoutingNote(kind, scope, subdir string) string {
	switch kind {
	case "learning":
		if scope == "" {
			return "routed to learnings/general (no scope field set — explicit cross-project bucket)"
		}
		return fmt.Sprintf("routed to learnings/%s (scope field set to %q; pass empty scope to route cross-project)", scope, scope)
	case "":
		return ""
	default:
		return fmt.Sprintf("routed to %s (cross-project by kind=%q)", subdir, kind)
	}
}

// resolveMarkdownRoot picks the on-disk root for a markdown-target
// schema. Resolution priority (highest → lowest):
//
//  1. FORGE_MARKDOWN_ROOT env var — test-injection override.
//  2. projects.path column for the call's `project` — the registered
//     repo root. Set via admin.project_register.
//  3. Vault branch: schemas with vault in the output_dir resolve under
//     ~/.claude/vault.
//  4. os.Getwd() — last-resort fallback. Logs a WARN because hitting
//     this branch in production means the project's path wasn't
//     registered AND the stdio MCP CWD probably isn't the target
//     project's repo root.
//
// Bug `forge-migration-writes-relative-to-toolkit-server-cwd-not-target-
// repo-root` (2026-05-23): pre-fix this function jumped straight from
// the env override to the cwd fallback, which meant the stdio MCP
// process (CWD = ~/dev/, NOT the target project's repo root) wrote
// migrations / retrospectives / report-cards under ~/dev/, not under
// the project. Observed three times during the work-batching-and-
// forge-templates chain's dog-fooding (T6 migration, T8 retro, T9
// report card); fix lifts the projects.path lookup into the priority
// chain so the production-stdio path lands files under the right repo
// root.
func resolveMarkdownRoot(ctx context.Context, q db.Queryer, project string, schema registry.Schema) string {
	if root := os.Getenv("FORGE_MARKDOWN_ROOT"); root != "" {
		return root
	}
	storage := schema.ResolvedStorage()
	dir := storage.OutputDir
	if storage.Markdown != nil && storage.Markdown.OutputDir != "" {
		dir = storage.Markdown.OutputDir
	}
	// Vault-branch check first — vault paths are project-agnostic and
	// always resolve under the home dir regardless of which project
	// triggered the forge.
	if strings.Contains(dir, "vault") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, ".claude")
		}
	}
	// Project-registered path: consult the projects table for the
	// dispatch-scope project's registered repo root.
	if q != nil && project != "" {
		var p string
		if err := q.QueryRowContext(ctx,
			`SELECT path FROM projects WHERE id = ?`, project,
		).Scan(&p); err == nil && p != "" {
			return p
		}
	}
	cwd, _ := os.Getwd()
	return cwd
}

// markdownOutputDirFromStorage returns the markdown-target's output dir
// regardless of whether the schema uses the flat or nested form.
func markdownOutputDirFromStorage(storage registry.Storage) string {
	if storage.Markdown != nil {
		return storage.Markdown.OutputDir
	}
	return storage.OutputDir
}

func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func currentDate() string {
	return time.Now().UTC().Format("2006-01-02")
}

// RejectDuplicateBySlug returns a *duplicateCreateError when a (project_id, slug)
// row already exists in table, else nil. Shared by the bug + suggestion create
// paths: create is once-only for these event-sourced shapes, so a second forge on
// an existing slug must REJECT (pointing at forge_edit) rather than silently
// re-emit + upsert via the fold's ON CONFLICT(project_id, slug) DO UPDATE (bug
// 946; the bug/suggestion siblings of the chain-934 fix). table is a fixed
// in-package literal (not caller input), so the fmt.Sprintf is injection-safe —
// same pattern as GenericStrategy.Create's INSERT builder.
//
// Exported + parameterized on db.Queryer (which *sql.Tx and *sql.DB both
// satisfy) so the record construction layer (go/internal/work) can REUSE forge's
// exact once-only-create check (B-D1) over the record path without re-implementing
// it — the re-home discipline of T7 §15. Forge's own callers still pass their tx.
func RejectDuplicateBySlug(ctx context.Context, q db.Queryer, schemaName, table, project, slug string) error {
	var exists int
	switch err := q.QueryRowContext(ctx,
		fmt.Sprintf("SELECT 1 FROM %s WHERE project_id = ? AND slug = ? LIMIT 1", table),
		project, slug).Scan(&exists); {
	case err == nil:
		return &duplicateCreateError{schemaName: schemaName, slug: slug}
	case errors.Is(err, sql.ErrNoRows):
		return nil
	default:
		return fmt.Errorf("%s duplicate-check: %w", schemaName, err)
	}
}
