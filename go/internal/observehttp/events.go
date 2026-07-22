package observehttp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"toolkit/internal/db"
)

// agent-substrate-frontend chain F2: HTTP readers for the append-only
// events ledger. The ledger is owned by go/internal/events/; this file
// is the read-side surface the dashboard's audit ledger consumes. See
// docs/SUBSTRATE_FRONTEND.md §2 (endpoint catalog) and §3 (pagination
// + filter shape) for the load-bearing design decisions this file
// implements.
//
// Three endpoints:
//   - GET /events/list                       — paginated, filterable, DESC
//   - GET /events/{event_id}                 — single event detail + cross-substrate join
//   - GET /entities/{kind}/{slug}/events     — per-entity timeline, ASC
//
// SSE stream stays at GET /events (mounted in router.go via the eventbus).
// /events/list disambiguates the JSON list from the SSE root; see the
// design doc §2.4.

// eventListLimitDefault is the page size when ?limit= is absent. Set
// lower than the bugs list (200) because each row carries more content
// (rationale + payload object).
const eventListLimitDefault = 50

// eventListLimitMax bounds the page size to keep the per-page payload
// from becoming a footgun for a careless caller.
const eventListLimitMax = 200

// eventEntityKinds is the closed set of entity kinds the per-entity
// timeline endpoint accepts. benchmark_metric is deliberately not in
// the set — metric events are sub-events of a benchmark run, accessed
// via caused_by_event_id linking. See docs/SUBSTRATE_FRONTEND.md §2.3.
var eventEntityKinds = map[string]struct{}{
	"bug":           {},
	"task":          {},
	"chain":         {},
	"benchmark_run": {},
	"suggestion":    {},
}

// eventRow mirrors one events table row as the dashboard sees it.
// payload and related_entities arrive from SQLite as JSON strings; we
// re-emit them as json.RawMessage so the response stays a structured
// object instead of a doubly-encoded string. caused_by_event_id is
// nullable in the schema; *string makes the JSON encoder emit explicit
// null rather than the empty string.
type eventRow struct {
	EventID         string          `json:"event_id"`
	Ts              string          `json:"ts"`
	Actor           eventActorWire  `json:"actor"`
	Type            string          `json:"type"`
	Entity          eventEntityWire `json:"entity"`
	Payload         json.RawMessage `json:"payload"`
	Rationale       *string         `json:"rationale"`
	CausedByEventID *string         `json:"caused_by_event_id"`
	RelatedEntities json.RawMessage `json:"related_entities"`
	SpanID          string          `json:"span_id"`
	SchemaVersion   int             `json:"schema_version"`
}

// eventActorWire and eventEntityWire keep the JSON shape symmetrical
// with go/internal/events/emit.go's envelope so a frontend parser can
// share a single TS type across the SSE bus side (future) and the
// audit-ledger side (now).
type eventActorWire struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type eventEntityWire struct {
	Kind      string  `json:"kind"`
	Slug      string  `json:"slug"`
	ProjectID *string `json:"project_id"`
}

// eventDetailResponse extends eventRow with the optional cross-substrate
// join surface. RelatedQueries is nil (JSON null) when the
// query_resolutions table is absent — the dashboard distinguishes that
// from an empty array (table present but no related rows). See
// docs/SUBSTRATE_FRONTEND.md §6.
type eventDetailResponse struct {
	eventRow
	RelatedQueries []relatedQuery `json:"related_queries"`
}

// relatedQuery is one row from the sibling query-telemetry-substrate's
// query_resolutions table that references this event in its
// write_event_ids JSON array. Shape mirrors the canonical TT2 schema
// (migration 037_telemetry_substrate.sql) without importing it (the
// sibling table is present after migration 037 but the field set is
// still read defensively for forward-compatibility).
type relatedQuery struct {
	ResolutionID string `json:"resolution_id"`
	EntityKind   string `json:"entity_kind"`
	EntitySlug   string `json:"entity_slug"`
	OutcomeKind  string `json:"outcome_kind"`
	PromptID     string `json:"prompt_id"`
}

// eventListResponse is the cursor-paginated wrapper around a page of
// events. NextCursor is the event_id of the next-page-start; nil
// (JSON null) signals end-of-stream.
type eventListResponse struct {
	Items      []eventRow `json:"items"`
	NextCursor *string    `json:"next_cursor"`
	PageSize   int        `json:"page_size"`
}

