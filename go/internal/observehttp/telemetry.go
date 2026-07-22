package observehttp

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"toolkit/internal/db"
)

// query-telemetry-substrate-frontend chain QF2: HTTP readers for the
// read-side telemetry substrate (grounding_events + query_interactions +
// query_resolutions + the three proj_query_* projections from
// migration 038). Companion to events.go (the write-side audit ledger);
// see docs/TELEMETRY_FRONTEND.md for the load-bearing decisions this
// file binds to — §2 (three-axis disambiguation), §3 (endpoint
// catalog), §4 (pagination + filter shape), §5 (read-source rules).
//
// Five endpoints, six router rules (the trajectory endpoint has two
// shapes: path-by-query_id and query-param-by-span_id):
//
//   - GET /telemetry/trajectories/{query_id}        — single trajectory
//   - GET /telemetry/trajectories?span_id=<uuid>    — span lookup (1..N trajectories)
//   - GET /telemetry/analytics/volume-by-source     — chart-ready volume time-series
//   - GET /telemetry/analytics/success-rate         — chart-ready success time-series
//   - GET /telemetry/training-pairs                 — paginated training-pair browser
//   - GET /telemetry/training-pairs/stats           — corpus-shape banner stats
//
// Discipline:
//   - explicit SELECT lists (no SELECT *)
//   - db.NewArgs() for every parameter bind
//   - snake_case JSON tags everywhere; field names always name which of
//     the three axes they belong to (action / query_source / source_type) —
//     no bare 'source' or 'type' (TELEMETRY_FRONTEND §2)
//   - chart endpoints cap at chartMaxBuckets days to keep a hostile
//     since/until range from blowing memory (TELEMETRY_FRONTEND §3)

// telemetryLimitDefault is the page size when ?limit= is absent on the
// training-pair browser. Matches the audit-ledger value for visual
// parity; QF5 may revisit after first usability pass.
const telemetryLimitDefault = 50

// telemetryLimitMax bounds the page size to keep a careless caller from
// pulling the full corpus in one request.
const telemetryLimitMax = 200

// chartMaxBuckets is the hard cap on time-series bucket rows returned
// by the analytics endpoints. A 1000-day window at day granularity
// covers ~2.7 years — beyond the homelab scale this surface targets.
// LIMIT applied server-side; truncation is silent (the caller picks
// the range, so range-too-wide is operator error, not a 400).
const chartMaxBuckets = 1000

// telemetrySegments is the closed enum for the analytics-chart segment
// axis. action and query_source are the two orthogonal slices defined
// by migration 037 + TELEMETRY_FRONTEND §2; never accept any other
// value.
var telemetrySegments = map[string]struct{}{
	"action":       {},
	"query_source": {},
}

// telemetryLabelKinds is the closed 5-value enum from TT1.5
// (TELEMETRY_LABEL_SPIKE §5). Validated on the training-pair filter
// surface so a typo doesn't silently match zero rows.
var telemetryLabelKinds = map[string]struct{}{
	"positive":        {},
	"weakly_positive": {},
	"negative":        {},
	"hard_negative":   {},
	"unlabeled":       {},
}

// telemetryQuerySources is the closed 4-value enum from migration 037's
// CHECK on grounding_events.query_source. Validated on the training-pair
// filter surface for the same reason.
var telemetryQuerySources = map[string]struct{}{
	"agent_initiated": {},
	"proactive_hook":  {},
	"dashboard_user":  {},
	"other":           {},
}

// --- wire types -------------------------------------------------------

// trajectoryResponse is the per-query full-audit envelope returned by
// /telemetry/trajectories/{query_id} (and as elements of the
// /telemetry/trajectories?span_id wrapper). See TELEMETRY_FRONTEND §3.1.
type trajectoryResponse struct {
	Query        trajectoryQuery         `json:"query"`
	Results      []trajectoryResult      `json:"results"`
	Interactions []trajectoryInteraction `json:"interactions"`
	Resolutions  []trajectoryResolution  `json:"resolutions"`
}

