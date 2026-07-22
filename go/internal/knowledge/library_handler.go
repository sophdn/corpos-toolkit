package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"toolkit/internal/knowledge/library"
	"toolkit/internal/mcpparam"
	"toolkit/internal/obs"
	"toolkit/internal/qwenctx"
	"toolkit/internal/qwenretrieve"
)

// HandleLibraryAdd implements library_add. Required: project, dewey, citation
// (or flat fields), primary_author, establishes, what_it_answers, invoke_when.
func HandleLibraryAdd(ctx context.Context, deps Deps, project string, params json.RawMessage) (LibrarySimpleResult, error) {
	if project == "" {
		return LibrarySimpleResult{Error: "project is required for library actions"}, nil
	}
	entry, parseErr := parseLibraryEntryForAdd(params)
	if parseErr != "" {
		return LibrarySimpleResult{Error: parseErr}, nil
	}
	if err := library.Add(ctx, deps.Pool, project, entry); err != nil {
		return LibrarySimpleResult{Error: err.Error()}, nil
	}
	return LibrarySimpleResult{OK: true, Dewey: entry.Dewey}, nil
}

// HandleLibraryGet implements library_get. Required params: dewey.
func HandleLibraryGet(ctx context.Context, deps Deps, project string, params json.RawMessage) (LibraryGetResult, error) {
	if project == "" {
		return LibraryGetResult{Error: "project is required for library actions"}, nil
	}
	dewey := mcpparam.String(params, "dewey")
	if dewey == "" {
		return LibraryGetResult{Error: "params.dewey is required"}, nil
	}
	entry, err := library.Get(ctx, deps.Pool, project, dewey)
	if err != nil {
		if errors.Is(err, library.ErrNotFound) {
			return LibraryGetResult{Error: "not_found", Dewey: dewey}, nil
		}
		return LibraryGetResult{Error: err.Error()}, nil
	}
	return LibraryGetResult{Entry: &entry}, nil
}

// LibraryUpdateResult wraps either the library.UpdateResult or an error envelope.
type LibraryUpdateResult struct {
	Result *library.UpdateResult `json:"result,omitempty"`
	Error  string                `json:"error,omitempty"`
}

// HandleLibraryUpdate implements library_update. Required: dewey + update.
func HandleLibraryUpdate(ctx context.Context, deps Deps, project string, params json.RawMessage) (LibraryUpdateResult, error) {
	if project == "" {
		return LibraryUpdateResult{Error: "project is required for library actions"}, nil
	}
	dewey := mcpparam.String(params, "dewey")
	if dewey == "" {
		return LibraryUpdateResult{Error: "params.dewey is required"}, nil
	}
	upd, parseErr := parseEntryUpdate(params)
	if parseErr != "" {
		return LibraryUpdateResult{Error: parseErr}, nil
	}
	result, err := library.Update(ctx, deps.Pool, project, dewey, upd)
	if err != nil {
		return LibraryUpdateResult{Error: err.Error()}, nil
	}
	return LibraryUpdateResult{Result: &result}, nil
}

// HandleLibraryRetire implements library_retire.
func HandleLibraryRetire(ctx context.Context, deps Deps, project string, params json.RawMessage) (LibrarySimpleResult, error) {
	if project == "" {
		return LibrarySimpleResult{Error: "project is required for library actions"}, nil
	}
	dewey := mcpparam.String(params, "dewey")
	reason := mcpparam.String(params, "reason")
	if dewey == "" {
		return LibrarySimpleResult{Error: "params.dewey is required"}, nil
	}
	if err := library.Retire(ctx, deps.Pool, project, dewey, reason); err != nil {
		return LibrarySimpleResult{Error: err.Error()}, nil
	}
	return LibrarySimpleResult{OK: true, Dewey: dewey}, nil
}

// LibraryListResult wraps either a typed entries slice or an error envelope.
type LibraryListResult struct {
	Entries []library.LibraryEntry `json:"entries,omitempty"`
	Error   string                 `json:"error,omitempty"`
}

// HandleLibraryListActive implements library_list_active.
func HandleLibraryListActive(ctx context.Context, deps Deps, project string, _ json.RawMessage) (LibraryListResult, error) {
	if project == "" {
		return LibraryListResult{Error: "project is required for library actions"}, nil
	}
	entries, err := library.ListActive(ctx, deps.Pool, project)
	if err != nil {
		return LibraryListResult{Error: err.Error()}, nil
	}
	return LibraryListResult{Entries: entries}, nil
}

