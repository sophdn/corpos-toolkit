package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// HashTableContent returns an order-independent SHA-256 content hash of an
// entire table: it hashes each row's `column=value` cells, sorts the per-row
// hashes, and hashes the sorted list. Two tables with the same rows in any
// order hash identically.
//
// This is the concentrated database/sql.Scan boundary for whole-table
// content comparison — it scans `SELECT *` into []sql.RawBytes over an
// arbitrary schema, which is exactly the `Scan(...any)` variadic the db
// package's forbidigo exemption exists for. Callers (e.g. internal/registry's
// DR byte-identity proof) consume the typed string hash and never touch the
// dynamic boundary themselves.
//
// table is interpolated directly into the SQL — it MUST be a trusted,
// code-supplied identifier (a projection table name), never caller input.
func HashTableContent(ctx context.Context, sdb *sql.DB, table string) (string, error) {
	rows, err := sdb.QueryContext(ctx, "SELECT * FROM "+table)
	if err != nil {
		return "", fmt.Errorf("select %s: %w", table, err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return "", fmt.Errorf("columns %s: %w", table, err)
	}

	var rowHashes []string
	for rows.Next() {
		cells := make([]sql.RawBytes, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", fmt.Errorf("scan %s: %w", table, err)
		}
		var sb strings.Builder
		for i, c := range cells {
			sb.WriteString(cols[i])
			sb.WriteByte('=')
			if c == nil {
				sb.WriteString("\x00NULL")
			} else {
				sb.Write(c)
			}
			sb.WriteByte('\x1f')
		}
		sum := sha256.Sum256([]byte(sb.String()))
		rowHashes = append(rowHashes, hex.EncodeToString(sum[:]))
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("row walk %s: %w", table, err)
	}
	sort.Strings(rowHashes)
	final := sha256.Sum256([]byte(strings.Join(rowHashes, "|")))
	return hex.EncodeToString(final[:]), nil
}
