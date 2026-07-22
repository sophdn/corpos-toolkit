package db_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Schema migrations live in TWO locations that must stay byte-identical:
//
//   - go/internal/db/migrations/        (canonical, consumed by the Go embed)
//   - go/internal/testutil/migrations/  (testutil hermetic mirror; a real
//     copy because Go embed rejects symlinks)
//
// The precommit gate SYNCS canonical→mirror inline, but the sync was the
// ONLY enforcer — nothing verified the two were actually byte-identical, so
// a direct edit to the mirror, or a silent sync failure, could drift them
// (the dual-source-of-truth shape that has bitten other two-copy substrates).
//
// TestMigrationsInSync closes that gap: it walks both directories and
// byte-compares every migration file, failing on ANY drift — a differing
// byte, a file present in one dir but not the other, or a stray extra file.
// Because it is a normal `go test` under ./internal/..., it runs in the gate
// (make -C go test / scripts/precommit.sh), so drift cannot pass the gate.
//
// Regression guard for bug 1035 (migration-dirs-can-silently-drift-no-byte-equality-guard).
func TestMigrationsInSync(t *testing.T) {
	// This test file lives in go/internal/db, so the canonical dir is a
	// sibling "migrations/" and the testutil mirror is "../testutil/migrations/".
	const (
		canonicalDir = "migrations"
		mirrorDir    = "../testutil/migrations"
	)

	canonical := readSQLDir(t, canonicalDir)
	mirror := readSQLDir(t, mirrorDir)

	// Filename-set equality: every canonical file must exist in the mirror,
	// and the mirror must carry no extras.
	for name := range canonical {
		if _, ok := mirror[name]; !ok {
			t.Errorf("migration %q exists in canonical (%s) but is MISSING from the testutil mirror (%s) — run scripts/precommit.sh to re-sync",
				name, canonicalDir, mirrorDir)
		}
	}
	for name := range mirror {
		if _, ok := canonical[name]; !ok {
			t.Errorf("migration %q exists in the testutil mirror (%s) but NOT in canonical (%s) — it is a stray copy; delete it or restore the canonical original",
				name, mirrorDir, canonicalDir)
		}
	}

	// Byte equality for every file present in both.
	for name, canonicalBytes := range canonical {
		mirrorBytes, ok := mirror[name]
		if !ok {
			continue // already reported as missing above
		}
		if !bytes.Equal(canonicalBytes, mirrorBytes) {
			t.Errorf("migration %q has DRIFTED: %s (%d bytes) != %s (%d bytes) — the testutil mirror must be byte-identical to canonical; run scripts/precommit.sh to re-sync",
				name, canonicalDir, len(canonicalBytes), mirrorDir, len(mirrorBytes))
		}
	}
}

// readSQLDir reads every *.sql file in dir, returning a map from filename
// (base name) to file contents. Fails the test if the directory cannot be
// read — a missing migrations dir is itself a drift the gate should catch.
func readSQLDir(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations dir %s: %v", dir, err)
	}
	out := make(map[string][]byte)
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".sql" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read migration %s: %v", path, err)
		}
		out[e.Name()] = data
	}
	if len(out) == 0 {
		t.Fatalf("no .sql migrations found under %s — directory empty or wrong path", dir)
	}
	return out
}
