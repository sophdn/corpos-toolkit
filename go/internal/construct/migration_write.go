package construct

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"toolkit/internal/db"
	"toolkit/internal/forge/registry"
)

// Migration file layout under the repo root. forge(migration) writes
// the same .sql body to both paths atomically; the precommit gate's
// canonical → mirror sync remains a belt-and-braces fallback for raw
// edits but is a no-op for forge-authored migrations.
const (
	migrationCanonicalSubdir = "go/internal/db/migrations"
	migrationMirrorSubdir    = "go/internal/testutil/migrations"
)

// migrationFilenamePattern matches `NNN_<slug>.sql` files in the canonical
// migrations directory. NNN is the zero-padded migration number (the
// existing corpus uses 3 digits; the splitter tolerates 3+).
var migrationFilenamePattern = regexp.MustCompile(`^(\d+)_(.+)\.sql$`)

// MigrationArtifactResult carries what the record construction layer needs
// after WriteMigrationArtifact writes a migration's dual files: the minted (or
// idempotently-resolved) number, the repo-relative [canonical, mirror] paths,
// the docstring/SQL byte lengths, and whether it was an idempotent re-resolve —
// exactly the MigrationForged payload fields, minus the event emit.
type MigrationArtifactResult struct {
	MigrationNumber int
	FilePaths       []string // [canonical, mirror], repo-relative
	DocstringLength int
	SQLLength       int
	Idempotent      bool
	RoutingNote     string // byte-identical to createMigrationInTx's migrationRoutingNote
}

// WriteMigrationArtifact re-homes createMigrationInTx's FILE path for the record
// construction layer: next-number allocation (max of the filesystem corpus AND
// the substrate's committed MigrationForged numbers — collision-free across
// worktrees), the EXPLAIN-against-in-memory parse check, the byte-identical
// canonical+mirror dual-write (with rollback on divergence), and idempotency
// (an existing slug returns its existing number, writes nothing). It composes
// the SAME helpers createMigrationInTx uses (scanMigrationDir / renderMigrationSQL
// / validateMigrationSQL / atomicWrite / verifyMigrationByteIdentical), so the
// on-disk bytes can't drift. It does NOT emit MigrationForged — the caller routes
// that through record. pool-based: the file writes are non-transactional and the
// substrate probe reads committed events via pool.DB().
func WriteMigrationArtifact(ctx context.Context, pool *db.Pool, schema registry.Schema, project, slug, upSQL, docstring string) (MigrationArtifactResult, error) {
	if strings.TrimSpace(upSQL) == "" {
		return MigrationArtifactResult{}, fmt.Errorf("forge(migration): up_sql is required")
	}
	root := resolveMarkdownRoot(ctx, pool.DB(), project, schema)
	canonicalDir := filepath.Join(root, migrationCanonicalSubdir)
	mirrorDir := filepath.Join(root, migrationMirrorSubdir)
	if err := ensureMigrationDirs(canonicalDir, mirrorDir); err != nil {
		return MigrationArtifactResult{}, err
	}
	existing, err := scanMigrationDir(canonicalDir)
	if err != nil {
		return MigrationArtifactResult{}, err
	}
	// Idempotency: an existing slug returns its existing number, no write.
	if priorNumber, hit := existing.bySlug[slug]; hit {
		canonicalPath := filepath.Join(canonicalDir, formatMigrationFilename(priorNumber, slug))
		mirrorPath := filepath.Join(mirrorDir, formatMigrationFilename(priorNumber, slug))
		return MigrationArtifactResult{
			MigrationNumber: priorNumber,
			FilePaths:       repoRelMigrationPaths(root, canonicalPath, mirrorPath),
			DocstringLength: len(docstring),
			SQLLength:       len(upSQL),
			Idempotent:      true,
			// Absolute mirrorPath, byte-identical to createMigrationInTx's note.
			RoutingNote: migrationRoutingNote(priorNumber, slug, true, mirrorPath),
		}, nil
	}
	substrateMax, err := substrateMaxMigrationNumber(ctx, pool.DB(), project)
	if err != nil {
		return MigrationArtifactResult{}, fmt.Errorf("forge(migration): substrate max-number probe: %w", err)
	}
	nextNumber := maxInt(existing.maxNumber, substrateMax) + 1
	body := renderMigrationSQL(docstring, upSQL)
	if err := validateMigrationSQL(ctx, upSQL); err != nil {
		return MigrationArtifactResult{}, fmt.Errorf("forge(migration): SQL parse-check failed: %w", err)
	}
	canonicalPath := filepath.Join(canonicalDir, formatMigrationFilename(nextNumber, slug))
	mirrorPath := filepath.Join(mirrorDir, formatMigrationFilename(nextNumber, slug))
	if err := atomicWrite(canonicalPath, canonicalDir, []byte(body)); err != nil {
		return MigrationArtifactResult{}, fmt.Errorf("forge(migration): write canonical: %w", err)
	}
	if err := atomicWrite(mirrorPath, mirrorDir, []byte(body)); err != nil {
		_ = os.Remove(canonicalPath)
		return MigrationArtifactResult{}, fmt.Errorf("forge(migration): write mirror (canonical rolled back): %w", err)
	}
	if err := verifyMigrationByteIdentical(canonicalPath, mirrorPath); err != nil {
		_ = os.Remove(canonicalPath)
		_ = os.Remove(mirrorPath)
		return MigrationArtifactResult{}, fmt.Errorf("forge(migration): post-write hash mismatch (both rolled back): %w", err)
	}
	return MigrationArtifactResult{
		MigrationNumber: nextNumber,
		FilePaths:       repoRelMigrationPaths(root, canonicalPath, mirrorPath),
		DocstringLength: len(docstring),
		SQLLength:       len(upSQL),
		Idempotent:      false,
		// Absolute mirrorPath, byte-identical to createMigrationInTx's note.
		RoutingNote: migrationRoutingNote(nextNumber, slug, false, mirrorPath),
	}, nil
}

