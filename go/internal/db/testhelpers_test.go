package db_test

import (
	"path/filepath"
	"testing"

	"toolkit/internal/db"
)

// freshPool opens a migrated, throwaway DB in the test's temp dir and
// registers cleanup. Shared by the db-package tests that need a real
// migrated pool (grounding-events, inference-telemetry, …). Relocated
// here from qwen_telemetry_test.go when that file was deleted with the
// retired qwen_invocations writer (chain legacy-telemetry-sink-retirement).
func freshPool(t *testing.T) *db.Pool {
	t.Helper()
	p, err := db.Open(filepath.Join(t.TempDir(), "db_test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}
