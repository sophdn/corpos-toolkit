package construct

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	"toolkit/internal/db"
	"toolkit/internal/forge/registry"
)

// survivors.go holds the forge-native persistence arms that had NO construct
// equivalent before the forge archive (chain 311 T7 Stage 6 P2-C.2). The
// PHASE 2 SCOPE FINDING: main.go's residual forge ExecutePreparedCreate/Edit
// path still served four shapes no construct file reached — vault-note CREATE
// (a no-event file+pointer write), vault-note EDIT, bench EDIT, and
// trained_model EDIT (generic UPDATE + set-by gate). With forge archiving,
// these "survivor arms" re-home here so construct.Create / construct.Update
// own every live write path. They are byte-faithful ports of
// vaultNoteStrategy.Create + GenericStrategy.Edit (former forge/strategy.go);
// the char-net (moved onto construct) is the parity gate.

// VaultNoteInput carries a vault-note create. vault-note is the lone no-event
// markdown create: the file IS the artifact, the knowledge_pointer is upserted
// by the dispatch host's AfterCreate notifier (IndexUpsertNotifier), and NO
// typed event is emitted. Rather than enumerate vault-note's flexible field
// set in a typed struct, the validated forge field map rides straight through
// (InputFromForge copies prep.Validated by reference — the SAME map the
// finalize notifier later reads, so the {subdir}/{date} fields createVaultNote
// derives are visible to buildVaultNotePointer, matching forge's in-place
// mutation of prep.Validated).
type VaultNoteInput struct {
	Slug   string
	Fields map[string]FieldValue
}

// createVaultNote writes the vault-note markdown file and returns its path +
// routing note WITHOUT emitting an event or submitting through record — a
// byte-faithful port of forge's vaultNoteStrategy.Create. It MUTATES the fields
// map in place (mergeDerived adds {subdir}; ensureDate adds {date}); because
// InputFromForge passed prep.Validated by reference, the dispatch host's
// AfterCreate notifier (wired with the full IndexUpsertNotifier for vault-note)
// then builds the knowledge_pointer from the same mutated map — exactly as
// forge's ExecutePreparedCreate threaded the map through Create → Finalize. The
// pointer upsert, the same-slug-reforge "updated" action verb, and the
// scope-change orphan-file cleanup all stay in that notifier (not here).
func createVaultNote(ctx context.Context, deps Deps, project, slug string, fields map[string]FieldValue) (CreateResult, error) {
	sc, ok := deps.Schemas.Get("vault-note")
	if !ok {
		return CreateResult{}, fmt.Errorf("construct.Create: vault-note schema not in registry")
	}
	extra, routingNote, _ := deriveRoutingFields("vault-note", project, slug, fields)
	mergeDerived(fields, extra)
	date := ensureDate(fields)
	body := renderMarkdown(sc, fields, nil, slug, date)
	path, _, err := writeMarkdownArtifact(ctx, deps.Pool.DB(), sc, project, slug, date, fields, body)
	if err != nil {
		return CreateResult{}, err
	}
	return CreateResult{Schema: "vault-note", EntitySlug: slug, FilePath: path, RoutingNote: routingNote}, nil
}

// writeMarkdownArtifact resolves the on-disk root (via q), builds the output
// path from the schema's filename_pattern + fields, and atomically writes the
// rendered body under the output-dir guard. Port of forge strategy.go's
// writeMarkdownArtifact, taking a db.Queryer (pool.DB()) instead of a *sql.Tx —
// the non-tx create path (construct.Create) resolves the markdown root through
// the pool, and the file write itself is non-transactional, as it always was.
func writeMarkdownArtifact(ctx context.Context, q db.Queryer, sc registry.Schema, project, slug, date string, fields map[string]FieldValue, body string) (path, root string, err error) {
	storage := sc.ResolvedStorage()
	root = resolveMarkdownRoot(ctx, q, project, sc)
	path = buildOutputPath(root, sc, slug, date, fields)
	guard := filepath.Join(root, firstNonEmptyStr(storage.OutputDir, markdownOutputDirFromStorage(storage)))
	if err := atomicWrite(path, guard, []byte(body)); err != nil {
		return "", "", err
	}
	return path, root, nil
}

// ── no-event EDIT survivors (vault-note / bench / trained_model) ─────────────

// isNoEventEdit reports whether the schema's edit path emits NO event and so
// can't run through construct.Update's event-centric orchestration tail. These
// are the §15 delta survivors forge served via vaultNoteStrategy.Edit
// (editMarkdown) + GenericStrategy.Edit (generic UPDATE): vault-note rewrites
// its markdown file; bench + trained_model do a generic (project_id, slug)
// UPDATE. The dispatch host's AfterEdit notifier (IndexUpsertOnEditNotifier)
// refreshes the index — file read-back for vault-note, no-op for the
// not-indexed bench/trained_model.
func isNoEventEdit(schema string) bool {
	switch schema {
	case "vault-note", "bench", "trained_model":
		return true
	default:
		return false
	}
}

