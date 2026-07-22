// Migration runner for the Go toolkit-server.
//
// Originally ported from crates/shared-db/src/lib.rs::run_migrations
// (archived 2026-05-22 alongside its host crate as part of chain
// rust-retirement-and-db-hardening T6). The Go binary applies pending
// migrations on Pool.Open; this package owns that path.
//
// Post-T6, `migrations/` here is the canonical source-of-truth for
// schema. The testutil mirror at go/internal/testutil/migrations/
// stays a real-copy convention (Go embed rejects symlinks); the
// precommit gate enforces canonical → mirror direction inline. See
// CONVENTIONS.md §"Migration Strategy" for the discipline.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"unicode"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// MissingFTS5HintFmt is the actionable error returned when a migration
// fails with the SQLite-layer "no such module: fts5" message. The
// default `go test` invocation omits the build tag the FTS5 module
// requires; this string points the caller at the canonical Makefile
// invocation instead of the raw SQLite error. Exported so regression
// tests (and the testutil wrapper) can assert the wording without
// duplicating the literal.
const MissingFTS5HintFmt = `migration %s requires SQLite FTS5 (knowledge_pointers and similar virtual tables use it). Build with the sqlite_fts5 tag:
  make -C go test                              (canonical — uses TAGS=sqlite_fts5)
  go test -tags sqlite_fts5 ./...              (direct invocation)
Underlying SQLite error: %s`

// IsMissingFTS5Error reports whether err is the SQLite-layer signal
// that the driver was built without FTS5 support. Case-insensitive
// substring match, tolerant of minor format drift across SQLite
// versions.
func IsMissingFTS5Error(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "no such module: fts5")
}

// MigrationHead returns the slug (filename without ".sql") of the
// latest embedded migration, e.g. "028_roadmap_items_view". After
// RunMigrations completes, SELECT name FROM _migrations ORDER BY id
// DESC LIMIT 1 returns the same value — mirroring shared_db's stored
// name format (build.rs strips ".sql" before recording, so any DB
// previously migrated by the Rust runner stores slugs).
//
// Storing slugs (not filenames) is load-bearing for cross-runner
// compatibility: a DB that's been migrated by both runners over its
// lifetime would otherwise see "028_roadmap_items_view" (from Rust)
// and "028_roadmap_items_view.sql" (from Go) as different migrations
// and re-attempt application.
func MigrationHead() (string, error) {
	slugs, _, err := embeddedMigrations()
	if err != nil {
		return "", err
	}
	if len(slugs) == 0 {
		return "", fmt.Errorf("no migrations embedded")
	}
	return slugs[len(slugs)-1], nil
}

// embeddedMigrations returns the migration set in name-sorted order as
// parallel slug + filename slices. slug is what we record in
// _migrations (".sql" stripped, matching shared_db); filename is what
// we read from the embed FS.
func embeddedMigrations() (slugs, filenames []string, err error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, nil, fmt.Errorf("read migrations dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		filenames = append(filenames, e.Name())
	}
	slices.Sort(filenames)
	slugs = make([]string, len(filenames))
	for i, f := range filenames {
		slugs[i] = strings.TrimSuffix(f, ".sql")
	}
	return slugs, filenames, nil
}