// trajectoryQuery mirrors one grounding_events row with the metadata
// the trajectory header needs.
type trajectoryQuery struct {
	QueryID      int64   `json:"query_id"`
	SpanID       string  `json:"span_id"`
	PromptID     *string `json:"prompt_id"`
	SessionID    string  `json:"session_id"`
	ParentSpanID *string `json:"parent_span_id"`
	ProjectID    string  `json:"project_id"`
	Action       string  `json:"action"`
	QuerySource  string  `json:"query_source"`
	QueryText    *string `json:"query_text"`
	ResultsCount int     `json:"results_count"`
	CreatedAt    string  `json:"created_at"`
}

// trajectoryResult is one row in the original result set. source_type
// is nullable because the candidate's knowledge_pointer row may have
// been retired since the search fired (LEFT JOIN, no scope mismatch
// silently masquerading as missing data).
type trajectoryResult struct {
	Position           int     `json:"position"`
	SourceRef          string  `json:"source_ref"`
	SourceType         *string `json:"source_type"`
	CandidatePointerID *int64  `json:"candidate_pointer_id"`
}

// trajectoryInteraction is one query_interactions row. position is
// nullable per the schema (positional rank may be unknown for some
// click_kinds).
type trajectoryInteraction struct {
	InteractionID   int64   `json:"interaction_id"`
	SourceRef       string  `json:"source_ref"`
	Position        *int    `json:"position"`
	ClickKind       string  `json:"click_kind"`
	ClickWeight     float64 `json:"click_weight"`
	CitationKind    *string `json:"citation_kind"`
	DwellMsEstimate *int64  `json:"dwell_ms_estimate"`
	WasInjected     int     `json:"was_injected"`
	DetectedAt      string  `json:"detected_at"`
}

// trajectoryResolution is one query_resolutions row. write_event_ids
// is passed through as a JSON array — the trajectory endpoint
// deliberately does NOT server-join to the events table (cross-substrate
// decoupling per TELEMETRY_FRONTEND §3.1); the client composes via
// agent-substrate-frontend's /events/{event_id}.
type trajectoryResolution struct {
	ResolutionID    string          `json:"resolution_id"`
	EntityKind      string          `json:"entity_kind"`
	EntitySlug      string          `json:"entity_slug"`
	EntityProjectID string          `json:"entity_project_id"`
	OutcomeKind     string          `json:"outcome_kind"`
	WriteEventIDs   json.RawMessage `json:"write_event_ids"`
	DetectedAt      string          `json:"detected_at"`
}

// trajectoryBySpanResponse wraps zero-to-many trajectories sharing a
// span_id. One span can legally fan out (vault_search + kiwix_search
// from the same tools/call), so the wire shape is a list even when
// most lookups produce one row.
type trajectoryBySpanResponse struct {
	Trajectories []trajectoryResponse `json:"trajectories"`
}

// analyticsVolumeBucket is one day of volume data; segments maps
// segment-value (e.g. "vault_search" or "agent_initiated") to count.
type analyticsVolumeBucket struct {
	Day      string         `json:"day"`
	Segments map[string]int `json:"segments"`
}

// analyticsVolumeResponse is the volume-by-source chart payload. The
// segment field echoes the caller's choice so the client doesn't have
// to track it out-of-band.
type analyticsVolumeResponse struct {
	Segment         string                  `json:"segment"`
	Buckets         []analyticsVolumeBucket `json:"buckets"`
	TotalsBySegment map[string]int          `json:"totals_by_segment"`
}

// analyticsSuccessCell carries the three numbers a success-rate chart
// needs per (day, segment): the denominator, the numerator, and the
// pre-computed rate so the client never re-divides (single source of
// truth, TELEMETRY_FRONTEND §3.3).
type analyticsSuccessCell struct {
	QueryCount   int     `json:"query_count"`
	SuccessCount int     `json:"success_count"`
	SuccessRate  float64 `json:"success_rate"`
}

type analyticsSuccessBucket struct {
	Day      string                          `json:"day"`
	Segments map[string]analyticsSuccessCell `json:"segments"`
}

type analyticsSuccessResponse struct {
	Segment         string                          `json:"segment"`
	Buckets         []analyticsSuccessBucket        `json:"buckets"`
	TotalsBySegment map[string]analyticsSuccessCell `json:"totals_by_segment"`
}

