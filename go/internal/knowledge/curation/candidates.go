package curation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/knowledge/pointers"
)

// ErrCandidateNotFound is returned when no curation_candidate matches an
// id, or when an operation requires status='pending' and the row exists
// in a different state. Disambiguation lives in the error's wrapped
// message; callers that need pattern-matching use errors.Is.
var ErrCandidateNotFound = errors.New("curation candidate not found or not in pending state")

// ErrInvalidCandidate is returned by AddCandidate when validation rules
// reject the input (empty question/invoke_when/description, invalid
// origin, empty source_ref). Same shape as the Rust validation in
// crates/knowledge-shared/src/pointers.rs:add_curation_candidate.
var ErrInvalidCandidate = errors.New("curation candidate invalid")

// validOrigins mirrors VALID_ORIGINS in the Rust source.
var validOrigins = map[string]struct{}{
	"zero_result_gap": {},
	"task_handoff":    {},
	"session_mining":  {},
}

// CandidateInsert is the input shape for AddCandidate. Mirror of the
// Rust CandidateInsert struct minus the row-managed fields (id, status,
// created_at, promoted_*).
type CandidateInsert struct {
	ProjectID    string
	SourceType   string
	SourceRef    string
	Question     string
	InvokeWhen   string
	Description  string
	Tags         []string
	QualityScore *float64
	Origin       string
	OriginRef    *string
	ExpiresAt    *string // RFC3339 / SQLite-datetime string; nil = no expiry (until lifecycle bug fix)
}

// ListFilter is the query shape for ListPending. Empty fields are
// treated as "no constraint" — empty ProjectID lists across projects;
// Limit <= 0 defaults to 50.
type ListFilter struct {
	ProjectID    string
	Origin       string
	UnscoredOnly bool
	Limit        int
}

// ListPending returns candidates matching the filter, status='pending',
// ordered by quality_score DESC (NULLs last), created_at ASC. The
// quality_score-first ordering matches the Rust list_curation_candidates
// so existing review patterns transfer.
func ListPending(ctx context.Context, pool *db.Pool, f ListFilter) ([]Candidate, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}

	where := []string{"status = 'pending'"}
	args := db.NewArgs()
	if f.ProjectID != "" {
		where = append(where, "project_id = ?")
		args.AddString(f.ProjectID)
	}
	if f.Origin != "" {
		where = append(where, "origin = ?")
		args.AddString(f.Origin)
	}
	if f.UnscoredOnly {
		where = append(where, "quality_score IS NULL")
	}

	query := fmt.Sprintf(`SELECT id, project_id, source_type, source_ref, question,
                                  invoke_when, description, tags, quality_score, origin,
                                  origin_ref, promoted_automatically, promoted_at,
                                  expires_at, status, created_at
                             FROM curation_candidates
                            WHERE %s
                         ORDER BY quality_score DESC NULLS LAST, created_at ASC
                            LIMIT ?`, strings.Join(where, " AND "))
	args.AddInt64(int64(limit))

	rows, err := pool.DB().QueryContext(ctx, query, args.Slice()...)
	if err != nil {
		return nil, fmt.Errorf("list_pending: %w", err)
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		c, err := scanCandidate(rows)
		if err != nil {
			return nil, fmt.Errorf("list_pending scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ReadCandidate returns the candidate row by id, regardless of status.
// Returns ErrCandidateNotFound if no row matches (no separate "wrong
// status" error — callers that care check c.Status).
func ReadCandidate(ctx context.Context, pool *db.Pool, id int64) (Candidate, error) {
	row := pool.DB().QueryRowContext(ctx,
		`SELECT id, project_id, source_type, source_ref, question,
		        invoke_when, description, tags, quality_score, origin,
		        origin_ref, promoted_automatically, promoted_at,
		        expires_at, status, created_at
		   FROM curation_candidates
		  WHERE id = ?`, id)
	c, err := scanCandidate(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Candidate{}, fmt.Errorf("%w: id=%d", ErrCandidateNotFound, id)
	}
	if err != nil {
		return Candidate{}, fmt.Errorf("read_candidate: %w", err)
	}
	return c, nil
}

// AddCandidate inserts a new candidate after validating its content.
// Mirrors crates/knowledge-shared/src/pointers.rs:add_curation_candidate
// validation rules: question, invoke_when, description must be
// non-empty after trim; origin must be in validOrigins; source_ref
// non-empty.
func AddCandidate(ctx context.Context, pool *db.Pool, ins CandidateInsert) (int64, error) {
	if strings.TrimSpace(ins.Question) == "" {
		return 0, fmt.Errorf("%w: question must not be empty", ErrInvalidCandidate)
	}
	if strings.TrimSpace(ins.InvokeWhen) == "" {
		return 0, fmt.Errorf("%w: invoke_when must not be empty", ErrInvalidCandidate)
	}
	if strings.TrimSpace(ins.Description) == "" {
		return 0, fmt.Errorf("%w: description must not be empty", ErrInvalidCandidate)
	}
	if _, ok := validOrigins[ins.Origin]; !ok {
		return 0, fmt.Errorf("%w: origin %q not in {zero_result_gap, task_handoff, session_mining}",
			ErrInvalidCandidate, ins.Origin)
	}
	if strings.TrimSpace(ins.SourceRef) == "" {
		return 0, fmt.Errorf("%w: source_ref must not be empty", ErrInvalidCandidate)
	}

	tagsJSON, err := json.Marshal(ins.Tags)
	if err != nil {
		return 0, fmt.Errorf("add_candidate marshal tags: %w", err)
	}

	var id int64
	err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`INSERT INTO curation_candidates
			    (project_id, source_type, source_ref, question, invoke_when,
			     description, tags, quality_score, origin, origin_ref, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 RETURNING id`,
			ins.ProjectID, ins.SourceType, ins.SourceRef, ins.Question, ins.InvokeWhen,
			ins.Description, string(tagsJSON), ins.QualityScore, ins.Origin,
			ins.OriginRef, ins.ExpiresAt,
		).Scan(&id)
	})
	if err != nil {
		return 0, fmt.Errorf("add_candidate insert: %w", err)
	}
	return id, nil
}