// substrateMaxMigrationNumber returns the highest migration_number ever
// recorded in a MigrationForged event for this project, or 0 when none
// exist. Read on the caller's write tx so it sees every MigrationForged the
// serialized write path has committed — including ones forged from a sibling
// worktree whose .sql file hasn't merged into this checkout's filesystem yet.
// That is what makes the next-number allocation collision-free across
// parallel worktree agents (chain T3): the shared substrate, not the local
// filesystem, is the single source of the number sequence under concurrency.
//
// Scoped to entity_project_id = project because the migration number space is
// per-repo (the canonical migrations dir lives in one repo / one project).
func substrateMaxMigrationNumber(ctx context.Context, q db.Queryer, project string) (int, error) {
	var max sql.NullInt64
	err := q.QueryRowContext(ctx,
		`SELECT MAX(CAST(json_extract(payload, '$.migration_number') AS INTEGER))
		   FROM events
		  WHERE type = 'MigrationForged' AND entity_project_id = ?`,
		project).Scan(&max)
	if err != nil {
		return 0, err
	}
	if !max.Valid {
		return 0, nil
	}
	return int(max.Int64), nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// migrationScan summarizes the canonical migrations directory: the
// highest existing migration number and a slug→number map for the
// idempotency check.
type migrationScan struct {
	maxNumber int
	bySlug    map[string]int
}

func scanMigrationDir(dir string) (migrationScan, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return migrationScan{}, fmt.Errorf("scan migrations dir %q: %w", dir, err)
	}
	out := migrationScan{bySlug: make(map[string]int)}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationFilenamePattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		out.bySlug[m[2]] = n
		if n > out.maxNumber {
			out.maxNumber = n
		}
	}
	return out, nil
}

// formatMigrationFilename produces the `NNN_<slug>.sql` filename using
// three-digit zero-padded numbers to match the existing corpus.
// Migrations beyond 999 zero-pad to the widest fixed width that fits
// (4+ digits emit unpadded — lexical order continues to match numeric
// order through 9999 because every digit count above 3 sorts after
// every 3-digit name).
func formatMigrationFilename(number int, slug string) string {
	if number < 1000 {
		return fmt.Sprintf("%03d_%s.sql", number, slug)
	}
	return fmt.Sprintf("%d_%s.sql", number, slug)
}

