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

// reference-resolution-substrate-frontend chain RF2: HTTP readers for the
// Context Pull Inspector page (RF3). Sits on top of the reference-
// resolution-substrate's grounding_events emits (query_source =
// 'reference_resolution', migration 040) and the side-table from
// migration 042 (reference_resolution_emits).
//
// See docs/REFERENCE_RESOLUTION_FRONTEND.md for the load-bearing decisions:
//   - §2 four-axis disambiguation (query_source / action / source_type /
//     shape). Filter param names use exact column names; NO generic
//     'source' or 'kind' that conflates the axes.
//   - §3 endpoint catalog.
//   - §4 pagination and filter shape.
//   - §9 entity-detail integration uses prompt_id as the trajectory
//     unit (NOT span_id) — per TT1 §2's three-layer hierarchy.
//
// Five endpoints, all mounted under /context-pulls/* in router.go:
//
//   - GET /context-pulls                                — paginated list, filterable
//   - GET /context-pulls/{grounding_event_id}           — per-resolution detail + cross-substrate joins
//   - GET /context-pulls/by-entity/{kind}/{slug}        — entity-scoped (prompt_id join)
//   - GET /context-pulls/stats                          — banner distributions
//   - GET /context-pulls/stats/timeseries               — trend bars
//
// Discipline mirrors telemetry.go / events.go:
//   - explicit SELECT lists (no SELECT *)
//   - db.NewArgs() for every parameter bind
//   - snake_case JSON tags; field names name which axis they belong to
//   - per-row payloads stay compact; the drawer endpoint is the place
//     that fetches the full per-resolution detail

const contextPullsLimitDefault = 50
const contextPullsLimitMax = 200

// contextPullsQuerySources is the closed set of query_source values from
// migrations 037/040/041. Validated for both the list filter and the
// available-axis legend rendered on the page header.
var contextPullsQuerySources = map[string]struct{}{
	"agent_initiated":               {},
	"proactive_hook":                {},
	"dashboard_user":                {},
	"reference_resolution":          {},
	"harness_reminder_interception": {},
	"other":                         {},
}

// contextPullsConfidenceTiers is the closed enum from refresolve.types
// (TierSingleExact / TierFuzzyMulti / TierWeakDomain / TierNoHit).
var contextPullsConfidenceTiers = map[string]struct{}{
	"single_exact": {},
	"fuzzy_multi":  {},
	"weak_domain":  {},
	"no_hit":       {},
}

// contextPullsEntityKinds is the closed set the entity-by-slug endpoint
// admits. Matches query_resolutions.entity_kind's CHECK from migration
// 037 — same surface as audit-ledger's /entities/{kind}/{slug}/events.
var contextPullsEntityKinds = map[string]struct{}{
	"bug":   {},
	"task":  {},
	"chain": {},
}

// contextPullsStatsSegments is the closed enum for the timeseries
// segment axis. shape / confidence_tier / source_type — exactly the
// columns the inspector's stats banner buckets across.
var contextPullsStatsSegments = map[string]struct{}{
	"shape":           {},
	"confidence_tier": {},
	"source_type":     {},
}

// --- wire types -------------------------------------------------------

// contextPullRow is one row in the inspector list view. Compact compared
// to contextPullDetail — the drawer fetches detail separately when the
// operator opens a row.
type contextPullRow struct {
	GroundingEventID           int64               `json:"grounding_event_id"`
	Ts                         string              `json:"ts"`
	ProjectID                  string              `json:"project_id"`
	SessionID                  string              `json:"session_id"`
	PromptID                   *string             `json:"prompt_id"`
	SpanID                     *string             `json:"span_id"`
	ParentSpanID               *string             `json:"parent_span_id"`
	Action                     string              `json:"action"`
	QuerySource                string              `json:"query_source"`
	QueryText                  *string             `json:"query_text"`
	Shape                      *string             `json:"shape"`
	ConfidenceTier             *string             `json:"confidence_tier"`
	PresentationRecommendation *string             `json:"presentation_recommendation"`
	PresentedAs                *string             `json:"presented_as"`
	ResultsCount               int                 `json:"results_count"`
	FirstCandidate             *contextPullPointer `json:"first_candidate"`
	ClickKindsFired            []string            `json:"click_kinds_fired"`
	MLConfidenceScore          *float64            `json:"ml_confidence_score"`
}

// contextPullPointer is the first-candidate summary embedded in the list
// row. source_type comes from knowledge_pointers (LEFT JOIN; nullable
// when the candidate's pointer row is absent — retired or never
// indexed).
type contextPullPointer struct {
	SourceRef  string `json:"source_ref"`
	SourceType string `json:"source_type"`
	Position   int    `json:"position"`
}

// contextPullListResponse is the cursor-paginated wrapper for §3.1.
// available_query_sources and available_shapes drive the page's
// chip-dropdown legend (legend-from-data so newly-added enum values
// appear without frontend redeploy).
type contextPullListResponse struct {
	Items                  []contextPullRow `json:"items"`
	NextCursor             *int64           `json:"next_cursor"`
	PageSize               int              `json:"page_size"`
	AvailableQuerySources  []string         `json:"available_query_sources"`
	AvailableShapes        []string         `json:"available_shapes"`
	AvailableConfidenceTrs []string         `json:"available_confidence_tiers"`
	AvailableSourceTypes   []string         `json:"available_source_types"`
}

// contextPullDetail is the per-resolution payload for the drawer (§3.2).
// Synthesizes grounding_events + reference_resolution_emits +
// query_interactions + knowledge_pointers, plus a cross-substrate
// linked_resolutions lookup.
type contextPullDetail struct {
	GroundingEvent     contextPullGroundingEvent `json:"grounding_event"`
	Detection          contextPullDetection      `json:"detection"`
	Resolver           contextPullResolver       `json:"resolver"`
	Candidates         []contextPullCandidate    `json:"candidates"`
	Outcome            contextPullOutcome        `json:"outcome"`
	Interactions       []contextPullInteraction  `json:"interactions"`
	LinkedResolutions  []contextPullLinkedRes    `json:"linked_resolutions"`
	TrajectoryDeepLink string                    `json:"trajectory_deep_link"`
}