// eventColumns is the explicit SELECT list — no SELECT *. Order
// matters: it pins to the Scan call in scanEventRow.
const eventColumns = `event_id, ts, actor_kind, actor_id, type,
    entity_kind, entity_slug, entity_project_id,
    payload, rationale, caused_by_event_id, related_entities,
    span_id, schema_version`

// eventsList handles GET /events/list. Newest-first, cursor on a
// (ts, event_id) tuple descending. Filters are AND-composed across keys,
// OR-composed within repeated type= params. See
// docs/SUBSTRATE_FRONTEND.md §3 for the full shape.
//
// IMPORTANT: ordering is on `ts` (primary) with `event_id` only as the
// tiebreaker for events sharing the same wall-clock timestamp (same-tx
// emits). A naive `ORDER BY event_id DESC` is WRONG: backfill / migration
// programs author synthetic event_ids with non-ULID prefixes (e.g.
// `started-<uuid>` / `completed-<uuid>` from the benchmark backfill),
// which lex-sort AFTER every ULID (`0xxx…` < `started-…`) and so
// dominate the top of an event_id DESC listing. The user-visible
// symptom is "audit ledger appears frozen weeks ago"; see bug
// `audit-ledger-orders-by-event-id-buries-real-events-behind-synthetic-backfill`.
func (s AppState) eventsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := parseLimit(q.Get("limit"), eventListLimitDefault, eventListLimitMax)
	cursorRaw := q.Get("cursor")

	var sb strings.Builder
	binds := db.NewArgs()
	sb.WriteString("SELECT ")
	sb.WriteString(eventColumns)
	sb.WriteString(" FROM events WHERE 1=1")

	if cursorRaw != "" {
		ts, eid, ok := decodeEventCursor(cursorRaw)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid cursor"})
			return
		}
		// Tuple-less predicate for DESC: rows strictly earlier than
		// (cursor_ts, cursor_event_id). Expanded as a disjunction so
		// SQLite can hit the ts index.
		sb.WriteString(" AND (ts < ? OR (ts = ? AND event_id < ?))")
		binds.AddString(ts).AddString(ts).AddString(eid)
	}

	applyEventFilters(&sb, binds, eventFilters{
		entityKind: q.Get("entity_kind"),
		entitySlug: q.Get("entity_slug"),
		types:      q["type"],
		project:    projectFilter(r),
		spanID:     q.Get("span_id"),
		actorKind:  q.Get("actor_kind"),
		actorID:    q.Get("actor_id"),
		since:      q.Get("since"),
		until:      q.Get("until"),
		rationaleQ: q.Get("q"),
	})

	// Read limit+1 to detect end-of-stream without a second round trip.
	sb.WriteString(" ORDER BY ts DESC, event_id DESC LIMIT ?")
	binds.AddInt64(int64(limit + 1))

	rows, err := s.Pool.DB().QueryContext(r.Context(), sb.String(), binds.Slice()...)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()

	items, err := scanEventRows(rows)
	if err != nil {
		dbErr(w, err)
		return
	}

	resp := eventListResponse{Items: items, NextCursor: nil, PageSize: limit}
	if len(items) > limit {
		// The cursor is the last *visible* row's (ts, event_id), NOT
		// the lookahead row's. The next page's predicate is strictly
		// less-than the cursor tuple, so storing the visible-tail puts
		// the lookahead row first on the next page instead of skipping
		// it.
		tail := encodeEventCursor(items[limit-1].Ts, items[limit-1].EventID)
		resp.NextCursor = &tail
		resp.Items = items[:limit]
	}

	setEventListCacheHeaders(w, cursorRaw != "")
	writeJSON(w, http.StatusOK, resp)
}

// eventsDetail handles GET /events/{event_id}. Returns the full envelope
// plus the optional related_queries cross-substrate join.
func (s AppState) eventsDetail(w http.ResponseWriter, r *http.Request) {
	eventID := r.PathValue("event_id")
	if !looksLikeUUIDv7(eventID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid event_id"})
		return
	}

	row := s.Pool.DB().QueryRowContext(r.Context(),
		"SELECT "+eventColumns+" FROM events WHERE event_id = ?", eventID)

	evt, err := scanOneEventRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "event not found"})
		return
	}
	if err != nil {
		dbErr(w, err)
		return
	}

	resp := eventDetailResponse{eventRow: evt, RelatedQueries: nil}
	if rqs, ok := loadRelatedQueries(r.Context(), s.Pool.DB(), eventID); ok {
		resp.RelatedQueries = rqs
	}

	// Detail responses are immutable except for related_queries
	// transitioning null → array when the sibling chain lands; bounded
	// cache.
	w.Header().Set("Cache-Control", "public, max-age=86400")
	writeJSON(w, http.StatusOK, resp)
}

