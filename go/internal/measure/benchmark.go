package measure

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/mcpparam"
)

// BenchmarkResult mirrors crates/measure-lib::types::BenchmarkResult — one row
// of the benchmark_results table. JSON shape matches the Rust handler's
// response (sqlx::FromRow + serde::Serialize) so dashboards and callers see
// identical keys across the migration boundary.
type BenchmarkResult struct {
	ID                  string   `json:"id"`
	ProjectID           string   `json:"project_id"`
	ScenarioID          string   `json:"scenario_id"`
	ToolName            string   `json:"tool_name"`
	ModelName           string   `json:"model_name"`
	RunID               *string  `json:"run_id"`
	RunAt               int64    `json:"run_at"`
	WallClockMS         int64    `json:"wall_clock_ms"`
	InputTokens         *int64   `json:"input_tokens"`
	OutputTokens        *int64   `json:"output_tokens"`
	InvokedContextually int64    `json:"invoked_contextually"`
	InvocationOK        int64    `json:"invocation_ok"`
	ArgsMatch           *int64   `json:"args_match"`
	ExtractedArgs       *string  `json:"extracted_args"`
	InterpretationOK    *int64   `json:"interpretation_ok"`
	DetectedTool        *string  `json:"detected_tool"`
	Notes               *string  `json:"notes"`
	TaskShape           *string  `json:"task_shape"`
	AccuracyScore       *float64 `json:"accuracy_score"`
	HonestyScore        *float64 `json:"honesty_score"`
	RankingQualityScore *float64 `json:"ranking_quality_score"`
	WithinBudgetScore   *float64 `json:"within_budget_score"`
	// ProvenanceID joins to benchmark_provenance(id). Required-by-trigger
	// after migration 035 (T6 cutover). Callers obtain it by recording the
	// provenance row + emitting BenchmarkRunStarted first (the Rust
	// benchmarks harness handles this via BenchmarkDb::start_run; Go-side
	// callers will gain a sister helper when needed).
	ProvenanceID string `json:"provenance_id"`
}

// BenchmarkDeps holds dependencies for benchmark handlers. Separate from
// ClassifyDeps because the recording surface doesn't need a rubric registry
// or inference router.
type BenchmarkDeps struct {
	Pool *db.Pool
}

// BenchmarkRecordResult is the response shape for benchmark_record.
// On success: OK + ID populated. On param-missing: Error populated. omitempty
// keeps the JSON output identical to the prior map[string]any/string shapes.
type BenchmarkRecordResult struct {
	OK    bool   `json:"ok,omitempty"`
	ID    string `json:"id,omitempty"`
	Error string `json:"error,omitempty"`
}

// HandleBenchmarkRecord implements the benchmark_record action. Mirrors the
// Rust dispatch/measure.rs branch + measure_lib::benchmarks::record_benchmark.
// Required params: scenario_id, tool_name, model_name, run_at, wall_clock_ms,
// invocation_ok. Project comes from the top-level dispatch scope.
func HandleBenchmarkRecord(ctx context.Context, deps BenchmarkDeps, project string, params json.RawMessage) (BenchmarkRecordResult, error) {
	if project == "" {
		return BenchmarkRecordResult{Error: "project is required"}, nil
	}
	row, parseErr := parseBenchmarkResult(project, params)
	if parseErr != "" {
		return BenchmarkRecordResult{Error: parseErr}, nil
	}
	if row.ID == "" {
		row.ID = newUUIDv4()
	}
	if err := insertBenchmarkRow(ctx, deps.Pool, row); err != nil {
		return BenchmarkRecordResult{}, fmt.Errorf("record benchmark: %w", err)
	}
	return BenchmarkRecordResult{OK: true, ID: row.ID}, nil
}

// HandleBenchmarkQuery implements the benchmark_query action. Mirrors
// measure_lib::benchmarks::query_benchmarks. Filters: tool_name, model_name,
// run_id, since, limit. Project comes from the top-level dispatch scope; an
// empty project means cross-project.
func HandleBenchmarkQuery(ctx context.Context, deps BenchmarkDeps, project string, params json.RawMessage) ([]BenchmarkResult, error) {
	filters := benchmarkFilters{
		Project:   project,
		ToolName:  mcpparam.String(params, "tool_name"),
		ModelName: mcpparam.String(params, "model_name"),
		RunID:     mcpparam.String(params, "run_id"),
		Since:     mcpparam.Int64Opt(params, "since"),
		Limit:     mcpparam.Int64Opt(params, "limit"),
	}
	rows, err := queryBenchmarkRows(ctx, deps.Pool, filters)
	if err != nil {
		return nil, fmt.Errorf("query benchmarks: %w", err)
	}
	return rows, nil
}

