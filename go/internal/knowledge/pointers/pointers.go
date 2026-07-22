// Package pointers backs knowledge_pointers CRUD and the FTS5 search powering
// the unified knowledge_search action. Mirrors knowledge_lib::pointers.
//
// knowledge_pointers_fts is a standalone content-less FTS5 virtual table; this
// module syncs the index on every write rather than using SQL triggers (the
// embedded migrations cannot parse BEGIN…END trigger bodies).
package pointers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"toolkit/internal/db"
)

// KnowledgePointer mirrors knowledge_lib::types::KnowledgePointer.
//
// Slug is the per-row stable identifier for source_type='vault' rows
// (chain `forge-vault-note-schema-rework` T4): vault entries are
// uniquely identified by their slug, independent of the routing subdir
// or project_id stamp. The partial UNIQUE INDEX
// `vault_pointer_slug_uniq` enforces this at the DB level. For
// non-vault sources the column is unused (nullable) — chain/task/bug
// identity stays on (project_id, source_type, source_ref).
type KnowledgePointer struct {
	ID                    int64    `json:"id"`
	ProjectID             string   `json:"project_id"`
	SourceType            string   `json:"source_type"`
	SourceRef             string   `json:"source_ref"`
	Slug                  string   `json:"slug,omitempty"`
	Question              string   `json:"question"`
	InvokeWhen            string   `json:"invoke_when"`
	Description           *string  `json:"description,omitempty"`
	Tags                  []string `json:"tags"`
	QualityScore          *float64 `json:"quality_score,omitempty"`
	StalenessHint         *string  `json:"staleness_hint,omitempty"`
	NegativeFeedbackCount int64    `json:"negative_feedback_count"`
	UsageCount            int64    `json:"usage_count"`
	LastUsedAt            *string  `json:"last_used_at,omitempty"`
	Status                string   `json:"status"`
	CreatedAt             string   `json:"created_at"`
}

// UpsertResult is the return shape of Upsert: in addition to the row's
// id, it carries the action verb describing what happened
// ("created" / "updated") and the previous source_ref when an update
// changed it (used by vault-note callers to clean up the old file on
// scope-change re-forges).
type UpsertResult struct {
	ID                int64
	Action            string
	PreviousSourceRef string
	// MergedConflictID is non-zero when a slug-keyed upsert encountered a
	// different row already at the target source_ref (e.g. legacy
	// long-form-slug pointer + canonical bare-slug pointer for the same
	// physical file after relocation). The conflicting row is deleted in
	// favor of the canonical-slug row; this field carries its former id
	// so callers can audit / log the merge. Zero on the non-conflict path.
	// Bug forge-edit-non-declared-frontmatter-keys-cant-be-dropped-or-renamed-on-relocate part 2.
	MergedConflictID int64
}

// ErrNotFound is returned when no row matches the given pointer id.
var ErrNotFound = errors.New("knowledge pointer not found")