// entityEvents handles GET /entities/{kind}/{slug}/events. Chronological
// (ASC) for the timeline reading order. Cursor semantics flip to "after
// this event_id".
func (s AppState) entityEvents(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	slug := r.PathValue("slug")
	if _, ok := eventEntityKinds[kind]; !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown entity_kind: " + kind})
		return
	}
	if slug == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "entity slug is required"})
		return
	}

	q := r.URL.Query()
	limit := parseLimit(q.Get("limit"), eventListLimitDefault, eventListLimitMax)
	cursorRaw := q.Get("cursor")

	var sb strings.Builder
	binds := db.NewArgs()
	sb.WriteString("SELECT ")
	sb.WriteString(eventColumns)
	sb.WriteString(" FROM events WHERE entity_kind = ? AND entity_slug = ?")
	binds.AddString(kind).AddString(slug)

	if cursorRaw != "" {
		ts, eid, ok := decodeEventCursor(cursorRaw)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid cursor"})
			return
		}
		// Tuple-greater predicate for ASC: rows strictly after the
		// cursor's (ts, event_id). See eventsList for the rationale on
		// not sorting by event_id alone.
		sb.WriteString(" AND (ts > ? OR (ts = ? AND event_id > ?))")
		binds.AddString(ts).AddString(ts).AddString(eid)
	}

	applyEventFilters(&sb, binds, eventFilters{
		// entity_kind / entity_slug are path-pinned; do NOT re-apply.
		types:      q["type"],
		project:    projectFilter(r),
		spanID:     q.Get("span_id"),
		actorKind:  q.Get("actor_kind"),
		actorID:    q.Get("actor_id"),
		since:      q.Get("since"),
		until:      q.Get("until"),
		rationaleQ: q.Get("q"),
	})

	sb.WriteString(" ORDER BY ts ASC, event_id ASC LIMIT ?")
	binds.AddInt64(int64(limit + 1))

	rows, err := s.Pool.DB().QueryContext(r.Context(), sb.String(), binds.Slice()...)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()

	items, err := scanEventRows(rows)
	if err != nil {
		dbErr(w, err)
		return
	}

	resp := eventListResponse{Items: items, NextCursor: nil, PageSize: limit}
	if len(items) > limit {
		// Tuple-tail cursor — same rationale as eventsList; the next
		// page's strict tuple-greater predicate puts the lookahead row
		// first instead of skipping it.
		tail := encodeEventCursor(items[limit-1].Ts, items[limit-1].EventID)
		resp.NextCursor = &tail
		resp.Items = items[:limit]
	}

	setEventListCacheHeaders(w, cursorRaw != "")
	writeJSON(w, http.StatusOK, resp)
}

// --- helpers ----------------------------------------------------------

// encodeEventCursor packs a (ts, event_id) tuple into the compact
// opaque-to-the-client cursor string the /events/list and /entities/*/
// events endpoints round-trip. Format: `<ts>|<event_id>`. Both fields
// already use URL-safe characters (ts is ISO-8601 with `T`, `:`, `.`, `Z`;
// event_id is hex + `-`); the `|` separator URL-encodes to `%7C` and
// decodes back losslessly. We bind both fields straight into SQL — no
// quoting interpolation — so the cursor cannot be a SQL-injection vector
// even with adversarial bytes; decodeEventCursor only checks for the
// single delimiter.
func encodeEventCursor(ts, eventID string) string {
	return ts + "|" + eventID
}

// decodeEventCursor splits the cursor back into (ts, event_id). Returns
// (_, _, false) for malformed input so the handler can reject with 400
// instead of running a query against garbage binds. The shape check is
// deliberately permissive about the ts / event_id formats themselves —
// future migrations might widen either field's lexicon — but rejects
// the missing-delimiter case which is the only way a SQL bind would
// silently land an unintended value.
func decodeEventCursor(raw string) (string, string, bool) {
	i := strings.IndexByte(raw, '|')
	if i <= 0 || i >= len(raw)-1 {
		return "", "", false
	}
	return raw[:i], raw[i+1:], true
}

