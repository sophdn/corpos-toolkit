package telemetry_test

import (
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"toolkit/internal/testutil"
)

// This file is the standing guard for the silently-inert-literal-filter
// class — the shape behind both the vault-note dedup bug (fix c9388bcb)
// and chain latent-inert-arm-audit's S1 parse_context finding.
//
// SHAPE. A reader runs `... WHERE col = 'literal'` (or `col IN (...)`).
// If no writer ever emits that literal — e.g. it violates the column's
// CHECK constraint, or the writer was changed to emit a different token —
// the query matches zero rows forever. The arm RUNS; it just never hits.
// No error, no panic; the feature it powers degrades to a silent no-op.
// In-memory unit tests that build the index directly never exercise the
// loader query, so the class ships untested.
//
// GUARD STRATEGY, by column kind:
//
//   - CHECK-governed enum columns (this file): the schema's CHECK IS the
//     authoritative writer contract — a value not in the CHECK can never
//     be inserted, so a reader filtering on it is provably inert. We scan
//     every non-test .go reader under go/internal for literal filters on
//     these columns and assert each literal is admitted by the live CHECK
//     set (read from the migrated schema). Zero per-arm maintenance: a
//     future reader that re-introduces a forbidden literal (e.g. someone
//     adds 'parse_context' back to the skip-rate query) fails this test
//     with a file:line, no registry to update.
//
//   - Free-text columns (NOT here): knowledge_pointers.source_type and
//     grounding_events.action have no CHECK, so there is no admitted-set
//     to validate against. They are guarded by seed+load DB tests that
//     seed the WRITER's literal and assert it reaches the reader — model:
//     arcreview.TestLoadExistingArtifactsForDedupe_IncludesVaultNotes,
//     which fails the moment dedupe.go's source_type literal drifts from
//     what ingestion writes ('vault').
//
// See learnings/general/2026-05-24_lookup-arm-querying-unwritten-literal-
// is-silently-inert.md for the full root-cause writeup and reflexes.

// checkGovernedEnumColumns are the (table, column) pairs whose reader-side
// literal filters this guard validates against the schema CHECK set. Add a
// pair here when a new CHECK-constrained enum column gains reader arms.
var checkGovernedEnumColumns = []struct {
	table  string
	column string
}{
	{"grounding_events", "query_source"},
	{"query_interactions", "click_kind"},
}

var quotedRE = regexp.MustCompile(`'([^']*)'`)

// inOpenRE matches the `IN (` opener of a CHECK list, tolerating arbitrary
// whitespace (migrations format it as both `IN (` and `IN\n(`).
var inOpenRE = regexp.MustCompile(`IN\s*\(`)

// admittedLiterals reads the CHECK (col IN ('a','b',…)) set for a column
// from the live migrated schema. The schema is the source of truth — not a
// hand-copied list — so the guard tracks migrations automatically.
func admittedLiterals(t *testing.T, db *sql.DB, table, column string) map[string]struct{} {
	t.Helper()
	var createSQL string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table,
	).Scan(&createSQL); err != nil {
		t.Fatalf("read schema for %s: %v", table, err)
	}
	colIdx := strings.Index(createSQL, column)
	if colIdx < 0 {
		t.Fatalf("column %q not found in %s schema", column, table)
	}
	after := createSQL[colIdx:]
	loc := inOpenRE.FindStringIndex(after)
	if loc == nil {
		t.Fatalf("no `CHECK (%s IN (...))` clause found in %s schema — guard assumes one", column, table)
	}
	after = after[loc[1]:]
	end := strings.Index(after, ")")
	if end < 0 {
		t.Fatalf("unterminated IN (...) for %s.%s", table, column)
	}
	set := map[string]struct{}{}
	for _, m := range quotedRE.FindAllStringSubmatch(after[:end], -1) {
		set[m[1]] = struct{}{}
	}
	if len(set) == 0 {
		t.Fatalf("parsed an empty CHECK set for %s.%s — parser drift, fix admittedLiterals", table, column)
	}
	return set
}

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// stripLineComment drops a `//` line comment so prose mentioning a literal
// (e.g. a doc comment explaining why 'parse_context' is NOT a valid value)
// is never mistaken for a live filter. The reader SQL in these files lives
// in backtick raw strings and contains no `//`.
func stripLineComment(line string) string {
	if i := strings.Index(line, "//"); i >= 0 {
		return line[:i]
	}
	return line
}

// goInternalDir resolves go/internal from this test file's compile-time
// path (.../go/internal/telemetry/literal_filter_contract_test.go).
func goInternalDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed — cannot locate go/internal")
	}
	return filepath.Dir(filepath.Dir(thisFile))
}