type contextPullGroundingEvent struct {
	ID            int64   `json:"id"`
	Ts            string  `json:"ts"`
	ProjectID     string  `json:"project_id"`
	SessionID     string  `json:"session_id"`
	PromptID      *string `json:"prompt_id"`
	SpanID        *string `json:"span_id"`
	ParentSpanID  *string `json:"parent_span_id"`
	Action        string  `json:"action"`
	QuerySource   string  `json:"query_source"`
	UserMessageID *string `json:"user_message_id"`
	ResultsCount  int     `json:"results_count"`
}

// contextPullDetection mirrors the detection-context block. The
// source_message_excerpt field is reserved for the forward-fill
// transcript lookup (RF1 §3.2); RF2 returns nil and the drawer
// renders only the token until the transcript reader lands.
type contextPullDetection struct {
	Token                string  `json:"token"`
	Shape                string  `json:"shape"`
	Confidence           float64 `json:"confidence"`
	DetectionMethod      string  `json:"detection_method"`
	StartPos             int     `json:"start_pos"`
	EndPos               int     `json:"end_pos"`
	SourceMessageExcerpt *string `json:"source_message_excerpt"`
}

type contextPullResolver struct {
	Name            string  `json:"name"`
	RetrievalCostMs int64   `json:"retrieval_cost_ms"`
	Err             *string `json:"err"`
}

type contextPullCandidate struct {
	Position          int      `json:"position"`
	SourceRef         string   `json:"source_ref"`
	SourceType        *string  `json:"source_type"`
	Title             *string  `json:"title"`
	Score             *float64 `json:"score"`
	DebugNotes        *string  `json:"debug_notes"`
	MLConfidenceScore *float64 `json:"ml_confidence_score"`
}

type contextPullOutcome struct {
	ConfidenceTier             string `json:"confidence_tier"`
	PresentationRecommendation string `json:"presentation_recommendation"`
	PresentedAs                string `json:"presented_as"`
}

type contextPullInteraction struct {
	InteractionID     int64   `json:"interaction_id"`
	SourceRef         string  `json:"source_ref"`
	CandidatePosition *int    `json:"candidate_position"`
	ClickKind         string  `json:"click_kind"`
	ClickWeight       float64 `json:"click_weight"`
	WasInjected       int     `json:"was_injected"`
	DetectedAt        string  `json:"detected_at"`
}

type contextPullLinkedRes struct {
	ResolutionID    string `json:"resolution_id"`
	EntityKind      string `json:"entity_kind"`
	EntitySlug      string `json:"entity_slug"`
	EntityProjectID string `json:"entity_project_id"`
	OutcomeKind     string `json:"outcome_kind"`
}

// contextPullByEntityResponse wraps the entity-scoped list (§3.3).
// matched_prompt_ids is surfaced so the inspector can render the
// absent-state copy distinctly from "entity exists but no resolutions".
type contextPullByEntityResponse struct {
	Entity           contextPullEntityRef `json:"entity"`
	MatchedPromptIDs []string             `json:"matched_prompt_ids"`
	Items            []contextPullRow     `json:"items"`
	NextCursor       *int64               `json:"next_cursor"`
	PageSize         int                  `json:"page_size"`
}

type contextPullEntityRef struct {
	Kind      string `json:"kind"`
	Slug      string `json:"slug"`
	ProjectID string `json:"project_id"`
}

// contextPullStatsResponse is the banner-stats payload (§3.4). Every
// known axis bucket is always present (legend-from-data) so the chart
// geometry is stable across filter narrowings.
type contextPullStatsResponse struct {
	TotalReferences  int            `json:"total_references"`
	ByShape          map[string]int `json:"by_shape"`
	ByConfidenceTier map[string]int `json:"by_confidence_tier"`
	BySourceType     map[string]int `json:"by_source_type"`
	ByQuerySource    map[string]int `json:"by_query_source"`
}

type contextPullsTimeseriesBucket struct {
	Day      string         `json:"day"`
	Segments map[string]int `json:"segments"`
}

type contextPullsTimeseriesResponse struct {
	Segment string                         `json:"segment"`
	Buckets []contextPullsTimeseriesBucket `json:"buckets"`
}

// --- handlers ---------------------------------------------------------

