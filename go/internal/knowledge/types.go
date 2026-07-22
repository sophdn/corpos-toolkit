package knowledge

import (
	"toolkit/internal/knowledge/kiwix"
	"toolkit/internal/knowledge/library"
	"toolkit/internal/knowledge/vault"
)

// This file consolidates the typed response shapes for every knowledge
// surface handler. Each Result type uses `omitempty` on conditional fields
// so the JSON output matches the prior map[string]any shapes exactly —
// fields that were absent in the map-based form remain absent here.
//
// The pattern: one Result struct per handler with both success fields and
// error envelope fields. The handler populates one subset; `omitempty`
// drops the rest at serialise time. This is the Go-idiomatic alternative
// to sum types — works cleanly with json.Marshal and reads as a single
// type at the call site.

// ── Vault surface ────────────────────────────────────────────────────

// VaultResultEntry is one row in VaultSearchResult.Results.
type VaultResultEntry struct {
	Path  string   `json:"path"`
	Title string   `json:"title"`
	Tags  []string `json:"tags"`
	Rank  int      `json:"rank"`
}

// VaultSearchResult is the response shape for vault_search.
type VaultSearchResult struct {
	// Success path
	Results       []VaultResultEntry `json:"results,omitempty"`
	VaultRoot     string             `json:"vault_root,omitempty"`
	VaultSize     int                `json:"vault_size,omitempty"`
	LatencyMS     int64              `json:"latency_ms,omitempty"`
	InputTokens   *int64             `json:"input_tokens,omitempty"`
	OutputTokens  *int64             `json:"output_tokens,omitempty"`
	Pass2FellBack bool               `json:"pass2_fell_back,omitempty"`
	// Optional truncation fields (populated only when MaxVaultCandidates was hit).
	// CandidatesTruncated is the boolean signal; CandidatesUsed is the
	// number of notes that reached Qwen rerank; TruncatedNoteCount is the
	// number of notes the keyword prefilter excluded (vault_size −
	// candidates_used) — surfaced as a structured field so agents can act
	// on it without parsing the candidates_hint prose (bug 1324).
	CandidatesTruncated bool   `json:"candidates_truncated,omitempty"`
	CandidatesUsed      int    `json:"candidates_used,omitempty"`
	TruncatedNoteCount  int    `json:"truncated_note_count,omitempty"`
	CandidatesHint      string `json:"candidates_hint,omitempty"`
	// Error envelope fields
	Error     string `json:"error,omitempty"`
	Transient bool   `json:"transient,omitempty"`
	Hint      string `json:"hint,omitempty"`
}

// VaultReadResult is the response shape for vault_read. Frontmatter is a
// typed vault.Frontmatter struct (nil pointer when the note has no
// frontmatter); fields are observed-vault conventions documented on the
// vault.Frontmatter type.
type VaultReadResult struct {
	// Success path
	Path               string             `json:"path,omitempty"`
	Frontmatter        *vault.Frontmatter `json:"frontmatter,omitempty"`
	Content            string             `json:"content,omitempty"`
	EditHint           string             `json:"edit_hint,omitempty"`
	FrontmatterWarning string             `json:"frontmatter_warning,omitempty"`
	// Error envelope
	Error string `json:"error,omitempty"`
	Hint  string `json:"hint,omitempty"`
}

// ── Kiwix surface ────────────────────────────────────────────────────

// KiwixSearchResult is the response shape for kiwix_search.
type KiwixSearchResult struct {
	// Success path
	Hits         []kiwix.SearchHit `json:"hits,omitempty"`
	QwenFellBack bool              `json:"qwen_fell_back,omitempty"`
	HitsIn       int               `json:"hits_in,omitempty"`
	HitsOut      int               `json:"hits_out,omitempty"`
	// Error envelope
	Error string `json:"error,omitempty"`
}

// KiwixListBooksResult is the response shape for kiwix_list_books.
type KiwixListBooksResult struct {
	// Success path
	Items         []kiwix.BookInfo `json:"items,omitempty"`
	TotalCount    int              `json:"total_count,omitempty"`
	ReturnedCount int              `json:"returned_count,omitempty"`
	Filter        string           `json:"filter,omitempty"`
	Note          string           `json:"note,omitempty"`
	// Error envelope
	Error string `json:"error,omitempty"`
}

// ── Library surface ──────────────────────────────────────────────────

// LibrarySimpleResult is the response for library_add / library_retire —
// both return {ok, dewey} on success or {error, …} on failure.
type LibrarySimpleResult struct {
	OK    bool   `json:"ok,omitempty"`
	Dewey string `json:"dewey,omitempty"`
	Error string `json:"error,omitempty"`
}

// LibraryGetResult is the response for library_get. On hit: Entry populated.
// On miss: Error="not_found" + Dewey set. On other failures: Error only.
type LibraryGetResult struct {
	Entry *library.LibraryEntry `json:"entry,omitempty"`
	Error string                `json:"error,omitempty"`
	Dewey string                `json:"dewey,omitempty"`
}

// LibraryListDeweyResult is the response for library_list_dewey.
type LibraryListDeweyResult struct {
	DeweyNumbers []string `json:"dewey_numbers,omitempty"`
	Prefix       string   `json:"prefix,omitempty"`
	Count        int      `json:"count,omitempty"`
	Error        string   `json:"error,omitempty"`
}

// LibrarySemanticResult is the response for library_find semantic mode.
type LibrarySemanticResult struct {
	Mode         string                 `json:"mode,omitempty"`
	Results      []library.LibraryEntry `json:"results,omitempty"`
	QwenFellBack bool                   `json:"qwen_fell_back,omitempty"`
	Error        string                 `json:"error,omitempty"`
}

// ── Knowledge unified surface ────────────────────────────────────────

// KnowledgePointerResult is one row in KnowledgeSearchResult.Results.
type KnowledgePointerResult struct {
	ID                    int64    `json:"id"`
	SourceType            string   `json:"source_type"`
	SourceRef             string   `json:"source_ref"`
	Question              string   `json:"question"`
	InvokeWhen            string   `json:"invoke_when"`
	QualityScore          *float64 `json:"quality_score"`
	UsageCount            int64    `json:"usage_count"`
	NegativeFeedbackCount int64    `json:"negative_feedback_count"`
}

// KnowledgeSearchResult is the response shape for knowledge_search.
type KnowledgeSearchResult struct {
	// Success path
	Results      []KnowledgePointerResult `json:"results"`
	ResultsCount int                      `json:"results_count"`
	Query        string                   `json:"query,omitempty"`
	QwenFellBack bool                     `json:"qwen_fell_back,omitempty"`
	// Error envelope
	Error string `json:"error,omitempty"`
}

// KnowledgeReportMissResult is the response shape for knowledge_report_miss.
type KnowledgeReportMissResult struct {
	OK        bool   `json:"ok,omitempty"`
	PointerID int64  `json:"pointer_id,omitempty"`
	Error     string `json:"error,omitempty"`
}
