package observehttp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/testutil"
)

// seedFullBench inserts one fully-typed proj_benchmark_results row.
// NULLs stand in for the optional columns the caller passes nil for.
// Post-T6 (agent-substrate-crud-retirement): the benchmark_results CRUD
// table is gone; the handlers read directly from proj_benchmark_results,
// so test fixtures direct-INSERT into the projection.
func seedFullBench(t *testing.T, pool *db.Pool, id, taskID, shape, model string, runAt, wallClock int64,
	accuracy *float64, detected string,
) {
	t.Helper()
	seedProject(t, pool, "test")
	// Empty detected_tool → SQL NULL so the per-task handler's
	// "detected_tool IS NULL OR != ''" filter keeps the row.
	var detectedArg any
	if detected != "" {
		detectedArg = detected
	}
	// benchmark_provenance is preserved post-T6; the projection's
	// provenance_id column FK-targets it. Seed one provenance row per
	// test row.
	provenanceID := seedFullBenchProvenance(t, pool, id)
	if _, err := pool.DB().Exec(
		`INSERT INTO proj_benchmark_results
		   (id, project_id, scenario_id, tool_name, model_name, run_at,
		    wall_clock_ms, invocation_ok, task_id, task_shape, run_shape,
		    accuracy_score, detected_tool, layer, provenance_id)
		 VALUES (?, 'test', 'scen', 'tool', ?, ?, ?, 1, ?, ?, 'production', ?, ?, 'l3', ?)`,
		id, model, runAt, wallClock, taskID, shape, accuracy, detectedArg, provenanceID,
	); err != nil {
		t.Fatal(err)
	}
}

func seedFullBenchProvenance(t *testing.T, pool *db.Pool, idSuffix string) string {
	t.Helper()
	id := "test-prov-" + idSuffix
	if _, err := pool.DB().Exec(
		`INSERT INTO benchmark_provenance
		   (id, run_id, model_id, model_version, prompt_template_hash,
		    corpus_hash, retriever_version, retriever_config_hash,
		    seed, env_hash, started_event_id)
		 VALUES (?, ?, 'm', 'mv', 'p', 'c', 'r', 'rc', 0, 'e', 'ev')`,
		id, "run-"+idSuffix,
	); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestBenchmarksList_FiltersAndLimit(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	for i := 0; i < 5; i++ {
		seedFullBench(t, pool, "id"+string(rune('0'+i)), "task-a", "Classify", "qwen", int64(1000+i*100), 200, nil, "")
	}
	srv := newTestServer(t, pool)
	var got []benchmarkRow
	getJSON(t, srv, "/benchmarks?model_name=qwen&limit=3", &got)
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3", len(got))
	}
	// Ordered by run_at DESC.
	if got[0].RunAt < got[1].RunAt {
		t.Errorf("order wrong: %+v", got)
	}
}

func TestBenchmarksTimeseries_BucketsByHour(t *testing.T) {
	pool := testutil.NewTestDB(t)
	// Three rows in two distinct hour buckets.
	seedFullBench(t, pool, "r1", "task", "Classify", "qwen", 3600*100, 100, nil, "")
	seedFullBench(t, pool, "r2", "task", "Classify", "qwen", 3600*100+10, 200, nil, "")
	seedFullBench(t, pool, "r3", "task", "Classify", "qwen", 3600*101, 300, nil, "")

	srv := newTestServer(t, pool)
	var got []TimeseriesPoint
	getJSON(t, srv, "/benchmarks/timeseries", &got)
	if len(got) != 2 {
		t.Fatalf("got %d buckets, want 2: %+v", len(got), got)
	}
	if got[0].BucketStart != 3600*100 || got[0].Total != 2 {
		t.Errorf("first bucket wrong: %+v", got[0])
	}
	if got[1].BucketStart != 3600*101 || got[1].Total != 1 {
		t.Errorf("second bucket wrong: %+v", got[1])
	}
}

