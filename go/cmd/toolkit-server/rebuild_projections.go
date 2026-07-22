package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/obs"
	"toolkit/internal/projections"

	// Side-effect import: registers the projections at init() time.
	_ "toolkit/internal/projections"
)

const (
	// snapshotKeepCount is how many auto-snapshots to retain in the
	// rotation. Older ones get deleted on each rebuild. Picked as a
	// safety net — 10 invocations give the operator ~weeks of recovery
	// headroom for typical operational cadence.
	snapshotKeepCount = 10

	// snapshotSuffix is the filename pattern for auto-written snapshots.
	// The full filename is <db_basename>.snapshot-pre-rebuild-<ISO8601>.db,
	// e.g. toolkit.db.snapshot-pre-rebuild-2026-05-22T22-30-00Z.db.
	snapshotPrefix = ".snapshot-pre-rebuild-"
)

// runRebuildProjections drives the `rebuild-projections` subcommand.
//
// Modes:
//
//  1. Default — auto-snapshot via VACUUM INTO, truncate all proj_*
//     tables, fold every event, run a state-diff check, restore from
//     snapshot if a regression direction is detected. The auto-snapshot
//     directory is the same dir as --db, with a rotation that keeps the
//     last [snapshotKeepCount] snapshots.
//
//  2. --from-snapshot=PATH — open the snapshot DB via ATTACH, copy each
//     proj_* row verbatim, then fold ONLY events with
//     event_id > the snapshot's max(event_id). Resumes from the
//     snapshot's frozen-in-time state instead of folding everything
//     from scratch.
//
//  3. --from-event=ID — incremental from a specific event_id (existing
//     mode; preserved for back-compat).
//
// Safety:
//
//   - --no-snapshot disables the auto-snapshot step.
//   - --force-allow-regression bypasses the post-rebuild state-diff
//     check. Use sparingly; the check exists precisely to catch the
//     2026-05-22 incident shape (rebuild from an incomplete events
//     table silently wipes terminal state).
//
// Exit code 0 on success; 1 on any error (logged to stderr); 1 on a
// detected regression without --force-allow-regression.
func runRebuildProjections(args []string) int {
	fs := flag.NewFlagSet("rebuild-projections", flag.ContinueOnError)
	var (
		dbPath, projectionName, fromEvent, fromSnapshot string
		noSnapshot, forceAllowRegression                bool
	)
	fs.StringVar(&dbPath, "db", "", "path to toolkit SQLite database (required)")
	fs.StringVar(&projectionName, "projection", "", "rebuild only this projection (default: all)")
	fs.StringVar(&fromEvent, "from-event", "", "incremental replay from this event_id (default: full snapshot)")
	fs.StringVar(&fromSnapshot, "from-snapshot", "", "seed proj_* tables from this snapshot DB, then fold events newer than its max event_id")
	fs.BoolVar(&noSnapshot, "no-snapshot", false, "skip the pre-rebuild auto-snapshot step (default: auto-snapshot enabled)")
	fs.BoolVar(&forceAllowRegression, "force-allow-regression", false, "bypass the post-rebuild state-diff regression guard")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: toolkit-server rebuild-projections --db=PATH [--projection=NAME] [--from-event=ID] [--from-snapshot=PATH] [--no-snapshot] [--force-allow-regression]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if dbPath == "" {
		fs.Usage()
		fmt.Fprintln(os.Stderr, "\nerror: --db is required")
		return 1
	}
	if fromEvent != "" && fromSnapshot != "" {
		fmt.Fprintln(os.Stderr, "error: --from-event and --from-snapshot are mutually exclusive")
		return 1
	}

	pool, err := db.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		return 1
	}
	defer pool.Close()

	var names []string
	if projectionName != "" {
		if _, ok := projections.Get(projectionName); !ok {
			fmt.Fprintf(os.Stderr, "unknown projection: %q\n", projectionName)
			fmt.Fprintln(os.Stderr, "registered:", joinedNames(projections.All()))
			return 1
		}
		names = []string{projectionName}
	}

	ctx := context.Background()

	// Capture pre-rebuild counts BEFORE any mutation so the state-diff
	// check has a baseline. Skipping the diff entirely (per-projection
	// runs OR --from-event partial-replay path) means the diff captures
	// less meaningful information; the guard fires only on full rebuilds.
	doStateDiff := projectionName == "" && fromEvent == ""
	var preCounts stateCounts
	if doStateDiff {
		preCounts, err = readStateCounts(ctx, pool)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read pre-rebuild counts: %v\n", err)
			return 1
		}
	}

	// Auto-snapshot before mutating, unless explicitly opted out.
	// The snapshot path is needed downstream by the regression-restore
	// branch, so capture it here.
	var snapshotPath string
	if !noSnapshot {
		snapshotPath, err = writeAutoSnapshot(ctx, pool, dbPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "auto-snapshot: %v\n", err)
			return 1
		}
		fmt.Printf("→ snapshot: %s\n", snapshotPath)
		if err := rotateSnapshots(filepath.Dir(dbPath), filepath.Base(dbPath)); err != nil {
			// Rotation failure is logged but non-fatal — the snapshot
			// itself succeeded, which is the load-bearing safety step.
			fmt.Fprintf(os.Stderr, "warn: rotate snapshots: %v\n", err)
		}
	}

	switch {
	case fromSnapshot != "":
		if rc := rebuildFromSnapshot(ctx, pool, fromSnapshot); rc != 0 {
			return rc
		}
	case fromEvent != "":
		if rc := rebuildFromEvent(ctx, pool, names, fromEvent); rc != 0 {
			return rc
		}
	default:
		if rc := rebuildFullSnapshot(ctx, pool, names); rc != 0 {
			return rc
		}
	}

	if !doStateDiff {
		return 0
	}

	postCounts, err := readStateCounts(ctx, pool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read post-rebuild counts: %v\n", err)
		return 1
	}
	regressions := preCounts.regressionsVs(postCounts)
	if len(regressions) == 0 {
		fmt.Printf("→ state-diff: clean (open bugs %d→%d, pending tasks %d→%d, open chains %d→%d)\n",
			preCounts.OpenBugs, postCounts.OpenBugs,
			preCounts.PendingTasks, postCounts.PendingTasks,
			preCounts.OpenChains, postCounts.OpenChains)
		return 0
	}

	fmt.Fprintln(os.Stderr, "── REGRESSION DETECTED ──")
	for _, line := range regressions {
		fmt.Fprintln(os.Stderr, "  "+line)
	}
	if forceAllowRegression {
		fmt.Fprintln(os.Stderr, "→ --force-allow-regression set; keeping the regressed state")
		return 0
	}
	if snapshotPath == "" {
		fmt.Fprintln(os.Stderr, "→ no snapshot to restore from (--no-snapshot was set); leaving DB in regressed state; exiting non-zero")
		return 1
	}
	fmt.Fprintf(os.Stderr, "→ restoring from snapshot %s\n", snapshotPath)
	if err := restoreFromSnapshot(ctx, pool, snapshotPath); err != nil {
		fmt.Fprintf(os.Stderr, "restore failed: %v\n", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "→ restore complete; pre-rebuild state preserved")
	return 1
}