// trainingPairItem is one proj_training_data_for_reranker row as the
// browser sees it. label_sources passes through as the stored JSON
// array (the projection emits json_group_array output).
type trainingPairItem struct {
	TrainingID         int64           `json:"training_id"`
	GroundingEventID   int64           `json:"grounding_event_id"`
	QueryText          *string         `json:"query_text"`
	CandidatePointerID *int64          `json:"candidate_pointer_id"`
	SourceRef          string          `json:"source_ref"`
	CandidatePosition  int             `json:"candidate_position"`
	LabelKind          string          `json:"label_kind"`
	Weight             float64         `json:"weight"`
	LabelSources       json.RawMessage `json:"label_sources"`
	QuerySource        string          `json:"query_source"`
	WasInjected        int             `json:"was_injected"`
	PromptID           *string         `json:"prompt_id"`
	SpanID             *string         `json:"span_id"`
}

type trainingPairsResponse struct {
	Items      []trainingPairItem `json:"items"`
	NextCursor *int64             `json:"next_cursor"`
	PageSize   int                `json:"page_size"`
}

// trainingPairsStatsResponse is the corpus-shape banner. Every label_kind
// bucket is always present (zero-filled) so the 5-cell mini-bar
// renders consistent geometry across filter narrowings.
type trainingPairsStatsResponse struct {
	TotalPairs    int            `json:"total_pairs"`
	ByLabelKind   map[string]int `json:"by_label_kind"`
	ByQuerySource map[string]int `json:"by_query_source"`
	ByAction      map[string]int `json:"by_action"`
}

// --- handlers ---------------------------------------------------------

// telemetryTrajectoryByID handles GET /telemetry/trajectories/{query_id}.
// The query_id path segment is the grounding_events.id INTEGER PK.
func (s AppState) telemetryTrajectoryByID(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("query_id")
	queryID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || queryID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid query_id"})
		return
	}

	traj, ok, err := s.loadTrajectory(r, queryID)
	if err != nil {
		dbErr(w, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "query not found"})
		return
	}

	// A trajectory is content-stable: query_interactions is
	// append-only-with-click_kind-refinement; the rate of change is
	// per-trajectory, never per-bucket-rollup. Bounded cache matches
	// the events-detail endpoint.
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, traj)
}

// telemetryTrajectoryBySpan handles GET /telemetry/trajectories?span_id=<uuid>.
// Returns the wrapped {trajectories: [...]} shape because a single
// tools/call (span) can legally fan multiple grounding_events.
func (s AppState) telemetryTrajectoryBySpan(w http.ResponseWriter, r *http.Request) {
	spanID := r.URL.Query().Get("span_id")
	if spanID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "either query_id (path) or span_id (query param) required",
		})
		return
	}
	if !looksLikeUUIDv7(spanID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid span_id"})
		return
	}

	ids, err := s.queryIDsForSpan(r, spanID)
	if err != nil {
		dbErr(w, err)
		return
	}

	out := trajectoryBySpanResponse{Trajectories: []trajectoryResponse{}}
	for _, id := range ids {
		traj, ok, err := s.loadTrajectory(r, id)
		if err != nil {
			dbErr(w, err)
			return
		}
		if ok {
			out.Trajectories = append(out.Trajectories, traj)
		}
	}

	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, out)
}

// telemetryVolumeBySource handles GET /telemetry/analytics/volume-by-source.
// Reads proj_query_volume_by_source, groups by (day, segment),
// returns a chart-ready time-series.
func (s AppState) telemetryVolumeBySource(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	segment, ok := parseSegment(q.Get("segment"))
	if !ok {
		writeJSON(w, http.StatusBadRequest, segmentError(q.Get("segment")))
		return
	}
	since, until := parseChartRange(q.Get("since"), q.Get("until"))
	project := projectFilter(r)

	binds := db.NewArgs()
	var sb strings.Builder
	sb.WriteString("SELECT day, ")
	sb.WriteString(segment)
	sb.WriteString(", SUM(query_count) AS qc")
	sb.WriteString(" FROM proj_query_volume_by_source WHERE day >= ? AND day <= ?")
	binds.AddString(since).AddString(until)
	if project != "" {
		sb.WriteString(" AND project_id = ?")
		binds.AddString(project)
	}
	sb.WriteString(" GROUP BY day, ")
	sb.WriteString(segment)
	sb.WriteString(" ORDER BY day ASC LIMIT ?")
	binds.AddInt64(int64(chartMaxBuckets) * 16) // headroom for many segments per day

	rows, err := s.Pool.DB().QueryContext(r.Context(), sb.String(), binds.Slice()...)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()

	buckets := []analyticsVolumeBucket{}
	totals := map[string]int{}
	byDay := map[string]*analyticsVolumeBucket{}
	for rows.Next() {
		var day, segVal string
		var count int
		if err := rows.Scan(&day, &segVal, &count); err != nil {
			dbErr(w, err)
			return
		}
		bk, exists := byDay[day]
		if !exists {
			buckets = append(buckets, analyticsVolumeBucket{Day: day, Segments: map[string]int{}})
			bk = &buckets[len(buckets)-1]
			byDay[day] = bk
		}
		bk.Segments[segVal] += count
		totals[segVal] += count
	}
	if err := rows.Err(); err != nil {
		dbErr(w, err)
		return
	}

	if len(buckets) > chartMaxBuckets {
		buckets = buckets[:chartMaxBuckets]
	}

	// no-cache; today's bucket is mutable.
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, analyticsVolumeResponse{
		Segment:         segment,
		Buckets:         buckets,
		TotalsBySegment: totals,
	})
}

