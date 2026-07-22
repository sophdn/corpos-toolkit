package pointers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"toolkit/internal/db"
)

// OrphanRow is one active vault pointer whose source_ref file no
// longer exists on disk. Returned by [ListVaultOrphans]; the
// admin.vault_integrity_sweep action drives [RetireOrphan] over the
// returned slice to state-transition each row.
type OrphanRow struct {
	ID        int64  `json:"id"`
	ProjectID string `json:"project_id"`
	SourceRef string `json:"source_ref"`
	Slug      string `json:"slug,omitempty"`
}

// VaultIntegrityReport names the counts of one integrity-sweep pass
// plus the orphan list. Shared between the dry-run lister
// (admin.vault_orphan_list) and the commit-and-retire path
// (admin.vault_integrity_sweep).
type VaultIntegrityReport struct {
	TotalPointers int         `json:"total_pointers"`
	FilesPresent  int         `json:"files_present"`
	OrphansFound  int         `json:"orphans_found"`
	Orphans       []OrphanRow `json:"orphans"`
}

// ListVaultOrphans walks every active source_type='vault' pointer
// and stat()s its source_ref under vaultRoot. Pointers whose files
// are missing land in the report's Orphans slice; the counts close
// over the full active set.
//
// No state-transitions happen here — [RetireOrphan] is the mutating
// step. The split lets dry-run inspection and committing the sweep
// share the same scan path without duplicating the walk.
//
// vaultRoot must be the on-disk vault directory (typically resolved
// via internal/knowledge/vault.ResolveRoot). An empty vaultRoot is
// rejected so a misconfigured caller doesn't silently mark every
// pointer as orphaned.
func ListVaultOrphans(ctx context.Context, pool *db.Pool, vaultRoot string) (VaultIntegrityReport, error) {
	if vaultRoot == "" {
		return VaultIntegrityReport{}, errors.New("vault root not configured")
	}
	rows, err := pool.DB().QueryContext(ctx,
		`SELECT id, project_id, source_ref, COALESCE(slug, '')
		   FROM knowledge_pointers
		  WHERE source_type = 'vault' AND status = 'active'`)
	if err != nil {
		return VaultIntegrityReport{}, fmt.Errorf("list vault pointers: %w", err)
	}
	defer rows.Close()
	report := VaultIntegrityReport{Orphans: []OrphanRow{}}
	for rows.Next() {
		var r OrphanRow
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.SourceRef, &r.Slug); err != nil {
			return VaultIntegrityReport{}, fmt.Errorf("scan pointer: %w", err)
		}
		report.TotalPointers++
		abs := filepath.Join(vaultRoot, r.SourceRef)
		_, statErr := os.Stat(abs)
		switch {
		case statErr == nil:
			report.FilesPresent++
		case errors.Is(statErr, os.ErrNotExist):
			report.OrphansFound++
			report.Orphans = append(report.Orphans, r)
		default:
			return VaultIntegrityReport{}, fmt.Errorf("stat %s: %w", abs, statErr)
		}
	}
	if err := rows.Err(); err != nil {
		return VaultIntegrityReport{}, fmt.Errorf("iterate vault pointers: %w", err)
	}
	return report, nil
}

// RetireOrphan state-transitions the named pointer to
// status='orphaned' and removes its FTS5 inverted-index entry. The
// pointer row itself is preserved — audit fields (created_at,
// usage_count, last_used_at) stay readable for forensic inspection.
// Returns [ErrNotFound] when no row matches.
//
// FTS removal mirrors the cleanup in [Delete] / [Upsert]: even though
// FTSSearch currently filters on status='active', leaving stale rows
// in the inverted index would surface false matches if a future
// search path dropped that filter.
func RetireOrphan(ctx context.Context, pool *db.Pool, id int64) error {
	return pool.WithWrite(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE knowledge_pointers SET status = 'orphaned' WHERE id = ?`,
			id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM knowledge_pointers_fts WHERE rowid = ?`, id); err != nil {
			return err
		}
		return nil
	})
}