// updateNoEvent runs the no-event edit survivors. The parse front
// (PrepareForgeEdit) already ran B-G1 placeholder guard + B-ED1 ValidatePartial,
// so this starts at B-ED2 set-by reject then dispatches to the markdown rewrite
// (vault-note) or the generic UPDATE (bench/trained_model). NotFound surfaces as
// a *NotFoundError so the dispatch host renders forge_edit's {error:"not_found"}
// envelope, identical to the covered edit path.
func updateNoEvent(ctx context.Context, deps Deps, s registry.Schema, prep ForgeEditPrep, project string) (UpdateResult, error) {
	schema := prep.SchemaName
	slug := prep.Slug
	validated := prep.Validated

	// B-ED2 set-by reject (matches GenericStrategy.Edit's inline gate wording;
	// no-ops for vault-note, which declares no set_by fields).
	if err := RejectSetByEditFields(s, validated); err != nil {
		return UpdateResult{}, err
	}

	switch schema {
	case "vault-note":
		res, _, err := EditMarkdownArtifact(ctx, deps.Pool.DB(), s, project, slug, validated, EditOpts{DropExtras: prep.DropExtras})
		if err != nil {
			return UpdateResult{}, err
		}
		if res.NotFound {
			return UpdateResult{}, &NotFoundError{Schema: schema, Slug: slug, Project: project}
		}
		return UpdateResult{
			Schema:        schema,
			EntitySlug:    slug,
			UpdatedFields: res.UpdatedFields,
			FilePath:      res.ArtifactPath,
			Relocated:     res.Relocated,
		}, nil
	case "bench", "trained_model":
		res, err := editGenericRow(ctx, deps.Pool, s, project, slug, validated)
		if err != nil {
			return UpdateResult{}, err
		}
		if res.NotFound {
			return UpdateResult{}, &NotFoundError{Schema: schema, Slug: slug, Project: project}
		}
		return UpdateResult{Schema: schema, EntitySlug: slug, UpdatedFields: res.UpdatedFields}, nil
	default:
		return UpdateResult{}, fmt.Errorf("construct.Update: schema %q has no no-event edit arm", schema)
	}
}

// editGenericRow applies a generic (project_id, slug)-keyed UPDATE — a
// byte-faithful port of GenericStrategy.Edit for the non-event-sourced tables
// (bench / trained_model). No typed event. The set_by gate is honored inline
// (redundant with updateNoEvent's RejectSetByEditFields, but kept so the
// wording matches forge exactly if a caller reaches this directly). Runs on a
// fresh write tx via pool.WithWrite (the standalone edit path; the batch path
// rejects markdown + is db-event-sourced only, so it never reaches here).
func editGenericRow(ctx context.Context, pool *db.Pool, sc registry.Schema, project, slug string, fields map[string]FieldValue) (EditResult, error) {
	storage := sc.ResolvedStorage()
	table := storage.Table
	colMap := storage.ColumnMap
	if storage.Target == registry.StorageTargetDual && storage.DB != nil {
		table = storage.DB.Table
		colMap = storage.DB.ColumnMap
	}
	declared := make(map[string]registry.Field, len(sc.Fields))
	for _, f := range sc.Fields {
		declared[f.Name] = f
	}
	var sets []string
	binds := db.NewArgs()
	var updated []string
	for name, v := range fields {
		fd, ok := declared[name]
		if !ok {
			continue
		}
		if fd.SetBy != "" {
			return EditResult{}, fmt.Errorf(
				"forge_edit can't set %q on %s — that field is owned by the %q action (its value lives on an event payload, not the projection). Use %s to (re)set it.",
				name, table, fd.SetBy, fd.SetBy)
		}
		col := name
		if mapped, has := colMap[name]; has {
			col = mapped
		}
		sets = append(sets, fmt.Sprintf("%s = ?", col))
		binds.AddString(v.AsJoined())
		updated = append(updated, name)
	}
	if len(sets) == 0 {
		return EditResult{}, fmt.Errorf("forge_edit: no recognized field updates")
	}
	sets = append(sets, "updated_at = datetime('now')")
	binds.AddString(project).AddString(slug)
	query := fmt.Sprintf("UPDATE %s SET %s WHERE project_id = ? AND slug = ?", table, strings.Join(sets, ", "))

	notFound := false
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, query, binds.Slice()...)
		if err != nil {
			return fmt.Errorf("update on %s failed: %w", table, err)
		}
		if affected, _ := res.RowsAffected(); affected == 0 {
			notFound = true
		}
		return nil
	})
	if err != nil {
		return EditResult{}, err
	}
	if notFound {
		return EditResult{NotFound: true}, nil
	}
	return EditResult{UpdatedFields: updated, Action: "updated"}, nil
}
