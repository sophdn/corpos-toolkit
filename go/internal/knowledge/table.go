// Package knowledge's table.go assembles the dispatch.Table for the
// knowledge meta-tool: vault search/read, kiwix corpora, library entries
// + dewey routing, and the unified knowledge_search. Pairs with the
// parallel BuildTable functions in internal/admin, internal/work,
// internal/measure.
//
// Every entry adapts a typed-return handler into dispatch.Handler via
// dispatch.Adapt / AdaptParamsOnly — that adapter is the single
// JSON-marshaling seam where typed results widen to `any` for the
// dispatcher.
package knowledge

import (
	"context"
	"encoding/json"

	"toolkit/internal/dispatch"
)

// BuildTable returns the knowledge surface's dispatch.Table. Every
// handler takes the same Deps bundle (pool + inference router); the
// caller has already configured both at this point.
func BuildTable(deps Deps) dispatch.Table {
	// vaultAdapt closes over deps for handlers that take (ctx, deps, params)
	// and ignore project.
	vaultAdapt := func(h func(context.Context, Deps, json.RawMessage) (VaultSearchResult, error)) dispatch.Handler {
		return dispatch.AdaptParamsOnly(func(ctx context.Context, params json.RawMessage) (VaultSearchResult, error) {
			return h(ctx, deps, params)
		})
	}
	vaultReadAdapt := func(h func(context.Context, Deps, json.RawMessage) (VaultReadResult, error)) dispatch.Handler {
		return dispatch.AdaptParamsOnly(func(ctx context.Context, params json.RawMessage) (VaultReadResult, error) {
			return h(ctx, deps, params)
		})
	}
	memoryReadAdapt := func(h func(context.Context, Deps, json.RawMessage) (MemoryReadResult, error)) dispatch.Handler {
		return dispatch.AdaptParamsOnly(func(ctx context.Context, params json.RawMessage) (MemoryReadResult, error) {
			return h(ctx, deps, params)
		})
	}
	recordQueryInteractionAdapt := func(h func(context.Context, Deps, json.RawMessage) (RecordQueryInteractionResult, error)) dispatch.Handler {
		return dispatch.AdaptParamsOnly(func(ctx context.Context, params json.RawMessage) (RecordQueryInteractionResult, error) {
			return h(ctx, deps, params)
		})
	}
	kiwixSearchAdapt := func(h func(context.Context, Deps, json.RawMessage) (KiwixSearchResult, error)) dispatch.Handler {
		return dispatch.AdaptParamsOnly(func(ctx context.Context, params json.RawMessage) (KiwixSearchResult, error) {
			return h(ctx, deps, params)
		})
	}
	kiwixFetchAdapt := func(h func(context.Context, Deps, json.RawMessage) (KiwixFetchResult, error)) dispatch.Handler {
		return dispatch.AdaptParamsOnly(func(ctx context.Context, params json.RawMessage) (KiwixFetchResult, error) {
			return h(ctx, deps, params)
		})
	}
	kiwixListAdapt := func(h func(context.Context, Deps, json.RawMessage) (KiwixListBooksResult, error)) dispatch.Handler {
		return dispatch.AdaptParamsOnly(func(ctx context.Context, params json.RawMessage) (KiwixListBooksResult, error) {
			return h(ctx, deps, params)
		})
	}

	// project-scoped handlers retain the dispatch project string.
	librarySimple := func(h func(context.Context, Deps, string, json.RawMessage) (LibrarySimpleResult, error)) dispatch.Handler {
		return dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (LibrarySimpleResult, error) {
			return h(ctx, deps, project, params)
		})
	}
	libraryGet := func(h func(context.Context, Deps, string, json.RawMessage) (LibraryGetResult, error)) dispatch.Handler {
		return dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (LibraryGetResult, error) {
			return h(ctx, deps, project, params)
		})
	}
	libraryUpdate := func(h func(context.Context, Deps, string, json.RawMessage) (LibraryUpdateResult, error)) dispatch.Handler {
		return dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (LibraryUpdateResult, error) {
			return h(ctx, deps, project, params)
		})
	}
	libraryList := func(h func(context.Context, Deps, string, json.RawMessage) (LibraryListResult, error)) dispatch.Handler {
		return dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (LibraryListResult, error) {
			return h(ctx, deps, project, params)
		})
	}
	librarySections := func(h func(context.Context, Deps, string, json.RawMessage) (LibrarySectionsResult, error)) dispatch.Handler {
		return dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (LibrarySectionsResult, error) {
			return h(ctx, deps, project, params)
		})
	}
	libraryListDewey := func(h func(context.Context, Deps, string, json.RawMessage) (LibraryListDeweyResult, error)) dispatch.Handler {
		return dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (LibraryListDeweyResult, error) {
			return h(ctx, deps, project, params)
		})
	}
	libraryFind := func(h func(context.Context, Deps, string, json.RawMessage) (LibraryFindResult, error)) dispatch.Handler {
		return dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (LibraryFindResult, error) {
			return h(ctx, deps, project, params)
		})
	}
	libraryCrossRef := func(h func(context.Context, Deps, string, json.RawMessage) (LibraryCrossRefResult, error)) dispatch.Handler {
		return dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (LibraryCrossRefResult, error) {
			return h(ctx, deps, project, params)
		})
	}
	knowSearchAdapt := func(h func(context.Context, Deps, string, json.RawMessage) (KnowledgeSearchResult, error)) dispatch.Handler {
		return dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (KnowledgeSearchResult, error) {
			return h(ctx, deps, project, params)
		})
	}
	knowMissAdapt := func(h func(context.Context, Deps, string, json.RawMessage) (KnowledgeReportMissResult, error)) dispatch.Handler {
		return dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (KnowledgeReportMissResult, error) {
			return h(ctx, deps, project, params)
		})
	}
	curationList := func(h func(context.Context, Deps, string, json.RawMessage) (CurationListResult, error)) dispatch.Handler {
		return dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (CurationListResult, error) {
			return h(ctx, deps, project, params)
		})
	}
	curationRead := func(h func(context.Context, Deps, string, json.RawMessage) (CurationReadResult, error)) dispatch.Handler {
		return dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (CurationReadResult, error) {
			return h(ctx, deps, project, params)
		})
	}
	curationPromote := func(h func(context.Context, Deps, string, json.RawMessage) (CurationPromoteResult, error)) dispatch.Handler {
		return dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (CurationPromoteResult, error) {
			return h(ctx, deps, project, params)
		})
	}
	curationReject := func(h func(context.Context, Deps, string, json.RawMessage) (CurationRejectResult, error)) dispatch.Handler {
		return dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (CurationRejectResult, error) {
			return h(ctx, deps, project, params)
		})
	}
	curationBulk := func(h func(context.Context, Deps, string, json.RawMessage) (CurationBulkActionResult, error)) dispatch.Handler {
		return dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (CurationBulkActionResult, error) {
			return h(ctx, deps, project, params)
		})
	}

	return dispatch.Table{
		"vault_search":             vaultAdapt(HandleVaultSearch),
		"vault_read":               vaultReadAdapt(HandleVaultRead),
		"memory_read":              memoryReadAdapt(HandleMemoryRead),
		"record_query_interaction": recordQueryInteractionAdapt(HandleRecordQueryInteraction),
		"kiwix_search":             kiwixSearchAdapt(HandleKiwixSearch),
		"kiwix_fetch":              kiwixFetchAdapt(HandleKiwixFetch),
		"kiwix_list_books":         kiwixListAdapt(HandleKiwixListBooks),
		"library_add":              librarySimple(HandleLibraryAdd),
		"library_get":              libraryGet(HandleLibraryGet),
		"library_update":           libraryUpdate(HandleLibraryUpdate),
		"library_retire":           librarySimple(HandleLibraryRetire),
		"library_list_active":      libraryList(HandleLibraryListActive),
		"library_list_sections":    librarySections(HandleLibraryListSections),
		"library_list_dewey":       libraryListDewey(HandleLibraryListDewey),
		"library_find":             libraryFind(HandleLibraryFind),
		"library_cross_reference":  libraryCrossRef(HandleLibraryCrossReference),
		"knowledge_search":         knowSearchAdapt(HandleKnowledgeSearch),
		"knowledge_report_miss":    knowMissAdapt(HandleKnowledgeReportMiss),
		"curation_list":            curationList(HandleCurationList),
		"curation_read":            curationRead(HandleCurationRead),
		"curation_promote":         curationPromote(HandleCurationPromote),
		"curation_reject":          curationReject(HandleCurationReject),
		"curation_bulk_action":     curationBulk(HandleCurationBulkAction),
	}
}
