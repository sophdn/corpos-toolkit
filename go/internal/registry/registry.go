package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/projections"

	// Side-effect import: registers the projections at init() so
	// [projections.RebuildAll] / [projections.All] see the full set when
	// this package drives a from-empty rebuild.
	_ "toolkit/internal/projections"
)

// eventsDir is the subdirectory under a registry checkout that holds the
// per-event JSON files. Kept as a named constant so the export writer and
// the validate/DR readers agree.
const eventsDir = "events"

// Event is one event in the registry's canonical envelope shape — the exact
// 11-field _envelope.json layout the events package validates. It round-
// trips losslessly to and from a row of the local `events` table.
type Event struct {
	EventID       string          `json:"event_id"`
	Ts            string          `json:"ts"`
	Actor         Actor           `json:"actor"`
	Type          string          `json:"type"`
	Entity        Entity          `json:"entity"`
	Payload       json.RawMessage `json:"payload"`
	Rationale     *string         `json:"rationale"`
	Refs          Refs            `json:"refs"`
	SpanID        string          `json:"span_id"`
	SchemaVersion int             `json:"schema_version"`
}

// Actor mirrors the envelope's actor object.
type Actor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Entity mirrors the envelope's entity object (also reused for
// refs.related_entities items).
type Entity struct {
	Kind      string  `json:"kind"`
	Slug      string  `json:"slug"`
	ProjectID *string `json:"project_id"`
}

// Refs mirrors the envelope's refs object. RelatedEntities is always a
// non-nil slice so the marshaled JSON is `[]`, never `null` — matching the
// schema's `"type": "array"`.
type Refs struct {
	CausedByEventID *string  `json:"caused_by_event_id"`
	RelatedEntities []Entity `json:"related_entities"`
}

// Filename returns the event's registry-relative path: events/<event_id>.json.
// The UUIDv7 event_id is unique, filename-safe, and time-sortable, so a flat
// directory stays in chronological order under `ls` while guaranteeing two
// concurrently-appended events never collide on a path.
func (e Event) Filename() string {
	return filepath.Join(eventsDir, e.EventID+".json")
}

// canonicalJSON renders an event as deterministic, indented JSON. Struct
// field order is fixed by declaration (stable across re-exports), and the
// payload bytes are passed through verbatim, so re-exporting an unchanged
// ledger yields byte-identical files — the property the ff-only registry
// needs so an unchanged event never shows up as a spurious diff.
func (e Event) canonicalJSON() ([]byte, error) {
	if e.Refs.RelatedEntities == nil {
		e.Refs.RelatedEntities = []Entity{}
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal event %s: %w", e.EventID, err)
	}
	return append(b, '\n'), nil
}

// eventColumns is the SELECT/INSERT column list for the events table,
// shared by the reader and the reconstruction writer so they stay aligned.
const eventColumns = `event_id, ts, actor_kind, actor_id, type,
	entity_kind, entity_slug, entity_project_id,
	payload, rationale, caused_by_event_id, related_entities,
	span_id, schema_version`

