package observehttp

import (
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"toolkit/internal/db"
)

// ── /benchmarks (list) ─────────────────────────────────────────────────────

type benchmarkRow struct {
	ID           string  `json:"id"`
	ProjectID    string  `json:"project_id"`
	ScenarioID   string  `json:"scenario_id"`
	ToolName     string  `json:"tool_name"`
	ModelName    string  `json:"model_name"`
	RunID        *string `json:"run_id"`
	RunAt        int64   `json:"run_at"`
	WallClockMs  int64   `json:"wall_clock_ms"`
	InputTokens  *int64  `json:"input_tokens"`
	OutputTokens *int64  `json:"output_tokens"`
	InvocationOK int64   `json:"invocation_ok"`
}

func (s AppState) benchmarksList(w http.ResponseWriter, r *http.Request) {
	project := projectFilter(r)
	toolName := r.URL.Query().Get("tool_name")
	modelName := r.URL.Query().Get("model_name")
	runID := r.URL.Query().Get("run_id")
	since, hasSince := optSince(r)
	limit := int64(500)
	if v := r.URL.Query().Get("limit"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
			limit = parsed
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 5000 {
		limit = 5000
	}

	var b strings.Builder
	b.WriteString(`SELECT id, project_id, scenario_id, tool_name, model_name, run_id, run_at,
	                      wall_clock_ms, input_tokens, output_tokens, invocation_ok
	               FROM proj_benchmark_results WHERE 1=1`)
	binds := db.NewArgs()
	if project != "" {
		b.WriteString(" AND project_id = ?")
		binds.AddString(project)
	}
	if toolName != "" {
		b.WriteString(" AND tool_name = ?")
		binds.AddString(toolName)
	}
	if modelName != "" {
		b.WriteString(" AND model_name = ?")
		binds.AddString(modelName)
	}
	if runID != "" {
		b.WriteString(" AND run_id = ?")
		binds.AddString(runID)
	}
	if hasSince {
		b.WriteString(" AND run_at >= ?")
		binds.AddInt64(since)
	}
	b.WriteString(fmt.Sprintf(" ORDER BY run_at DESC LIMIT %d", limit))

	rows, err := s.Pool.DB().QueryContext(r.Context(), b.String(), binds.Slice()...)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()
	out := []benchmarkRow{}
	for rows.Next() {
		var br benchmarkRow
		if err := rows.Scan(&br.ID, &br.ProjectID, &br.ScenarioID, &br.ToolName,
			&br.ModelName, &br.RunID, &br.RunAt, &br.WallClockMs,
			&br.InputTokens, &br.OutputTokens, &br.InvocationOK); err != nil {
			dbErr(w, err)
			return
		}
		out = append(out, br)
	}
	if err := rows.Err(); err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ── /benchmarks/timeseries ─────────────────────────────────────────────────

type TimeseriesPoint struct {
	BucketStart     int64   `json:"bucket_start"`
	ModelName       string  `json:"model_name"`
	Layer           string  `json:"layer"`
	Total           int64   `json:"total"`
	OkCount         int64   `json:"ok_count"`
	ArgsOkCount     *int64  `json:"args_ok_count"`
	InterpOkCount   *int64  `json:"interp_ok_count"`
	MeanWallClockMs float64 `json:"mean_wall_clock_ms"`
}

func (s AppState) benchmarksTimeseries(w http.ResponseWriter, r *http.Request) {
	project := projectFilter(r)
	toolName := r.URL.Query().Get("tool_name")
	modelName := r.URL.Query().Get("model_name")
	layer := r.URL.Query().Get("layer")
	bucket := int64(3600)
	if v := r.URL.Query().Get("bucket_seconds"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
			bucket = parsed
		}
	}
	if bucket < 60 {
		bucket = 60
	}
	since, hasSince := optSince(r)

	var b strings.Builder
	fmt.Fprintf(&b, `SELECT (run_at / %d) * %d AS bucket_start,
	                        model_name,
	                        layer,
	                        COUNT(*) AS total,
	                        COALESCE(SUM(invocation_ok), 0) AS ok_count,
	                        SUM(args_match) AS args_ok_count,
	                        SUM(interpretation_ok) AS interp_ok_count,
	                        AVG(wall_clock_ms) AS mean_wall_clock_ms
	                 FROM proj_benchmark_results WHERE 1=1`, bucket, bucket)
	binds := db.NewArgs()
	if project != "" {
		b.WriteString(" AND project_id = ?")
		binds.AddString(project)
	}
	if toolName != "" {
		b.WriteString(" AND tool_name = ?")
		binds.AddString(toolName)
	}
	if modelName != "" {
		b.WriteString(" AND model_name = ?")
		binds.AddString(modelName)
	}
	if layer != "" {
		b.WriteString(" AND layer = ?")
		binds.AddString(layer)
	}
	if hasSince {
		b.WriteString(" AND run_at >= ?")
		binds.AddInt64(since)
	}
	b.WriteString(` GROUP BY bucket_start, model_name, layer
	                ORDER BY bucket_start, model_name, layer`)

	rows, err := s.Pool.DB().QueryContext(r.Context(), b.String(), binds.Slice()...)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()
	out := []TimeseriesPoint{}
	for rows.Next() {
		var p TimeseriesPoint
		var layerStr sql.NullString
		var meanLatency sql.NullFloat64
		if err := rows.Scan(&p.BucketStart, &p.ModelName, &layerStr,
			&p.Total, &p.OkCount, &p.ArgsOkCount, &p.InterpOkCount,
			&meanLatency); err != nil {
			dbErr(w, err)
			return
		}
		if layerStr.Valid {
			p.Layer = layerStr.String
		} else {
			// Backfill 012 set every existing row's layer; defensive default
			// matches the Rust handler for rows that slipped in mid-migration.
			p.Layer = "l3"
		}
		p.MeanWallClockMs = meanLatency.Float64
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ── /benchmarks/cards ──────────────────────────────────────────────────────

type ShapeCard struct {
	TaskShape string         `json:"task_shape"`
	Models    []ModelMetrics `json:"models"`
}

func (s AppState) benchmarksCards(w http.ResponseWriter, r *http.Request) {
	recentN := clampRecentN(r)
	project := projectFilter(r)
	since, hasSince := optSince(r)

	sqlStr, args := windowedCardsQuery(
		`task_shape, model_name`,
		`task_shape, model_name, wall_clock_ms,
		 input_tokens, output_tokens,
		 accuracy_score, honesty_score,
		 ranking_quality_score, within_budget_score`,
		// Empty-string task_shape (ping health-checks) is not a real
		// shape and has no AXES_BY_SHAPE entry dashboard-side, so exclude
		// it alongside NULL.
		`WHERE task_shape IS NOT NULL AND task_shape != ''`,
		project, since, hasSince, recentN,
	)
	rows, err := s.Pool.DB().QueryContext(r.Context(), sqlStr, args.Slice()...)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()
	var card cardScan
	cards := []cardRow{}
	for rows.Next() {
		if err := rows.Scan(&card.shape, &card.model, &card.wallClock,
			&card.input, &card.output, &card.accuracy, &card.honesty,
			&card.ranking, &card.withinBudget); err != nil {
			dbErr(w, err)
			return
		}
		cards = append(cards, cardRow{
			Key:                 card.shape,
			ModelName:           card.model,
			WallClockMs:         card.wallClock,
			InputTokens:         nullIntPtr(card.input),
			OutputTokens:        nullIntPtr(card.output),
			AccuracyScore:       nullFloatPtr(card.accuracy),
			HonestyScore:        nullFloatPtr(card.honesty),
			RankingQualityScore: nullFloatPtr(card.ranking),
			WithinBudgetScore:   nullFloatPtr(card.withinBudget),
		})
	}
	if err := rows.Err(); err != nil {
		dbErr(w, err)
		return
	}
	entries := aggregateToModelMetrics(cards)
	byKey := map[string]*ShapeCard{}
	order := []string{}
	for _, e := range entries {
		c, ok := byKey[e.Key]
		if !ok {
			c = &ShapeCard{TaskShape: e.Key, Models: []ModelMetrics{}}
			byKey[e.Key] = c
			order = append(order, e.Key)
		}
		c.Models = append(c.Models, e.Metrics)
	}
	out := make([]ShapeCard, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	writeJSON(w, http.StatusOK, out)
}

// ── /benchmarks/rubric-cards ───────────────────────────────────────────────

type RubricCard struct {
	RubricName         string         `json:"rubric_name"`
	Deployable         bool           `json:"deployable"`
	Verdict            string         `json:"verdict"`
	VerdictNote        string         `json:"verdict_note"`
	RetriggerCondition *string        `json:"retrigger_condition"`
	Models             []ModelMetrics `json:"models"`
}

func (s AppState) benchmarksRubricCards(w http.ResponseWriter, r *http.Request) {
	recentN := clampRecentN(r)
	project := projectFilter(r)
	since, hasSince := optSince(r)

	sqlStr, args := windowedCardsQuery(
		`task_id, model_name`,
		`task_id AS rubric_name, model_name, wall_clock_ms,
		 input_tokens, output_tokens,
		 accuracy_score, honesty_score,
		 ranking_quality_score, within_budget_score`,
		// Empty-string task_id (ping health-checks) is not a real rubric;
		// exclude it alongside NULL so it can't seed a phantom card.
		`WHERE task_id IS NOT NULL AND task_id != ''`,
		project, since, hasSince, recentN,
	)
	rows, err := s.Pool.DB().QueryContext(r.Context(), sqlStr, args.Slice()...)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()
	var c cardScan
	cards := []cardRow{}
	for rows.Next() {
		if err := rows.Scan(&c.shape, &c.model, &c.wallClock,
			&c.input, &c.output, &c.accuracy, &c.honesty,
			&c.ranking, &c.withinBudget); err != nil {
			dbErr(w, err)
			return
		}
		cards = append(cards, cardRow{
			Key:                 c.shape, // shape field reused as rubric_name here
			ModelName:           c.model,
			WallClockMs:         c.wallClock,
			InputTokens:         nullIntPtr(c.input),
			OutputTokens:        nullIntPtr(c.output),
			AccuracyScore:       nullFloatPtr(c.accuracy),
			HonestyScore:        nullFloatPtr(c.honesty),
			RankingQualityScore: nullFloatPtr(c.ranking),
			WithinBudgetScore:   nullFloatPtr(c.withinBudget),
		})
	}
	if err := rows.Err(); err != nil {
		dbErr(w, err)
		return
	}
	entries := aggregateToModelMetrics(cards)

	byKey := map[string]*RubricCard{}
	// Seed every registered rubric so the response carries placeholder
	// cards alongside the active ones (matches Rust behaviour).
	for _, m := range rubricRegistry {
		byKey[m.Name] = &RubricCard{
			RubricName:         m.Name,
			Deployable:         m.Deployable,
			Verdict:            m.Verdict,
			VerdictNote:        m.VerdictNote,
			RetriggerCondition: m.RetriggerCondition,
			Models:             []ModelMetrics{},
		}
	}
	for _, e := range entries {
		c, ok := byKey[e.Key]
		if !ok {
			c = &RubricCard{
				RubricName:  e.Key,
				Verdict:     "Unknown",
				VerdictNote: "rubric not in rubric_lib::registry",
				Models:      []ModelMetrics{},
			}
			byKey[e.Key] = c
		}
		c.Models = append(c.Models, e.Metrics)
	}
	names := make([]string, 0, len(byKey))
	for n := range byKey {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]RubricCard, 0, len(names))
	for _, n := range names {
		out = append(out, *byKey[n])
	}
	writeJSON(w, http.StatusOK, out)
}

// ── /benchmarks/tasks ──────────────────────────────────────────────────────

type TaskCard struct {
	TaskID             string         `json:"task_id"`
	TaskShape          string         `json:"task_shape"`
	Deployable         bool           `json:"deployable"`
	Verdict            *string        `json:"verdict"`
	VerdictNote        *string        `json:"verdict_note"`
	RetriggerCondition *string        `json:"retrigger_condition"`
	Models             []ModelMetrics `json:"models"`
}

func (s AppState) benchmarksTasks(w http.ResponseWriter, r *http.Request) {
	recentN := clampRecentN(r)
	project := projectFilter(r)
	since, hasSince := optSince(r)

	// Per-task aggregation excludes rows with empty detected_tool so a
	// half-graded run doesn't poison the verdict distribution.
	innerSelect := `task_id, task_shape, model_name, run_at,
	                wall_clock_ms, input_tokens, output_tokens,
	                accuracy_score, honesty_score,
	                ranking_quality_score, within_budget_score,
	                detected_tool,
	                ROW_NUMBER() OVER (
	                    PARTITION BY task_id, model_name
	                    ORDER BY run_at DESC
	                ) AS rn
	         FROM proj_benchmark_results
	         WHERE task_id IS NOT NULL AND task_id != ''
	           AND task_shape IS NOT NULL AND task_shape != ''
	           AND (detected_tool IS NULL OR detected_tool != '')`
	outerSelect := `task_id, task_shape, model_name, wall_clock_ms,
	                input_tokens, output_tokens,
	                accuracy_score, honesty_score,
	                ranking_quality_score, within_budget_score,
	                detected_tool`

	var b strings.Builder
	b.WriteString("WITH ranked AS (\n        SELECT ")
	b.WriteString(innerSelect)
	binds := db.NewArgs()
	if project != "" {
		b.WriteString(" AND project_id = ?")
		binds.AddString(project)
	}
	if hasSince {
		b.WriteString(" AND run_at >= ?")
		binds.AddInt64(since)
	}
	b.WriteString("\n    ) SELECT ")
	b.WriteString(outerSelect)
	b.WriteString(" FROM ranked WHERE rn <= ?")
	binds.AddInt64(recentN)

	rows, err := s.Pool.DB().QueryContext(r.Context(), b.String(), binds.Slice()...)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()

	shapeByTask := map[string]string{}
	cards := []cardRow{}
	for rows.Next() {
		var (
			taskID, shape, model string
			wallClock            int64
			input, output        sql.NullInt64
			accuracy, honesty    sql.NullFloat64
			ranking, withinBud   sql.NullFloat64
			detected             sql.NullString
		)
		if err := rows.Scan(&taskID, &shape, &model, &wallClock,
			&input, &output, &accuracy, &honesty, &ranking, &withinBud,
			&detected); err != nil {
			dbErr(w, err)
			return
		}
		if _, ok := shapeByTask[taskID]; !ok {
			shapeByTask[taskID] = shape
		}
		cards = append(cards, cardRow{
			Key:                 taskID,
			ModelName:           model,
			WallClockMs:         wallClock,
			InputTokens:         nullIntPtr(input),
			OutputTokens:        nullIntPtr(output),
			AccuracyScore:       nullFloatPtr(accuracy),
			HonestyScore:        nullFloatPtr(honesty),
			RankingQualityScore: nullFloatPtr(ranking),
			WithinBudgetScore:   nullFloatPtr(withinBud),
			VerdictLabel:        nullStringPtr(detected),
		})
	}
	if err := rows.Err(); err != nil {
		dbErr(w, err)
		return
	}

	entries := aggregateToModelMetrics(cards)

	byKey := map[string]*TaskCard{}
	// Seed from observed rows.
	for taskID, shape := range shapeByTask {
		tc := &TaskCard{
			TaskID:     taskID,
			TaskShape:  shape,
			Deployable: true,
			Models:     []ModelMetrics{},
		}
		if rb, ok := rubricLookup(taskID); ok {
			tc.Deployable = rb.Deployable
			v := rb.Verdict
			tc.Verdict = &v
			n := rb.VerdictNote
			tc.VerdictNote = &n
			tc.RetriggerCondition = rb.RetriggerCondition
		}
		byKey[taskID] = tc
	}
	// Seed zero-row rubric placeholders.
	for _, m := range rubricRegistry {
		if _, exists := byKey[m.Name]; exists {
			continue
		}
		shape := "Classify"
		if m.Name == "pre-context-summarization" {
			shape = "Summarize"
		}
		v := m.Verdict
		n := m.VerdictNote
		byKey[m.Name] = &TaskCard{
			TaskID:             m.Name,
			TaskShape:          shape,
			Deployable:         m.Deployable,
			Verdict:            &v,
			VerdictNote:        &n,
			RetriggerCondition: m.RetriggerCondition,
			Models:             []ModelMetrics{},
		}
	}
	for _, e := range entries {
		if tc, ok := byKey[e.Key]; ok {
			tc.Models = append(tc.Models, e.Metrics)
		}
	}
	names := make([]string, 0, len(byKey))
	for n := range byKey {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]TaskCard, 0, len(names))
	for _, n := range names {
		out = append(out, *byKey[n])
	}
	writeJSON(w, http.StatusOK, out)
}

// ── shared helpers ─────────────────────────────────────────────────────────

type cardScan struct {
	shape, model                             string
	wallClock                                int64
	input, output                            sql.NullInt64
	accuracy, honesty, ranking, withinBudget sql.NullFloat64
}

func clampRecentN(r *http.Request) int64 {
	n := int64(50)
	if v := r.URL.Query().Get("recent_n"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
			n = parsed
		}
	}
	if n < 1 {
		n = 1
	}
	if n > 5000 {
		n = 5000
	}
	return n
}

// windowedCardsQuery builds the WITH ranked … SELECT … WHERE rn <= ?
// sandwich used by /cards and /rubric-cards. partitionCols names the
// PARTITION BY columns; outerSelect names the columns to project; extra
// is the WHERE clause inside the CTE (excluding the project/since binds).
// Returns the SQL string and a populated *db.Args; the caller splats
// binds.Slice()... into QueryContext.
func windowedCardsQuery(partitionCols, outerSelect, extra, project string, since int64, hasSince bool, recentN int64) (string, *db.Args) {
	innerSelect := `task_shape, model_name, run_at,
	                wall_clock_ms, input_tokens, output_tokens,
	                accuracy_score, honesty_score,
	                ranking_quality_score, within_budget_score,
	                ROW_NUMBER() OVER (
	                    PARTITION BY ` + partitionCols + `
	                    ORDER BY run_at DESC
	                ) AS rn`
	// For /rubric-cards we project task_id as rubric_name, so we add it to inner.
	if strings.Contains(partitionCols, "task_id") {
		innerSelect = `task_id, ` + innerSelect
	}

	var b strings.Builder
	b.WriteString("WITH ranked AS (\n        SELECT ")
	b.WriteString(innerSelect)
	b.WriteString(" FROM proj_benchmark_results ")
	b.WriteString(extra)
	binds := db.NewArgs()
	if project != "" {
		b.WriteString(" AND project_id = ?")
		binds.AddString(project)
	}
	if hasSince {
		b.WriteString(" AND run_at >= ?")
		binds.AddInt64(since)
	}
	b.WriteString("\n    ) SELECT ")
	b.WriteString(outerSelect)
	b.WriteString(" FROM ranked WHERE rn <= ?")
	binds.AddInt64(recentN)
	return b.String(), binds
}

func nullIntPtr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

func nullFloatPtr(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	v := n.Float64
	return &v
}

func nullStringPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
}