// telemetrySuccessRate handles GET /telemetry/analytics/success-rate.
// proj_retrieval_success_per_query is one row per query (not pre-bucketed
// by day), so we JOIN to grounding_events for the date + query_source
// and GROUP BY day server-side. success_rate is computed in Go from the
// pair counts so the JSON only carries the three primitives.
func (s AppState) telemetrySuccessRate(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	segment, ok := parseSegment(q.Get("segment"))
	if !ok {
		writeJSON(w, http.StatusBadRequest, segmentError(q.Get("segment")))
		return
	}
	since, until := parseChartRange(q.Get("since"), q.Get("until"))
	project := projectFilter(r)

	// Column on the joined grounding_events table that matches the
	// caller's segment axis. action is on both tables; query_source is
	// on grounding_events only. We pin to grounding_events for both so
	// the GROUP BY shape stays uniform.
	segCol := "ge." + segment

	binds := db.NewArgs()
	var sb strings.Builder
	sb.WriteString("SELECT substr(ge.created_at, 1, 10) AS day, ")
	sb.WriteString(segCol)
	sb.WriteString(" AS seg, COUNT(*) AS qc, SUM(p.success) AS sc")
	sb.WriteString(" FROM proj_retrieval_success_per_query p")
	sb.WriteString(" JOIN grounding_events ge ON ge.id = p.grounding_event_id")
	sb.WriteString(" WHERE substr(ge.created_at, 1, 10) >= ? AND substr(ge.created_at, 1, 10) <= ?")
	binds.AddString(since).AddString(until)
	if project != "" {
		sb.WriteString(" AND p.project_id = ?")
		binds.AddString(project)
	}
	sb.WriteString(" GROUP BY day, seg ORDER BY day ASC LIMIT ?")
	binds.AddInt64(int64(chartMaxBuckets) * 16)

	rows, err := s.Pool.DB().QueryContext(r.Context(), sb.String(), binds.Slice()...)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()

	buckets := []analyticsSuccessBucket{}
	totals := map[string]analyticsSuccessCell{}
	byDay := map[string]*analyticsSuccessBucket{}
	for rows.Next() {
		var day, segVal string
		var qc, sc int
		if err := rows.Scan(&day, &segVal, &qc, &sc); err != nil {
			dbErr(w, err)
			return
		}
		bk, exists := byDay[day]
		if !exists {
			buckets = append(buckets, analyticsSuccessBucket{
				Day:      day,
				Segments: map[string]analyticsSuccessCell{},
			})
			bk = &buckets[len(buckets)-1]
			byDay[day] = bk
		}
		cell := analyticsSuccessCell{
			QueryCount:   qc,
			SuccessCount: sc,
			SuccessRate:  successRate(qc, sc),
		}
		bk.Segments[segVal] = cell
		t := totals[segVal]
		t.QueryCount += qc
		t.SuccessCount += sc
		totals[segVal] = t
	}
	if err := rows.Err(); err != nil {
		dbErr(w, err)
		return
	}
	for k, v := range totals {
		v.SuccessRate = successRate(v.QueryCount, v.SuccessCount)
		totals[k] = v
	}

	if len(buckets) > chartMaxBuckets {
		buckets = buckets[:chartMaxBuckets]
	}

	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, analyticsSuccessResponse{
		Segment:         segment,
		Buckets:         buckets,
		TotalsBySegment: totals,
	})
}