type benchmarkFilters struct {
	Project   string
	ToolName  string
	ModelName string
	RunID     string
	Since     *int64
	Limit     *int64
}

// insertBenchmarkRow runs the same 22-column INSERT as the Rust
// measure_lib::benchmarks::record_benchmark — PARITY_STANDARD §1a (verbatim
// SQL). The newer benchmark_results columns (layer, task_id, rubric_name,
// run_shape) are intentionally not written here; the Rust source doesn't
// populate them from this handler either. record_benchmark_dispatch (Go
// equivalent: db.RecordBenchmarkDispatch) handles those.
//
// Nullable columns receive *string / *int64 / *float64 pointers directly;
// database/sql's default value converter serialises nil pointers as NULL
// and dereferences non-nil pointers to the underlying value.
func insertBenchmarkRow(ctx context.Context, pool *db.Pool, r BenchmarkResult) error {
	// T5-benchmarks: CRUD INSERT INTO benchmark_results dropped; the
	// fold for BenchmarkRunCompleted constructs proj_benchmark_results
	// from the payload (with identifying columns added in T5-benchmarks's
	// payload bump).
	return pool.WithWrite(ctx, func(tx *sql.Tx) error {
		// Per T3 of agent-substrate-crud-retirement (§9.6 audit finding),
		// the benchmark_results INSERT is paired with a BenchmarkRunCompleted
		// emit carrying the rubric-side columns so payload-only fold
		// reconstruction (T5's contract) can rebuild proj_benchmark_results
		// without joining the CRUD table. HandleBenchmarkRecord does NOT
		// emit a Started event today (the row already represents a
		// terminal observation handed in by the caller), so the Completed
		// event here is the sole substrate record for this row.
		runIDForEmit := ""
		if r.RunID != nil {
			runIDForEmit = *r.RunID
		}
		// T5-benchmarks: emit is the sole write path now; don't skip on
		// empty run_id. Use the row id as a fallback entity_slug so the
		// envelope stays well-formed.
		entityID := runIDForEmit
		if entityID == "" {
			entityID = r.ID
		}
		_, emitErr := events.Emit(ctx, tx, events.EmitArgs{
			Entity:  events.NewCrossCuttingEntityRef("benchmark_run", entityID),
			Payload: buildBenchmarkRunCompletedFromRow(r),
		})
		return emitErr
	})
}

// buildBenchmarkRunCompletedFromRow constructs a BenchmarkRunCompletedPayload
// from a BenchmarkResult row — including the optional result_columns block
// (T3 of agent-substrate-crud-retirement, §9.6) populated from every rubric-
// side column of the row. nil pointers stay nil so payload-validator's
// additionalProperties:false on the sub-object stays satisfied.
func buildBenchmarkRunCompletedFromRow(r BenchmarkResult) events.BenchmarkRunCompletedPayload {
	runIDStr := ""
	if r.RunID != nil {
		runIDStr = *r.RunID
	}
	// T5-benchmarks: schema requires run_id minLength 1; fall back to
	// row id when the caller didn't supply one.
	if runIDStr == "" {
		runIDStr = r.ID
	}
	var input, output *int
	if r.InputTokens != nil {
		n := int(*r.InputTokens)
		input = &n
	}
	if r.OutputTokens != nil {
		n := int(*r.OutputTokens)
		output = &n
	}
	rc := &events.BenchmarkResultColumns{
		ToolName:            r.ToolName,
		ModelName:           r.ModelName,
		TaskShape:           r.TaskShape,
		AccuracyScore:       r.AccuracyScore,
		HonestyScore:        r.HonestyScore,
		RankingQualityScore: r.RankingQualityScore,
		WithinBudgetScore:   r.WithinBudgetScore,
		InvocationOK:        r.InvocationOK != 0,
		ExtractedArgs:       r.ExtractedArgs,
		DetectedTool:        r.DetectedTool,
		Notes:               r.Notes,
		InvokedContextually: r.InvokedContextually != 0,
	}
	if r.ArgsMatch != nil {
		v := *r.ArgsMatch != 0
		rc.ArgsMatch = &v
	}
	if r.InterpretationOK != nil {
		v := *r.InterpretationOK != 0
		rc.InterpretationOK = &v
	}
	// T5-benchmarks identifying columns (additive payload bump).
	idCopy := r.ID
	projCopy := r.ProjectID
	scenarioCopy := r.ScenarioID
	runAt := r.RunAt
	pl := events.BenchmarkRunCompletedPayload{
		RunID:             runIDStr,
		WallClockMS:       int(r.WallClockMS),
		InputTokens:       input,
		OutputTokens:      output,
		ResultColumns:     rc,
		BenchmarkResultID: &idCopy,
		ProjectID:         &projCopy,
		ScenarioID:        &scenarioCopy,
		RunAt:             &runAt,
	}
	if r.ProvenanceID != "" {
		p := r.ProvenanceID
		pl.ProvenanceID = &p
	}
	return pl
}

