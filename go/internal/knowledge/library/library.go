// Package library hosts the library_entries CRUD + find + cross-reference
// operations. Mirrors knowledge_lib::library on the Rust side.
//
// The library_entries.index_pointers column stores a JSON-serialised
// []IndexPointer; serialisation happens on every read/write through this
// module. Generic taxonomy: section and role are plain strings — projects
// supply their own vocabulary, the toolkit ships no domain content.
package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"toolkit/internal/db"
)

// ── Types ─────────────────────────────────────────────────────────────

// Citation pairs the raw citation string with structured author/year fields.
type Citation struct {
	Raw           string  `json:"raw"`
	PrimaryAuthor string  `json:"primary_author"`
	Year          *uint32 `json:"year"`
}

// EntryStatus is one of Active or Retired{reason}. Serialised with a tagged
// `type` discriminator to match Rust serde:
//
//	{"type":"active"}
//	{"type":"retired","reason":"…"}
type EntryStatus struct {
	Type   string `json:"type"`
	Reason string `json:"reason,omitempty"`
}

// IndexPointer points from an entry into one (section, question, role) slot.
type IndexPointer struct {
	Section  string `json:"section"`
	Question string `json:"question"`
	Role     string `json:"role"`
}

// LibraryEntry is one row of library_entries.
type LibraryEntry struct {
	Dewey         string         `json:"dewey"`
	Citation      Citation       `json:"citation"`
	Status        EntryStatus    `json:"status"`
	Establishes   string         `json:"establishes"`
	WhatItAnswers string         `json:"what_it_answers"`
	InvokeWhen    string         `json:"invoke_when"`
	Tags          []string       `json:"tags"`
	IndexPointers []IndexPointer `json:"index_pointers"`
	LastUpdated   string         `json:"last_updated"`
}

// EntryUpdate is a partial update payload. nil fields are skipped.
type EntryUpdate struct {
	Citation      *Citation       `json:"citation,omitempty"`
	Status        *EntryStatus    `json:"status,omitempty"`
	Establishes   *string         `json:"establishes,omitempty"`
	WhatItAnswers *string         `json:"what_it_answers,omitempty"`
	InvokeWhen    *string         `json:"invoke_when,omitempty"`
	Tags          *[]string       `json:"tags,omitempty"`
	IndexPointers *[]IndexPointer `json:"index_pointers,omitempty"`
}

// UpdateResult names the dewey of the updated entry and the fields changed.
type UpdateResult struct {
	Dewey         string   `json:"dewey"`
	FieldsChanged []string `json:"fields_changed"`
}

// KeywordMatch is one library_find keyword-mode hit.
type KeywordMatch struct {
	Dewey         string  `json:"dewey"`
	PrimaryAuthor string  `json:"primary_author"`
	Year          *uint32 `json:"year"`
	Establishes   string  `json:"establishes"`
	MatchedField  string  `json:"matched_field"`
}

// ManifestEntry is one library_find manifest-mode hit.
type ManifestEntry struct {
	Dewey                string  `json:"dewey"`
	PrimaryAuthor        string  `json:"primary_author"`
	Year                 *uint32 `json:"year"`
	WhatItAnswersSummary string  `json:"what_it_answers_summary"`
}

// CrossRefMode selects how cross-reference grouping happens. Section shares any
// section; Question shares any (section, question) pair.
type CrossRefMode int

const (
	// CrossRefModeSection groups by shared section.
	CrossRefModeSection CrossRefMode = iota
	// CrossRefModeQuestion groups by shared (section, question) pair.
	CrossRefModeQuestion
)

// CrossRefEntry is one cross-reference hit.
type CrossRefEntry struct {
	Dewey         string  `json:"dewey"`
	PrimaryAuthor string  `json:"primary_author"`
	Year          *uint32 `json:"year"`
	Establishes   string  `json:"establishes"`
	Role          *string `json:"role,omitempty"`
}

// CrossRefResult carries the target entry plus grouped neighbours.
type CrossRefResult struct {
	Dewey      string                     `json:"dewey"`
	BySection  map[string][]CrossRefEntry `json:"by_section"`
	ByQuestion map[string][]CrossRefEntry `json:"by_question"`
}

// ── Errors ────────────────────────────────────────────────────────────

var (
	// ErrAlreadyExists fires when a (project_id, dewey) pair is taken.
	ErrAlreadyExists = errors.New("library entry already exists")
	// ErrNotFound fires when a get/update/retire target doesn't exist.
	ErrNotFound = errors.New("library entry not found")
	// ErrValidation fires for required-field / shape failures.
	ErrValidation = errors.New("library validation")
	// ErrInvalidDewey fires when a dewey string fails ValidateDewey.
	ErrInvalidDewey = errors.New("invalid dewey number")
)