// FTSSearch runs an FTS5 OR-tokenised search over knowledge_pointers_fts and
// returns pointer IDs ranked by FTS5 relevance.
//
// Tokenisation strips non-alphanumeric chars and joins ≥2-char tokens with OR.
// This maximises recall for the pre-filter; Qwen handles relevance ranking
// downstream. Hyphenated terms (e.g. "two-pass") would trip FTS5's "NOT may
// only appear right of AND" rule without this split — see vault learning
// 2026-05-11_fts5-or-semantics-for-retrieval.
func FTSSearch(ctx context.Context, pool *db.Pool, query string, limit int) ([]int64, error) {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return nil, nil
	}
	var subTokens []string
	for _, tok := range strings.Fields(trimmed) {
		var b strings.Builder
		flush := func() {
			if b.Len() >= 2 {
				subTokens = append(subTokens, b.String())
			}
			b.Reset()
		}
		for _, r := range tok {
			isAlnum := ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9')
			if isAlnum {
				b.WriteRune(r)
			} else {
				flush()
			}
		}
		flush()
	}
	if len(subTokens) == 0 {
		return nil, nil
	}
	ftsQuery := strings.Join(subTokens, " OR ")

	rows, err := pool.DB().QueryContext(ctx,
		`SELECT kp.id
		   FROM knowledge_pointers kp
		   JOIN knowledge_pointers_fts fts ON fts.rowid = kp.id
		  WHERE knowledge_pointers_fts MATCH ?
		    AND kp.status = 'active'
		  ORDER BY rank
		  LIMIT ?`,
		ftsQuery, limit)
	if err != nil {
		return nil, fmt.Errorf("fts search: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("fts scan: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetByIDs returns pointers in the input id slice's order. Rows missing from
// the DB are silently skipped (matches Rust).
func GetByIDs(ctx context.Context, pool *db.Pool, ids []int64) ([]KnowledgePointer, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	q := `SELECT id, project_id, source_type, source_ref, question, invoke_when,
	             description, tags, quality_score, staleness_hint,
	             negative_feedback_count, usage_count, last_used_at, status, created_at
	        FROM knowledge_pointers WHERE id IN (` + placeholders + `)`
	args := db.NewArgs()
	for _, id := range ids {
		args.AddInt64(id)
	}
	rows, err := pool.DB().QueryContext(ctx, q, args.Slice()...)
	if err != nil {
		return nil, fmt.Errorf("get pointers by ids: %w", err)
	}
	defer rows.Close()
	byID := make(map[int64]KnowledgePointer, len(ids))
	for rows.Next() {
		p, err := scanPointer(rows)
		if err != nil {
			return nil, err
		}
		byID[p.ID] = p
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]KnowledgePointer, 0, len(ids))
	for _, id := range ids {
		if p, ok := byID[id]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// IncrementUsage bumps usage_count and stamps last_used_at. Best-effort —
// failure is logged by callers but never blocks the response.
func IncrementUsage(ctx context.Context, pool *db.Pool, id int64) error {
	return pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE knowledge_pointers
			    SET usage_count = usage_count + 1,
			        last_used_at = datetime('now')
			  WHERE id = ?`, id)
		return err
	})
}

// RecordMiss increments negative_feedback_count and optionally sets staleness_hint.
// Mirrors record_miss; the hint is COALESCE'd so a subsequent miss can set
// the hint even if the first call didn't carry one.
func RecordMiss(ctx context.Context, pool *db.Pool, id int64, stalenessReason *string) error {
	return pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE knowledge_pointers
			    SET negative_feedback_count = negative_feedback_count + 1,
			        staleness_hint = COALESCE(?, staleness_hint)
			  WHERE id = ?`,
			stalenessReason, id)
		return err
	})
}

// Insert adds a knowledge_pointer row and syncs the FTS5 index. Used by the
// seeder + admin / curation flows. Returns the inserted id. Callers are
// expected to validate the source_ref shape before calling.
func Insert(ctx context.Context, pool *db.Pool, p KnowledgePointer) (int64, error) {
	tagsJSON, err := json.Marshal(p.Tags)
	if err != nil {
		return 0, fmt.Errorf("marshal tags: %w", err)
	}
	var id int64
	err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx,
			`INSERT INTO knowledge_pointers
				(project_id, source_type, source_ref, question, invoke_when,
				 description, tags, quality_score, staleness_hint, status)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, COALESCE(?, 'active'))
			 RETURNING id`,
			p.ProjectID, p.SourceType, p.SourceRef, p.Question, p.InvokeWhen,
			p.Description, string(tagsJSON), p.QualityScore, p.StalenessHint,
			optString(p.Status))
		if err := row.Scan(&id); err != nil {
			return err
		}
		// Sync the standalone FTS5 index. Rowid = pointer id.
		_, err := tx.ExecContext(ctx,
			`INSERT INTO knowledge_pointers_fts (rowid, question, invoke_when)
			 VALUES (?, ?, ?)`,
			id, p.Question, p.InvokeWhen)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("insert pointer: %w", err)
	}
	return id, nil
}

