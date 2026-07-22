// Command arcreview-snapshot-recover reconstructs point-in-time snapshots
// for historical ArcCloseFilingReviewed fires from their session transcripts
// and writes them into arcreview_snapshot_corpus as source=recovered.
//
// Chain arc-close-snapshot-corpus-capture T4. The live writer (T2) captures
// snapshots forward; this cmd backfills the ~261 historical fires that
// predate the writer, so the corpus reaches the classifier's >=200-fire
// training threshold now instead of after months of forward accrual.
//
// For each ArcCloseFilingReviewed event it maps session_id -> transcript
// (<projects-root>/<dir>/<session_id>.jsonl), reconstructs the snapshot the
// review saw via ExtractSnapshotAsOf (filter rows to ts <= fire_ts, then the
// identical turn/token truncation), and INSERTs it source=recovered. It is:
//   - by-session-aware: each fire gets its own point-in-time cut, so
//     multi-fire sessions (261/265 fires) recover faithfully.
//   - idempotent: ON CONFLICT(event_id) DO NOTHING never overwrites a live
//     row or a prior recovery.
//   - honest about gaps: sessions whose transcript is gone are skipped and
//     reported, never fabricated.
//
// Usage:
//
//	arcreview-snapshot-recover --db=PATH [--projects-root=DIR] [--dry-run]
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"toolkit/internal/arcreview"
	"toolkit/internal/db"
)

func main() {
	dbPath := flag.String("db", "", "path to toolkit SQLite database (required)")
	projectsRoot := flag.String("projects-root", defaultProjectsRoot(),
		"Claude Code projects root holding <dir>/<session_id>.jsonl transcripts")
	dryRun := flag.Bool("dry-run", false, "report coverage without writing recovered rows")
	flag.Parse()

	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "error: --db is required")
		os.Exit(1)
	}

	pool, err := db.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open db: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = pool.Close() }()

	if err := run(context.Background(), pool, *projectsRoot, *dryRun); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func defaultProjectsRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// fireRow is one ArcCloseFilingReviewed event to consider for recovery.
type fireRow struct {
	eventID   string
	sessionID string
	fireTS    string
}

// recoveredRow is a reconstructed corpus row ready to insert.
type recoveredRow struct {
	eventID   string
	sessionID string
	fireTS    string
	messages  string
	msgCount  int
	estTokens int
	truncated int
}