// ── DeweyNumber validation ────────────────────────────────────────────

// ValidateDewey enforces the dewey-number shape: ≥3 leading digits, optional
// `.<digits>` decimal. Mirrors DeweyNumber::new.
func ValidateDewey(s string) error {
	if len(s) < 3 {
		return fmt.Errorf("%w: too short: %q", ErrInvalidDewey, s)
	}
	for i := 0; i < 3; i++ {
		if s[i] < '0' || s[i] > '9' {
			return fmt.Errorf("%w: non-digit prefix: %q", ErrInvalidDewey, s)
		}
	}
	if len(s) == 3 {
		return nil
	}
	if s[3] != '.' {
		return fmt.Errorf("%w: missing decimal point: %q", ErrInvalidDewey, s)
	}
	if len(s) == 4 {
		return fmt.Errorf("%w: empty decimal: %q", ErrInvalidDewey, s)
	}
	for i := 4; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return fmt.Errorf("%w: non-digit decimal: %q", ErrInvalidDewey, s)
		}
	}
	return nil
}

// ── CRUD ──────────────────────────────────────────────────────────────

// Add inserts a new library entry. Validates required fields and refuses to
// overwrite an existing (project_id, dewey) pair.
func Add(ctx context.Context, pool *db.Pool, projectID string, entry LibraryEntry) error {
	if err := validateEntry(entry); err != nil {
		return err
	}
	var existing int64
	row := pool.DB().QueryRowContext(ctx,
		`SELECT id FROM library_entries WHERE project_id = ? AND dewey = ?`,
		projectID, entry.Dewey)
	switch err := row.Scan(&existing); {
	case err == nil:
		return fmt.Errorf("%w: %s@%s", ErrAlreadyExists, entry.Dewey, projectID)
	case errors.Is(err, sql.ErrNoRows):
		// fall through
	default:
		return fmt.Errorf("library add: existence check: %w", err)
	}

	pointersJSON, err := json.Marshal(entry.IndexPointers)
	if err != nil {
		return fmt.Errorf("library add: marshal pointers: %w", err)
	}
	tagsJSON, err := json.Marshal(entry.Tags)
	if err != nil {
		return fmt.Errorf("library add: marshal tags: %w", err)
	}
	status := entry.Status.Type
	if status == "" {
		status = "active"
	}
	// entry.Citation.Year is already *uint32; database/sql serialises a nil
	// pointer as SQL NULL and dereferences a non-nil one to its underlying
	// value — no wrapper needed.
	year := entry.Citation.Year
	return pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO library_entries
				(project_id, dewey, primary_author, year, citation,
				 establishes, what_it_answers, invoke_when, tags,
				 status, index_pointers)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			projectID, entry.Dewey, entry.Citation.PrimaryAuthor, year, entry.Citation.Raw,
			entry.Establishes, entry.WhatItAnswers, entry.InvokeWhen, string(tagsJSON),
			status, string(pointersJSON),
		)
		return err
	})
}

// Get returns one entry by dewey, or ErrNotFound. Dewey validation failures
// silently return ErrNotFound (matching Rust: invalid dewey can't exist).
func Get(ctx context.Context, pool *db.Pool, projectID, dewey string) (LibraryEntry, error) {
	if err := ValidateDewey(dewey); err != nil {
		return LibraryEntry{}, fmt.Errorf("%w: %s", ErrNotFound, dewey)
	}
	row := pool.DB().QueryRowContext(ctx,
		`SELECT dewey, primary_author, year, citation, establishes,
		        what_it_answers, invoke_when, tags, status, index_pointers,
		        updated_at
		 FROM library_entries WHERE project_id = ? AND dewey = ?`,
		projectID, dewey)
	entry, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return LibraryEntry{}, fmt.Errorf("%w: %s", ErrNotFound, dewey)
	}
	if err != nil {
		return LibraryEntry{}, fmt.Errorf("library get %s: %w", dewey, err)
	}
	return entry, nil
}

