package db_test

import (
	"path/filepath"
	"strings"
	"testing"

	"toolkit/internal/db"
)

// db.Open applies the full embedded migration set on a fresh on-disk
// path. The regression target is bug 1326: a new .sql in migrations/
// must land on production via the binary's normal start path, not via
// the Rust benchmark binaries (which were the only callers of
// shared_db::open_pool after the Rust toolkit-server archive).
func TestOpen_AppliesAllMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	p, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { p.Close() })

	// _migrations bookkeeping table exists and has one row per embedded
	// migration. Head row name equals MigrationHead() — the same
	// invariant shared_db::migration_head asserts in the Rust suite.
	var count int
	if err := p.DB().QueryRow("SELECT COUNT(*) FROM _migrations").Scan(&count); err != nil {
		t.Fatalf("count _migrations: %v", err)
	}
	if count == 0 {
		t.Fatal("expected _migrations to have rows after Open")
	}

	head, err := db.MigrationHead()
	if err != nil {
		t.Fatalf("MigrationHead: %v", err)
	}
	var tailName string
	if err := p.DB().QueryRow(
		"SELECT name FROM _migrations ORDER BY id DESC LIMIT 1",
	).Scan(&tailName); err != nil {
		t.Fatalf("read head: %v", err)
	}
	if tailName != head {
		t.Errorf("tail of _migrations = %q, MigrationHead = %q", tailName, head)
	}

	// Spot-check a few load-bearing tables exist post-migration. Pick
	// surfaces from each migration era so a future drop in any era trips.
	// Post-T6 (agent-substrate-crud-retirement, migration 060): the
	// retired CRUD tables (chains/tasks/bugs/benchmark_results/...) are
	// gone; their proj_* projection counterparts are the source of truth.
	for _, table := range []string{
		"projects",
		"proj_chain_status", "proj_current_tasks", "proj_current_bugs",
		"proj_benchmark_results",
		"knowledge_pointers",
		"events",
	} {
		var n int
		if err := p.DB().QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE name = ? AND type IN ('table','view','virtual')",
			table,
		).Scan(&n); err != nil {
			t.Errorf("check %s: %v", table, err)
			continue
		}
		if n == 0 {
			t.Errorf("expected table/view %q to exist after migrations", table)
		}
	}

	// Inverse spot-check: retired tables must NOT exist.
	//   - the eight artifact-lifecycle CRUD tables (agent-substrate-crud-
	//     retirement, migration 060); their proj_* counterparts are canonical.
	//   - the three legacy telemetry sinks (legacy-telemetry-sink-retirement,
	//     Chain 5): qwen_invocations (migration 083, superseded by
	//     inference_invocations) and vault_search_invocations /
	//     kiwix_offload_invocations (migration 047, absorbed into
	//     grounding_events by migration 046). No projection ever folded from
	//     them — proven by chain T1's characterization net before the drop.
	for _, table := range []string{
		"bugs", "tasks", "chains", "task_blockers", "task_dependencies",
		"benchmark_results", "roadmap_items", "suggestions",
		"qwen_invocations", "vault_search_invocations", "kiwix_offload_invocations",
	} {
		var n int
		if err := p.DB().QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE name = ? AND type = 'table'",
			table,
		).Scan(&n); err != nil {
			t.Errorf("check absence of %s: %v", table, err)
			continue
		}
		if n != 0 {
			t.Errorf("retired table %q still present after migrations", table)
		}
	}
}

// Re-opening an already-migrated DB is a no-op — migrations are
// gated on _migrations.name presence, not file mtime.
func TestOpen_IsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idempotent.db")
	p1, err := db.Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	var first int
	if err := p1.DB().QueryRow("SELECT COUNT(*) FROM _migrations").Scan(&first); err != nil {
		t.Fatalf("count after first open: %v", err)
	}
	p1.Close()

	p2, err := db.Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	t.Cleanup(func() { p2.Close() })

	var second int
	if err := p2.DB().QueryRow("SELECT COUNT(*) FROM _migrations").Scan(&second); err != nil {
		t.Fatalf("count after second open: %v", err)
	}
	if first != second {
		t.Errorf("idempotent open changed _migrations count: first=%d second=%d", first, second)
	}
}