func run(ctx context.Context, pool *db.Pool, projectsRoot string, dryRun bool) error {
	transcripts, err := indexTranscripts(projectsRoot)
	if err != nil {
		return fmt.Errorf("index transcripts under %s: %w", projectsRoot, err)
	}
	fmt.Printf("transcript index: %d session files under %s\n", len(transcripts), projectsRoot)

	fires, err := loadFires(ctx, pool)
	if err != nil {
		return fmt.Errorf("load fires: %w", err)
	}
	fmt.Printf("ArcCloseFilingReviewed fires: %d\n", len(fires))

	var (
		toInsert        []recoveredRow
		skippedExisting int
		noTranscript    int
		emptyRecon      int
		parseErr        int
		missingSessions = map[string]struct{}{}
	)

	for _, f := range fires {
		exists, err := corpusRowExists(ctx, pool, f.eventID)
		if err != nil {
			return fmt.Errorf("check existing %s: %w", f.eventID, err)
		}
		if exists {
			skippedExisting++
			continue
		}
		path, ok := transcripts[f.sessionID]
		if !ok {
			noTranscript++
			missingSessions[f.sessionID] = struct{}{}
			continue
		}
		fireTS, perr := time.Parse(time.RFC3339, f.fireTS)
		if perr != nil {
			parseErr++
			fmt.Fprintf(os.Stderr, "  warn: unparseable fire_ts %q for event %s; skipping\n", f.fireTS, f.eventID)
			continue
		}
		snap, serr := arcreview.ExtractSnapshotAsOf(path, arcreview.DefaultMaxTurns, arcreview.DefaultMaxTokens, fireTS)
		if serr != nil {
			fmt.Fprintf(os.Stderr, "  warn: snapshot reconstruct failed for %s (%s): %v\n", f.eventID, path, serr)
			noTranscript++ // unreadable transcript counts as unrecoverable
			continue
		}
		if len(snap.Messages) == 0 {
			emptyRecon++ // no rows at/before fire_ts — nothing to capture
			continue
		}
		msgsJSON, merr := json.Marshal(snap.Messages)
		if merr != nil {
			return fmt.Errorf("marshal messages for %s: %w", f.eventID, merr)
		}
		trunc := 0
		if snap.Truncated {
			trunc = 1
		}
		toInsert = append(toInsert, recoveredRow{
			eventID:   f.eventID,
			sessionID: f.sessionID,
			fireTS:    f.fireTS,
			messages:  string(msgsJSON),
			msgCount:  len(snap.Messages),
			estTokens: snap.EstimatedTokens,
			truncated: trunc,
		})
	}

	written := 0
	if !dryRun && len(toInsert) > 0 {
		written, err = insertRecovered(ctx, pool, toInsert)
		if err != nil {
			return fmt.Errorf("insert recovered rows: %w", err)
		}
	}

	// Coverage report.
	fmt.Println("\n── recovery coverage ──────────────────────────────")
	fmt.Printf("  fires total:            %d\n", len(fires))
	fmt.Printf("  already in corpus:      %d  (live or prior recovery; skipped)\n", skippedExisting)
	fmt.Printf("  reconstructable:        %d\n", len(toInsert))
	if dryRun {
		fmt.Printf("    (dry-run: not written)\n")
	} else {
		fmt.Printf("  written (source=recovered): %d\n", written)
	}
	fmt.Printf("  no transcript / unreadable: %d  (%d distinct sessions gone)\n", noTranscript, len(missingSessions))
	fmt.Printf("  empty reconstruction:   %d  (no rows at/before fire_ts)\n", emptyRecon)
	fmt.Printf("  unparseable fire_ts:    %d\n", parseErr)
	return nil
}

// indexTranscripts walks projectsRoot for <dir>/<session_id>.jsonl files and
// returns session_id -> absolute path. Last-writer-wins on duplicate
// session_ids across project dirs (rare; same session never spans dirs).
func indexTranscripts(root string) (map[string]string, error) {
	out := map[string]string{}
	if root == "" {
		return out, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			name := f.Name()
			if filepath.Ext(name) != ".jsonl" {
				continue
			}
			sid := name[:len(name)-len(".jsonl")]
			out[sid] = filepath.Join(dir, name)
		}
	}
	return out, nil
}

func loadFires(ctx context.Context, pool *db.Pool) ([]fireRow, error) {
	rows, err := pool.DB().QueryContext(ctx, `
		SELECT event_id, entity_slug, ts
		FROM events
		WHERE type = 'ArcCloseFilingReviewed' AND entity_slug <> ''
		ORDER BY ts ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []fireRow
	for rows.Next() {
		var f fireRow
		if err := rows.Scan(&f.eventID, &f.sessionID, &f.fireTS); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func corpusRowExists(ctx context.Context, pool *db.Pool, eventID string) (bool, error) {
	var one int
	err := pool.DB().QueryRowContext(ctx,
		`SELECT 1 FROM arcreview_snapshot_corpus WHERE event_id = ?`, eventID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// insertRecovered writes all reconstructed rows in one tx; ON CONFLICT
// DO NOTHING keeps it idempotent and never clobbers a live row.
func insertRecovered(ctx context.Context, pool *db.Pool, rows []recoveredRow) (int, error) {
	written := 0
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		for _, r := range rows {
			res, err := tx.ExecContext(ctx, `
				INSERT INTO arcreview_snapshot_corpus
					(event_id, session_id, fire_ts, messages_json, message_count,
					 estimated_tokens, truncated, max_turns, max_tokens, source, schema_version)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'recovered', 1)
				ON CONFLICT(event_id) DO NOTHING`,
				r.eventID, r.sessionID, r.fireTS, r.messages, r.msgCount,
				r.estTokens, r.truncated, arcreview.DefaultMaxTurns, arcreview.DefaultMaxTokens)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				written++
			}
		}
		return nil
	})
	return written, err
}