// UpdateCandidateScoring is the NEW path the rescore pass needs (T7).
// Refuses if status != 'pending' — rescoring a promoted/rejected/expired
// row is a programmer error, not a runtime fallback. Returns
// ErrCandidateNotFound when zero rows are affected.
//
// Description is overwritten if non-empty in meta, preserved otherwise
// (allows Extract to fail-soft on description without erasing the
// existing one — matches the lenient description handling in
// ParseExtraction).
func UpdateCandidateScoring(
	ctx context.Context, pool *db.Pool, id int64,
	meta ExtractedMeta, qualityScore float64,
) error {
	if strings.TrimSpace(meta.Question) == "" {
		return fmt.Errorf("%w: question must not be empty", ErrInvalidCandidate)
	}
	if strings.TrimSpace(meta.InvokeWhen) == "" {
		return fmt.Errorf("%w: invoke_when must not be empty", ErrInvalidCandidate)
	}

	desc := meta.Description // empty allowed
	return pool.WithWrite(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE curation_candidates
			    SET question = ?, invoke_when = ?,
			        description = CASE WHEN ? = '' THEN description ELSE ? END,
			        quality_score = ?
			  WHERE id = ? AND status = 'pending'`,
			meta.Question, meta.InvokeWhen, desc, desc, qualityScore, id)
		if err != nil {
			return fmt.Errorf("update_scoring: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("update_scoring rows_affected: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("%w: id=%d (not pending or absent)", ErrCandidateNotFound, id)
		}
		return nil
	})
}

// PromoteCandidate creates a knowledge_pointer row from the candidate
// and marks the candidate status='promoted'. Mirror of the Rust
// promote_candidate. Returns the new pointer id.
//
// Status precondition: candidate must be 'pending'. ErrCandidateNotFound
// on any other state (matches the Rust .ok_or_else NotFound shape).
func PromoteCandidate(
	ctx context.Context, pool *db.Pool, id int64, promotedAutomatically bool,
) (int64, error) {
	cand, err := readPendingForPromote(ctx, pool, id)
	if err != nil {
		return 0, err
	}

	// Insert pointer outside the write txn used to mark status='promoted'
	// because pointers.Insert opens its own WithWrite. Two sequential
	// transactions: a transient inconsistency (pointer exists but
	// candidate still shows 'pending') is benign — subsequent reads see
	// either both pre or both post; the rescore pass tolerates seeing
	// the freshly-inserted pointer.
	description := cand.Description
	var descPtr *string
	if description != "" {
		descPtr = &description
	}
	pointerID, err := pointers.Insert(ctx, pool, pointers.KnowledgePointer{
		ProjectID:    cand.ProjectID,
		SourceType:   cand.SourceType,
		SourceRef:    cand.SourceRef,
		Question:     cand.Question,
		InvokeWhen:   cand.InvokeWhen,
		Description:  descPtr,
		Tags:         cand.Tags,
		QualityScore: cand.QualityScore,
	})
	if err != nil {
		return 0, fmt.Errorf("promote_candidate insert pointer: %w", err)
	}

	err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE curation_candidates
			    SET status = 'promoted',
			        promoted_at = datetime('now'),
			        promoted_automatically = ?
			  WHERE id = ?`,
			boolToInt(promotedAutomatically), id)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("promote_candidate mark promoted: %w", err)
	}
	return pointerID, nil
}