// contextPullsList handles GET /context-pulls. Newest-first; cursor on
// grounding_events.id descending.
func (s AppState) contextPullsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := parseLimit(q.Get("limit"), contextPullsLimitDefault, contextPullsLimitMax)

	filters, errResp := parseContextPullFilters(q, r, true)
	if errResp != nil {
		writeJSON(w, http.StatusBadRequest, errResp)
		return
	}

	binds := db.NewArgs()
	var sb strings.Builder
	sb.WriteString("SELECT ")
	sb.WriteString(contextPullRowColumns)
	sb.WriteString(" FROM grounding_events ge")
	sb.WriteString(" LEFT JOIN reference_resolution_emits rre ON rre.grounding_event_id = ge.id")
	sb.WriteString(" WHERE 1=1")

	applyContextPullFilters(&sb, binds, filters)

	if cursor := q.Get("cursor"); cursor != "" {
		if c, perr := strconv.ParseInt(cursor, 10, 64); perr == nil && c > 0 {
			sb.WriteString(" AND ge.id < ?")
			binds.AddInt64(c)
		}
	}

	sb.WriteString(" ORDER BY ge.id DESC LIMIT ?")
	binds.AddInt64(int64(limit + 1))

	rows, err := s.Pool.DB().QueryContext(r.Context(), sb.String(), binds.Slice()...)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()

	items, err := scanContextPullRows(r, s.Pool.DB(), rows)
	if err != nil {
		dbErr(w, err)
		return
	}

	resp := contextPullListResponse{
		Items:                  items,
		PageSize:               limit,
		AvailableQuerySources:  closedEnumKeys(contextPullsQuerySources),
		AvailableShapes:        contextPullsAvailableShapes(),
		AvailableConfidenceTrs: closedEnumKeys(contextPullsConfidenceTiers),
		AvailableSourceTypes:   contextPullsAvailableSourceTypes(r.Context(), s.Pool.DB()),
	}
	if len(items) > limit {
		tail := items[limit-1].GroundingEventID
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

// contextPullsDetail handles GET /context-pulls/{grounding_event_id}.
// Returns 404 either on absent id OR on a row whose query_source is not
// 'reference_resolution' (the endpoint is scoped to reference-resolution
// emits; a non-reference-resolution grounding_event is out of scope).
func (s AppState) contextPullsDetail(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("grounding_event_id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid grounding_event_id"})
		return
	}

	detail, ok, err := s.loadContextPullDetail(r, id)
	if err != nil {
		dbErr(w, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "reference resolution not found"})
		return
	}

	// Per-row content is immutable post-emit (the side-table is
	// append-only; query_interactions append over time but the typical
	// per-event read targets a stable snapshot). Bounded cache matches
	// /events/{event_id}.
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, detail)
}

// contextPullsByEntity handles GET /context-pulls/by-entity/{kind}/{slug}.
// Joins through query_resolutions.prompt_id (the user-arc trajectory key
// per TT1 §2 three-layer hierarchy) to surface reference resolutions
// that fired while the agent was working on the named entity.
func (s AppState) contextPullsByEntity(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	slug := r.PathValue("slug")
	if _, ok := contextPullsEntityKinds[kind]; !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown entity_kind: " + kind})
		return
	}
	if slug == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "entity slug is required"})
		return
	}
	project := projectFilter(r)
	if project == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "project (or project_id) query parameter is required — entity slugs aren't globally unique across projects",
		})
		return
	}

	q := r.URL.Query()
	limit := parseLimit(q.Get("limit"), contextPullsLimitDefault, contextPullsLimitMax)
	outcomeKind := q.Get("outcome_kind") // empty = any outcome

	promptIDs, err := s.queryResolutionsPromptIDs(r, kind, slug, project, outcomeKind)
	if err != nil {
		dbErr(w, err)
		return
	}

	resp := contextPullByEntityResponse{
		Entity:           contextPullEntityRef{Kind: kind, Slug: slug, ProjectID: project},
		MatchedPromptIDs: promptIDs,
		Items:            []contextPullRow{},
		PageSize:         limit,
	}

	if len(promptIDs) == 0 {
		w.Header().Set("Cache-Control", "no-cache")
		writeJSON(w, http.StatusOK, resp)
		return
	}

	binds := db.NewArgs()
	var sb strings.Builder
	sb.WriteString("SELECT ")
	sb.WriteString(contextPullRowColumns)
	sb.WriteString(" FROM grounding_events ge")
	sb.WriteString(" LEFT JOIN reference_resolution_emits rre ON rre.grounding_event_id = ge.id")
	sb.WriteString(" WHERE ge.query_source = 'reference_resolution'")
	sb.WriteString(" AND ge.prompt_id IN (")
	for i, pid := range promptIDs {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("?")
		binds.AddString(pid)
	}
	sb.WriteString(")")

	if cursor := q.Get("cursor"); cursor != "" {
		if c, perr := strconv.ParseInt(cursor, 10, 64); perr == nil && c > 0 {
			// Entity-arc reads chronologically — page ASC.
			sb.WriteString(" AND ge.id > ?")
			binds.AddInt64(c)
		}
	}

	sb.WriteString(" ORDER BY ge.id ASC LIMIT ?")
	binds.AddInt64(int64(limit + 1))

	rows, err := s.Pool.DB().QueryContext(r.Context(), sb.String(), binds.Slice()...)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()

	items, err := scanContextPullRows(r, s.Pool.DB(), rows)
	if err != nil {
		dbErr(w, err)
		return
	}

	resp.Items = items
	if len(items) > limit {
		tail := items[limit-1].GroundingEventID
		resp.NextCursor = &tail
		resp.Items = items[:limit]
	}

	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, resp)
}

// contextPullsStats handles GET /context-pulls/stats. Honors the same
// filter set as the list endpoint so the banner updates as the operator
// narrows.
func (s AppState) contextPullsStats(w http.ResponseWriter, r *http.Request) {
	filters, errResp := parseContextPullFilters(r.URL.Query(), r, true)
	if errResp != nil {
		writeJSON(w, http.StatusBadRequest, errResp)
		return
	}

	totalRefs, byShape, byTier, bySource, byQS, err := s.aggregateContextPullStats(r, filters)
	if err != nil {
		dbErr(w, err)
		return
	}

	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, contextPullStatsResponse{
		TotalReferences:  totalRefs,
		ByShape:          byShape,
		ByConfidenceTier: byTier,
		BySourceType:     bySource,
		ByQuerySource:    byQS,
	})
}

// contextPullsStatsTimeseries handles GET /context-pulls/stats/timeseries.
// Daily bucketed counts segmented by shape | confidence_tier |
// source_type. Mirrors the QF1 telemetry timeseries shape so the
// dashboard can reuse the same recharts <BarChart> wiring.
func (s AppState) contextPullsStatsTimeseries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	segment := q.Get("segment")
	if segment == "" {
		segment = "shape"
	}
	if _, ok := contextPullsStatsSegments[segment]; !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid segment: " + segment + " (valid values: shape, confidence_tier, source_type)",
		})
		return
	}
	filters, errResp := parseContextPullFilters(q, r, true)
	if errResp != nil {
		writeJSON(w, http.StatusBadRequest, errResp)
		return
	}

	buckets, err := s.aggregateContextPullTimeseries(r, filters, segment)
	if err != nil {
		dbErr(w, err)
		return
	}

	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, contextPullsTimeseriesResponse{
		Segment: segment,
		Buckets: buckets,
	})
}

