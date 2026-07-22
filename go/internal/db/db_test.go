package db_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"toolkit/internal/db"
)

// TestWithWrite_NestedReentrancyReturnsErrorNotDeadlock is the self-
// enforcing contract for the bug
// `forge-edit-in-batch-deadlocks-via-nested-pool-withwrite-in-onedit-notifier`:
// a write path invoked INSIDE another WithWrite (same goroutine) must fail
// fast with ErrNestedWrite, not block forever on the non-reentrant write
// mutex. Pre-guard this deadlocks (the inner Lock waits on a lock the
// outer call holds), so the test is timeout-guarded; the 505s production
// hang that motivated this would have surfaced here as an instant error.
func TestWithWrite_NestedReentrancyReturnsErrorNotDeadlock(t *testing.T) {
	p := newTmpPool(t)

	done := make(chan error, 1)
	go func() {
		// Outer write tx; inside it, re-enter WithWrite on the SAME
		// goroutine — the exact shape of the OnEdit-notifier deadlock.
		done <- p.WithWrite(context.Background(), func(_ *sql.Tx) error {
			return p.WithWrite(context.Background(), func(_ *sql.Tx) error {
				return nil
			})
		})
	}()

	select {
	case err := <-done:
		if !errors.Is(err, db.ErrNestedWrite) {
			t.Fatalf("nested WithWrite: got %v, want ErrNestedWrite", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nested WithWrite deadlocked (re-entrancy guard absent); did not return in 10s")
	}

	// The guard must release the mutex + clear the owner on rejection, so
	// the pool stays usable for subsequent (non-nested) writes.
	if err := p.WithWrite(context.Background(), func(_ *sql.Tx) error { return nil }); err != nil {
		t.Fatalf("pool unusable after nested-write rejection: %v", err)
	}
}

func newTmpPool(t *testing.T) *db.Pool {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	p, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func TestOpen_WALMode(t *testing.T) {
	p := newTmpPool(t)
	var mode string
	if err := p.DB().QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode: want wal, got %q", mode)
	}
}

func TestOpen_BadPath(t *testing.T) {
	_, err := db.Open("/no/such/dir/test.db")
	if err == nil {
		t.Error("expected error for invalid path, got nil")
	}
}

func TestWithWrite_CommitAndRollback(t *testing.T) {
	p := newTmpPool(t)
	ctx := context.Background()

	if _, err := p.DB().Exec("CREATE TABLE kv (k TEXT PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}

	tests := []struct {
		name    string
		key     string
		val     string
		wantErr bool
		fail    bool
	}{
		{name: "commit on success", key: "a", val: "1", wantErr: false, fail: false},
		{name: "rollback on error", key: "b", val: "2", wantErr: true, fail: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := p.WithWrite(ctx, func(tx *sql.Tx) error {
				if _, err := tx.Exec("INSERT INTO kv VALUES (?, ?)", tt.key, tt.val); err != nil {
					return err
				}
				if tt.fail {
					return fmt.Errorf("simulated failure")
				}
				return nil
			})

			if (err != nil) != tt.wantErr {
				t.Errorf("WithWrite error = %v, wantErr %v", err, tt.wantErr)
			}

			var count int
			_ = p.DB().QueryRow("SELECT COUNT(*) FROM kv WHERE k=?", tt.key).Scan(&count)
			if tt.fail && count != 0 {
				t.Errorf("rollback expected: key %q should not exist", tt.key)
			}
			if !tt.fail && count != 1 {
				t.Errorf("commit expected: key %q should exist", tt.key)
			}
		})
	}
}

func TestWithWrite_SerializesConcurrentWrites(t *testing.T) {
	p := newTmpPool(t)
	ctx := context.Background()

	if _, err := p.DB().Exec("CREATE TABLE cnt (n INTEGER)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := p.DB().Exec("INSERT INTO cnt VALUES (0)"); err != nil {
		t.Fatalf("insert seed: %v", err)
	}

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			_ = p.WithWrite(ctx, func(tx *sql.Tx) error {
				_, err := tx.Exec("UPDATE cnt SET n = n + 1")
				return err
			})
		}()
	}
	wg.Wait()

	var n int
	if err := p.DB().QueryRow("SELECT n FROM cnt").Scan(&n); err != nil {
		t.Fatalf("read count: %v", err)
	}
	if n != goroutines {
		t.Errorf("serialized writes: want %d, got %d", goroutines, n)
	}
}

func TestConcurrentReads_NoDeadlock(t *testing.T) {
	p := newTmpPool(t)

	if _, err := p.DB().Exec("CREATE TABLE data (v INTEGER)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	for i := range 5 {
		if _, err := p.DB().Exec("INSERT INTO data VALUES (?)", i); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			rows, err := p.DB().QueryContext(context.Background(), "SELECT v FROM data")
			if err != nil {
				t.Errorf("query: %v", err)
				return
			}
			defer rows.Close()
			for rows.Next() {
				var v int
				_ = rows.Scan(&v)
			}
		}()
	}
	wg.Wait()
}