// telemetryTrainingPairs handles GET /telemetry/training-pairs. Cursor
// descends on training_id (AUTOINCREMENT PK; monotonic). Repeatable
// label_kind / query_source filters are OR-composed within each name,
// AND-composed across names.
func (s AppState) telemetryTrainingPairs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := parseLimit(q.Get("limit"), telemetryLimitDefault, telemetryLimitMax)

	labelKinds, err := validatedEnum(q["label_kind"], telemetryLabelKinds, "label_kind")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	querySources, err := validatedEnum(q["query_source"], telemetryQuerySources, "query_source")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	binds := db.NewArgs()
	var sb strings.Builder
	sb.WriteString(`SELECT t.training_id, t.grounding_event_id, t.query_text,
	    t.candidate_pointer_id, t.source_ref, t.candidate_position,
	    t.label_kind, t.weight, t.label_sources, t.query_source,
	    t.was_injected, t.prompt_id, t.span_id
	    FROM proj_training_data_for_reranker t`)

	project := projectFilter(r)
	needsJoin := project != ""
	if needsJoin {
		sb.WriteString(" JOIN grounding_events ge ON ge.id = t.grounding_event_id")
	}
	sb.WriteString(" WHERE 1=1")

	if cursor := q.Get("cursor"); cursor != "" {
		if c, perr := strconv.ParseInt(cursor, 10, 64); perr == nil && c > 0 {
			sb.WriteString(" AND t.training_id < ?")
			binds.AddInt64(c)
		}
	}
	appendInClause(&sb, binds, "t.label_kind", labelKinds)
	appendInClause(&sb, binds, "t.query_source", querySources)
	if needsJoin {
		sb.WriteString(" AND ge.project_id = ?")
		binds.AddString(project)
	}
	if text := q.Get("q"); text != "" {
		sb.WriteString(" AND t.query_text LIKE ? ESCAPE '\\' COLLATE NOCASE")
		binds.AddString("%" + escapeLikePattern(text) + "%")
	}

	sb.WriteString(" ORDER BY t.training_id DESC LIMIT ?")
	binds.AddInt64(int64(limit + 1))

	rows, err := s.Pool.DB().QueryContext(r.Context(), sb.String(), binds.Slice()...)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()

	items := []trainingPairItem{}
	for rows.Next() {
		item, err := scanTrainingPair(rows)
		if err != nil {
			dbErr(w, err)
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		dbErr(w, err)
		return
	}

	resp := trainingPairsResponse{Items: items, PageSize: limit}
	if len(items) > limit {
		tail := items[limit-1].TrainingID
		resp.NextCursor = &tail
		resp.Items = items[:limit]
	}

	if q.Get("cursor") != "" {
		w.Header().Set("Cache-Control", "public, max-age=300")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	writeJSON(w, http.StatusOK, resp)
}

// telemetryTrainingPairsStats handles GET /telemetry/training-pairs/stats.
// Three aggregate queries, one per axis, all zero-filled so the
// 5-cell label_kind mini-bar (and the 4-cell query_source bar) render
// uniformly across filter narrowings.
func (s AppState) telemetryTrainingPairsStats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	labelKinds, err := validatedEnum(q["label_kind"], telemetryLabelKinds, "label_kind")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	querySources, err := validatedEnum(q["query_source"], telemetryQuerySources, "query_source")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	project := projectFilter(r)

	resp := trainingPairsStatsResponse{
		ByLabelKind:   zeroFill(telemetryLabelKinds),
		ByQuerySource: zeroFill(telemetryQuerySources),
		ByAction:      map[string]int{},
	}

	totalByAxis := func(axis string, target *map[string]int) error {
		binds := db.NewArgs()
		var sb strings.Builder
		sb.WriteString("SELECT ")
		sb.WriteString(axis)
		sb.WriteString(" AS k, COUNT(*) AS c FROM proj_training_data_for_reranker t")

		joinForAction := axis == "ge.action"
		if project != "" || joinForAction {
			sb.WriteString(" JOIN grounding_events ge ON ge.id = t.grounding_event_id")
		}
		sb.WriteString(" WHERE 1=1")
		appendInClause(&sb, binds, "t.label_kind", labelKinds)
		appendInClause(&sb, binds, "t.query_source", querySources)
		if project != "" {
			sb.WriteString(" AND ge.project_id = ?")
			binds.AddString(project)
		}
		sb.WriteString(" GROUP BY k")

		rows, err := s.Pool.DB().QueryContext(r.Context(), sb.String(), binds.Slice()...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			var c int
			if err := rows.Scan(&k, &c); err != nil {
				return err
			}
			(*target)[k] = c
			resp.TotalPairs += c
		}
		return rows.Err()
	}

	// Run by_label_kind separately to compute the total exactly once
	// (sum of any of the three axes is the total). Use label_kind as
	// the canonical axis to total against because it's the closed
	// 5-value enum that appears in every row.
	resp.TotalPairs = 0
	if err := totalByAxis("t.label_kind", &resp.ByLabelKind); err != nil {
		dbErr(w, err)
		return
	}

	// Recount the secondary axes WITHOUT bumping TotalPairs (the count
	// is already pinned). totalByAxis adds to TotalPairs every call;
	// pin and restore around the next two queries.
	pinnedTotal := resp.TotalPairs
	if err := totalByAxis("t.query_source", &resp.ByQuerySource); err != nil {
		dbErr(w, err)
		return
	}
	if err := totalByAxis("ge.action", &resp.ByAction); err != nil {
		dbErr(w, err)
		return
	}
	resp.TotalPairs = pinnedTotal

	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, resp)
}

