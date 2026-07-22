package construct

// forge_dispatch.go reconstructs the per-schema behavior the retired forge
// Strategy interface dispatched (chain 311 T7 Stage 6 P2-C.2). With the Strategy
// registry archived, the relocated markdown-edit + index seams can't call
// strategyFor(name).DeriveRoutingFields() / .Indexed() / .BuildPointer() /
// .ReadCanonicalFields() — these name-keyed functions replace those four methods
// over the same per-shape impls (build*Pointer / read*ForIndex / the routing-field
// derivers), a faithful transcription of the former forge/strategy.go method set.

import (
	"context"
	"fmt"

	"toolkit/internal/db"
	"toolkit/internal/forge/registry"
	"toolkit/internal/knowledge/pointers"
)

// deriveRoutingFields computes the extra filename / frontmatter routing fields a
// markdown shape needs but the caller didn't supply, plus a one-line routingNote
// (the former Strategy.DeriveRoutingFields). vault-note → {subdir}; memory →
// {kind} (memory_kind alias); retrospective / report-card → {chain_slug_upper}.
// Every db / generic shape returns (nil, "", nil).
func deriveRoutingFields(name, project, slug string, fields map[string]FieldValue) (map[string]FieldValue, string, error) {
	switch name {
	case "vault-note":
		noteKind := stringField(fields, "note_kind")
		noteScope := stringField(fields, "scope")
		subdir := vaultNoteSubdir(noteKind, noteScope)
		return map[string]FieldValue{"subdir": SingleValue(subdir)},
			vaultNoteRoutingNote(noteKind, noteScope, subdir), nil
	case "memory":
		memKind := stringField(fields, "memory_kind")
		return map[string]FieldValue{"kind": SingleValue(memKind)},
			fmt.Sprintf("routed to memory/%s (memory_kind=%q)", memKind, memKind), nil
	case "retrospective", "report-card":
		return chainAnchoredRoutingFields(fields)
	default:
		return nil, "", nil
	}
}

// chainAnchoredRoutingFields derives the {chain_slug_upper} filename driver
// shared by retrospective + report-card. Returns no extra fields when chain_slug
// is absent — the create path surfaces the chain_slug-required error first.
func chainAnchoredRoutingFields(fields map[string]FieldValue) (map[string]FieldValue, string, error) {
	chainSlug := stringField(fields, "chain_slug")
	if chainSlug == "" {
		return nil, "", nil
	}
	return map[string]FieldValue{"chain_slug_upper": SingleValue(kebabToScreamingSnake(chainSlug))}, "", nil
}

// mergeDerived merges DeriveRoutingFields output into the live fields map.
func mergeDerived(fields, extra map[string]FieldValue) {
	for k, v := range extra {
		fields[k] = v
	}
}

// ensureDate returns fields["date"], stamping today's date when absent so the
// frontmatter + filename_pattern see a consistent value.
func ensureDate(fields map[string]FieldValue) string {
	date := stringField(fields, "date")
	if date == "" {
		date = currentDate()
		fields["date"] = SingleValue(date)
	}
	return date
}

// indexedSchema reports whether the schema mirrors into knowledge_pointers + FTS5
// (the former Strategy.Indexed()). Indexed shapes: chain, task, bug, vault-note,
// retrospective, report-card. All others (suggestion, memory, migration, bench,
// trained_model) are not indexed.
func indexedSchema(name string) bool {
	switch name {
	case "chain", "task", "bug", "vault-note", "retrospective", "report-card":
		return true
	default:
		return false
	}
}

// buildPointerFor builds the knowledge_pointer for an indexed schema (the former
// Strategy.BuildPointer()). Non-indexed schemas return the zero pointer.
func buildPointerFor(name, project, slug string, fields map[string]FieldValue) pointers.KnowledgePointer {
	switch name {
	case "chain":
		return buildChainPointer(project, slug, fields)
	case "task":
		return buildTaskPointer(project, slug, fields)
	case "bug":
		return buildBugPointer(project, slug, fields)
	case "vault-note":
		return buildVaultNotePointer(project, slug, fields)
	case "retrospective":
		return buildChainAnchoredDocPointer(project, slug, "retrospective", fields)
	case "report-card":
		return buildChainAnchoredDocPointer(project, slug, "report-card", fields)
	default:
		return pointers.KnowledgePointer{}
	}
}

// readCanonicalFieldsFor reads back an indexed artifact's canonical field map for
// edit-side index refresh (the former Strategy.ReadCanonicalFields()). DB-target
// shapes read from SQL; markdown-target shapes (vault-note / chain-anchored docs)
// read from disk. Non-indexed shapes return ok=false.
func readCanonicalFieldsFor(name string, ctx context.Context, q db.Queryer, schemas *registry.Registry, project, slug, editedPath string) (map[string]FieldValue, bool, error) {
	switch name {
	case "chain":
		return readChainFieldsForIndex(ctx, q, project, slug)
	case "task":
		return readTaskFieldsForIndex(ctx, q, project, slug)
	case "bug":
		return readBugFieldsForIndex(ctx, q, project, slug)
	case "vault-note":
		return readVaultNoteForIndex(schemas, project, slug)
	case "retrospective":
		return readChainAnchoredDocForIndex(schemas, "retrospective", editedPath)
	case "report-card":
		return readChainAnchoredDocForIndex(schemas, "report-card", editedPath)
	default:
		return nil, false, nil
	}
}