// RejectCandidate is the NEW path the MCP curation_reject action (T9)
// needs. Sets status='rejected' and stores the reason. Refuses if
// status != 'pending'. Reason must be non-empty — the MCP action
// surface enforces this at the boundary, this is the second line of
// defense.
func RejectCandidate(ctx context.Context, pool *db.Pool, id int64, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: reject reason must not be empty", ErrInvalidCandidate)
	}

	return pool.WithWrite(ctx, func(tx *sql.Tx) error {
		// Reason is stored by appending a marker to the tags JSON. The
		// curation_candidates schema doesn't have a dedicated
		// resolution_note column today (the migration shipped before
		// reject was a first-class state); piggybacking on tags keeps
		// us schema-compatible and avoids a migration. A future schema
		// pass can promote this to a dedicated column without changing
		// the public API.
		res, err := tx.ExecContext(ctx,
			`UPDATE curation_candidates
			    SET status = 'rejected',
			        tags = json_insert(COALESCE(tags, '[]'), '$[#]', ?)
			  WHERE id = ? AND status = 'pending'`,
			"rejected_reason: "+reason, id)
		if err != nil {
			return fmt.Errorf("reject_candidate: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("reject_candidate rows_affected: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("%w: id=%d (not pending or absent)", ErrCandidateNotFound, id)
		}
		return nil
	})
}

// PointerExistsForSourceRef returns the id of an active knowledge_pointer
// matching (project_id, source_type, source_ref), or 0 if none exists.
// Used by discovery passes to pre-check before attempting PromoteCandidate
// — avoids the UNIQUE-constraint failure mode bug
// curate-rescore-no-precheck-for-existing-pointer documented during the
// T13 backlog drain.
func PointerExistsForSourceRef(
	ctx context.Context, pool *db.Pool, projectID, sourceType, sourceRef string,
) (int64, error) {
	var id int64
	err := pool.DB().QueryRowContext(ctx,
		`SELECT id FROM knowledge_pointers
		  WHERE project_id = ? AND source_type = ? AND source_ref = ? AND status = 'active'`,
		projectID, sourceType, sourceRef,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("pointer_exists: %w", err)
	}
	return id, nil
}

// CandidateExistsForSourceRef returns true if a curation_candidates row
// matches (source_type, source_ref) with status in the supplied list.
// Used by discovery passes to skip duplicates without going through the
// AddCandidate insert path.
//
// Empty statuses defaults to checking 'pending' and 'promoted' (the
// Rust secondary_pass behavior) so a single argless call covers the
// common case.
func CandidateExistsForSourceRef(
	ctx context.Context, pool *db.Pool, sourceType, sourceRef string, statuses ...string,
) (bool, error) {
	if len(statuses) == 0 {
		statuses = []string{"pending", "promoted"}
	}
	args := db.NewArgs().AddString(sourceType).AddString(sourceRef)
	placeholders := make([]string, len(statuses))
	for i, s := range statuses {
		placeholders[i] = "?"
		args.AddString(s)
	}
	query := fmt.Sprintf(
		`SELECT 1 FROM curation_candidates
		  WHERE source_type = ? AND source_ref = ?
		    AND status IN (%s)
		  LIMIT 1`,
		strings.Join(placeholders, ","),
	)
	var one int
	err := pool.DB().QueryRowContext(ctx, query, args.Slice()...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("candidate_exists: %w", err)
	}
	return true, nil
}

// ProposeLinks runs FTS5 search against question over knowledge_pointers,
// then inserts up to N see-also link proposals (confirmed=false) from
// pointerID to each match other than itself. Returns the count
// inserted. Errors from individual AddPointerLink calls are logged via
// the underlying function's error chain but don't fail the whole batch
// — link proposals are advisory.
//
// Mirror of the Rust propose_links helper used post-PromoteCandidate
// in both primary and secondary passes.
func ProposeLinks(
	ctx context.Context, pool *db.Pool, pointerID int64, question string, limit int,
) (int, error) {
	if limit <= 0 {
		limit = 5
	}
	hits, err := pointers.FTSSearch(ctx, pool, question, limit)
	if err != nil {
		return 0, fmt.Errorf("propose_links search: %w", err)
	}
	proposed := 0
	for _, relatedID := range hits {
		if relatedID == pointerID {
			continue
		}
		if err := AddPointerLink(ctx, pool, pointerID, relatedID, "see-also", false); err != nil {
			// Log via error chain rather than halt — one bad link
			// shouldn't drop the rest.
			continue
		}
		proposed++
	}
	return proposed, nil
}