// --- helpers ----------------------------------------------------------

// loadTrajectory composes the four-section trajectory record for one
// grounding_event_id. Returns (zero, false, nil) on absent row.
func (s AppState) loadTrajectory(r *http.Request, queryID int64) (trajectoryResponse, bool, error) {
	var out trajectoryResponse
	out.Query.QueryID = queryID

	// Section 1: the grounding_events row.
	row := s.Pool.DB().QueryRowContext(r.Context(), `
	    SELECT span_id, prompt_id, session_id, parent_span_id,
	           project_id, action, query_source, query_text,
	           results_count, created_at
	    FROM grounding_events WHERE id = ?`, queryID)
	var (
		prompt    sql.NullString
		parent    sql.NullString
		queryText sql.NullString
	)
	if err := row.Scan(
		&out.Query.SpanID, &prompt, &out.Query.SessionID, &parent,
		&out.Query.ProjectID, &out.Query.Action, &out.Query.QuerySource, &queryText,
		&out.Query.ResultsCount, &out.Query.CreatedAt,
	); errors.Is(err, sql.ErrNoRows) {
		return trajectoryResponse{}, false, nil
	} else if err != nil {
		return trajectoryResponse{}, false, err
	}
	if prompt.Valid {
		s := prompt.String
		out.Query.PromptID = &s
	}
	if parent.Valid {
		s := parent.String
		out.Query.ParentSpanID = &s
	}
	if queryText.Valid {
		s := queryText.String
		out.Query.QueryText = &s
	}

	// Section 2: the result set. json_each.key is 0-indexed; +1 for the
	// 1-indexed position convention. knowledge_pointers join is
	// project-scoped to avoid pulling pointers from other projects
	// that happen to share a source_ref.
	resRows, err := s.Pool.DB().QueryContext(r.Context(), `
	    SELECT
	        CAST(je.key AS INTEGER) + 1 AS position,
	        je.value AS source_ref,
	        (SELECT kp.source_type FROM knowledge_pointers kp
	           WHERE kp.source_ref = je.value AND kp.project_id = ge.project_id
	           LIMIT 1) AS source_type,
	        (SELECT kp.id FROM knowledge_pointers kp
	           WHERE kp.source_ref = je.value AND kp.project_id = ge.project_id
	           LIMIT 1) AS candidate_pointer_id
	    FROM grounding_events ge, json_each(ge.source_refs) AS je
	    WHERE ge.id = ?
	    ORDER BY je.key ASC`, queryID)
	if err != nil {
		return trajectoryResponse{}, false, err
	}
	out.Results = []trajectoryResult{}
	for resRows.Next() {
		var (
			rr      trajectoryResult
			st      sql.NullString
			pointer sql.NullInt64
		)
		if err := resRows.Scan(&rr.Position, &rr.SourceRef, &st, &pointer); err != nil {
			resRows.Close()
			return trajectoryResponse{}, false, err
		}
		if st.Valid {
			s := st.String
			rr.SourceType = &s
		}
		if pointer.Valid {
			id := pointer.Int64
			rr.CandidatePointerID = &id
		}
		out.Results = append(out.Results, rr)
	}
	if err := resRows.Err(); err != nil {
		resRows.Close()
		return trajectoryResponse{}, false, err
	}
	resRows.Close()

	// Section 3: query_interactions for this query.
	intRows, err := s.Pool.DB().QueryContext(r.Context(), `
	    SELECT id, source_ref, position, click_kind, click_weight,
	           citation_kind, dwell_ms_estimate, was_injected, detected_at
	    FROM query_interactions WHERE grounding_event_id = ?
	    ORDER BY detected_at ASC, id ASC`, queryID)
	if err != nil {
		return trajectoryResponse{}, false, err
	}
	out.Interactions = []trajectoryInteraction{}
	for intRows.Next() {
		var (
			ti       trajectoryInteraction
			position sql.NullInt64
			cite     sql.NullString
			dwell    sql.NullInt64
		)
		if err := intRows.Scan(
			&ti.InteractionID, &ti.SourceRef, &position, &ti.ClickKind, &ti.ClickWeight,
			&cite, &dwell, &ti.WasInjected, &ti.DetectedAt,
		); err != nil {
			intRows.Close()
			return trajectoryResponse{}, false, err
		}
		if position.Valid {
			p := int(position.Int64)
			ti.Position = &p
		}
		if cite.Valid {
			s := cite.String
			ti.CitationKind = &s
		}
		if dwell.Valid {
			v := dwell.Int64
			ti.DwellMsEstimate = &v
		}
		out.Interactions = append(out.Interactions, ti)
	}
	if err := intRows.Err(); err != nil {
		intRows.Close()
		return trajectoryResponse{}, false, err
	}
	intRows.Close()

	// Section 4: query_resolutions whose grounding_event_ids array
	// contains this query_id. json_each + EXISTS keeps the JSON walk
	// scoped to the right rows.
	resoRows, err := s.Pool.DB().QueryContext(r.Context(), `
	    SELECT resolution_id, entity_kind, entity_slug, entity_project_id,
	           outcome_kind, write_event_ids, detected_at
	    FROM query_resolutions
	    WHERE EXISTS (
	        SELECT 1 FROM json_each(query_resolutions.grounding_event_ids) je
	        WHERE CAST(je.value AS INTEGER) = ?
	    )
	    ORDER BY detected_at ASC`, queryID)
	if err != nil {
		return trajectoryResponse{}, false, err
	}
	out.Resolutions = []trajectoryResolution{}
	for resoRows.Next() {
		var (
			tr            trajectoryResolution
			writeEventTxt string
		)
		if err := resoRows.Scan(
			&tr.ResolutionID, &tr.EntityKind, &tr.EntitySlug, &tr.EntityProjectID,
			&tr.OutcomeKind, &writeEventTxt, &tr.DetectedAt,
		); err != nil {
			resoRows.Close()
			return trajectoryResponse{}, false, err
		}
		if writeEventTxt == "" {
			tr.WriteEventIDs = json.RawMessage("[]")
		} else {
			tr.WriteEventIDs = json.RawMessage(writeEventTxt)
		}
		out.Resolutions = append(out.Resolutions, tr)
	}
	if err := resoRows.Err(); err != nil {
		resoRows.Close()
		return trajectoryResponse{}, false, err
	}
	resoRows.Close()

	return out, true, nil
}