// ReadEvents reads every event from a live ledger in (ts, event_id) order —
// the chronological authority order ([events] doc §Invariant). Used by the
// exporter and by [VerifyDR] to snapshot the source ledger.
func ReadEvents(ctx context.Context, pool *db.Pool) ([]Event, error) {
	rows, err := pool.DB().QueryContext(ctx, `SELECT `+eventColumns+`
		FROM events ORDER BY ts ASC, event_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events row walk: %w", err)
	}
	return out, nil
}

// scanEvent reads one events row into the registry envelope shape,
// reconstructing the nested actor/entity/refs objects from the flat columns.
func scanEvent(rows *sql.Rows) (Event, error) {
	var (
		eid, ts, ak, aid, typ, ek, es, span string
		epid, rat, cbe                      sql.NullString
		payload, related                    []byte
		sv                                  int
	)
	if err := rows.Scan(&eid, &ts, &ak, &aid, &typ, &ek, &es, &epid,
		&payload, &rat, &cbe, &related, &span, &sv); err != nil {
		return Event{}, fmt.Errorf("scan event: %w", err)
	}
	var relatedEntities []Entity
	if len(related) > 0 {
		if err := json.Unmarshal(related, &relatedEntities); err != nil {
			return Event{}, fmt.Errorf("decode related_entities for %s: %w", eid, err)
		}
	}
	if relatedEntities == nil {
		relatedEntities = []Entity{}
	}
	return Event{
		EventID:       eid,
		Ts:            ts,
		Actor:         Actor{Kind: ak, ID: aid},
		Type:          typ,
		Entity:        Entity{Kind: ek, Slug: es, ProjectID: nullPtr(epid)},
		Payload:       json.RawMessage(payload),
		Rationale:     nullPtr(rat),
		Refs:          Refs{CausedByEventID: nullPtr(cbe), RelatedEntities: relatedEntities},
		SpanID:        span,
		SchemaVersion: sv,
	}, nil
}

// ExportFromDB serializes the live events ledger to a registry checkout at
// destDir, writing one canonical JSON file per event under destDir/events/.
// Returns the number of events written. Existing event files are
// overwritten with byte-identical content (the canonical render is
// deterministic), so a re-export of an unchanged ledger is a git no-op.
func ExportFromDB(ctx context.Context, pool *db.Pool, destDir string) (int, error) {
	evs, err := ReadEvents(ctx, pool)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Join(destDir, eventsDir), 0o755); err != nil {
		return 0, fmt.Errorf("create events dir: %w", err)
	}
	for _, ev := range evs {
		b, err := ev.canonicalJSON()
		if err != nil {
			return 0, err
		}
		if err := os.WriteFile(filepath.Join(destDir, ev.Filename()), b, 0o644); err != nil {
			return 0, fmt.Errorf("write %s: %w", ev.Filename(), err)
		}
	}
	return len(evs), nil
}

// LoadDir reads every event file under dir/events/, returning the decoded
// events alongside their raw bytes (the bytes feed the schema validator,
// which works on the canonical envelope JSON). Files are returned in
// (ts, event_id) order.
func LoadDir(dir string) ([]Event, map[string][]byte, error) {
	pattern := filepath.Join(dir, eventsDir, "*.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, nil, fmt.Errorf("glob %s: %w", pattern, err)
	}
	evs := make([]Event, 0, len(matches))
	raws := make(map[string][]byte, len(matches))
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", path, err)
		}
		var ev Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil, nil, fmt.Errorf("decode %s: %w", filepath.Base(path), err)
		}
		evs = append(evs, ev)
		raws[ev.EventID] = raw
	}
	sort.Slice(evs, func(i, j int) bool {
		if evs[i].Ts != evs[j].Ts {
			return evs[i].Ts < evs[j].Ts
		}
		return evs[i].EventID < evs[j].EventID
	})
	return evs, raws, nil
}

// Failure is one event's validation failure, naming the event and the tier
// (schema / causal) plus a human-readable reason.
type Failure struct {
	EventID string
	Tier    string
	Reason  string
}

func (f Failure) String() string {
	return fmt.Sprintf("[%s] %s: %s", f.Tier, f.EventID, f.Reason)
}

// Report is the outcome of [Validate]: the totals plus any per-event
// failures. OK is true iff Failures is empty. Grandfathered counts events
// skipped by the strict schema tier because they are part of the immutable
// canonical baseline (see [ValidateOptions.StrictSchemaEventIDs]).
type Report struct {
	Total         int
	Grandfathered int
	Failures      []Failure
}

// OK reports whether validation passed (no failures).
func (r Report) OK() bool { return len(r.Failures) == 0 }

// ValidateOptions tunes the CI validity-stamp tier.
type ValidateOptions struct {
	// StrictSchemaEventIDs, when non-nil, limits the strict per-event schema
	// tier to exactly these event_ids — the newly-pushed delta on an
	// ff-only registry. Every other event is the IMMUTABLE, already-canonical
	// baseline and is grandfathered out of the schema tier.
	//
	// This is load-bearing and semantically required, not a convenience:
	// the canonical ledger is append-only-immutable (§7 invariant 1). A
	// historical event that no longer satisfies a since-tightened schema
	// (e.g. a pre-minLength ChainCreated with an empty output, or a
	// synthetic `started-<uuid>` backfill id minted before the UUIDv7
	// envelope pattern) CANNOT be "fixed" — a published event is corrected
	// only by a compensating event, never by editing. Re-validating the
	// baseline against current schemas would make the registry permanently
	// red for a class of events that are correct-as-of-their-emit-time.
	// So the CI validates the DELTA strictly and grandfathers the baseline;
	// the causal and projection-coherence tiers still run over the WHOLE set
	// (they are about the consistency of the whole, and the baseline must
	// remain coherent).
	//
	// nil means "schema-check every event" — the full-audit mode a human
	// runs to survey baseline health, distinct from the per-push CI gate.
	StrictSchemaEventIDs map[string]bool
}

// Validate runs the CI validity-stamp tier over a registry checkout at dir.
// Three sub-tiers, per the §11 CI-rule taxonomy:
//
//   - schema (structural + per-type): events pass [events.ValidateRecordJSON]
//     — the same closed-enum, envelope, and per-type-payload check the local
//     tier runs. Scoped to the pushed delta when opts.StrictSchemaEventIDs is
//     set (see that field); otherwise every event is checked.
//   - causal: every non-null caused_by_event_id resolves to another event
//     present in the registry (rejects an event claiming a non-canonical
//     parent — the two-machine guard). Runs over the WHOLE set.
//   - projection-coherence: the whole event set folds cleanly into a
//     from-empty projection rebuild. A fold error means the events are
//     mutually inconsistent (an orphaned reference, a contradiction) even
//     when each is individually schema-valid. Runs over the WHOLE set.
//
// projection-coherence is the expensive tier — it stands up a temp SQLite
// DB and replays — so it runs once over the whole set, after the cheap
// per-event tiers pass.
func Validate(ctx context.Context, dir string, opts ValidateOptions) (Report, error) {
	evs, raws, err := LoadDir(dir)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Total: len(evs)}

	present := make(map[string]struct{}, len(evs))
	for _, ev := range evs {
		present[ev.EventID] = struct{}{}
	}

	for _, ev := range evs {
		// Schema tier — strict only on the pushed delta (or everything when
		// no delta set is supplied). Grandfathered baseline events skip it.
		if opts.StrictSchemaEventIDs == nil || opts.StrictSchemaEventIDs[ev.EventID] {
			if err := events.ValidateRecordJSON(raws[ev.EventID]); err != nil {
				rep.Failures = append(rep.Failures, Failure{EventID: ev.EventID, Tier: "schema", Reason: err.Error()})
			}
		} else {
			rep.Grandfathered++
		}
		// Causal tier — always over the whole set.
		if ev.Refs.CausedByEventID != nil {
			if _, ok := present[*ev.Refs.CausedByEventID]; !ok {
				rep.Failures = append(rep.Failures, Failure{
					EventID: ev.EventID,
					Tier:    "causal",
					Reason:  "caused_by_event_id " + *ev.Refs.CausedByEventID + " is not present on canonical (non-canonical parent)",
				})
			}
		}
	}

	// Only attempt the expensive projection-coherence replay when the cheap
	// tiers are clean — replaying a schema-invalid set would fail for the
	// already-reported reason and bury the real coherence signal.
	if rep.OK() {
		if err := checkProjectionCoherence(ctx, evs); err != nil {
			rep.Failures = append(rep.Failures, Failure{Tier: "projection-coherence", Reason: err.Error()})
		}
	}
	return rep, nil
}

// checkProjectionCoherence reconstructs the events into a fresh temp DB and
// runs a from-empty projection rebuild. A nil return means the whole set
// folds cleanly (projections are internally consistent at HEAD).
func checkProjectionCoherence(ctx context.Context, evs []Event) error {
	pool, cleanup, err := reconstructDB(ctx, evs)
	if err != nil {
		return err
	}
	defer cleanup()
	return pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, rebuildErr := projections.RebuildAll(ctx, tx, nil)
		return rebuildErr
	})
}

// VerifyDR is the disaster-recovery proof. It reconstructs the events ledger
// from the registry checkout at dir, rebuilds projections from empty, and
// asserts the result is byte-identical (per-table content hash) to a
// from-empty rebuild of the SOURCE ledger. A match proves the registry is a
// faithful, sufficient reconstruction authority: clone → replay → rebuild
// reproduces the source's projected state exactly.
//
// Comparing two from-empty rebuilds (registry-side and source-side) isolates
// the registry round-trip from any incremental-fold drift the live DB might
// carry — the question this answers is "is the registry a lossless DR
// source," not "has the live DB drifted from its events" (a separate audit).
// It additionally checks event-set parity first: a different event count or
// content between registry and source is a lossy export and fails loudly
// before the projection comparison.
func VerifyDR(ctx context.Context, srcPool *db.Pool, dir string) error {
	regEvents, _, err := LoadDir(dir)
	if err != nil {
		return err
	}
	srcEvents, err := ReadEvents(ctx, srcPool)
	if err != nil {
		return err
	}

	if err := eventSetParity(srcEvents, regEvents); err != nil {
		return fmt.Errorf("event-set parity (export is lossy): %w", err)
	}

	regHashes, err := rebuildAndHashProjections(ctx, regEvents)
	if err != nil {
		return fmt.Errorf("rebuild from registry: %w", err)
	}
	srcHashes, err := rebuildAndHashProjections(ctx, srcEvents)
	if err != nil {
		return fmt.Errorf("rebuild from source: %w", err)
	}

	var mismatches []string
	for table, srcHash := range srcHashes {
		regHash, ok := regHashes[table]
		if !ok {
			mismatches = append(mismatches, table+": present in source rebuild, absent in registry rebuild")
			continue
		}
		if regHash != srcHash {
			mismatches = append(mismatches, fmt.Sprintf("%s: source=%s registry=%s", table, srcHash[:12], regHash[:12]))
		}
	}
	if len(mismatches) > 0 {
		sort.Strings(mismatches)
		return fmt.Errorf("projection rebuild not byte-identical:\n  %s", strings.Join(mismatches, "\n  "))
	}
	return nil
}

// eventSetParity asserts two event slices are identical as sets: same count
// and same canonical bytes per event_id. Order-independent.
func eventSetParity(a, b []Event) error {
	if len(a) != len(b) {
		return fmt.Errorf("event count differs: source=%d registry=%d", len(a), len(b))
	}
	index := make(map[string]string, len(a))
	for _, ev := range a {
		j, err := ev.canonicalJSON()
		if err != nil {
			return err
		}
		index[ev.EventID] = string(j)
	}
	for _, ev := range b {
		want, ok := index[ev.EventID]
		if !ok {
			return fmt.Errorf("event %s present in registry, absent in source", ev.EventID)
		}
		got, err := ev.canonicalJSON()
		if err != nil {
			return err
		}
		if string(got) != want {
			return fmt.Errorf("event %s content differs between source and registry", ev.EventID)
		}
	}
	return nil
}

// rebuildAndHashProjections reconstructs events into a temp DB, runs a
// from-empty projection rebuild, and returns a per-projection-table content
// hash. The hashes are order-independent (each row is hashed, the row hashes
// are sorted, then hashed together) so a table's identity doesn't depend on
// SELECT order.
func rebuildAndHashProjections(ctx context.Context, evs []Event) (map[string]string, error) {
	pool, cleanup, err := reconstructDB(ctx, evs)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	if err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, rebuildErr := projections.RebuildAll(ctx, tx, nil)
		return rebuildErr
	}); err != nil {
		return nil, fmt.Errorf("rebuild projections: %w", err)
	}

	out := make(map[string]string)
	for _, p := range projections.All() {
		// Whole-table content hashing crosses the database/sql.Scan
		// dynamic-schema boundary; it lives in internal/db (the
		// concentrated boundary) so this package stays free of bare `any`.
		h, err := db.HashTableContent(ctx, pool.DB(), p.TableName())
		if err != nil {
			return nil, err
		}
		out[p.TableName()] = h
	}
	return out, nil
}

// reconstructDB opens a fresh migrated temp SQLite DB and inserts every
// event verbatim (preserving event_id/ts so the rebuild is deterministic).
// Events are inserted in (ts, event_id) order, which guarantees a
// caused_by_event_id parent is always inserted before its child, satisfying
// the events table's self-referential FK without deferring it. Returns the
// pool and a cleanup func that closes it and removes the temp dir.
func reconstructDB(ctx context.Context, evs []Event) (*db.Pool, func(), error) {
	tmpDir, err := os.MkdirTemp("", "event-registry-dr-*")
	if err != nil {
		return nil, nil, fmt.Errorf("temp dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }

	pool, err := db.Open(filepath.Join(tmpDir, "reconstructed.db"))
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("open temp db: %w", err)
	}
	closeAll := func() {
		_ = pool.Close()
		cleanup()
	}

	ordered := make([]Event, len(evs))
	copy(ordered, evs)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Ts != ordered[j].Ts {
			return ordered[i].Ts < ordered[j].Ts
		}
		return ordered[i].EventID < ordered[j].EventID
	})

	err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
		for _, ev := range ordered {
			related, err := json.Marshal(relatedOrEmpty(ev.Refs.RelatedEntities))
			if err != nil {
				return fmt.Errorf("marshal related_entities for %s: %w", ev.EventID, err)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO events (`+eventColumns+`)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				ev.EventID, ev.Ts, ev.Actor.Kind, ev.Actor.ID, ev.Type,
				ev.Entity.Kind, ev.Entity.Slug, ev.Entity.ProjectID,
				string(ev.Payload), nullStr(ev.Rationale), nullStr(ev.Refs.CausedByEventID), string(related),
				ev.SpanID, ev.SchemaVersion,
			); err != nil {
				return fmt.Errorf("insert event %s: %w", ev.EventID, err)
			}
		}
		return nil
	})
	if err != nil {
		closeAll()
		return nil, nil, err
	}
	return pool, closeAll, nil
}

func relatedOrEmpty(e []Entity) []Entity {
	if e == nil {
		return []Entity{}
	}
	return e
}

func nullPtr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	v := s.String
	return &v
}

func nullStr(p *string) sql.NullString {
	if p == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *p, Valid: true}
}