func queryBenchmarkRows(ctx context.Context, pool *db.Pool, f benchmarkFilters) ([]BenchmarkResult, error) {
	var sb strings.Builder
	sb.WriteString(`SELECT id, project_id, scenario_id, tool_name, model_name, run_id, run_at,
	                       wall_clock_ms, input_tokens, output_tokens,
	                       invoked_contextually, invocation_ok, args_match, extracted_args,
	                       interpretation_ok, detected_tool, notes,
	                       task_shape, accuracy_score, honesty_score,
	                       ranking_quality_score, within_budget_score,
	                       COALESCE(provenance_id, '')
	                FROM proj_benchmark_results WHERE 1=1`)
	binds := db.NewArgs()
	if f.Project != "" {
		sb.WriteString(" AND project_id = ?")
		binds.AddString(f.Project)
	}
	if f.ToolName != "" {
		sb.WriteString(" AND tool_name = ?")
		binds.AddString(f.ToolName)
	}
	if f.ModelName != "" {
		sb.WriteString(" AND model_name = ?")
		binds.AddString(f.ModelName)
	}
	if f.RunID != "" {
		sb.WriteString(" AND run_id = ?")
		binds.AddString(f.RunID)
	}
	if f.Since != nil {
		sb.WriteString(" AND run_at >= ?")
		binds.AddInt64(*f.Since)
	}
	sb.WriteString(" ORDER BY run_at DESC")
	if f.Limit != nil {
		fmt.Fprintf(&sb, " LIMIT %d", *f.Limit)
	}

	rows, err := pool.DB().QueryContext(ctx, sb.String(), binds.Slice()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]BenchmarkResult, 0)
	for rows.Next() {
		var r BenchmarkResult
		var runID, extracted, detected, notes, taskShape sql.NullString
		var inTok, outTok, argsMatch, interp sql.NullInt64
		var acc, honest, ranking, budget sql.NullFloat64
		err := rows.Scan(
			&r.ID, &r.ProjectID, &r.ScenarioID, &r.ToolName, &r.ModelName,
			&runID, &r.RunAt,
			&r.WallClockMS, &inTok, &outTok,
			&r.InvokedContextually, &r.InvocationOK,
			&argsMatch, &extracted,
			&interp, &detected, &notes,
			&taskShape, &acc, &honest, &ranking, &budget,
			&r.ProvenanceID,
		)
		if err != nil {
			return nil, err
		}
		r.RunID = nullableStringPtr(runID)
		r.InputTokens = nullableInt64Ptr(inTok)
		r.OutputTokens = nullableInt64Ptr(outTok)
		r.ArgsMatch = nullableInt64Ptr(argsMatch)
		r.ExtractedArgs = nullableStringPtr(extracted)
		r.InterpretationOK = nullableInt64Ptr(interp)
		r.DetectedTool = nullableStringPtr(detected)
		r.Notes = nullableStringPtr(notes)
		r.TaskShape = nullableStringPtr(taskShape)
		r.AccuracyScore = nullableFloat64Ptr(acc)
		r.HonestyScore = nullableFloat64Ptr(honest)
		r.RankingQualityScore = nullableFloat64Ptr(ranking)
		r.WithinBudgetScore = nullableFloat64Ptr(budget)
		out = append(out, r)
	}
	return out, rows.Err()
}