// queryIDsForSpan looks up every grounding_events.id whose span_id
// matches. Returns an empty slice when no rows match (the caller
// chooses whether that's 404 or an empty 200).
func (s AppState) queryIDsForSpan(r *http.Request, spanID string) ([]int64, error) {
	rows, err := s.Pool.DB().QueryContext(r.Context(),
		`SELECT id FROM grounding_events WHERE span_id = ? ORDER BY id ASC`, spanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// scanTrainingPair reads one row from the training-pair browser query
// into trainingPairItem.
func scanTrainingPair(rows *sql.Rows) (trainingPairItem, error) {
	var (
		item        trainingPairItem
		queryText   sql.NullString
		pointerID   sql.NullInt64
		labelSrcTxt string
		promptID    sql.NullString
		spanID      sql.NullString
	)
	if err := rows.Scan(
		&item.TrainingID, &item.GroundingEventID, &queryText,
		&pointerID, &item.SourceRef, &item.CandidatePosition,
		&item.LabelKind, &item.Weight, &labelSrcTxt, &item.QuerySource,
		&item.WasInjected, &promptID, &spanID,
	); err != nil {
		return trainingPairItem{}, err
	}
	if queryText.Valid {
		s := queryText.String
		item.QueryText = &s
	}
	if pointerID.Valid {
		v := pointerID.Int64
		item.CandidatePointerID = &v
	}
	if promptID.Valid {
		s := promptID.String
		item.PromptID = &s
	}
	if spanID.Valid {
		s := spanID.String
		item.SpanID = &s
	}
	if labelSrcTxt == "" {
		item.LabelSources = json.RawMessage("[]")
	} else {
		item.LabelSources = json.RawMessage(labelSrcTxt)
	}
	return item, nil
}

// parseSegment validates the segment query param against the closed
// telemetrySegments set. Returns ("", false) for the missing-or-invalid
// case so the caller can produce the canonical error.
func parseSegment(raw string) (string, bool) {
	if _, ok := telemetrySegments[raw]; !ok {
		return "", false
	}
	return raw, true
}

// segmentError is the 400 body for a missing or invalid segment param.
// The error message names BOTH valid axes so the caller doesn't have
// to read the design doc to fix their request.
func segmentError(raw string) map[string]string {
	if raw == "" {
		return map[string]string{"error": "segment query param required; valid values: action, query_source"}
	}
	return map[string]string{"error": "invalid segment: " + raw + " (valid values: action, query_source)"}
}

// parseChartRange folds the since/until query params into a (since, until)
// YYYY-MM-DD pair. Default range is the last 30 days inclusive of today.
// Invalid date strings reset to defaults rather than 400 — analytics
// charts degrade to "wider range" not "broken request".
func parseChartRange(since, until string) (string, string) {
	today := time.Now().UTC().Format("2006-01-02")
	thirtyAgo := time.Now().UTC().AddDate(0, 0, -30).Format("2006-01-02")
	if !looksLikeISODate(since) {
		since = thirtyAgo
	}
	if !looksLikeISODate(until) {
		until = today
	}
	return since, until
}

// looksLikeISODate is a cheap shape check for YYYY-MM-DD. Doesn't
// validate the calendar (Feb 30 sneaks through) — the SQL comparison
// is lexicographic on the TEXT column, so a malformed date can't
// damage the query, just match zero rows.
func looksLikeISODate(s string) bool {
	if len(s) != 10 {
		return false
	}
	for i, r := range s {
		switch i {
		case 4, 7:
			if r != '-' {
				return false
			}
		default:
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// successRate divides safely; (0, 0) returns 0.0 rather than NaN.
func successRate(qc, sc int) float64 {
	if qc == 0 {
		return 0
	}
	return float64(sc) / float64(qc)
}

// validatedEnum filters a list of caller-supplied values down to the
// allowed set. Returns an error naming the bad value so the operator
// can fix their request. An empty input slice passes through (the
// no-filter case).
func validatedEnum(values []string, allowed map[string]struct{}, name string) ([]string, error) {
	for _, v := range values {
		if _, ok := allowed[v]; !ok {
			return nil, errors.New("invalid " + name + ": " + v)
		}
	}
	return values, nil
}

// appendInClause writes ` AND <col> IN (?, ?, ...)` and pushes the bind
// values. Empty `vals` is a no-op (no filter applied).
func appendInClause(sb *strings.Builder, binds *db.Args, col string, vals []string) {
	if len(vals) == 0 {
		return
	}
	sb.WriteString(" AND ")
	sb.WriteString(col)
	sb.WriteString(" IN (")
	for i, v := range vals {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("?")
		binds.AddString(v)
	}
	sb.WriteString(")")
}

// zeroFill returns a map with every key from `keys` mapped to 0. Used
// to pre-populate the stats response so missing categories render as
// explicit zero rather than absent keys (consistent chart geometry).
func zeroFill(keys map[string]struct{}) map[string]int {
	out := make(map[string]int, len(keys))
	for k := range keys {
		out[k] = 0
	}
	return out
}