// AddPointerLink inserts a pointer_links row joining two pointers with
// a typed relationship. Mirror of the Rust add_pointer_link. Used by
// the post-promote propose_links step in discovery passes (T8).
//
// confirmed=false marks the link as a proposal awaiting human review;
// confirmed=true marks it as endorsed. The post-promote auto-proposes
// always pass false.
func AddPointerLink(
	ctx context.Context, pool *db.Pool,
	pointerID, relatedID int64, relationship string, confirmed bool,
) error {
	if pointerID == relatedID {
		return fmt.Errorf("%w: pointer_id and related_id must differ", ErrInvalidCandidate)
	}
	switch relationship {
	case "extends", "contradicts", "see-also", "supersedes":
	default:
		return fmt.Errorf("%w: relationship must be one of: extends, contradicts, see-also, supersedes",
			ErrInvalidCandidate)
	}

	return pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO pointer_links (pointer_id, related_id, relationship, confirmed)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT (pointer_id, related_id) DO NOTHING`,
			pointerID, relatedID, relationship, boolToInt(confirmed))
		if err != nil {
			return fmt.Errorf("add_pointer_link: %w", err)
		}
		return nil
	})
}

// readPendingForPromote loads a candidate by id, returning
// ErrCandidateNotFound if it doesn't exist or isn't 'pending'. Internal
// to PromoteCandidate; mirrors the Rust .ok_or_else NotFound shape.
func readPendingForPromote(ctx context.Context, pool *db.Pool, id int64) (Candidate, error) {
	row := pool.DB().QueryRowContext(ctx,
		`SELECT id, project_id, source_type, source_ref, question,
		        invoke_when, description, tags, quality_score, origin,
		        origin_ref, promoted_automatically, promoted_at,
		        expires_at, status, created_at
		   FROM curation_candidates
		  WHERE id = ? AND status = 'pending'`, id)
	c, err := scanCandidate(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Candidate{}, fmt.Errorf("%w: id=%d", ErrCandidateNotFound, id)
	}
	if err != nil {
		return Candidate{}, err
	}
	return c, nil
}

// scanCandidate decodes one row into a Candidate. Uses db.Scanner so
// both *sql.Row (QueryRowContext) and *sql.Rows (QueryContext.Next) can
// share the same scan path — db.Scanner is the canonical exemption
// boundary for the variadic-any in the stdlib Scan signature.
func scanCandidate(s db.Scanner) (Candidate, error) {
	var (
		c            Candidate
		tagsJSON     string
		qualityScore sql.NullFloat64
		originRef    sql.NullString
		promotedAt   sql.NullString
		expiresAt    sql.NullString
		createdAtRaw string
	)
	err := s.Scan(
		&c.ID, &c.ProjectID, &c.SourceType, &c.SourceRef, &c.Question,
		&c.InvokeWhen, &c.Description, &tagsJSON, &qualityScore, &c.Origin,
		&originRef, &c.PromotedAutomatically, &promotedAt,
		&expiresAt, &c.Status, &createdAtRaw,
	)
	if err != nil {
		return Candidate{}, err
	}
	if tagsJSON == "" {
		c.Tags = nil
	} else if err := json.Unmarshal([]byte(tagsJSON), &c.Tags); err != nil {
		return Candidate{}, fmt.Errorf("scan: tags unmarshal: %w", err)
	}
	if qualityScore.Valid {
		v := qualityScore.Float64
		c.QualityScore = &v
	}
	if originRef.Valid {
		v := originRef.String
		c.OriginRef = &v
	}
	if promotedAt.Valid {
		t, err := parseSQLiteDatetime(promotedAt.String)
		if err == nil {
			c.PromotedAt = &t
		}
	}
	if expiresAt.Valid {
		t, err := parseSQLiteDatetime(expiresAt.String)
		if err == nil {
			c.ExpiresAt = &t
		}
	}
	if t, err := parseSQLiteDatetime(createdAtRaw); err == nil {
		c.CreatedAt = t
	}
	return c, nil
}

// parseSQLiteDatetime parses SQLite's default datetime('now') format
// ("YYYY-MM-DD HH:MM:SS"). Falls back to RFC3339 for rows written via
// other paths.
func parseSQLiteDatetime(s string) (time.Time, error) {
	const sqliteLayout = "2006-01-02 15:04:05"
	if t, err := time.Parse(sqliteLayout, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