// eventFilters bundles the shared filter knobs across /events/list and
// /entities/{kind}/{slug}/events. entity_kind and entity_slug are NOT
// in this struct because the entity endpoint pins them from the URL
// path and must not let the caller override.
type eventFilters struct {
	entityKind string
	entitySlug string
	types      []string
	project    string
	spanID     string
	actorKind  string
	actorID    string
	since      string
	until      string
	rationaleQ string
}

// applyEventFilters appends AND-composed filter clauses to sb and pushes
// the corresponding bind values. Multi-value filters (types) emit an
// IN (...) clause when more than one value is supplied; a single value
// uses = for parity with the single-value path.
func applyEventFilters(sb *strings.Builder, binds *db.Args, f eventFilters) {
	if f.entityKind != "" {
		sb.WriteString(" AND entity_kind = ?")
		binds.AddString(f.entityKind)
	}
	if f.entitySlug != "" {
		sb.WriteString(" AND entity_slug = ?")
		binds.AddString(f.entitySlug)
	}
	switch len(f.types) {
	case 0:
		// no-op
	case 1:
		sb.WriteString(" AND type = ?")
		binds.AddString(f.types[0])
	default:
		sb.WriteString(" AND type IN (")
		for i, t := range f.types {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("?")
			binds.AddString(t)
		}
		sb.WriteString(")")
	}
	if f.project != "" {
		sb.WriteString(" AND entity_project_id = ?")
		binds.AddString(f.project)
	}
	if f.spanID != "" {
		sb.WriteString(" AND span_id = ?")
		binds.AddString(f.spanID)
	}
	if f.actorKind != "" {
		sb.WriteString(" AND actor_kind = ?")
		binds.AddString(f.actorKind)
	}
	if f.actorID != "" {
		sb.WriteString(" AND actor_id = ?")
		binds.AddString(f.actorID)
	}
	if f.since != "" {
		sb.WriteString(" AND ts >= ?")
		binds.AddString(f.since)
	}
	if f.until != "" {
		sb.WriteString(" AND ts < ?")
		binds.AddString(f.until)
	}
	if f.rationaleQ != "" {
		// COLLATE NOCASE gives a case-insensitive LIKE without lower()-ing
		// the column (which would prevent any future indexed search).
		// The bind escapes special LIKE characters (%, _) so they're treated
		// as literals — the audit-ledger search box is plain text, not a
		// pattern language.
		sb.WriteString(" AND rationale LIKE ? ESCAPE '\\' COLLATE NOCASE")
		binds.AddString("%" + escapeLikePattern(f.rationaleQ) + "%")
	}
}

// escapeLikePattern escapes the three LIKE metacharacters (%, _, \) so a
// user-supplied search term is treated as a literal. The corresponding
// LIKE clause uses `ESCAPE '\\'` to set \ as the escape character.
func escapeLikePattern(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\\', '%', '_':
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// parseLimit clamps a query-string limit to [1, max]; absent or
// unparseable values fall back to dflt.
func parseLimit(raw string, dflt, max int) int {
	if raw == "" {
		return dflt
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return dflt
	}
	if n > max {
		return max
	}
	return n
}

// looksLikeUUIDv7 is a cheap pre-DB shape check for the event_id path
// segment. We don't validate the version nibble (7) because that would
// reject older test-seeded UUIDs; canonical 8-4-4-4-12 hex is enough
// to defend against a garbage path segment hammering the DB.
func looksLikeUUIDv7(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !isHex(r) {
				return false
			}
		}
	}
	return true
}