// The missing-FTS5 hint format string must mention the build tag and
// both invocation paths so a reader hitting the error has at least two
// actionable next steps. Mirrors the bug-1323 assertion that previously
// lived in testutil_test.go; this is the canonical home now that the
// constant lives in the db package.
func TestMissingFTS5HintFormat(t *testing.T) {
	hint := db.MissingFTS5HintFmt
	for _, want := range []string{
		"sqlite_fts5",
		"make -C go test",
		"-tags sqlite_fts5",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("missing-FTS5 hint must mention %q; got %q", want, hint)
		}
	}
}

func TestIsMissingFTS5Error(t *testing.T) {
	tests := []struct {
		name    string
		errText string
		want    bool
	}{
		{"canonical sqlite error", "apply 020_knowledge_pointers.sql: no such module: fts5", true},
		{"capitalised variant", "No Such Module: FTS5", true},
		{"unrelated sqlite error", "apply 999_widget.sql: syntax error near WIDGET", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := db.IsMissingFTS5Error(stringError(tt.errText)); got != tt.want {
				t.Errorf("IsMissingFTS5Error(%q) = %v, want %v", tt.errText, got, tt.want)
			}
		})
	}
}

type stringError string

func (s stringError) Error() string { return string(s) }

// The SQL splitter must handle the same edge cases the Rust source-of-
// truth handles (`crates/shared-db/src/lib.rs::split_sql_statements`),
// because future migrations may use any of these constructs and the
// runner has to apply them statement-by-statement. These tests are
// reachable via the package-internal export below; the export is
// test-only to avoid leaking parser internals to other packages.
func TestSplitSQLStatements(t *testing.T) {
	tests := []struct {
		name     string
		sql      string
		wantLen  int
		mustHave []string
	}{
		{
			name:     "ignores semicolons in line comments",
			sql:      "-- header; with semicolon\nCREATE TABLE foo (id INTEGER);\n-- trailing; comment\nCREATE TABLE bar (id INTEGER);",
			wantLen:  2,
			mustHave: []string{"CREATE TABLE foo", "CREATE TABLE bar"},
		},
		{
			name:    "ignores semicolons in block comments",
			sql:     "/* a; b; c */\nCREATE TABLE foo (id INTEGER);",
			wantLen: 1,
		},
		{
			name:     "ignores semicolons inside single-quoted strings",
			sql:      "INSERT INTO t VALUES ('a;b;c'); INSERT INTO t VALUES ('x');",
			wantLen:  2,
			mustHave: []string{"'a;b;c'", "'x'"},
		},
		{
			name: "handles trigger BEGIN END as one statement",
			sql: "CREATE TABLE t (id INTEGER);\n" +
				"CREATE TRIGGER trg AFTER INSERT ON t BEGIN\n" +
				"  INSERT INTO t VALUES (NEW.id + 1);\nEND;\n" +
				"CREATE TABLE u (id INTEGER);",
			wantLen:  3,
			mustHave: []string{"CREATE TRIGGER", "END"},
		},
		{
			name:    "BEGIN at statement start is not a trigger depth opener",
			sql:     "BEGIN;\nINSERT INTO t VALUES (1);\nCOMMIT;",
			wantLen: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nonEmpty(db.SplitSQLStatementsForTest(tt.sql))
			if len(got) != tt.wantLen {
				t.Fatalf("split len = %d, want %d: %q", len(got), tt.wantLen, got)
			}
			for _, want := range tt.mustHave {
				found := false
				for _, s := range got {
					if strings.Contains(s, want) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("missing %q in split output: %q", want, got)
				}
			}
		})
	}
}

func nonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}
