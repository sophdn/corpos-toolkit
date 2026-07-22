package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	_ "modernc.org/sqlite"
)

// busyTimeoutMS is how long a connection waits for a competing writer to
// release the database write lock before giving up with SQLITE_BUSY. Set
// high enough that cross-process write contention — the canonical :3000
// daemon, a worktree's ephemeral per-branch server, and stdio MCP sessions
// all sharing data/toolkit.db under multi-agent orchestration — serializes
// by *waiting* rather than erroring. See docs/MULTI_AGENT_WORKTREE_WORKFLOW.md
// §"Shared-DB concurrency model under parallel worktree agents".
const busyTimeoutMS = 5000

// ErrNestedWrite is returned by WithWrite when the calling goroutine is
// already inside a WithWrite call on the same Pool. Writes are serialized
// by a NON-reentrant mutex, so a nested WithWrite would block forever on a
// lock its own call stack holds. Returning this error instead turns that
// whole class of bug — a write-path helper invoked from inside another
// write tx — into an immediate, named failure rather than an invisible
// hang (bug forge-edit-in-batch-deadlocks-via-nested-pool-withwrite-in-onedit-notifier
// cost a 505s production deadlock before this guard existed). The fix is
// always the same: thread the outer *sql.Tx through to the inner write
// (use the package's *InTx variant) instead of re-entering WithWrite.
var ErrNestedWrite = errors.New("db: nested WithWrite on the same goroutine — a write path was called inside another write tx; this would deadlock the non-reentrant write mutex. Thread the outer *sql.Tx through (use an *InTx variant) instead of re-entering WithWrite")

// Pool wraps a *sql.DB with serialized write access.
type Pool struct {
	db *sql.DB
	mu sync.Mutex
	// ownerGID is the goroutine id currently holding mu, or 0 when mu is
	// unheld. Written under mu (the holder sets it after Lock, clears it
	// before Unlock); read locklessly via atomic by WithWrite's re-entrancy
	// guard. Lets WithWrite detect a same-goroutine nested call and fail
	// with ErrNestedWrite instead of deadlocking on the non-reentrant mu.
	ownerGID atomic.Int64
}

// Open opens the SQLite database at path, enables WAL mode, applies
// any pending embedded migrations, and returns a Pool.
//
// Auto-migration on Open restores the pre-T68 invariant that the
// running binary keeps the schema current. Before bug 1326 the only
// path that ran migrations was shared_db::open_pool, called from the
// archived Rust toolkit-server and a handful of benchmark binaries —
// so new migrations sat dormant on production until somebody ran a
// benchmark. See CONVENTIONS.md §"Migration Strategy".
//
// The connection DSN carries the cross-process-safe write settings
// (busy_timeout + BEGIN IMMEDIATE) that let multiple toolkit-server
// processes share one database file without losing or corrupting writes
// — see [dsn] and docs/MULTI_AGENT_WORKTREE_WORKFLOW.md.
func Open(path string) (*Pool, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}
	pool := &Pool{db: db}
	if err := RunMigrations(pool); err != nil {
		db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}
	return pool, nil
}

// dsn appends the connection parameters that make a shared SQLite file
// safe for concurrent writers in *different processes* — the multi-agent
// worktree case where the :3000 daemon, a per-worktree server, and stdio
// sessions all open the same data/toolkit.db:
//
//   - _pragma=busy_timeout(ms): a writer that finds the database locked by
//     another process WAITS up to busyTimeoutMS for the lock instead of
//     failing immediately with SQLITE_BUSY. Turns cross-process write
//     contention into serialization-by-waiting rather than a lost forge.
//     (The pure-Go modernc.org/sqlite driver spells busy_timeout as a
//     _pragma= DSN param; the CGo mattn driver spelled it _busy_timeout=.)
//   - _txlock=immediate: every BeginTx issues BEGIN IMMEDIATE, acquiring
//     the write lock at transaction start — *before* a fold's read-then-
//     write MAX(id)+1 step. A deferred tx instead takes a read snapshot,
//     then fails to upgrade to a writer (SQLITE_BUSY_SNAPSHOT) if another
//     process committed since the snapshot; worse, two deferred writers
//     that both read the same MAX(id) under their own snapshots could
//     assign colliding primary keys. Taking the write lock up front makes
//     each writer observe a post-commit snapshot, so MAX(id)+1 is
//     collision-free across processes.
//
// WAL mode (set separately via PRAGMA, persisted in the file header) lets
// readers proceed without blocking the single writer. Reads in this
// package go through [Pool.DB] in autocommit mode and are unaffected by
// _txlock — the only BeginTx caller is [Pool.WithWrite] (writes).
func dsn(path string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return fmt.Sprintf("%s%s_txlock=immediate&_pragma=busy_timeout(%d)", path, sep, busyTimeoutMS)
}

// DB returns the underlying *sql.DB for read-only queries.
// Callers must not use it for writes; use WithWrite instead.
func (p *Pool) DB() *sql.DB {
	return p.db
}

// WithWrite acquires the write mutex and calls fn inside a transaction.
// The transaction is committed on nil return from fn; rolled back otherwise.
func (p *Pool) WithWrite(ctx context.Context, fn func(*sql.Tx) error) error {
	// Re-entrancy guard: if this goroutine already holds the write mutex,
	// a second WithWrite would block forever on its own lock. Fail fast
	// with a named, actionable error instead of deadlocking. gid > 0
	// gates the check so a (practically impossible) stack-parse miss
	// degrades to the old blocking behavior rather than a false positive.
	gid := goroutineID()
	if gid > 0 && p.ownerGID.Load() == gid {
		return ErrNestedWrite
	}

	p.mu.Lock()
	p.ownerGID.Store(gid)
	defer func() {
		p.ownerGID.Store(0)
		p.mu.Unlock()
	}()

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Close closes the underlying database connection.
func (p *Pool) Close() error {
	return p.db.Close()
}

// goroutineID returns the current goroutine's id by parsing the runtime
// stack header ("goroutine <id> [running]:\n..."). This is the standard
// technique for the one place Go legitimately needs goroutine identity:
// deadlock detection on a non-reentrant lock. It is used ONLY by
// WithWrite's re-entrancy guard — not a general-purpose facility.
// internal/db is the concentrated database/sql + sqlite boundary package
// (and is forbidigo-exempt), so this low-level parse is contained here.
// Returns -1 on the practically-impossible parse miss; the caller gates
// the guard on a positive id so a miss degrades safely.
func goroutineID() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	s := bytes.TrimPrefix(buf[:n], []byte("goroutine "))
	if i := bytes.IndexByte(s, ' '); i >= 0 {
		s = s[:i]
	}
	id, err := strconv.ParseInt(string(s), 10, 64)
	if err != nil {
		return -1
	}
	return id
}

// Queryer abstracts the read-method subset shared by *sql.DB and
// *sql.Tx. Handlers that need to compose inside an outer write tx
// (work.HandleBatch's tx-aware path) thread a Queryer through their
// read helpers so the same call site can run against either a free-
// standing DB connection or the batch's pending-write tx — without
// the helper needing to know which. Lives in package db because the
// dispatch-layer forbidigo `any` rule restricts bare-any signatures
// to the concentrated stdlib-boundary packages (internal/db,
// internal/dispatch); the stdlib's *sql.Row / *sql.Rows already
// require `...any` so the interface can't be expressed without it.
type Queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}