// Upsert inserts a pointer row, or updates the existing matching row's
// content-derived columns. The match key depends on source_type:
//
//   - source_type = 'vault' with a non-empty p.Slug: match by
//     (source_type, slug). Chain `forge-vault-note-schema-rework` T4
//     made slug the canonical identity for vault rows; the project_id
//     stamp and source_ref subdir are routing/attribution details that
//     may change between re-forges of the same slug.
//   - Otherwise: match by (project_id, source_type, source_ref) — the
//     identity convention chain/task/bug use today.
//
// On the UPDATE path the columns refreshed are question / invoke_when
// / description / tags / quality_score / staleness_hint AND, for
// vault rows where the slug-keyed lookup hit, the project_id +
// source_ref columns also update so a scope-change re-forge moves
// the pointer cleanly. The FTS5 index always re-syncs via
// DELETE-then-INSERT against the virtual table (per vault learning
// reference_fts5_shadow_table_sync). Counter columns (usage_count,
// negative_feedback_count, last_used_at, created_at, status) are
// preserved across updates so re-running a sync does not erase usage
// history.
//
// Returns UpsertResult with the row's id, the action verb describing
// what happened ("created" / "updated"), and the previous source_ref
// when an update changed it (used by vault-note callers to clean up
// the old file on scope-change re-forges).
func Upsert(ctx context.Context, pool *db.Pool, p KnowledgePointer) (UpsertResult, error) {
	var res UpsertResult
	if err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		var e error
		res, e = UpsertInTx(ctx, tx, p)
		return e
	}); err != nil {
		return UpsertResult{}, err
	}
	return res, nil
}