// Update applies a partial update. Dewey is immutable; passing a new dewey is
// out of contract.
func Update(ctx context.Context, pool *db.Pool, projectID, dewey string, upd EntryUpdate) (UpdateResult, error) {
	entry, err := Get(ctx, pool, projectID, dewey)
	if err != nil {
		return UpdateResult{}, err
	}
	var changed []string
	if upd.Citation != nil {
		entry.Citation = *upd.Citation
		changed = append(changed, "citation")
	}
	if upd.Status != nil {
		entry.Status = *upd.Status
		changed = append(changed, "status")
	}
	if upd.Establishes != nil {
		entry.Establishes = *upd.Establishes
		changed = append(changed, "establishes")
	}
	if upd.WhatItAnswers != nil {
		entry.WhatItAnswers = *upd.WhatItAnswers
		changed = append(changed, "what_it_answers")
	}
	if upd.InvokeWhen != nil {
		entry.InvokeWhen = *upd.InvokeWhen
		changed = append(changed, "invoke_when")
	}
	if upd.Tags != nil {
		entry.Tags = *upd.Tags
		changed = append(changed, "tags")
	}
	if upd.IndexPointers != nil {
		for i, ptr := range *upd.IndexPointers {
			if strings.TrimSpace(ptr.Question) == "" {
				return UpdateResult{}, fmt.Errorf("%w: index_pointers[%d].question must not be empty", ErrValidation, i)
			}
		}
		entry.IndexPointers = *upd.IndexPointers
		changed = append(changed, "index_pointers")
	}

	status := entry.Status.Type
	if status == "" {
		status = "active"
	}
	pointersJSON, err := json.Marshal(entry.IndexPointers)
	if err != nil {
		return UpdateResult{}, fmt.Errorf("library update: marshal pointers: %w", err)
	}
	tagsJSON, err := json.Marshal(entry.Tags)
	if err != nil {
		return UpdateResult{}, fmt.Errorf("library update: marshal tags: %w", err)
	}
	// entry.Citation.Year is already *uint32; database/sql serialises a nil
	// pointer as SQL NULL and dereferences a non-nil one to its underlying
	// value — no wrapper needed.
	year := entry.Citation.Year
	err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE library_entries SET
				primary_author = ?, year = ?, citation = ?,
				establishes = ?, what_it_answers = ?, invoke_when = ?,
				tags = ?, status = ?, index_pointers = ?,
				updated_at = datetime('now')
			 WHERE project_id = ? AND dewey = ?`,
			entry.Citation.PrimaryAuthor, year, entry.Citation.Raw,
			entry.Establishes, entry.WhatItAnswers, entry.InvokeWhen,
			string(tagsJSON), status, string(pointersJSON),
			projectID, dewey,
		)
		return err
	})
	if err != nil {
		return UpdateResult{}, fmt.Errorf("library update: %w", err)
	}
	return UpdateResult{Dewey: dewey, FieldsChanged: changed}, nil
}

// Retire flips an entry's status to retired{reason}. Returns ErrValidation if
// the entry is already retired (mirrors Rust).
func Retire(ctx context.Context, pool *db.Pool, projectID, dewey, reason string) error {
	entry, err := Get(ctx, pool, projectID, dewey)
	if err != nil {
		return err
	}
	if entry.Status.Type == "retired" {
		return fmt.Errorf("%w: library entry %s already retired", ErrValidation, dewey)
	}
	_, err = Update(ctx, pool, projectID, dewey, EntryUpdate{
		Status: &EntryStatus{Type: "retired", Reason: reason},
	})
	return err
}

// ListActive returns all active entries for projectID, sorted by dewey.
func ListActive(ctx context.Context, pool *db.Pool, projectID string) ([]LibraryEntry, error) {
	rows, err := pool.DB().QueryContext(ctx,
		`SELECT dewey, primary_author, year, citation, establishes,
		        what_it_answers, invoke_when, tags, status, index_pointers,
		        updated_at
		 FROM library_entries
		 WHERE project_id = ? AND status = 'active'
		 ORDER BY dewey`,
		projectID)
	if err != nil {
		return nil, fmt.Errorf("library list_active: %w", err)
	}
	defer rows.Close()
	// Non-nil zero-length slice so JSON marshals as `[]`, not `null`.
	out := []LibraryEntry{}
	for rows.Next() {
		entry, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("library list_active: scan: %w", err)
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

// ListSections returns every distinct section name across active entries'
// index_pointers, sorted alphabetically. Lightweight alternative to
// ListActive for agents that need section names before find.
func ListSections(ctx context.Context, pool *db.Pool, projectID string) ([]string, error) {
	entries, err := ListActive(ctx, pool, projectID)
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{})
	for _, e := range entries {
		for _, p := range e.IndexPointers {
			set[p.Section] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// ListDeweyByPrefix returns active deweys starting with prefix, sorted.
func ListDeweyByPrefix(ctx context.Context, pool *db.Pool, projectID, prefix string) ([]string, error) {
	pattern := prefix + "%"
	rows, err := pool.DB().QueryContext(ctx,
		`SELECT dewey FROM library_entries
		 WHERE project_id = ? AND dewey LIKE ? AND status = 'active'
		 ORDER BY dewey`,
		projectID, pattern)
	if err != nil {
		return nil, fmt.Errorf("library list_dewey: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ── Find ──────────────────────────────────────────────────────────────

// FindKeyword scans active entries for case-insensitive substring matches
// against (in priority order): establishes, invoke_when, primary_author, tags,
// index_pointer.question. Returns one KeywordMatch per matching entry.
func FindKeyword(ctx context.Context, pool *db.Pool, projectID, q string) ([]KeywordMatch, error) {
	entries, err := ListActive(ctx, pool, projectID)
	if err != nil {
		return nil, err
	}
	qLower := strings.ToLower(q)
	out := []KeywordMatch{}
	for _, e := range entries {
		field := keywordMatchField(e, qLower)
		if field == "" {
			continue
		}
		out = append(out, KeywordMatch{
			Dewey:         e.Dewey,
			PrimaryAuthor: e.Citation.PrimaryAuthor,
			Year:          e.Citation.Year,
			Establishes:   e.Establishes,
			MatchedField:  field,
		})
	}
	return out, nil
}

func keywordMatchField(e LibraryEntry, qLower string) string {
	if strings.Contains(strings.ToLower(e.Establishes), qLower) {
		return "establishes"
	}
	if strings.Contains(strings.ToLower(e.InvokeWhen), qLower) {
		return "invoke_when"
	}
	if strings.Contains(strings.ToLower(e.Citation.PrimaryAuthor), qLower) {
		return "primary_author"
	}
	for _, t := range e.Tags {
		if strings.Contains(strings.ToLower(t), qLower) {
			return "tags"
		}
	}
	for _, ptr := range e.IndexPointers {
		if strings.Contains(strings.ToLower(ptr.Question), qLower) {
			return "index_pointer.question"
		}
	}
	return ""
}

// FindSemantic returns active entries with at least one index_pointer in the
// given section.
func FindSemantic(ctx context.Context, pool *db.Pool, projectID, section string) ([]LibraryEntry, error) {
	entries, err := ListActive(ctx, pool, projectID)
	if err != nil {
		return nil, err
	}
	out := []LibraryEntry{}
	for _, e := range entries {
		for _, p := range e.IndexPointers {
			if p.Section == section {
				out = append(out, e)
				break
			}
		}
	}
	return out, nil
}

// FindManifest returns compact manifest views of active entries with at least
// one index_pointer in the given section.
func FindManifest(ctx context.Context, pool *db.Pool, projectID, section string) ([]ManifestEntry, error) {
	entries, err := FindSemantic(ctx, pool, projectID, section)
	if err != nil {
		return nil, err
	}
	out := make([]ManifestEntry, len(entries))
	for i, e := range entries {
		out[i] = ManifestEntry{
			Dewey:                e.Dewey,
			PrimaryAuthor:        e.Citation.PrimaryAuthor,
			Year:                 e.Citation.Year,
			WhatItAnswersSummary: summarizeFirstLine(e.WhatItAnswers, 160),
		}
	}
	return out, nil
}

func summarizeFirstLine(text string, maxChars int) string {
	for _, line := range strings.Split(text, "\n") {
		first := strings.TrimSpace(line)
		if first == "" {
			continue
		}
		runes := []rune(first)
		if len(runes) <= maxChars {
			return first
		}
		return string(runes[:maxChars]) + "…"
	}
	return ""
}

// ── Cross-reference ──────────────────────────────────────────────────

// CrossReference returns other active entries that share at least one
// section (Section mode) or one (section, question) pair (Question mode)
// with the target dewey.
func CrossReference(ctx context.Context, pool *db.Pool, projectID, dewey string, mode CrossRefMode) (CrossRefResult, error) {
	target, err := Get(ctx, pool, projectID, dewey)
	if err != nil {
		return CrossRefResult{}, err
	}
	entries, err := ListActive(ctx, pool, projectID)
	if err != nil {
		return CrossRefResult{}, err
	}
	result := CrossRefResult{
		Dewey:      target.Dewey,
		BySection:  map[string][]CrossRefEntry{},
		ByQuestion: map[string][]CrossRefEntry{},
	}

	switch mode {
	case CrossRefModeSection:
		targetSections := make(map[string]struct{}, len(target.IndexPointers))
		for _, p := range target.IndexPointers {
			targetSections[p.Section] = struct{}{}
		}
		for _, e := range entries {
			if e.Dewey == target.Dewey {
				continue
			}
			cross := CrossRefEntry{
				Dewey:         e.Dewey,
				PrimaryAuthor: e.Citation.PrimaryAuthor,
				Year:          e.Citation.Year,
				Establishes:   e.Establishes,
			}
			seen := make(map[string]struct{})
			for _, p := range e.IndexPointers {
				if _, ok := targetSections[p.Section]; !ok {
					continue
				}
				if _, dup := seen[p.Section]; dup {
					continue
				}
				seen[p.Section] = struct{}{}
				result.BySection[p.Section] = append(result.BySection[p.Section], cross)
			}
		}
	case CrossRefModeQuestion:
		type pair struct{ section, question string }
		targetPairs := make(map[pair]struct{}, len(target.IndexPointers))
		for _, p := range target.IndexPointers {
			targetPairs[pair{p.Section, p.Question}] = struct{}{}
		}
		for _, e := range entries {
			if e.Dewey == target.Dewey {
				continue
			}
			seen := make(map[string]struct{})
			for _, p := range e.IndexPointers {
				if _, ok := targetPairs[pair{p.Section, p.Question}]; !ok {
					continue
				}
				key := p.Section + "::" + p.Question
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}
				role := p.Role
				result.ByQuestion[key] = append(result.ByQuestion[key], CrossRefEntry{
					Dewey:         e.Dewey,
					PrimaryAuthor: e.Citation.PrimaryAuthor,
					Year:          e.Citation.Year,
					Establishes:   e.Establishes,
					Role:          &role,
				})
			}
		}
	}
	return result, nil
}

// ── Row scanning + validation helpers ────────────────────────────────

func scanEntry(s db.Scanner) (LibraryEntry, error) {
	var (
		dewey, primaryAuthor, citation, establishes string
		whatItAnswers, invokeWhen, tagsJSON         string
		status, pointersJSON, updatedAt             string
		year                                        sql.NullInt64
	)
	if err := s.Scan(&dewey, &primaryAuthor, &year, &citation, &establishes,
		&whatItAnswers, &invokeWhen, &tagsJSON, &status, &pointersJSON, &updatedAt); err != nil {
		return LibraryEntry{}, err
	}
	if err := ValidateDewey(dewey); err != nil {
		return LibraryEntry{}, fmt.Errorf("library entry has invalid dewey: %w", err)
	}
	var pointers []IndexPointer
	if pointersJSON != "" {
		if err := json.Unmarshal([]byte(pointersJSON), &pointers); err != nil {
			return LibraryEntry{}, fmt.Errorf("decode index_pointers: %w", err)
		}
	}
	var tags []string
	if tagsJSON != "" {
		if err := json.Unmarshal([]byte(tagsJSON), &tags); err != nil {
			// Mirror Rust: malformed tags JSON falls back to empty rather than
			// surfacing the error.
			tags = nil
		}
	}
	es := EntryStatus{Type: "active"}
	switch status {
	case "active":
		es = EntryStatus{Type: "active"}
	case "retired":
		es = EntryStatus{Type: "retired"}
	default:
		es = EntryStatus{Type: "retired", Reason: "unknown status '" + status + "'"}
	}
	var yearPtr *uint32
	if year.Valid {
		v := uint32(year.Int64)
		yearPtr = &v
	}
	return LibraryEntry{
		Dewey: dewey,
		Citation: Citation{
			Raw:           citation,
			PrimaryAuthor: primaryAuthor,
			Year:          yearPtr,
		},
		Status:        es,
		Establishes:   establishes,
		WhatItAnswers: whatItAnswers,
		InvokeWhen:    invokeWhen,
		Tags:          tags,
		IndexPointers: pointers,
		LastUpdated:   updatedAt,
	}, nil
}

func validateEntry(e LibraryEntry) error {
	if err := ValidateDewey(e.Dewey); err != nil {
		return err
	}
	if strings.TrimSpace(e.Citation.Raw) == "" {
		return fmt.Errorf("%w: citation.raw is required", ErrValidation)
	}
	if strings.TrimSpace(e.Citation.PrimaryAuthor) == "" {
		return fmt.Errorf("%w: citation.primary_author is required", ErrValidation)
	}
	for i, p := range e.IndexPointers {
		if strings.TrimSpace(p.Question) == "" {
			return fmt.Errorf("%w: index_pointers[%d].question is required", ErrValidation, i)
		}
	}
	return nil
}