// TestLiteralFilterArms_StayWithinCheckSet walks every non-test reader
// under go/internal and asserts that no literal filtered against a
// CHECK-governed enum column falls outside the schema's admitted set. A
// violation is a silently-inert arm — the vault-note / parse_context class.
func TestLiteralFilterArms_StayWithinCheckSet(t *testing.T) {
	pool := testutil.NewTestDB(t)

	type colMatcher struct {
		table string
		set   map[string]struct{}
		eqRE  *regexp.Regexp
		inRE  *regexp.Regexp
	}
	matchers := map[string]colMatcher{}
	for _, c := range checkGovernedEnumColumns {
		matchers[c.column] = colMatcher{
			table: c.table,
			set:   admittedLiterals(t, pool.DB(), c.table, c.column),
			eqRE:  regexp.MustCompile(c.column + `\s*=\s*'([^']*)'`),
			inRE:  regexp.MustCompile(c.column + `\s+IN\s*\(([^)]*)\)`),
		}
	}

	root := goInternalDir(t)
	var violations []string
	scanned := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		for lineNo, raw := range strings.Split(string(data), "\n") {
			line := stripLineComment(raw)
			for col, m := range matchers {
				lits := m.eqRE.FindAllStringSubmatch(line, -1)
				for _, g := range m.inRE.FindAllStringSubmatch(line, -1) {
					lits = append(lits, quotedRE.FindAllStringSubmatch(g[1], -1)...)
				}
				for _, g := range lits {
					lit := g[1]
					if _, ok := m.set[lit]; !ok {
						rel, _ := filepath.Rel(root, path)
						violations = append(violations, fmt.Sprintf(
							"%s:%d filters %s on %q, which is NOT in the %s CHECK set %v",
							rel, lineNo+1, col, lit, m.table, sortedKeys(m.set)))
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk go/internal: %v", err)
	}
	if scanned == 0 {
		t.Fatalf("scanned 0 .go files under %s — path resolution broke", root)
	}
	if len(violations) > 0 {
		t.Fatalf("silently-inert literal-filter arm(s) — a reader filters on a value the schema CHECK forbids, so it matches zero rows forever (the vault-note / parse_context class):\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// freeTextLiteralFilterColumns are the no-CHECK columns whose reader-side
// literal filters CANNOT be validated against an admitted set (there is none —
// see the GUARD STRATEGY note above). The convention instead mandates a
// seed+load DB test per arm. This guard supplies the missing AUTOMATED
// enforcement for that rule: it fails when a reader filters one of these
// columns on a literal that NO _test.go under go/internal ever writes — the
// silently-inert class for free-text columns. Add a column here when a new
// no-CHECK enum-like column gains reader arms (mirrors checkGovernedEnumColumns).
//
// Scope note: this enforces the FLOOR (the literal is known to at least one
// test), not the ceiling (that the test asserts the arm's OUTPUT). The per-arm
// seed+load test still owns the output assertion — model: arcreview.
// TestLoadExistingArtifactsForDedupe_IncludesVaultNotes. Bug
// free-text-literal-filter-arms-missing-mandated-seed-load-tests: the one-time
// sweep confirmed every current arm (source_type='vault', action='vault_search'
// /'knowledge_search') already has such a test; this guard keeps it true.
var freeTextLiteralFilterColumns = []string{"source_type", "action"}

var doubleQuotedRE = regexp.MustCompile(`"([^"]*)"`)

// collectTestQuotedLiterals returns the set of every single- AND double-quoted
// string literal appearing in any _test.go under root — the "seeding corpus":
// a literal some test knows about, whether seeded via SQL ('vault_search' in a
// raw VALUES string) or via a Go helper (WithGroundingAction("vault_search")).
// Permissive by design: a literal present anywhere in a test counts, so the
// guard errs toward passing and only fires on the genuinely-untested case.
func collectTestQuotedLiterals(t *testing.T, root string) (map[string]struct{}, int) {
	t.Helper()
	lits := map[string]struct{}{}
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		s := string(data)
		for _, m := range quotedRE.FindAllStringSubmatch(s, -1) {
			lits[m[1]] = struct{}{}
		}
		for _, m := range doubleQuotedRE.FindAllStringSubmatch(s, -1) {
			lits[m[1]] = struct{}{}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk for test literals: %v", err)
	}
	return lits, files
}

// TestFreeTextLiteralFilterArms_AreSeededByATest walks every non-test reader
// under go/internal, collects each literal filtered on a free-text column, and
// fails when that literal is written by NO _test.go — the no-CHECK counterpart
// to TestLiteralFilterArms_StayWithinCheckSet. This is the automated
// enforcement the free-text half of the convention previously lacked (it relied
// on a manual sweep, which is exactly how bug free-text-literal-filter-arms-
// missing-mandated-seed-load-tests arose).
func TestFreeTextLiteralFilterArms_AreSeededByATest(t *testing.T) {
	root := goInternalDir(t)

	testLiterals, testFiles := collectTestQuotedLiterals(t, root)
	if testFiles == 0 {
		t.Fatalf("scanned 0 _test.go files under %s — path resolution broke", root)
	}

	eqMatchers := map[string]*regexp.Regexp{}
	inMatchers := map[string]*regexp.Regexp{}
	for _, col := range freeTextLiteralFilterColumns {
		eqMatchers[col] = regexp.MustCompile(`\b` + col + `\s*=\s*'([^']*)'`)
		inMatchers[col] = regexp.MustCompile(`\b` + col + `\s+IN\s*\(([^)]*)\)`)
	}
	// Only string literals that actually carry SQL are scanned — this is what
	// distinguishes a reader arm (`WHERE ge.action = 'vault_search'`, in a
	// backtick SQL string) from prose that merely mentions the work-surface
	// `action` field (e.g. a Hint string `work.action='trained_model_promote'`,
	// a normal Go string with no SQL). Without this an `action` filter on the
	// work-action NAME is a false positive.
	sqlRE := regexp.MustCompile(`(?i)\b(SELECT|UPDATE|DELETE|INSERT|WHERE)\b`)

	type arm struct{ col, lit, loc string }
	var arms []arm
	scanned := 0
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// A genuine parse error fails `go build`; the guard skips the file
			// rather than double-reporting.
			return nil
		}
		scanned++
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			val, uerr := strconv.Unquote(lit.Value)
			if uerr != nil || !sqlRE.MatchString(val) {
				return true // not a SQL-bearing string literal
			}
			for col, eqRE := range eqMatchers {
				hits := eqRE.FindAllStringSubmatch(val, -1)
				for _, g := range inMatchers[col].FindAllStringSubmatch(val, -1) {
					hits = append(hits, quotedRE.FindAllStringSubmatch(g[1], -1)...)
				}
				for _, g := range hits {
					arms = append(arms, arm{col, g[1],
						fmt.Sprintf("%s:%d", rel, fset.Position(lit.Pos()).Line)})
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk go/internal: %v", err)
	}
	if scanned == 0 {
		t.Fatalf("scanned 0 non-test .go files under %s — path resolution broke", root)
	}
	if len(arms) == 0 {
		t.Fatal("found 0 free-text literal-filter arms — scanner drift (expected source_type='vault', action='vault_search', etc.)")
	}

	var violations []string
	for _, a := range arms {
		if _, ok := testLiterals[a.lit]; !ok {
			violations = append(violations, fmt.Sprintf(
				"%s filters %s on %q, but NO _test.go under go/internal writes that literal — add a seed+load DB test that seeds the writer's literal and asserts it reaches the reader (model: arcreview.TestLoadExistingArtifactsForDedupe_IncludesVaultNotes)",
				a.loc, a.col, a.lit))
		}
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("free-text literal-filter arm(s) with no seeding test — the silently-inert class for no-CHECK columns:\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// TestFreeTextLiteralFilterGuard_CatchesUnseededLiteral is the negative control
// proving the membership check is not vacuous: a synthetic literal no test ever
// writes must be absent from the corpus, while a real seeded literal must be
// present.
func TestFreeTextLiteralFilterGuard_CatchesUnseededLiteral(t *testing.T) {
	root := goInternalDir(t)
	testLiterals, _ := collectTestQuotedLiterals(t, root)

	const synthetic = "zzz_never_seeded_freetext_literal_xyzzy"
	if _, ok := testLiterals[synthetic]; ok {
		t.Fatalf("control literal %q is somehow present in a _test.go — pick a more unique token", synthetic)
	}
	if _, ok := testLiterals["vault_search"]; !ok {
		t.Error("'vault_search' should appear in a _test.go (admin/observehttp seed+load tests) — the guard would over-fire if absent")
	}
}

// TestLiteralFilterContract_CatchesForbiddenLiteral is the negative control:
// it proves the guard's membership check actually distinguishes a forbidden
// literal from an admitted one. 'parse_context' is the action NAME of the
// resolution surface, never a grounding_events.query_source value — the
// masked-inert literal removed in chain latent-inert-arm-audit S1. Had any
// reader still filtered on it, TestLiteralFilterArms_StayWithinCheckSet
// above would have failed.
func TestLiteralFilterContract_CatchesForbiddenLiteral(t *testing.T) {
	pool := testutil.NewTestDB(t)
	set := admittedLiterals(t, pool.DB(), "grounding_events", "query_source")

	if _, ok := set["parse_context"]; ok {
		t.Error("'parse_context' must NOT be an admitted query_source value — guard would miss the S1 class")
	}
	if _, ok := set["reference_resolution"]; !ok {
		t.Errorf("'reference_resolution' must be an admitted query_source value; CHECK set = %v", sortedKeys(set))
	}
}