// UpsertInTx is the tx-aware variant of Upsert: it runs the pointer
// lookup + insert/update + FTS5 sync against the caller's *sql.Tx rather
// than opening its own pool.WithWrite. Use it from INSIDE an existing
// write tx (e.g. work.HandleBatch's forge_edit path) — calling the
// pool-based Upsert there re-enters db.Pool's non-reentrant write mutex
// on the same goroutine and deadlocks (bug
// `forge-edit-in-batch-deadlocks-via-nested-pool-withwrite-in-onedit-notifier`).
func UpsertInTx(ctx context.Context, tx *sql.Tx, p KnowledgePointer) (UpsertResult, error) {
	tagsJSON, err := json.Marshal(p.Tags)
	if err != nil {
		return UpsertResult{}, fmt.Errorf("marshal tags: %w", err)
	}
	var (
		id               int64
		action           string
		prevSourceRef    string
		mergedConflictID int64
	)
	useSlugKey := p.SourceType == "vault" && p.Slug != ""
	run := func() error {
		// Look up existing. Vault rows key off slug; everything else
		// uses the (project_id, source_type, source_ref) tuple.
		//
		// Vault fallback (chain 617 T2): if the canonical-slug lookup
		// misses, retry by (project_id, source_type, source_ref). Legacy
		// seeder rows carry long-form slugs like
		// `general/2026-05-07_<name>` while post-rework forge writes
		// produce the bare `<name>` slug. Without the fallback, a re-forge
		// of a legacy row INSERTs in parallel and trips the UNIQUE
		// constraint on source_ref. The UPDATE path below normalizes the
		// slug to the canonical form as it touches the row.
		var lookupErr error
		var existingSourceRef string
		if useSlugKey {
			lookupErr = tx.QueryRowContext(ctx,
				`SELECT id, source_ref FROM knowledge_pointers
				  WHERE source_type = 'vault' AND slug = ?`,
				p.Slug).Scan(&id, &existingSourceRef)
			if errors.Is(lookupErr, sql.ErrNoRows) && p.SourceRef != "" {
				fallbackErr := tx.QueryRowContext(ctx,
					`SELECT id, source_ref FROM knowledge_pointers
					  WHERE source_type = 'vault'
					    AND project_id = ?
					    AND source_ref = ?`,
					p.ProjectID, p.SourceRef).Scan(&id, &existingSourceRef)
				if !errors.Is(fallbackErr, sql.ErrNoRows) {
					lookupErr = fallbackErr
				}
			}
		} else {
			lookupErr = tx.QueryRowContext(ctx,
				`SELECT id FROM knowledge_pointers
				  WHERE project_id = ? AND source_type = ? AND source_ref = ?`,
				p.ProjectID, p.SourceType, p.SourceRef).Scan(&id)
			_ = existingSourceRef
		}
		switch {
		case errors.Is(lookupErr, sql.ErrNoRows):
			// INSERT path — mirror Insert but inside this tx.
			row := tx.QueryRowContext(ctx,
				`INSERT INTO knowledge_pointers
					(project_id, source_type, source_ref, slug, question, invoke_when,
					 description, tags, quality_score, staleness_hint, status)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, COALESCE(?, 'active'))
				 RETURNING id`,
				p.ProjectID, p.SourceType, p.SourceRef, optString(p.Slug),
				p.Question, p.InvokeWhen,
				p.Description, string(tagsJSON), p.QualityScore, p.StalenessHint,
				optString(p.Status))
			if scanErr := row.Scan(&id); scanErr != nil {
				return scanErr
			}
			action = "created"
		case lookupErr != nil:
			return lookupErr
		default:
			action = "updated"
			if useSlugKey && existingSourceRef != p.SourceRef {
				prevSourceRef = existingSourceRef
			}
			// UPDATE path — touch only the content-derived columns; usage
			// counters and last_used_at survive the upsert.
			// For slug-keyed vault rows the project_id + source_ref also
			// update so a scope-change re-forge moves the pointer cleanly.
			if useSlugKey {
				// Pre-flight: if the target (project_id, source_type,
				// source_ref) is occupied by a DIFFERENT row, the UPDATE
				// below would trip the table-level UNIQUE constraint. This
				// happens when a legacy long-form-slug row + a canonical
				// bare-slug row both reference the same physical file after
				// a relocation (bug
				// forge-edit-non-declared-frontmatter-keys-cant-be-dropped-or-renamed-on-relocate
				// part 2). Resolve by deleting the conflicting row in
				// favor of the canonical-slug row we're about to update.
				// The lookup matched `slug = p.Slug`, so `id` IS the
				// canonical-slug row by construction; the row at the
				// target source_ref must be non-canonical (different slug)
				// and is the safe one to retire.
				var conflictID int64
				conflictErr := tx.QueryRowContext(ctx,
					`SELECT id FROM knowledge_pointers
					  WHERE project_id = ? AND source_type = ?
					    AND source_ref = ? AND id != ?`,
					p.ProjectID, p.SourceType, p.SourceRef, id).Scan(&conflictID)
				switch {
				case errors.Is(conflictErr, sql.ErrNoRows):
					// No conflict — proceed with the UPDATE as-is.
				case conflictErr != nil:
					return conflictErr
				default:
					// Conflict — drop the FTS5 row first (the partial
					// content table needs the virtual table for proper
					// rowid cleanup), then the pointer row.
					if _, err := tx.ExecContext(ctx,
						`DELETE FROM knowledge_pointers_fts WHERE rowid = ?`, conflictID); err != nil {
						return err
					}
					if _, err := tx.ExecContext(ctx,
						`DELETE FROM knowledge_pointers WHERE id = ?`, conflictID); err != nil {
						return err
					}
					mergedConflictID = conflictID
				}

				// slug also updates so a legacy long-form slug
				// (`general/2026-05-07_<name>` from the early seeder)
				// normalises to the canonical bare form on first re-forge.
				// vault_pointer_slug_uniq still holds — the new slug is
				// unique-by-construction (caller computed it from a slug
				// that has no other vault row at this point, otherwise
				// the slug-lookup branch would have hit, not the
				// source_ref fallback).
				if _, err := tx.ExecContext(ctx,
					`UPDATE knowledge_pointers
					    SET project_id = ?, source_ref = ?, slug = ?,
					        question = ?, invoke_when = ?, description = ?,
					        tags = ?, quality_score = COALESCE(?, quality_score),
					        staleness_hint = COALESCE(?, staleness_hint)
					  WHERE id = ?`,
					p.ProjectID, p.SourceRef, p.Slug,
					p.Question, p.InvokeWhen, p.Description,
					string(tagsJSON), p.QualityScore, p.StalenessHint, id); err != nil {
					return err
				}
			} else {
				if _, err := tx.ExecContext(ctx,
					`UPDATE knowledge_pointers
					    SET question = ?, invoke_when = ?, description = ?,
					        tags = ?, quality_score = COALESCE(?, quality_score),
					        staleness_hint = COALESCE(?, staleness_hint)
					  WHERE id = ?`,
					p.Question, p.InvokeWhen, p.Description,
					string(tagsJSON), p.QualityScore, p.StalenessHint, id); err != nil {
					return err
				}
			}
		}
		// FTS5 sync — DELETE-then-INSERT via the virtual table. The
		// pre-DELETE is harmless on the insert path (no rowid yet), and
		// load-bearing on the update path (without it, the inverted index
		// carries the stale question / invoke_when tokens and FTS5 returns
		// false-positive matches for retired text).
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM knowledge_pointers_fts WHERE rowid = ?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO knowledge_pointers_fts (rowid, question, invoke_when)
			 VALUES (?, ?, ?)`,
			id, p.Question, p.InvokeWhen); err != nil {
			return err
		}
		return nil
	}
	if err := run(); err != nil {
		return UpsertResult{}, fmt.Errorf("upsert pointer: %w", err)
	}
	return UpsertResult{
		ID:                id,
		Action:            action,
		PreviousSourceRef: prevSourceRef,
		MergedConflictID:  mergedConflictID,
	}, nil
}

// Delete removes a pointer row and its FTS5 entry. The FTS5 row is
// removed via the virtual table (DELETE … WHERE rowid = ?), not via a
// direct write to knowledge_pointers_fts_content, so the inverted index
// updates correctly.
func Delete(ctx context.Context, pool *db.Pool, id int64) error {
	return pool.WithWrite(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM knowledge_pointers_fts WHERE rowid = ?`, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`DELETE FROM knowledge_pointers WHERE id = ?`, id)
		return err
	})
}