// stateCounts holds the three observability axes the post-rebuild guard
// watches. A regression direction is "more open bugs OR more pending
// tasks OR more open chains" — those are the columns the 2026-05-22
// incident flipped on. The dashboard's main counts derive from these
// same projections; if any axis regresses, downstream views become
// silently misleading.
type stateCounts struct {
	OpenBugs     int64
	PendingTasks int64
	OpenChains   int64
}

func readStateCounts(ctx context.Context, pool *db.Pool) (stateCounts, error) {
	var c stateCounts
	rows := pool.DB().QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM proj_current_bugs WHERE status='open'),
			(SELECT COUNT(*) FROM proj_current_tasks WHERE status='pending'),
			(SELECT COUNT(*) FROM proj_chain_status WHERE status='open')`)
	if err := rows.Scan(&c.OpenBugs, &c.PendingTasks, &c.OpenChains); err != nil {
		return stateCounts{}, fmt.Errorf("count axes: %w", err)
	}
	return c, nil
}

// regressionsVs returns one diff line per axis that REGRESSED (i.e.,
// post-rebuild has MORE rows than pre-rebuild on that axis). Returns
// an empty slice when the post-rebuild state is healthy.
func (pre stateCounts) regressionsVs(post stateCounts) []string {
	var out []string
	if post.OpenBugs > pre.OpenBugs {
		out = append(out, fmt.Sprintf("open bugs:     pre=%d post=%d  Δ=+%d  (more open = formerly-terminal bugs regressed)",
			pre.OpenBugs, post.OpenBugs, post.OpenBugs-pre.OpenBugs))
	}
	if post.PendingTasks > pre.PendingTasks {
		out = append(out, fmt.Sprintf("pending tasks: pre=%d post=%d  Δ=+%d  (more pending = formerly-closed tasks regressed)",
			pre.PendingTasks, post.PendingTasks, post.PendingTasks-pre.PendingTasks))
	}
	if post.OpenChains > pre.OpenChains {
		out = append(out, fmt.Sprintf("open chains:   pre=%d post=%d  Δ=+%d  (more open = formerly-closed chains regressed)",
			pre.OpenChains, post.OpenChains, post.OpenChains-pre.OpenChains))
	}
	return out
}

// writeAutoSnapshot writes a snapshot to <dir(db)>/<basename(db)><snapshotPrefix><UTC-ISO8601>.db
// via VACUUM INTO. Returns the snapshot's on-disk path. VACUUM INTO is
// safer than file-copy: it produces a defragmented, fully-committed
// SQLite DB even when the source has open writes.
func writeAutoSnapshot(ctx context.Context, pool *db.Pool, dbPath string) (string, error) {
	dir := filepath.Dir(dbPath)
	base := filepath.Base(dbPath)
	// SQLite's default time format uses ':' which is fine on POSIX
	// filesystems; substitute '-' for cross-platform sanity and
	// readability in `ls`.
	ts := time.Now().UTC().Format("2006-01-02T15-04-05Z")
	snapPath := filepath.Join(dir, base+snapshotPrefix+ts+".db")

	if _, err := pool.DB().ExecContext(ctx, "VACUUM INTO ?", snapPath); err != nil {
		return "", fmt.Errorf("VACUUM INTO %s: %w", snapPath, err)
	}
	return snapPath, nil
}

// rotateSnapshots keeps the [snapshotKeepCount] most recent snapshots
// in `dir` matching `<dbBasename><snapshotPrefix>*.db`. Older snapshots
// are deleted. Errors from individual deletes are logged but don't
// fail the rotation as a whole — a failed delete is non-fatal
// (disk-space pressure is a soft signal, not a hard correctness one).
func rotateSnapshots(dir, dbBasename string) error {
	pattern := filepath.Join(dir, dbBasename+snapshotPrefix+"*.db")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("glob %s: %w", pattern, err)
	}
	if len(matches) <= snapshotKeepCount {
		return nil
	}
	// Sort lexicographically — the ISO 8601 timestamp suffix makes
	// this equivalent to chronological order, oldest first.
	sort.Strings(matches)
	toDelete := matches[:len(matches)-snapshotKeepCount]
	for _, p := range toDelete {
		if err := os.Remove(p); err != nil {
			obs.L().Warn("rotate snapshot: remove failed",
				slog.String("path", p), slog.String("err", err.Error()))
		}
	}
	return nil
}

// restoreFromSnapshot brings the pool's DB back to the snapshot's
// state by ATTACHing the snapshot + copying every proj_* row + the
// watermark table verbatim. Used when the post-rebuild state-diff
// detects a regression and --force-allow-regression isn't set.
//
// ATTACH is issued OUTSIDE the write tx because SQLite rejects DETACH
// while a transaction is active on the attached schema; running both
// outside the tx lets the connection-pool lifecycle clean up cleanly.
func restoreFromSnapshot(ctx context.Context, pool *db.Pool, snapPath string) error {
	if _, err := pool.DB().ExecContext(ctx, "ATTACH DATABASE ? AS snap", snapPath); err != nil {
		return fmt.Errorf("ATTACH %s: %w", snapPath, err)
	}
	defer func() {
		if _, err := pool.DB().ExecContext(ctx, "DETACH DATABASE snap"); err != nil {
			obs.L().Warn("restore: DETACH failed",
				slog.String("err", err.Error()))
		}
	}()

	return pool.WithWrite(ctx, func(tx *sql.Tx) error {
		for _, p := range projections.All() {
			tbl := p.TableName()
			if _, err := tx.ExecContext(ctx, "DELETE FROM "+tbl); err != nil {
				return fmt.Errorf("truncate %s: %w", tbl, err)
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO "+tbl+" SELECT * FROM snap."+tbl); err != nil {
				return fmt.Errorf("copy snap.%s: %w", tbl, err)
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM projections_watermark"); err != nil {
			return fmt.Errorf("truncate watermark: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO projections_watermark SELECT * FROM snap.projections_watermark"); err != nil {
			return fmt.Errorf("copy snap.projections_watermark: %w", err)
		}
		return nil
	})
}

// rebuildFromSnapshot seeds proj_* tables from a snapshot's projection
// rows, then folds events newer than the snapshot's max event_id.
// Cheaper than a full rebuild when the snapshot is recent + the events
// table since then is small.
func rebuildFromSnapshot(ctx context.Context, pool *db.Pool, snapPath string) int {
	// Pre-flight: verify the snapshot file exists; surface a clean
	// error rather than an opaque ATTACH failure.
	if _, err := os.Stat(snapPath); err != nil {
		fmt.Fprintf(os.Stderr, "snapshot path: %v\n", err)
		return 1
	}

	// ATTACH outside the write tx — SQLite rejects DETACH while a tx
	// is active on the attached schema. See restoreFromSnapshot for
	// the same pattern.
	if _, err := pool.DB().ExecContext(ctx, "ATTACH DATABASE ? AS snap", snapPath); err != nil {
		fmt.Fprintf(os.Stderr, "ATTACH %s: %v\n", snapPath, err)
		return 1
	}
	defer func() {
		if _, err := pool.DB().ExecContext(ctx, "DETACH DATABASE snap"); err != nil {
			obs.L().Warn("rebuildFromSnapshot: DETACH failed",
				slog.String("err", err.Error()))
		}
	}()

	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		// Read the snapshot's chronological max — defines the boundary
		// between "covered by snapshot" and "needs folding". Ordering on
		// (ts, event_id) per the invariant in go/internal/events/doc.go;
		// event_id-alone would pick up a synthetic-backfill row at the
		// lex-top with an old ts and silently re-fold real events.
		var snapMaxEventID, snapMaxTs sql.NullString
		if err := tx.QueryRowContext(ctx,
			`SELECT event_id, ts FROM snap.events ORDER BY ts DESC, event_id DESC LIMIT 1`,
		).Scan(&snapMaxEventID, &snapMaxTs); err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("read snap.events max: %w", err)
		}

		// Truncate target proj_* + watermark, then INSERT…SELECT FROM
		// snap.proj_*. This brings the target to the snapshot's exact
		// state.
		for _, p := range projections.All() {
			tbl := p.TableName()
			if _, err := tx.ExecContext(ctx, "DELETE FROM "+tbl); err != nil {
				return fmt.Errorf("truncate %s: %w", tbl, err)
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO "+tbl+" SELECT * FROM snap."+tbl); err != nil {
				return fmt.Errorf("seed from snap.%s: %w", tbl, err)
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM projections_watermark"); err != nil {
			return fmt.Errorf("truncate watermark: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO projections_watermark SELECT * FROM snap.projections_watermark"); err != nil {
			return fmt.Errorf("seed watermark: %w", err)
		}

		// Now fold events with event_id > snap_max_event_id. If the
		// snapshot has no events, fold every event.
		var rows *sql.Rows
		var err error
		if snapMaxEventID.Valid {
			// Tuple-greater predicate: events strictly after the
			// snapshot's chronological max. The same invariant that
			// motivates the ORDER BY also motivates this WHERE shape —
			// `event_id > ?` alone would let synthetic-backfill rows
			// (lex-greater than ULIDs) pass through unfolded.
			rows, err = tx.QueryContext(ctx, `
				SELECT event_id, ts, actor_kind, actor_id, type,
				       entity_kind, entity_slug, entity_project_id,
				       payload, rationale, caused_by_event_id, related_entities,
				       span_id, schema_version
				FROM events
				WHERE ts > ? OR (ts = ? AND event_id > ?)
				ORDER BY ts ASC, event_id ASC`,
				snapMaxTs.String, snapMaxTs.String, snapMaxEventID.String)
		} else {
			rows, err = tx.QueryContext(ctx, `
				SELECT event_id, ts, actor_kind, actor_id, type,
				       entity_kind, entity_slug, entity_project_id,
				       payload, rationale, caused_by_event_id, related_entities,
				       span_id, schema_version
				FROM events
				ORDER BY ts ASC, event_id ASC`)
		}
		if err != nil {
			return fmt.Errorf("query new events: %w", err)
		}
		defer rows.Close()

		var count int
		var lastEventID, lastTs string
		for rows.Next() {
			evt, err := scanRawEvent(rows)
			if err != nil {
				return err
			}
			if err := projections.FoldAll(ctx, tx, evt); err != nil {
				return fmt.Errorf("fold event %s: %w", evt.EventID, err)
			}
			lastEventID = evt.EventID
			lastTs = evt.Ts
			count++
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("event-walk rows.Err: %w", err)
		}

		// Advance each projection's watermark to the last folded event.
		// If zero events were folded, the watermark stays at the snapshot's
		// value (which was already copied in above).
		if lastEventID != "" {
			for _, p := range projections.All() {
				if err := projections.WriteWatermark(ctx, tx, p.Name(), lastEventID, lastTs); err != nil {
					return fmt.Errorf("watermark %s: %w", p.Name(), err)
				}
			}
		}

		fmt.Printf("seeded from snapshot, folded %d event(s) > snapshot max\n", count)
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "rebuild from snapshot: %v\n", err)
		return 1
	}
	return 0
}

// scanRawEvent reads a single events row from rows.Scan into the
// projections.RawEvent shape used by FoldAll. Shared between the
// snapshot-mode replay and the from-event mode. (Pre-existing
// `rebuildFromEvent` has a copy; this version centralises the
// boilerplate.)
func scanRawEvent(rows *sql.Rows) (projections.RawEvent, error) {
	var (
		eid, ts, ak, aid, typ, ek, es, span string
		epid, rat, cbe                      sql.NullString
		payload, related                    []byte
		sv                                  int
	)
	if err := rows.Scan(&eid, &ts, &ak, &aid, &typ, &ek, &es, &epid,
		&payload, &rat, &cbe, &related, &span, &sv); err != nil {
		return projections.RawEvent{}, fmt.Errorf("scan event: %w", err)
	}
	return projections.RawEvent{
		EventID:         eid,
		Ts:              ts,
		ActorKind:       ak,
		ActorID:         aid,
		Type:            typ,
		EntityKind:      ek,
		EntitySlug:      es,
		EntityProjectID: nullStringPtr(epid),
		Payload:         json.RawMessage(payload),
		Rationale:       nullStringPtr(rat),
		CausedByEventID: nullStringPtr(cbe),
		RelatedEntities: json.RawMessage(related),
		SpanID:          span,
		SchemaVersion:   sv,
	}, nil
}

// rebuildFullSnapshot drives [projections.RebuildAll] inside one write
// tx. Output: one line per projection to stdout.
func rebuildFullSnapshot(ctx context.Context, pool *db.Pool, names []string) int {
	var results []projections.RebuildResult
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		var err error
		results, err = projections.RebuildAll(ctx, tx, names)
		return err
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "rebuild: %v\n", err)
		return 1
	}
	for _, r := range results {
		fmt.Printf("%s: rebuilt %d rows, watermark=%s\n", r.Name, r.Rows, displayWatermark(r.Watermark))
	}
	return 0
}

// rebuildFromEvent handles the --from-event=ID branch. For each target
// projection: read events whose event_id >= ID in event_id order and
// fold them; for each event, the fold path refreshes the entity row
// from current CRUD (same idempotent fold as the live-emit hook). The
// watermark advances to the highest event_id replayed.
//
// This does NOT truncate the projection table — the assumption is the
// projection state is approximately correct and only some rows touched
// by post-ID events need re-derivation. Tests + docs/PROJECTIONS.md
// describe the precise semantics; the live-emit hook is the more common
// path.
func rebuildFromEvent(ctx context.Context, pool *db.Pool, names []string, fromEvent string) int {
	targets := projections.All()
	if len(names) > 0 {
		targets = nil
		for _, n := range names {
			p, _ := projections.Get(n)
			targets = append(targets, p)
		}
	}

	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		// --from-event takes a single event_id but the user means "from
		// this point onward chronologically". Resolve fromEvent's ts so
		// the filter can use the canonical (ts, event_id) tuple — see
		// go/internal/events/doc.go §Invariant. event_id-alone here
		// would skip every ULID-style event when fromEvent happens to
		// be a synthetic-backfill row (lex-greater than ULIDs).
		var fromTs string
		if err := tx.QueryRowContext(ctx,
			`SELECT ts FROM events WHERE event_id = ?`, fromEvent,
		).Scan(&fromTs); err != nil {
			return fmt.Errorf("resolve --from-event %q: %w", fromEvent, err)
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT event_id, ts, actor_kind, actor_id, type,
			        entity_kind, entity_slug, entity_project_id,
			        payload, rationale, caused_by_event_id, related_entities,
			        span_id, schema_version
			 FROM events
			 WHERE ts > ? OR (ts = ? AND event_id >= ?)
			 ORDER BY ts ASC, event_id ASC`, fromTs, fromTs, fromEvent)
		if err != nil {
			return err
		}
		defer rows.Close()
		var count int
		for rows.Next() {
			evt, err := scanRawEvent(rows)
			if err != nil {
				return err
			}
			for _, p := range targets {
				if err := p.Fold(ctx, tx, evt); err != nil {
					return fmt.Errorf("fold %s event %s: %w", p.Name(), evt.EventID, err)
				}
				if err := projections.WriteWatermark(ctx, tx, p.Name(), evt.EventID, evt.Ts); err != nil {
					return err
				}
			}
			count++
		}
		fmt.Printf("replayed %d events from %s\n", count, fromEvent)
		return rows.Err()
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "rebuild from event: %v\n", err)
		return 1
	}
	return 0
}

// nullStringPtr returns a pointer to the contained string when valid,
// nil otherwise. SQLite NULL → Go nil pointer.
func nullStringPtr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	v := s.String
	return &v
}

// joinedNames is a stdout helper for the unknown-projection error path.
func joinedNames(ps []projections.Projection) string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name()
	}
	return strings.Join(out, ", ")
}

// displayWatermark renders the watermark for human consumption — empty
// string when no events have been emitted yet (fresh DB).
func displayWatermark(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}

// registerProjectionsFoldHook wires the projections package as the
// post-INSERT hook on the events package. Called once at server
// startup from main(). The closure converts between the two RawEvent
// shapes (field-by-field identical) so neither package imports the
// other.
func registerProjectionsFoldHook() {
	// Imported here so the lazy linker doesn't drop the projection
	// init() registrations when only the rebuild subcommand path
	// references the package.
	_ = projections.All
	obs.L().Info("projections registered",
		slog.Int("count", len(projections.All())),
		slog.String("names", joinedNames(projections.All())),
	)
}

// errMissingArg is the sentinel returned when a required flag is
// missing — kept locally; callers don't need to type-assert.
var errMissingArg = errors.New("missing required argument")