func TestBenchmarksTasks_RubricMetadataJoined(t *testing.T) {
	pool := testutil.NewTestDB(t)
	// One known rubric (chain-assessment, deployable) and one legacy task.
	acc := 0.9
	seedFullBench(t, pool, "r1", "chain-assessment", "Classify", "qwen", 1000, 250, &acc, "open-with-caveat")
	seedFullBench(t, pool, "r2", "vault-rerank-retrieve", "Retrieve", "qwen", 1001, 110, nil, "")

	srv := newTestServer(t, pool)
	var got []TaskCard
	getJSON(t, srv, "/benchmarks/tasks", &got)

	var chain, vault *TaskCard
	for i := range got {
		switch got[i].TaskID {
		case "chain-assessment":
			chain = &got[i]
		case "vault-rerank-retrieve":
			vault = &got[i]
		}
	}
	if chain == nil || chain.Verdict == nil || *chain.Verdict != "ExtractNowWithQwenDispatch" {
		t.Errorf("chain-assessment verdict missing: %+v", chain)
	}
	if !chain.Deployable {
		t.Errorf("chain-assessment should be deployable")
	}
	if vault == nil || vault.Verdict != nil {
		t.Errorf("vault legacy task should have null verdict: %+v", vault)
	}
}

func TestBenchmarksTasks_SeedsZeroRowRubricPlaceholders(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newTestServer(t, pool)
	var got []TaskCard
	getJSON(t, srv, "/benchmarks/tasks", &got)
	// All 10 registered rubrics seed as zero-row placeholders.
	if len(got) != len(rubricRegistry) {
		t.Errorf("got %d placeholder cards, want %d", len(got), len(rubricRegistry))
	}
	// pre-context-summarization is the Summarize-shape outlier.
	for _, c := range got {
		if c.TaskID == "pre-context-summarization" && c.TaskShape != "Summarize" {
			t.Errorf("pre-context-summarization shape = %q, want Summarize", c.TaskShape)
		}
	}
}

func TestBenchmarksTasks_VerdictDistribution(t *testing.T) {
	pool := testutil.NewTestDB(t)
	// Three rows with detected_tool labels — 2x "needs-investigation", 1x "deployable".
	seedFullBench(t, pool, "r1", "task-z", "Classify", "qwen", 1000, 100, nil, "needs-investigation")
	seedFullBench(t, pool, "r2", "task-z", "Classify", "qwen", 1001, 100, nil, "needs-investigation")
	seedFullBench(t, pool, "r3", "task-z", "Classify", "qwen", 1002, 100, nil, "deployable")

	srv := newTestServer(t, pool)
	var got []TaskCard
	getJSON(t, srv, "/benchmarks/tasks", &got)
	var z *TaskCard
	for i := range got {
		if got[i].TaskID == "task-z" {
			z = &got[i]
		}
	}
	if z == nil || len(z.Models) != 1 {
		t.Fatalf("task-z card missing or no models: %+v", z)
	}
	dist := z.Models[0].VerdictDistribution
	if dist["needs-investigation"] != 2 || dist["deployable"] != 1 {
		t.Errorf("verdict_distribution wrong: %+v", dist)
	}
}

func TestBenchmarksEndpoints_ExcludeEmptyKeyPingRows(t *testing.T) {
	pool := testutil.NewTestDB(t)
	// A ping health-check run is recorded in proj_benchmark_results with
	// empty task_id AND empty task_shape (tool_name/detected_tool="ping").
	// It is not an offload task and must not surface as a card on any
	// benchmark endpoint. Regression: empty strings slipped past the
	// `IS NOT NULL` filters, so the /benchmarks/tasks card carried
	// task_shape="" and crashed the dashboard radar with
	// `axes is undefined` (AXES_BY_SHAPE has no "" key).
	seedFullBench(t, pool, "ping1", "", "", "qwen", 1000, 700, nil, "ping")
	acc := 0.9
	seedFullBench(t, pool, "r1", "chain-assessment", "Classify", "qwen", 1001, 200, &acc, "open-with-caveat")
	srv := newTestServer(t, pool)

	var tasks []TaskCard
	getJSON(t, srv, "/benchmarks/tasks", &tasks)
	for _, c := range tasks {
		if c.TaskID == "" || c.TaskShape == "" {
			t.Errorf("/benchmarks/tasks leaked empty-keyed ping card: %+v", c)
		}
	}

	var cards []ShapeCard
	getJSON(t, srv, "/benchmarks/cards", &cards)
	for _, c := range cards {
		if c.TaskShape == "" {
			t.Errorf("/benchmarks/cards leaked empty-shape ping card: %+v", c)
		}
	}

	var rubrics []RubricCard
	getJSON(t, srv, "/benchmarks/rubric-cards", &rubrics)
	for _, c := range rubrics {
		if c.RubricName == "" {
			t.Errorf("/benchmarks/rubric-cards leaked empty-rubric ping card: %+v", c)
		}
	}
}