// DeleteByRef removes a pointer identified by its (project_id, source_type,
// source_ref) tuple and its FTS5 entry. Used by forge_delete and the
// edit-cleanup path when an artifact moves to a deleted state. Returns
// nil if no row matched.
func DeleteByRef(ctx context.Context, pool *db.Pool, projectID, sourceType, sourceRef string) error {
	return pool.WithWrite(ctx, func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM knowledge_pointers
			  WHERE project_id = ? AND source_type = ? AND source_ref = ?`,
			projectID, sourceType, sourceRef).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM knowledge_pointers_fts WHERE rowid = ?`, id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`DELETE FROM knowledge_pointers WHERE id = ?`, id)
		return err
	})
}

// optString converts an empty string to a nil *string so database/sql writes
// SQL NULL instead of an empty-string row. Non-empty values pass through as
// a pointer the driver dereferences. Replaces a previous `any` wrapper —
// pointers are the idiomatic way to express nullable SQL args.
func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// scanPointer extracts one row into a KnowledgePointer.
func scanPointer(s db.Scanner) (KnowledgePointer, error) {
	var (
		p           KnowledgePointer
		desc, stale sql.NullString
		lastUsed    sql.NullString
		quality     sql.NullFloat64
		tagsJSON    string
	)
	if err := s.Scan(
		&p.ID, &p.ProjectID, &p.SourceType, &p.SourceRef,
		&p.Question, &p.InvokeWhen,
		&desc, &tagsJSON, &quality, &stale,
		&p.NegativeFeedbackCount, &p.UsageCount, &lastUsed, &p.Status, &p.CreatedAt,
	); err != nil {
		return KnowledgePointer{}, err
	}
	if desc.Valid {
		d := desc.String
		p.Description = &d
	}
	if stale.Valid {
		s := stale.String
		p.StalenessHint = &s
	}
	if lastUsed.Valid {
		l := lastUsed.String
		p.LastUsedAt = &l
	}
	if quality.Valid {
		q := quality.Float64
		p.QualityScore = &q
	}
	if tagsJSON != "" {
		var tags []string
		if err := json.Unmarshal([]byte(tagsJSON), &tags); err != nil {
			// Matches Rust: malformed tags fall back to empty rather than aborting.
			tags = nil
		}
		p.Tags = tags
	}
	return p, nil
}