// --- filter parsing ---------------------------------------------------

// contextPullFilters bundles the shared filter knobs across the list,
// stats, and timeseries endpoints. Pin-the-axis discipline: every
// caller-supplied filter names the canonical column name.
type contextPullFilters struct {
	querySources    []string
	shapes          []string
	confidenceTiers []string
	sourceTypes     []string
	sessionID       string
	promptID        string
	spanID          string
	project         string
	queryText       string
	since           string
	until           string
}

// parseContextPullFilters reads the four-axis filter set + scoping +
// time range. defaultQuerySource=true applies the reference_resolution
// default when no query_source filter is supplied (the canonical scope
// for this surface); pass false for endpoints that don't impose the
// default (currently none).
func parseContextPullFilters(q map[string][]string, r *http.Request, defaultQuerySource bool) (contextPullFilters, map[string]string) {
	f := contextPullFilters{
		querySources:    q["query_source"],
		shapes:          q["shape"],
		confidenceTiers: q["confidence_tier"],
		sourceTypes:     q["source_type"],
		project:         projectFilter(r),
	}
	if len(f.querySources) == 0 && defaultQuerySource {
		f.querySources = []string{"reference_resolution"}
	}

	for _, qs := range f.querySources {
		if _, ok := contextPullsQuerySources[qs]; !ok {
			return f, map[string]string{"error": "invalid query_source: " + qs}
		}
	}
	for _, tier := range f.confidenceTiers {
		if _, ok := contextPullsConfidenceTiers[tier]; !ok {
			return f, map[string]string{"error": "invalid confidence_tier: " + tier}
		}
	}
	// Shapes and source_types are open-ended (new values land via
	// detector / pointer-emit code without dashboard rollouts), so we
	// don't whitelist — the SQL safely binds whatever string lands.

	if v := getFirst(q, "session_id"); v != "" {
		f.sessionID = v
	}
	if v := getFirst(q, "prompt_id"); v != "" {
		f.promptID = v
	}
	if v := getFirst(q, "span_id"); v != "" {
		f.spanID = v
	}
	if v := getFirst(q, "q"); v != "" {
		f.queryText = v
	} else if v := getFirst(q, "reference_text"); v != "" {
		// reference_text is an alias for q (RF1 §3.1); one of them honored.
		f.queryText = v
	}
	if v := getFirst(q, "since"); v != "" {
		f.since = v
	}
	if v := getFirst(q, "until"); v != "" {
		f.until = v
	}
	return f, nil
}

// applyContextPullFilters appends AND-composed clauses to sb and pushes
// binds. Caller has already opened the SELECT and the WHERE 1=1 base.
func applyContextPullFilters(sb *strings.Builder, binds *db.Args, f contextPullFilters) {
	appendInClause(sb, binds, "ge.query_source", f.querySources)
	appendInClause(sb, binds, "rre.shape", f.shapes)
	appendInClause(sb, binds, "rre.confidence_tier", f.confidenceTiers)
	if len(f.sourceTypes) > 0 {
		// source_type filter: ge.source_refs entries are `<type>:<rest>`
		// strings authored by refresolve/resolvers_*.go ("chain:my-slug",
		// "skill:...", "schema:..."). The type prefix IS the source_type
		// by construction, so the filter is just a LIKE-prefix match on
		// any element of the JSON array. The earlier knowledge_pointers
		// JOIN here was a no-op: ge.source_refs uses "<type>:<rest>" but
		// kp.source_ref uses "<project>::<slug>" — they never matched,
		// so the filter silently rejected every row. Bug
		// `context-pulls-first-candidate-source-type-empty-due-to-source-
		// ref-format-mismatch`.
		sb.WriteString(" AND EXISTS (")
		sb.WriteString("SELECT 1 FROM json_each(ge.source_refs) je WHERE (")
		for i, st := range f.sourceTypes {
			if i > 0 {
				sb.WriteString(" OR ")
			}
			sb.WriteString("je.value LIKE ?")
			// Escape LIKE meta-chars in the source_type (defensive: enum
			// values are alphanumeric + underscore today, but pinning the
			// escape keeps the filter safe if the closed set widens).
			binds.AddString(escapeLikePattern(st) + ":%")
		}
		sb.WriteString("))")
	}
	if f.sessionID != "" {
		sb.WriteString(" AND ge.session_id = ?")
		binds.AddString(f.sessionID)
	}
	if f.promptID != "" {
		sb.WriteString(" AND ge.prompt_id = ?")
		binds.AddString(f.promptID)
	}
	if f.spanID != "" {
		sb.WriteString(" AND ge.span_id = ?")
		binds.AddString(f.spanID)
	}
	if f.project != "" {
		sb.WriteString(" AND ge.project_id = ?")
		binds.AddString(f.project)
	}
	if f.queryText != "" {
		sb.WriteString(" AND ge.query_text LIKE ? ESCAPE '\\' COLLATE NOCASE")
		binds.AddString("%" + escapeLikePattern(f.queryText) + "%")
	}
	if f.since != "" {
		sb.WriteString(" AND ge.created_at >= ?")
		binds.AddString(f.since)
	}
	if f.until != "" {
		sb.WriteString(" AND ge.created_at <= ?")
		binds.AddString(f.until)
	}
}

// --- list-row scanning ------------------------------------------------