func TestBenchmarksCards_GroupsByShape(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedFullBench(t, pool, "r1", "t1", "Classify", "qwen", 1000, 100, nil, "")
	seedFullBench(t, pool, "r2", "t2", "Classify", "claude", 1001, 200, nil, "")
	seedFullBench(t, pool, "r3", "t3", "Retrieve", "qwen", 1002, 150, nil, "")

	srv := newTestServer(t, pool)
	var got []ShapeCard
	getJSON(t, srv, "/benchmarks/cards", &got)
	shapes := map[string]int{}
	for _, c := range got {
		shapes[c.TaskShape] = len(c.Models)
	}
	if shapes["Classify"] != 2 || shapes["Retrieve"] != 1 {
		t.Errorf("group-by-shape wrong: %+v", shapes)
	}
}

func TestBenchmarksRubricCards_SeedsAllRubrics(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newTestServer(t, pool)
	resp, err := http.Get(srv.URL + "/benchmarks/rubric-cards")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got []RubricCard
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) < len(rubricRegistry) {
		t.Fatalf("got %d cards, want >= %d", len(got), len(rubricRegistry))
	}
}

// Regression: zero-row placeholder paths in benchmarksTasks and
// benchmarksRubricCards previously left `Models` as a nil slice, which
// marshals to JSON `null`. The dashboard's TypeScript types declare
// `models: ModelMetrics[]` (non-nullable) and iterates with for-of, so
// `null` blew up the page with "can't access property Symbol.iterator,
// t.models is null". Lock in the empty-slice contract at the JSON
// boundary, not just the Go-struct boundary.
func TestBenchmarksTasksAndRubricCards_PlaceholderModelsSerializeAsEmptyArray(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newTestServer(t, pool)

	for _, path := range []string{"/benchmarks/tasks", "/benchmarks/rubric-cards", "/benchmarks/cards"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		// Raw-string check: no `"models":null` should ever appear.
		if bytes.Contains(body, []byte(`"models":null`)) {
			t.Errorf("%s: response contains `\"models\":null` (breaks the dashboard's for-of iteration)", path)
		}
		// Belt-and-suspenders: decode and confirm every Models slice is
		// non-nil (catches future paths that might serialize via a
		// non-omitempty alternative).
		var rows []map[string]any
		if err := json.Unmarshal(body, &rows); err != nil {
			t.Fatalf("%s: decode: %v", path, err)
		}
		for i, row := range rows {
			models, present := row["models"]
			if !present {
				t.Errorf("%s row %d: missing `models` field", path, i)
				continue
			}
			if models == nil {
				t.Errorf("%s row %d: `models` is null", path, i)
			}
		}
	}
}

func TestMedianInt64(t *testing.T) {
	cases := []struct {
		in   []int64
		want int64
	}{
		{nil, 0},
		{[]int64{}, 0},
		{[]int64{42}, 42},
		{[]int64{1, 2, 3}, 2},
		{[]int64{1, 2, 3, 4}, 2}, // (2+3)/2 = 2 (integer truncation, matches Rust)
		{[]int64{10, 1, 5}, 5},
	}
	for _, c := range cases {
		if got := medianInt64(c.in); got != c.want {
			t.Errorf("medianInt64(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestLabelDistribution_EmptyReturnsNil(t *testing.T) {
	if labelDistribution(nil) != nil {
		t.Error("nil input should yield nil")
	}
	if labelDistribution([]string{}) != nil {
		t.Error("empty input should yield nil")
	}
	got := labelDistribution([]string{"a", "b", "a"})
	if got["a"] != 2 || got["b"] != 1 {
		t.Errorf("got = %+v", got)
	}
}