// LibrarySectionsResult wraps either a typed sections slice or an error envelope.
// library.ListSections returns []string (distinct section names).
type LibrarySectionsResult struct {
	Sections []string `json:"sections,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// HandleLibraryListSections implements library_list_sections.
func HandleLibraryListSections(ctx context.Context, deps Deps, project string, _ json.RawMessage) (LibrarySectionsResult, error) {
	if project == "" {
		return LibrarySectionsResult{Error: "project is required for library actions"}, nil
	}
	sections, err := library.ListSections(ctx, deps.Pool, project)
	if err != nil {
		return LibrarySectionsResult{Error: err.Error()}, nil
	}
	return LibrarySectionsResult{Sections: sections}, nil
}

// HandleLibraryListDewey implements library_list_dewey.
func HandleLibraryListDewey(ctx context.Context, deps Deps, project string, params json.RawMessage) (LibraryListDeweyResult, error) {
	if project == "" {
		return LibraryListDeweyResult{Error: "project is required for library actions"}, nil
	}
	prefix := mcpparam.String(params, "prefix")
	numbers, err := library.ListDeweyByPrefix(ctx, deps.Pool, project, prefix)
	if err != nil {
		return LibraryListDeweyResult{Error: err.Error()}, nil
	}
	return LibraryListDeweyResult{
		DeweyNumbers: numbers,
		Prefix:       prefix,
		Count:        len(numbers),
	}, nil
}

// LibraryFindResult is the discriminated-union response for library_find.
// Exactly one of KeywordResults / SemanticResults / ManifestResults is
// populated, picked by Mode. The custom MarshalJSON emits the shape the
// caller expects: `{mode, results, [qwen_fell_back]}` with results' element
// type matching the mode.
//
// Each per-mode slice is its own concrete type (no `any`), so callers
// inside Go get full type safety; the JSON wire format remains identical
// to the prior map-based form via the MarshalJSON branches below.
type LibraryFindResult struct {
	Mode            string                  `json:"-"`
	KeywordResults  []library.KeywordMatch  `json:"-"`
	SemanticResults []library.LibraryEntry  `json:"-"`
	ManifestResults []library.ManifestEntry `json:"-"`
	QwenFellBack    bool                    `json:"-"`
	Error           string                  `json:"-"`
}

// MarshalJSON emits the per-mode JSON shape. Each branch builds an inline
// typed struct so the results slice keeps its element type; no `any` and no
// map[string]any anywhere in the marshaling path.
func (r LibraryFindResult) MarshalJSON() ([]byte, error) {
	if r.Error != "" {
		return json.Marshal(struct {
			Error string `json:"error"`
		}{r.Error})
	}
	switch r.Mode {
	case "keyword":
		return json.Marshal(struct {
			Mode    string                 `json:"mode"`
			Results []library.KeywordMatch `json:"results"`
		}{r.Mode, r.KeywordResults})
	case "semantic":
		return json.Marshal(struct {
			Mode         string                 `json:"mode"`
			Results      []library.LibraryEntry `json:"results"`
			QwenFellBack bool                   `json:"qwen_fell_back,omitempty"`
		}{r.Mode, r.SemanticResults, r.QwenFellBack})
	case "manifest":
		return json.Marshal(struct {
			Mode    string                  `json:"mode"`
			Results []library.ManifestEntry `json:"results"`
		}{r.Mode, r.ManifestResults})
	}
	return json.Marshal(struct {
		Mode string `json:"mode,omitempty"`
	}{r.Mode})
}

// HandleLibraryFind implements library_find with Qwen rerank for semantic mode.
//
// Required params: mode ∈ {keyword|semantic|manifest}.
// Keyword needs `query`. Semantic / Manifest need `section`. Semantic also
// accepts free-text `query` to drive a Qwen rerank.
func HandleLibraryFind(ctx context.Context, deps Deps, project string, params json.RawMessage) (LibraryFindResult, error) {
	if project == "" {
		return LibraryFindResult{Error: "project is required for library actions"}, nil
	}
	mode := strings.ToLower(mcpparam.String(params, "mode"))
	query := mcpparam.String(params, "query")
	section := mcpparam.String(params, "section")

	switch mode {
	case "keyword":
		if query == "" {
			return LibraryFindResult{Error: "library_find keyword mode requires params.query"}, nil
		}
		matches, err := library.FindKeyword(ctx, deps.Pool, project, query)
		if err != nil {
			return LibraryFindResult{Error: err.Error()}, nil
		}
		return LibraryFindResult{Mode: "keyword", KeywordResults: matches}, nil

	case "manifest":
		if section == "" {
			return LibraryFindResult{Error: "library_find manifest mode requires params.section"}, nil
		}
		manifest, err := library.FindManifest(ctx, deps.Pool, project, section)
		if err != nil {
			return LibraryFindResult{Error: err.Error()}, nil
		}
		return LibraryFindResult{Mode: "manifest", ManifestResults: manifest}, nil

	case "semantic":
		if section == "" {
			return LibraryFindResult{Error: "library_find semantic mode requires params.section"}, nil
		}
		topK := int(mcpparam.Int64(params, "top_k", 5))
		if topK < 1 {
			topK = 1
		} else if topK > 20 {
			topK = 20
		}
		entries, err := library.FindSemantic(ctx, deps.Pool, project, section)
		if err != nil {
			return LibraryFindResult{Error: err.Error()}, nil
		}
		if len(entries) == 0 || query == "" {
			return LibraryFindResult{Mode: "semantic", SemanticResults: entries}, nil
		}
		// Qwen rerank section-filtered candidates. Prefix paths with "lib/" so
		// the retrieve-response parser doesn't strip the leading-digit dewey.
		candidates := make([]qwenretrieve.RetrieveCandidate, len(entries))
		for i, e := range entries {
			title := authorYearTitle(e)
			summary := truncateRunes(e.InvokeWhen, 160)
			c := qwenretrieve.RetrieveCandidate{Path: "lib/" + e.Dewey}
			if title != "" {
				c.Title = &title
			}
			c.Tags = append([]string(nil), e.Tags...)
			if summary != "" {
				c.Summary = &summary
			}
			candidates[i] = c
		}
		ctx := qwenctx.WithTaskID(ctx, "library-find")
		result, err := qwenretrieve.DispatchTwoPassRetrieve(
			ctx, deps.Router, query, topK, candidates,
			qwenretrieve.CorpusShapeVault, nil,
		)
		qwenFellBack := true
		if err != nil {
			obs.Logger(ctx).Warn("library_find: Qwen rerank failed; using section-filter order",
				slog.String("err", err.Error()))
		} else if !result.FellBack {
			byDewey := make(map[string]library.LibraryEntry, len(entries))
			for _, e := range entries {
				byDewey[e.Dewey] = e
			}
			reranked := make([]library.LibraryEntry, 0, len(result.RankedPaths))
			for _, p := range result.RankedPaths {
				if d := strings.TrimPrefix(p, "lib/"); d != p {
					if e, ok := byDewey[d]; ok {
						reranked = append(reranked, e)
					}
				}
			}
			if len(reranked) > 0 {
				entries = reranked
				qwenFellBack = false
			}
		}
		return LibraryFindResult{
			Mode:            "semantic",
			SemanticResults: entries,
			QwenFellBack:    qwenFellBack,
		}, nil

	default:
		return LibraryFindResult{Error: "params.mode must be keyword|semantic|manifest"}, nil
	}
}

// LibraryCrossRefResult wraps either a library.CrossRefResult or an error envelope.
type LibraryCrossRefResult struct {
	Result *library.CrossRefResult `json:"result,omitempty"`
	Error  string                  `json:"error,omitempty"`
}

// HandleLibraryCrossReference implements library_cross_reference.
func HandleLibraryCrossReference(ctx context.Context, deps Deps, project string, params json.RawMessage) (LibraryCrossRefResult, error) {
	if project == "" {
		return LibraryCrossRefResult{Error: "project is required for library actions"}, nil
	}
	dewey := mcpparam.String(params, "dewey")
	if dewey == "" {
		return LibraryCrossRefResult{Error: "params.dewey is required"}, nil
	}
	modeStr := strings.ToLower(mcpparam.String(params, "mode"))
	mode := library.CrossRefModeSection
	if modeStr == "question" {
		mode = library.CrossRefModeQuestion
	}
	result, err := library.CrossReference(ctx, deps.Pool, project, dewey, mode)
	if err != nil {
		return LibraryCrossRefResult{Error: err.Error()}, nil
	}
	return LibraryCrossRefResult{Result: &result}, nil
}

// ── Helpers ──────────────────────────────────────────────────────────

// parseLibraryEntryForAdd accepts two shapes (mirrors Rust):
//
//   - Nested: {"entry": {<full LibraryEntry>}}
//   - Flat:   top-level keys dewey, primary_author, establishes,
//     what_it_answers, invoke_when (required); year, tags, citation_raw,
//     index_pointers, citation, last_updated (optional).
func parseLibraryEntryForAdd(params json.RawMessage) (library.LibraryEntry, string) {
	if len(params) == 0 {
		return library.LibraryEntry{}, "library_add: params are required"
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(params, &m); err != nil {
		return library.LibraryEntry{}, "library_add: malformed params: " + err.Error()
	}
	// Nested form short-circuit.
	if raw, ok := m["entry"]; ok {
		var entry library.LibraryEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			return library.LibraryEntry{}, "library_add: malformed entry: " + err.Error()
		}
		return entry, ""
	}

	dewey := unmarshalString(m, "dewey")
	primaryAuthor := unmarshalString(m, "primary_author")
	establishes := unmarshalString(m, "establishes")
	whatItAnswers := unmarshalString(m, "what_it_answers")
	invokeWhen := unmarshalString(m, "invoke_when")
	if dewey == "" {
		return library.LibraryEntry{}, "library_add: dewey is required"
	}
	if primaryAuthor == "" {
		return library.LibraryEntry{}, "library_add: primary_author is required"
	}
	if establishes == "" {
		return library.LibraryEntry{}, "library_add: establishes is required"
	}
	if whatItAnswers == "" {
		return library.LibraryEntry{}, "library_add: what_it_answers is required"
	}
	if invokeWhen == "" {
		return library.LibraryEntry{}, "library_add: invoke_when is required"
	}

	citation := library.Citation{PrimaryAuthor: primaryAuthor}
	if raw, ok := m["citation"]; ok {
		// Nested form: caller passed the citation object.
		if err := json.Unmarshal(raw, &citation); err != nil {
			return library.LibraryEntry{}, "library_add: malformed citation: " + err.Error()
		}
		// Top-level primary_author always wins (matches Rust).
		citation.PrimaryAuthor = primaryAuthor
	} else if rawStr := unmarshalString(m, "citation_raw"); rawStr != "" {
		citation.Raw = rawStr
	}
	if citation.Raw == "" {
		return library.LibraryEntry{}, "library_add: citation.raw or citation_raw is required"
	}
	if raw, ok := m["year"]; ok {
		var y uint32
		if err := json.Unmarshal(raw, &y); err == nil && y > 0 {
			citation.Year = &y
		}
	}

	var tags []string
	if raw, ok := m["tags"]; ok {
		if err := json.Unmarshal(raw, &tags); err != nil {
			return library.LibraryEntry{}, "library_add: tags must be an array of strings"
		}
	}

	var pointers []library.IndexPointer
	if raw, ok := m["index_pointers"]; ok {
		if err := json.Unmarshal(raw, &pointers); err != nil {
			return library.LibraryEntry{}, "library_add: index_pointers must be an array of {section, question, role}"
		}
	}

	return library.LibraryEntry{
		Dewey:         dewey,
		Citation:      citation,
		Status:        library.EntryStatus{Type: "active"},
		Establishes:   establishes,
		WhatItAnswers: whatItAnswers,
		InvokeWhen:    invokeWhen,
		Tags:          tags,
		IndexPointers: pointers,
	}, ""
}

// parseEntryUpdate accepts {update: {<partial fields>}} or top-level partial
// fields. Returns the partial update payload + error message string.
func parseEntryUpdate(params json.RawMessage) (library.EntryUpdate, string) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(params, &m); err != nil {
		return library.EntryUpdate{}, "library_update: malformed params: " + err.Error()
	}
	updRaw, ok := m["update"]
	if !ok {
		return library.EntryUpdate{}, "library_update: params.update is required (partial fields)"
	}
	var upd library.EntryUpdate
	if err := json.Unmarshal(updRaw, &upd); err != nil {
		return library.EntryUpdate{}, "library_update: malformed update: " + err.Error()
	}
	return upd, ""
}

func unmarshalString(m map[string]json.RawMessage, key string) string {
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

func authorYearTitle(e library.LibraryEntry) string {
	if e.Citation.Year != nil {
		return fmt.Sprintf("%s (%d)", e.Citation.PrimaryAuthor, *e.Citation.Year)
	}
	return fmt.Sprintf("%s ()", e.Citation.PrimaryAuthor)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