// RunMigrations applies every embedded migration not yet recorded in
// _migrations, in filename order. Idempotent — re-running is a no-op
// once the head is current.
//
// All operations execute on a single connection held for the duration
// of this function. This is load-bearing for the same reason the Rust
// runner holds one connection: SQLite WAL uses snapshot isolation, so
// a connection acquired before a DDL commit sees the pre-DDL schema
// for its reader-snapshot lifetime. Splitting "already_run" checks
// across pool connections opens a race where the second connection's
// snapshot predates the first's DDL commit and the next migration
// queries the stale schema.
//
// Per-migration savepoints roll back partial writes if any statement
// in a migration fails. Without this, a half-applied migration leaves
// orphan tables that trip "table X already exists" on the next open.
func RunMigrations(pool *Pool) error {
	slugs, filenames, err := embeddedMigrations()
	if err != nil {
		return err
	}

	ctx := context.Background()
	conn, err := pool.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration conn: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS _migrations (
			id      INTEGER PRIMARY KEY,
			name    TEXT NOT NULL UNIQUE,
			run_at  TEXT NOT NULL DEFAULT (datetime('now'))
		)`); err != nil {
		return fmt.Errorf("create _migrations: %w", err)
	}

	for i, slug := range slugs {
		filename := filenames[i]

		var alreadyRun int
		if err := conn.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM _migrations WHERE name = ?", slug,
		).Scan(&alreadyRun); err != nil {
			return fmt.Errorf("check %s: %w", slug, err)
		}
		if alreadyRun > 0 {
			continue
		}

		data, err := migrationFS.ReadFile("migrations/" + filename)
		if err != nil {
			return fmt.Errorf("read %s: %w", filename, err)
		}

		if err := applyMigration(ctx, conn, slug, string(data)); err != nil {
			if IsMissingFTS5Error(err) {
				return fmt.Errorf(MissingFTS5HintFmt, slug, err)
			}
			return err
		}
	}
	return nil
}

func applyMigration(ctx context.Context, conn *sql.Conn, name, body string) error {
	sp := "sp_mig_" + sanitizeSavepoint(name)
	if _, err := conn.ExecContext(ctx, "SAVEPOINT "+sp); err != nil {
		return fmt.Errorf("savepoint %s: %w", name, err)
	}

	for _, stmt := range splitSQLStatements(body) {
		s := strings.TrimSpace(stmt)
		if s == "" {
			continue
		}
		if _, err := conn.ExecContext(ctx, s); err != nil {
			// Roll back partial work, then release so the savepoint frame is closed.
			_, _ = conn.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+sp)
			_, _ = conn.ExecContext(ctx, "RELEASE SAVEPOINT "+sp)
			return fmt.Errorf("apply %s: %w", name, err)
		}
	}

	if _, err := conn.ExecContext(ctx,
		"INSERT INTO _migrations (name) VALUES (?)", name,
	); err != nil {
		_, _ = conn.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+sp)
		_, _ = conn.ExecContext(ctx, "RELEASE SAVEPOINT "+sp)
		return fmt.Errorf("record %s: %w", name, err)
	}

	if _, err := conn.ExecContext(ctx, "RELEASE SAVEPOINT "+sp); err != nil {
		return fmt.Errorf("release %s: %w", name, err)
	}
	return nil
}

func sanitizeSavepoint(name string) string {
	var b strings.Builder
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

// splitSQLStatements splits a migration body into individual SQL
// statements at `;` boundaries, while preserving `;` inside:
//   - `--` line comments
//   - `/* */` block comments
//   - single-quoted string literals (with `”` escape)
//   - `BEGIN … END` trigger bodies
//
// A `BEGIN` token only opens a depth frame when the current statement
// buffer is non-empty — that disambiguates `CREATE TRIGGER … BEGIN`
// (trigger body) from a standalone `BEGIN TRANSACTION;` statement.
//
// Mirrors crates/shared-db/src/lib.rs::split_sql_statements; ported so
// future migrations that use triggers don't trip the runner.
func splitSQLStatements(sql string) []string {
	var (
		statements    []string
		current       strings.Builder
		inLineComment bool
		inBlockComm   bool
		inString      bool
		beginDepth    int
	)
	runes := []rune(sql)
	i := 0
	for i < len(runes) {
		c := runes[i]
		next := func() rune {
			if i+1 < len(runes) {
				return runes[i+1]
			}
			return 0
		}

		if inLineComment {
			current.WriteRune(c)
			if c == '\n' {
				inLineComment = false
			}
			i++
			continue
		}
		if inBlockComm {
			current.WriteRune(c)
			if c == '*' && next() == '/' {
				current.WriteRune(next())
				i += 2
				inBlockComm = false
				continue
			}
			i++
			continue
		}
		if inString {
			current.WriteRune(c)
			if c == '\'' {
				if next() == '\'' {
					current.WriteRune(next())
					i += 2
					continue
				}
				inString = false
			}
			i++
			continue
		}

		switch {
		case c == '\'':
			current.WriteRune(c)
			inString = true
			i++
		case c == '-' && next() == '-':
			current.WriteRune(c)
			current.WriteRune(next())
			i += 2
			inLineComment = true
		case c == '/' && next() == '*':
			current.WriteRune(c)
			current.WriteRune(next())
			i += 2
			inBlockComm = true
		case c == ';':
			if beginDepth == 0 {
				statements = append(statements, current.String())
				current.Reset()
			} else {
				current.WriteRune(c)
			}
			i++
		case unicode.IsLetter(c) || c == '_':
			// Greedy identifier read so BEGIN/END detection looks at full words.
			start := i
			for i < len(runes) {
				r := runes[i]
				if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
					i++
					continue
				}
				break
			}
			word := string(runes[start:i])
			upper := strings.ToUpper(word)
			if upper == "BEGIN" && strings.TrimSpace(current.String()) != "" {
				// Non-empty buffer means BEGIN is opening a trigger body,
				// not a standalone BEGIN TRANSACTION.
				beginDepth++
			} else if upper == "END" && beginDepth > 0 {
				beginDepth--
			}
			current.WriteString(word)
		default:
			current.WriteRune(c)
			i++
		}
	}
	if strings.TrimSpace(current.String()) != "" {
		statements = append(statements, current.String())
	}
	return statements
}