// renderMigrationSQL composes the file body: optional `-- ` docstring
// block, blank line, then up_sql verbatim. Mirrors the inline-prose
// shape predecessor migrations author by hand (066_proj_check_
// constraints.sql) so forge-authored and hand-authored migrations are
// indistinguishable on disk.
func renderMigrationSQL(docstring, upSQL string) string {
	var b strings.Builder
	if strings.TrimSpace(docstring) != "" {
		for _, line := range strings.Split(strings.TrimRight(docstring, "\n"), "\n") {
			if line == "" {
				b.WriteString("--\n")
				continue
			}
			b.WriteString("-- ")
			b.WriteString(line)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	b.WriteString(upSQL)
	if !strings.HasSuffix(upSQL, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

// validateMigrationSQL runs the up_sql body against a fresh in-memory
// SQLite DB to catch parse errors at forge time rather than at next
// boot. The in-memory DB is discarded regardless of outcome. Uses the
// same statement splitter the migration runner uses so trigger bodies
// + comment-embedded semicolons don't trip the check.
func validateMigrationSQL(ctx context.Context, upSQL string) error {
	mem, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return fmt.Errorf("open in-memory db: %w", err)
	}
	defer mem.Close()
	for _, stmt := range splitMigrationStatements(upSQL) {
		s := strings.TrimSpace(stmt)
		if s == "" {
			continue
		}
		if _, err := mem.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("statement %q: %w", truncateForError(s, 80), err)
		}
	}
	return nil
}

// truncateForError shortens a statement for inclusion in an error
// message — first N characters + "..." marker when truncated. Keeps the
// surfaced error compact when an unparseable migration is very long.
func truncateForError(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// verifyMigrationByteIdentical hashes both files and compares — guards
// against a write that returned success but landed corrupt bytes (disk
// full, fs quirk, race against another writer). Cheaper than reading
// both files into memory for a Compare since we only need equality.
func verifyMigrationByteIdentical(a, b string) error {
	ha, err := hashFile(a)
	if err != nil {
		return fmt.Errorf("hash %s: %w", a, err)
	}
	hb, err := hashFile(b)
	if err != nil {
		return fmt.Errorf("hash %s: %w", b, err)
	}
	if !bytes.Equal(ha, hb) {
		return fmt.Errorf("canonical %s and mirror %s differ post-write", a, b)
	}
	return nil
}

func hashFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(data)
	return h[:], nil
}

// ensureMigrationDirs creates both target directories when missing —
// fresh repos / test temp roots won't have them yet. atomicWrite already
// MkdirAll's the leaf path, but forge(migration) needs the parents to
// exist before scanMigrationDir runs (the scan reports a fresh corpus
// rather than failing on a missing dir).
func ensureMigrationDirs(canonical, mirror string) error {
	if err := os.MkdirAll(canonical, 0o755); err != nil {
		return fmt.Errorf("mkdir canonical migrations dir: %w", err)
	}
	if err := os.MkdirAll(mirror, 0o755); err != nil {
		return fmt.Errorf("mkdir mirror migrations dir: %w", err)
	}
	return nil
}

// repoRelMigrationPaths normalizes the two on-disk migration paths to
// repo-relative form for the event payload's file_paths field. Always
// returns a two-element slice [canonical, mirror] in that order; the
// event schema's minItems=maxItems=2 holds the consumer contract.
func repoRelMigrationPaths(root, canonical, mirror string) []string {
	return []string{
		repoRelMigrationPath(root, canonical),
		repoRelMigrationPath(root, mirror),
	}
}

func repoRelMigrationPath(root, full string) string {
	rel, err := filepath.Rel(root, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return full
	}
	return rel
}

// migrationRoutingNote produces the one-line summary surfaced on every
// migration forge response (parallel to vaultNoteRoutingNote). Names
// the migration number, the dual-write target, and the idempotent flag
// so the caller sees the routing decision without inspecting the paths.
func migrationRoutingNote(number int, slug string, idempotent bool, mirrorPath string) string {
	verb := "minted"
	if idempotent {
		verb = "re-resolved (idempotent — existing row returned)"
	}
	return fmt.Sprintf(
		"migration %s %s; canonical + testutil mirror written byte-identical (mirror=%s)",
		formatMigrationFilename(number, slug), verb, mirrorPath,
	)
}

// splitMigrationStatements is the package-local statement splitter for
// the forge-time SQL parse check. Mirrors db.splitSQLStatements (kept
// unexported in the db package) so forge doesn't pull a cross-package
// dependency for a small helper. Splits on `;` while preserving the
// same four contexts the runner does: line comments, block comments,
// single-quoted strings, and BEGIN…END trigger bodies.
//
// If a future migration shape exposes a parse-time gap the runner's
// splitter handles (e.g. dollar-quoted strings), the two splitters
// would diverge here — fix that by lifting db.splitSQLStatements to an
// exported helper rather than divergent forks.
func splitMigrationStatements(sqlBody string) []string {
	var (
		statements    []string
		current       strings.Builder
		inLineComment bool
		inBlockComm   bool
		inString      bool
		beginDepth    int
	)
	runes := []rune(sqlBody)
	i := 0
	flush := func() {
		s := current.String()
		statements = append(statements, s)
		current.Reset()
	}
	hasIdentPrefix := func(s, want string) bool {
		s = strings.TrimSpace(s)
		if len(s) < len(want) {
			return false
		}
		return strings.EqualFold(s[len(s)-len(want):], want)
	}
	for i < len(runes) {
		c := runes[i]
		nextRune := rune(0)
		if i+1 < len(runes) {
			nextRune = runes[i+1]
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
			if c == '*' && nextRune == '/' {
				current.WriteRune(nextRune)
				inBlockComm = false
				i += 2
				continue
			}
			i++
			continue
		}
		if inString {
			current.WriteRune(c)
			if c == '\'' {
				if nextRune == '\'' {
					current.WriteRune(nextRune)
					i += 2
					continue
				}
				inString = false
			}
			i++
			continue
		}
		if c == '-' && nextRune == '-' {
			current.WriteRune(c)
			current.WriteRune(nextRune)
			inLineComment = true
			i += 2
			continue
		}
		if c == '/' && nextRune == '*' {
			current.WriteRune(c)
			current.WriteRune(nextRune)
			inBlockComm = true
			i += 2
			continue
		}
		if c == '\'' {
			current.WriteRune(c)
			inString = true
			i++
			continue
		}
		// BEGIN/END tracking: only treat BEGIN as opening a frame when
		// the current statement buffer already has non-trivial content
		// (i.e. CREATE TRIGGER ... BEGIN); a bare `BEGIN TRANSACTION`
		// at the head of a statement is treated as ordinary SQL.
		if (c == 'B' || c == 'b') && hasWordBoundaryAhead(runes, i, "BEGIN") {
			if hasIdentPrefix(current.String(), "TRIGGER") || beginDepth > 0 {
				beginDepth++
			}
			current.WriteString("BEGIN")
			i += len("BEGIN")
			continue
		}
		if (c == 'E' || c == 'e') && hasWordBoundaryAhead(runes, i, "END") && beginDepth > 0 {
			beginDepth--
			current.WriteString("END")
			i += len("END")
			continue
		}
		if c == ';' && beginDepth == 0 {
			current.WriteRune(c)
			flush()
			i++
			continue
		}
		current.WriteRune(c)
		i++
	}
	if strings.TrimSpace(current.String()) != "" {
		flush()
	}
	return statements
}

// hasWordBoundaryAhead returns true when runes[i:i+len(word)] matches
// word case-insensitively AND both the preceding rune (if any) and the
// following rune (if any) are non-identifier characters. Used by the
// splitter to detect BEGIN/END tokens without matching mid-identifier
// occurrences (e.g. ENDPOINT).
func hasWordBoundaryAhead(runes []rune, i int, word string) bool {
	if i+len(word) > len(runes) {
		return false
	}
	for k, wr := range word {
		rr := runes[i+k]
		if rr >= 'a' && rr <= 'z' {
			rr -= 32
		}
		if rr != wr {
			return false
		}
	}
	if i > 0 {
		prev := runes[i-1]
		if isIdentRune(prev) {
			return false
		}
	}
	if i+len(word) < len(runes) {
		next := runes[i+len(word)]
		if isIdentRune(next) {
			return false
		}
	}
	return true
}

func isIdentRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'A' && r <= 'Z':
		return true
	case r >= '0' && r <= '9':
		return true
	case r == '_':
		return true
	}
	return false
}