// parseBenchmarkResult mirrors dispatch/measure.rs::parse_benchmark_result.
// Collects every missing required key into one error rather than failing per
// round-trip. Boolean-flavoured INTEGER columns accept either JSON int or
// bool. Returns parseErr=="" on success.
func parseBenchmarkResult(project string, params json.RawMessage) (BenchmarkResult, string) {
	var m map[string]json.RawMessage
	if len(params) > 0 {
		if err := json.Unmarshal(params, &m); err != nil {
			return BenchmarkResult{}, fmt.Sprintf("params: %s", err.Error())
		}
	}

	var missing []string

	takeStr := func(key string) string {
		raw, ok := m[key]
		if !ok {
			missing = append(missing, "params."+key)
			return ""
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || s == "" {
			missing = append(missing, "params."+key)
			return ""
		}
		return s
	}
	takeIntOrBool := func(key string) int64 {
		raw, ok := m[key]
		if !ok {
			missing = append(missing, "params."+key)
			return 0
		}
		v, err := decodeIntOrBool(raw)
		if err != nil {
			missing = append(missing, "params."+key)
			return 0
		}
		return v
	}

	scenarioID := takeStr("scenario_id")
	toolName := takeStr("tool_name")
	modelName := takeStr("model_name")
	runAt := takeIntOrBool("run_at")
	wallClockMS := takeIntOrBool("wall_clock_ms")
	invocationOK := takeIntOrBool("invocation_ok")
	// provenance_id is required-by-trigger after migration 035 (T6 cutover).
	// Surface the missing-param up here so the caller gets a structured
	// error rather than a trigger-text panic at INSERT time.
	provenanceID := takeStr("provenance_id")

	if len(missing) > 0 {
		return BenchmarkResult{}, "missing required params: " + strings.Join(missing, ", ")
	}

	r := BenchmarkResult{
		ID:                  optStr(m, "id"),
		ProjectID:           project,
		ScenarioID:          scenarioID,
		ToolName:            toolName,
		ModelName:           modelName,
		RunID:               optStrPtr(m, "run_id"),
		RunAt:               runAt,
		WallClockMS:         wallClockMS,
		InputTokens:         optIntPtr(m, "input_tokens"),
		OutputTokens:        optIntPtr(m, "output_tokens"),
		InvokedContextually: optIntOrBool(m, "invoked_contextually", 1),
		InvocationOK:        invocationOK,
		ArgsMatch:           optIntOrBoolPtr(m, "args_match"),
		ExtractedArgs:       optStrPtr(m, "extracted_args"),
		InterpretationOK:    optIntOrBoolPtr(m, "interpretation_ok"),
		DetectedTool:        optStrPtr(m, "detected_tool"),
		Notes:               optStrPtr(m, "notes"),
		TaskShape:           optStrPtr(m, "task_shape"),
		AccuracyScore:       optFloatPtr(m, "accuracy_score"),
		HonestyScore:        optFloatPtr(m, "honesty_score"),
		RankingQualityScore: optFloatPtr(m, "ranking_quality_score"),
		WithinBudgetScore:   optFloatPtr(m, "within_budget_score"),
		ProvenanceID:        provenanceID,
	}
	return r, ""
}

func decodeIntOrBool(raw json.RawMessage) (int64, error) {
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		if b {
			return 1, nil
		}
		return 0, nil
	}
	return 0, fmt.Errorf("not an integer or boolean")
}

func optStr(m map[string]json.RawMessage, key string) string {
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

func optStrPtr(m map[string]json.RawMessage, key string) *string {
	raw, ok := m[key]
	if !ok {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}
	return &s
}

func optIntPtr(m map[string]json.RawMessage, key string) *int64 {
	raw, ok := m[key]
	if !ok {
		return nil
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil
	}
	return &n
}

func optIntOrBool(m map[string]json.RawMessage, key string, def int64) int64 {
	raw, ok := m[key]
	if !ok {
		return def
	}
	v, err := decodeIntOrBool(raw)
	if err != nil {
		return def
	}
	return v
}

func optIntOrBoolPtr(m map[string]json.RawMessage, key string) *int64 {
	raw, ok := m[key]
	if !ok {
		return nil
	}
	v, err := decodeIntOrBool(raw)
	if err != nil {
		return nil
	}
	return &v
}

func optFloatPtr(m map[string]json.RawMessage, key string) *float64 {
	raw, ok := m[key]
	if !ok {
		return nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil
	}
	return &f
}

// nullableStringPtr / nullableInt64Ptr / nullableFloat64Ptr convert
// database/sql's sql.NullX scan targets into *T pointers for the
// JSON-serialised BenchmarkResult struct. These are scan-side helpers, not
// driver-args-side helpers — the previous nullableString/Int/Float wrappers
// were deleted because database/sql accepts *string/*int64/*float64
// directly as variadic args (nil pointer → SQL NULL).

func nullableStringPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	s := ns.String
	return &s
}

func nullableInt64Ptr(ni sql.NullInt64) *int64 {
	if !ni.Valid {
		return nil
	}
	n := ni.Int64
	return &n
}

func nullableFloat64Ptr(nf sql.NullFloat64) *float64 {
	if !nf.Valid {
		return nil
	}
	f := nf.Float64
	return &f
}

// newUUIDv4 generates an RFC-4122 v4 UUID string, matching Rust's
// uuid::Uuid::new_v4().to_string() so IDs are visually indistinguishable
// across the migration boundary.
func newUUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