// contextPullRowColumns is the explicit SELECT list shared by the list
// and by-entity endpoints. Pins to scanContextPullRow's column order.
const contextPullRowColumns = `ge.id, ge.created_at, ge.project_id, ge.session_id,
    ge.prompt_id, ge.span_id, ge.parent_span_id, ge.action, ge.query_source,
    ge.query_text, ge.results_count, ge.source_refs,
    rre.shape, rre.confidence_tier, rre.presentation_recommendation,
    rre.presented_as, rre.ml_confidence_score`

// scanContextPullRows reads list-row results. Each row's first_candidate
// is resolved through a per-row knowledge_pointers lookup (matches the
// per-row dispatch — knowledge_pointers JOIN can't run cheaply against
// json_each on the row's source_refs).
//
// click_kinds_fired is fetched in one followup query that aggregates
// query_interactions per grounding_event_id; cheaper than N+1.
func scanContextPullRows(r *http.Request, sqlDB *sql.DB, rows *sql.Rows) ([]contextPullRow, error) {
	out := []contextPullRow{}
	ids := []int64{}
	// Index, not pointer: append grows the slice and reallocates, which
	// would invalidate any &out[i] taken before the grow. Indices stay
	// stable across appends. Caught after RF3 ship — 84 orphan rows
	// showed first_candidate=null in production because of this.
	indexByID := map[int64]int{}
	projectByID := map[int64]string{}
	sourceRefsByID := map[int64]string{}

	for rows.Next() {
		var (
			row             contextPullRow
			promptID        sql.NullString
			spanID          sql.NullString
			parentSpanID    sql.NullString
			queryText       sql.NullString
			sourceRefsTxt   string
			shape           sql.NullString
			tier            sql.NullString
			presentationRec sql.NullString
			presentedAs     sql.NullString
			mlConfidence    sql.NullFloat64
		)
		if err := rows.Scan(
			&row.GroundingEventID, &row.Ts, &row.ProjectID, &row.SessionID,
			&promptID, &spanID, &parentSpanID, &row.Action, &row.QuerySource,
			&queryText, &row.ResultsCount, &sourceRefsTxt,
			&shape, &tier, &presentationRec, &presentedAs, &mlConfidence,
		); err != nil {
			return nil, err
		}
		if promptID.Valid {
			s := promptID.String
			row.PromptID = &s
		}
		if spanID.Valid {
			s := spanID.String
			row.SpanID = &s
		}
		if parentSpanID.Valid {
			s := parentSpanID.String
			row.ParentSpanID = &s
		}
		if queryText.Valid {
			s := queryText.String
			row.QueryText = &s
		}
		if shape.Valid {
			s := shape.String
			row.Shape = &s
		}
		if tier.Valid {
			s := tier.String
			row.ConfidenceTier = &s
		}
		if presentationRec.Valid {
			s := presentationRec.String
			row.PresentationRecommendation = &s
		}
		if presentedAs.Valid {
			s := presentedAs.String
			row.PresentedAs = &s
		}
		if mlConfidence.Valid {
			v := mlConfidence.Float64
			row.MLConfidenceScore = &v
		}
		row.ClickKindsFired = []string{}
		out = append(out, row)
		indexByID[row.GroundingEventID] = len(out) - 1
		projectByID[row.GroundingEventID] = row.ProjectID
		sourceRefsByID[row.GroundingEventID] = sourceRefsTxt
		ids = append(ids, row.GroundingEventID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(ids) == 0 {
		return out, nil
	}

	// first_candidate: walk each row's source_refs JSON array; first
	// non-empty element is position 1. Source ref strings are authored
	// by refresolve/resolvers_*.go as `<source_type>:<rest>` (e.g.
	// "chain:my-slug", "skill:body/path", "schema:blueprints/..."), so
	// source_type derives from the prefix without a DB lookup.
	//
	// The earlier batched knowledge_pointers JOIN here was buggy: the
	// pointers table uses `<project>::<slug>` keying, which never
	// matched ge.source_refs entries — every row landed with empty
	// source_type. See bug `context-pulls-first-candidate-source-type-
	// empty-due-to-source-ref-format-mismatch`.
	for _, id := range ids {
		var refs []string
		if err := json.Unmarshal([]byte(sourceRefsByID[id]), &refs); err != nil {
			continue
		}
		if len(refs) == 0 || refs[0] == "" {
			continue
		}
		sref := refs[0]
		fc := &contextPullPointer{SourceRef: sref, Position: 1}
		if idx := strings.IndexByte(sref, ':'); idx > 0 {
			fc.SourceType = sref[:idx]
		}
		out[indexByID[id]].FirstCandidate = fc
	}

	// click_kinds_fired: one query collecting DISTINCT click_kind per
	// grounding_event_id over the page's id set.
	binds := db.NewArgs()
	var sb strings.Builder
	sb.WriteString("SELECT grounding_event_id, click_kind FROM query_interactions WHERE grounding_event_id IN (")
	for i, id := range ids {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("?")
		binds.AddInt64(id)
	}
	sb.WriteString(") GROUP BY grounding_event_id, click_kind")
	clickRows, err := sqlDB.QueryContext(r.Context(), sb.String(), binds.Slice()...)
	if err != nil {
		return nil, err
	}
	defer clickRows.Close()
	for clickRows.Next() {
		var gid int64
		var kind string
		if err := clickRows.Scan(&gid, &kind); err != nil {
			return nil, err
		}
		if idx, ok := indexByID[gid]; ok {
			out[idx].ClickKindsFired = append(out[idx].ClickKindsFired, kind)
		}
	}
	if err := clickRows.Err(); err != nil {
		return nil, err
	}

	return out, nil
}

// --- detail loader ----------------------------------------------------

// loadContextPullDetail composes the five-section drawer record.
// Returns (zero, false, nil) when the grounding_event_id is absent OR
// its query_source is not 'reference_resolution' (the detail endpoint
// is scoped — see RF1 §3.2 404 semantics).
func (s AppState) loadContextPullDetail(r *http.Request, id int64) (contextPullDetail, bool, error) {
	var out contextPullDetail

	// Section 1: grounding_events.
	row := s.Pool.DB().QueryRowContext(r.Context(), `
	    SELECT id, created_at, project_id, session_id,
	           prompt_id, span_id, parent_span_id, action, query_source,
	           user_message_id, results_count
	    FROM grounding_events
	    WHERE id = ? AND query_source = 'reference_resolution'`, id)
	var (
		promptID      sql.NullString
		spanID        sql.NullString
		parentSpanID  sql.NullString
		userMessageID sql.NullString
	)
	if err := row.Scan(
		&out.GroundingEvent.ID, &out.GroundingEvent.Ts, &out.GroundingEvent.ProjectID,
		&out.GroundingEvent.SessionID, &promptID, &spanID, &parentSpanID,
		&out.GroundingEvent.Action, &out.GroundingEvent.QuerySource, &userMessageID,
		&out.GroundingEvent.ResultsCount,
	); errors.Is(err, sql.ErrNoRows) {
		return contextPullDetail{}, false, nil
	} else if err != nil {
		return contextPullDetail{}, false, err
	}
	if promptID.Valid {
		s := promptID.String
		out.GroundingEvent.PromptID = &s
	}
	if spanID.Valid {
		s := spanID.String
		out.GroundingEvent.SpanID = &s
	}
	if parentSpanID.Valid {
		s := parentSpanID.String
		out.GroundingEvent.ParentSpanID = &s
	}
	if userMessageID.Valid {
		s := userMessageID.String
		out.GroundingEvent.UserMessageID = &s
	}

	// Section 2 + 3: detection + resolver + outcome — all from the
	// side-table emit row. LEFT JOIN: side-table may be absent for rows
	// emitted before migration 042 landed. Tolerated; the drawer
	// renders absent state.
	sideRow := s.Pool.DB().QueryRowContext(r.Context(), `
	    SELECT shape, confidence_score, detection_method, start_pos, end_pos,
	           confidence_tier, presentation_recommendation, presented_as,
	           resolver_name, retrieval_cost_ms, ml_confidence_score
	    FROM reference_resolution_emits
	    WHERE grounding_event_id = ?`, id)
	var (
		shape           string
		confidence      float64
		detectionMethod string
		startPos        int
		endPos          int
		tier            string
		presentationRec string
		presentedAs     string
		resolverName    string
		retrievalMs     int64
		mlScore         sql.NullFloat64
	)
	err := sideRow.Scan(
		&shape, &confidence, &detectionMethod, &startPos, &endPos,
		&tier, &presentationRec, &presentedAs, &resolverName, &retrievalMs, &mlScore,
	)
	if errors.Is(err, sql.ErrNoRows) {
		out.Detection.Token = ""
	} else if err != nil {
		return contextPullDetail{}, false, err
	} else {
		out.Detection.Shape = shape
		out.Detection.Confidence = confidence
		out.Detection.DetectionMethod = detectionMethod
		out.Detection.StartPos = startPos
		out.Detection.EndPos = endPos
		out.Resolver.Name = resolverName
		out.Resolver.RetrievalCostMs = retrievalMs
		out.Outcome.ConfidenceTier = tier
		out.Outcome.PresentationRecommendation = presentationRec
		out.Outcome.PresentedAs = presentedAs
	}

	// Token comes from grounding_events.query_text (the message_text
	// pointer flows from the parse_context handler). May be NULL for
	// legacy callers; drawer falls back to detection.token = "".
	var queryText sql.NullString
	if err := s.Pool.DB().QueryRowContext(r.Context(),
		`SELECT query_text FROM grounding_events WHERE id = ?`, id,
	).Scan(&queryText); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return contextPullDetail{}, false, err
	}
	if queryText.Valid {
		out.Detection.Token = queryText.String
	}
	// source_message_excerpt is reserved for the transcript-lookup path
	// (RF1 §3.2 forward-fill caveat). RF2 leaves it nil; the inspector
	// drawer renders only detection.token.

	// Section 4: candidates — walk source_refs JSON array, position is
	// 1-indexed (matches grounding_events SourceRefs convention), join
	// knowledge_pointers for source_type. Title / score / debug_notes
	// are NOT stored on grounding_events (they're per-resolve-call
	// envelope fields) — drawer renders absent state for now.
	candRows, err := s.Pool.DB().QueryContext(r.Context(), `
	    SELECT
	        CAST(je.key AS INTEGER) + 1 AS position,
	        je.value AS source_ref,
	        (SELECT kp.source_type FROM knowledge_pointers kp
	           WHERE kp.source_ref = je.value AND kp.project_id = ge.project_id
	           LIMIT 1) AS source_type
	    FROM grounding_events ge, json_each(ge.source_refs) AS je
	    WHERE ge.id = ?
	    ORDER BY je.key ASC`, id)
	if err != nil {
		return contextPullDetail{}, false, err
	}
	out.Candidates = []contextPullCandidate{}
	for candRows.Next() {
		var (
			c  contextPullCandidate
			st sql.NullString
		)
		if err := candRows.Scan(&c.Position, &c.SourceRef, &st); err != nil {
			candRows.Close()
			return contextPullDetail{}, false, err
		}
		if st.Valid {
			s := st.String
			c.SourceType = &s
		}
		out.Candidates = append(out.Candidates, c)
	}
	if err := candRows.Err(); err != nil {
		candRows.Close()
		return contextPullDetail{}, false, err
	}
	candRows.Close()

	// Section 5: query_interactions.
	intRows, err := s.Pool.DB().QueryContext(r.Context(), `
	    SELECT id, source_ref, position, click_kind, click_weight,
	           was_injected, detected_at
	    FROM query_interactions
	    WHERE grounding_event_id = ?
	    ORDER BY detected_at ASC, id ASC`, id)
	if err != nil {
		return contextPullDetail{}, false, err
	}
	out.Interactions = []contextPullInteraction{}
	for intRows.Next() {
		var (
			i        contextPullInteraction
			position sql.NullInt64
		)
		if err := intRows.Scan(
			&i.InteractionID, &i.SourceRef, &position, &i.ClickKind, &i.ClickWeight,
			&i.WasInjected, &i.DetectedAt,
		); err != nil {
			intRows.Close()
			return contextPullDetail{}, false, err
		}
		if position.Valid {
			p := int(position.Int64)
			i.CandidatePosition = &p
		}
		out.Interactions = append(out.Interactions, i)
	}
	if err := intRows.Err(); err != nil {
		intRows.Close()
		return contextPullDetail{}, false, err
	}
	intRows.Close()

	// Section 6: linked_resolutions — query_resolutions whose
	// grounding_event_ids JSON array contains this id. Best-effort
	// gracefully absent when the table doesn't exist (cross-substrate
	// decoupling per F2 pattern).
	out.LinkedResolutions = []contextPullLinkedRes{}
	resoRows, err := s.Pool.DB().QueryContext(r.Context(), `
	    SELECT resolution_id, entity_kind, entity_slug, entity_project_id, outcome_kind
	    FROM query_resolutions
	    WHERE EXISTS (
	        SELECT 1 FROM json_each(query_resolutions.grounding_event_ids) je
	        WHERE CAST(je.value AS INTEGER) = ?
	    )
	    ORDER BY detected_at ASC`, id)
	if err == nil {
		for resoRows.Next() {
			var lr contextPullLinkedRes
			if scanErr := resoRows.Scan(
				&lr.ResolutionID, &lr.EntityKind, &lr.EntitySlug,
				&lr.EntityProjectID, &lr.OutcomeKind,
			); scanErr != nil {
				resoRows.Close()
				return contextPullDetail{}, false, scanErr
			}
			out.LinkedResolutions = append(out.LinkedResolutions, lr)
		}
		resoRows.Close()
	}

	out.TrajectoryDeepLink = "/telemetry/trajectories/" + strconv.FormatInt(id, 10)

	return out, true, nil
}

// queryResolutionsPromptIDs returns the DISTINCT prompt_ids that
// resolved this entity. Empty slice when no resolution rows match —
// "entity created pre-substrate" or "entity hasn't been worked on yet"
// both produce the same shape (the inspector's badge copy distinguishes
// them by the matched_prompt_ids field).
func (s AppState) queryResolutionsPromptIDs(r *http.Request, kind, slug, project, outcomeKind string) ([]string, error) {
	binds := db.NewArgs()
	var sb strings.Builder
	sb.WriteString(`SELECT DISTINCT prompt_id FROM query_resolutions
	    WHERE entity_kind = ? AND entity_slug = ? AND entity_project_id = ?`)
	binds.AddString(kind).AddString(slug).AddString(project)
	if outcomeKind != "" {
		sb.WriteString(" AND outcome_kind = ?")
		binds.AddString(outcomeKind)
	}
	rows, err := s.Pool.DB().QueryContext(r.Context(), sb.String(), binds.Slice()...)
	if err != nil {
		// query_resolutions may be absent on a pre-migration-037 DB;
		// return empty rather than 500.
		return []string{}, nil
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err != nil {
			return nil, err
		}
		out = append(out, pid)
	}
	return out, rows.Err()
}

// --- stats aggregates -------------------------------------------------

// aggregateContextPullStats runs one COUNT(*) + three GROUP-BY queries
// over the same filter set. zeroFill prepares the closed-enum maps so
// the chart geometry stays uniform across narrowings.
func (s AppState) aggregateContextPullStats(r *http.Request, f contextPullFilters) (int, map[string]int, map[string]int, map[string]int, map[string]int, error) {
	byShape := map[string]int{}
	byTier := zeroFill(contextPullsConfidenceTiers)
	bySource := map[string]int{}
	byQS := zeroFill(contextPullsQuerySources)

	total := 0

	// total
	{
		binds := db.NewArgs()
		var sb strings.Builder
		sb.WriteString("SELECT COUNT(*) FROM grounding_events ge LEFT JOIN reference_resolution_emits rre ON rre.grounding_event_id = ge.id WHERE 1=1")
		applyContextPullFilters(&sb, binds, f)
		if err := s.Pool.DB().QueryRowContext(r.Context(), sb.String(), binds.Slice()...).Scan(&total); err != nil {
			return 0, nil, nil, nil, nil, err
		}
	}

	groupBy := func(col string, target map[string]int) error {
		binds := db.NewArgs()
		var sb strings.Builder
		sb.WriteString("SELECT ")
		sb.WriteString(col)
		sb.WriteString(" AS k, COUNT(*) AS c FROM grounding_events ge LEFT JOIN reference_resolution_emits rre ON rre.grounding_event_id = ge.id WHERE 1=1")
		applyContextPullFilters(&sb, binds, f)
		sb.WriteString(" GROUP BY k")
		rows, err := s.Pool.DB().QueryContext(r.Context(), sb.String(), binds.Slice()...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k sql.NullString
			var c int
			if err := rows.Scan(&k, &c); err != nil {
				return err
			}
			if !k.Valid {
				continue
			}
			target[k.String] = c
		}
		return rows.Err()
	}

	if err := groupBy("rre.shape", byShape); err != nil {
		return 0, nil, nil, nil, nil, err
	}
	if err := groupBy("rre.confidence_tier", byTier); err != nil {
		return 0, nil, nil, nil, nil, err
	}
	if err := groupBy("ge.query_source", byQS); err != nil {
		return 0, nil, nil, nil, nil, err
	}

	// by_source_type: counts each row according to the source_type of
	// its FIRST candidate (matches the list view's first_candidate).
	{
		binds := db.NewArgs()
		var sb strings.Builder
		sb.WriteString(`SELECT kp.source_type AS k, COUNT(*) AS c
		    FROM grounding_events ge
		    LEFT JOIN reference_resolution_emits rre ON rre.grounding_event_id = ge.id
		    LEFT JOIN knowledge_pointers kp
		      ON kp.project_id = ge.project_id
		     AND kp.source_ref = (SELECT je.value FROM json_each(ge.source_refs) je ORDER BY je.key ASC LIMIT 1)
		    WHERE 1=1`)
		applyContextPullFilters(&sb, binds, f)
		sb.WriteString(" GROUP BY k")
		rows, err := s.Pool.DB().QueryContext(r.Context(), sb.String(), binds.Slice()...)
		if err != nil {
			return 0, nil, nil, nil, nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var k sql.NullString
			var c int
			if err := rows.Scan(&k, &c); err != nil {
				return 0, nil, nil, nil, nil, err
			}
			if !k.Valid {
				continue
			}
			bySource[k.String] = c
		}
		if err := rows.Err(); err != nil {
			return 0, nil, nil, nil, nil, err
		}
	}

	return total, byShape, byTier, bySource, byQS, nil
}

// aggregateContextPullTimeseries returns daily-bucketed counts.
func (s AppState) aggregateContextPullTimeseries(r *http.Request, f contextPullFilters, segment string) ([]contextPullsTimeseriesBucket, error) {
	var segCol string
	switch segment {
	case "shape":
		segCol = "rre.shape"
	case "confidence_tier":
		segCol = "rre.confidence_tier"
	case "source_type":
		segCol = `(SELECT kp.source_type FROM knowledge_pointers kp
		    WHERE kp.source_ref = (SELECT je.value FROM json_each(ge.source_refs) je ORDER BY je.key ASC LIMIT 1)
		      AND kp.project_id = ge.project_id LIMIT 1)`
	}

	binds := db.NewArgs()
	var sb strings.Builder
	sb.WriteString("SELECT substr(ge.created_at, 1, 10) AS day, ")
	sb.WriteString(segCol)
	sb.WriteString(" AS seg, COUNT(*) AS c FROM grounding_events ge LEFT JOIN reference_resolution_emits rre ON rre.grounding_event_id = ge.id WHERE 1=1")
	applyContextPullFilters(&sb, binds, f)
	sb.WriteString(" GROUP BY day, seg ORDER BY day ASC LIMIT ?")
	binds.AddInt64(int64(chartMaxBuckets) * 32)

	rows, err := s.Pool.DB().QueryContext(r.Context(), sb.String(), binds.Slice()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	buckets := []contextPullsTimeseriesBucket{}
	byDay := map[string]*contextPullsTimeseriesBucket{}
	for rows.Next() {
		var day string
		var seg sql.NullString
		var c int
		if err := rows.Scan(&day, &seg, &c); err != nil {
			return nil, err
		}
		bk, exists := byDay[day]
		if !exists {
			buckets = append(buckets, contextPullsTimeseriesBucket{Day: day, Segments: map[string]int{}})
			bk = &buckets[len(buckets)-1]
			byDay[day] = bk
		}
		if seg.Valid {
			bk.Segments[seg.String] += c
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(buckets) > chartMaxBuckets {
		buckets = buckets[:chartMaxBuckets]
	}
	return buckets, nil
}

// --- enum legend helpers ----------------------------------------------

// closedEnumKeys returns the keys of a closed-enum map as a sorted-ish
// slice. Caller renders these as chip-dropdown options; consistent
// ordering across requests would be nice-to-have but the dashboard
// re-sorts anyway, so insertion order from a map literal suffices.
func closedEnumKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// contextPullsAvailableShapes lists every ShapeCategory the detector
// currently emits. Mirrors go/internal/refresolve/types.go without
// importing it (the inspector page reads the wire shape, not the Go
// constants — and the import would couple two layers that don't need
// the coupling).
func contextPullsAvailableShapes() []string {
	return []string{
		"chain_slug", "task_slug", "bug_slug",
		"path",
		"skill_name", "project_name", "tool_name", "forge_schema",
		"library_entry",
		"domain_term", "external_technical",
		"friction_shape",
		"skill_trigger", "memory_entry", "vault_candidate", "kiwix_bridge",
		"discipline_skill",
		"skill_candidate",
	}
}

// contextPullsAvailableSourceTypes returns the DISTINCT set of source_type
// prefixes that actually appear in grounding_events.source_refs (each
// entry's text before the first ':'). Reading from the live data shape
// keeps the filter dropdown in sync with the prefixes refresolve/
// resolvers_*.go authors today — including types like `skill`, `schema`,
// `path`, `memory`, `tool` that have no knowledge_pointers row.
//
// The prior implementation read DISTINCT source_type from knowledge_
// pointers, which is a different keying scheme — pointers use
// `<project>::<slug>` for source_ref + a closed-ish enum for
// source_type (bug, chain, task, vault, library, kiwix_reference). The
// resulting dropdown showed values like `kiwix_reference` that never
// match any grounding_events row, and missed values like `skill` that
// every reference-resolution fire emits. Bug `context-pulls-first-
// candidate-source-type-empty-due-to-source-ref-format-mismatch`.
func contextPullsAvailableSourceTypes(ctx context.Context, sqlDB *sql.DB) []string {
	rows, err := sqlDB.QueryContext(ctx, `
		SELECT DISTINCT SUBSTR(je.value, 1, INSTR(je.value, ':') - 1) AS source_type
		FROM grounding_events ge, json_each(ge.source_refs) je
		WHERE INSTR(je.value, ':') > 1
		ORDER BY source_type`)
	if err != nil {
		return []string{}
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return out
		}
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// getFirst pulls a single value out of the URL-query map without
// allocating a *http.Request slice.
func getFirst(q map[string][]string, key string) string {
	if v, ok := q[key]; ok && len(v) > 0 {
		return v[0]
	}
	return ""
}