func isHex(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

// scanEventRows iterates a *sql.Rows and decodes each row into eventRow.
// payload and related_entities arrive as TEXT (SQLite JSON storage); we
// pass them through as json.RawMessage so the response keeps them as
// structured objects.
func scanEventRows(rows *sql.Rows) ([]eventRow, error) {
	out := []eventRow{}
	for rows.Next() {
		evt, err := scanEventRowFromRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, evt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// scanEventRowFromRows reads one event row from *sql.Rows. Split from
// scanOneEventRow so the two scanner shapes (*sql.Row from QueryRow vs
// *sql.Rows from Query) can share the column-list pinning.
func scanEventRowFromRows(rows *sql.Rows) (eventRow, error) {
	var (
		evt        eventRow
		projectID  sql.NullString
		rationale  sql.NullString
		causedBy   sql.NullString
		payloadTxt string
		relatedTxt string
	)
	if err := rows.Scan(
		&evt.EventID, &evt.Ts, &evt.Actor.Kind, &evt.Actor.ID, &evt.Type,
		&evt.Entity.Kind, &evt.Entity.Slug, &projectID,
		&payloadTxt, &rationale, &causedBy, &relatedTxt,
		&evt.SpanID, &evt.SchemaVersion,
	); err != nil {
		return eventRow{}, err
	}
	finishEventRow(&evt, projectID, rationale, causedBy, payloadTxt, relatedTxt)
	return evt, nil
}

// scanOneEventRow reads exactly one row from a *sql.Row. Returns
// sql.ErrNoRows when the query found no match.
func scanOneEventRow(row *sql.Row) (eventRow, error) {
	var (
		evt        eventRow
		projectID  sql.NullString
		rationale  sql.NullString
		causedBy   sql.NullString
		payloadTxt string
		relatedTxt string
	)
	if err := row.Scan(
		&evt.EventID, &evt.Ts, &evt.Actor.Kind, &evt.Actor.ID, &evt.Type,
		&evt.Entity.Kind, &evt.Entity.Slug, &projectID,
		&payloadTxt, &rationale, &causedBy, &relatedTxt,
		&evt.SpanID, &evt.SchemaVersion,
	); err != nil {
		return eventRow{}, err
	}
	finishEventRow(&evt, projectID, rationale, causedBy, payloadTxt, relatedTxt)
	return evt, nil
}

// finishEventRow folds the nullable + JSON-text columns into the evt
// fields. payload and related_entities default to JSON null / [] when
// the stored text is empty (defensive — the schema's NOT NULL +
// DEFAULT '[]' makes this unreachable, but a malformed manual INSERT
// shouldn't crash the dashboard).
func finishEventRow(evt *eventRow, projectID, rationale, causedBy sql.NullString, payloadTxt, relatedTxt string) {
	if projectID.Valid {
		s := projectID.String
		evt.Entity.ProjectID = &s
	}
	if rationale.Valid {
		s := rationale.String
		evt.Rationale = &s
	}
	if causedBy.Valid {
		s := causedBy.String
		evt.CausedByEventID = &s
	}
	if payloadTxt == "" {
		evt.Payload = json.RawMessage("null")
	} else {
		evt.Payload = json.RawMessage(payloadTxt)
	}
	if relatedTxt == "" {
		evt.RelatedEntities = json.RawMessage("[]")
	} else {
		evt.RelatedEntities = json.RawMessage(relatedTxt)
	}
}

// loadRelatedQueries best-efforts the cross-substrate join to
// query_resolutions. Returns (rows, true) on a successful query (even if
// the row set is empty); returns (nil, false) when the table is absent
// or any error occurred — the parent request still succeeds with
// related_queries: null in the response.
//
// Detection is per-request via sqlite_master because it's a microsecond
// lookup on an in-memory system table; caching the bool across requests
// would require an admin.schema_reload invalidation hook that doesn't
// exist yet. See docs/SUBSTRATE_FRONTEND.md §6.2.
func loadRelatedQueries(ctx context.Context, sqlDB *sql.DB, eventID string) ([]relatedQuery, bool) {
	var present int
	err := sqlDB.QueryRowContext(ctx,
		`SELECT 1 FROM sqlite_master WHERE type='table' AND name='query_resolutions' LIMIT 1`,
	).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false
	}
	if err != nil {
		return nil, false
	}

	rows, err := sqlDB.QueryContext(ctx, `
		SELECT resolution_id, entity_kind, entity_slug, outcome_kind, prompt_id
		FROM query_resolutions
		WHERE write_event_ids LIKE ?
	`, "%"+eventID+"%")
	if err != nil {
		return nil, false
	}
	defer rows.Close()

	out := []relatedQuery{}
	for rows.Next() {
		var rq relatedQuery
		if err := rows.Scan(&rq.ResolutionID, &rq.EntityKind, &rq.EntitySlug, &rq.OutcomeKind, &rq.PromptID); err != nil {
			return nil, false
		}
		out = append(out, rq)
	}
	if err := rows.Err(); err != nil {
		return nil, false
	}
	return out, true
}

// setEventListCacheHeaders writes Cache-Control matching the F1 §3.4
// contract: list pages with a non-empty cursor are content-stable
// (events are append-only); the latest page may grow between requests.
func setEventListCacheHeaders(w http.ResponseWriter, hasCursor bool) {
	if hasCursor {
		w.Header().Set("Cache-Control", "public, max-age=300")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
}
