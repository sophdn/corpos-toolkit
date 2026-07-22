// parse-context-bench drives the directive-intent fixture set
// through parse_context against a target toolkit-server binary's
// HTTP /knowledge endpoint and prints per-prompt + per-shape envelope
// size + latency. T10 of chain parse-context-lean-orienting invokes
// this twice — once against a pre-T5 binary (379b1d15) and once
// against a post-T9 binary — and diffs the tables to compute the
// envelope-budget delta and the cumulative-latency-growth gate.
//
// Why HTTP and not in-process: the binary under test may be a
// historical pre-T5 build whose Go API has since drifted. HTTP is
// the stable contract surface that survives.
//
// Inputs (CLI flags):
//
//	--http-url     http://localhost:3000  base URL of the running
//	                                      toolkit-server-under-test
//	--fixture      go/internal/refresolve/testdata/directive_intent_fixtures.json
//	--project      mcp-servers            project arg threaded into
//	                                      each parse_context call
//	--out-json     /tmp/parse-bench.json  per-prompt raw results
//	--out-markdown /tmp/parse-bench.md    per-shape p50/p99/byte
//	                                      summary table
//	--out-aggregate-json (unset)          flat per-shape aggregate
//	                                      stats keyed by "shape.metric";
//	                                      value "-" writes to stdout
//
// Output: per-prompt envelope size + ResolutionTimeMs, plus per-shape
// p50 / p99 latency and median envelope-byte size. Markdown table
// suitable for the T10 retrospective doc.
//
// --out-aggregate-json emits a flat JSON object keyed by
// "<shape>.<metric>" (e.g. "chain_slug.resolution_p50_ms") with numeric
// leaves — the shape a forge(bench) baseline_json_path expects so
// measure.bench_run can diff it. It is unset by default (no aggregate
// written). measure.bench_run captures the bench's stdout, so register
// the harness with "--out-aggregate-json -" (the "-"/"/dev/stdout"
// sentinel routes the aggregate to stdout); progress logs go to stderr,
// keeping stdout pure JSON.
//
// Run shape:
//
//	go run ./go/cmd/parse-context-bench --http-url http://localhost:3000
//
// Compare two binaries: boot binary A on port 3000, run; boot binary
// B on port 3001, run; diff the two --out-markdown tables.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// fixtureFile mirrors the on-disk shape of
// go/internal/refresolve/testdata/directive_intent_fixtures.json.
type fixtureFile struct {
	Meta struct {
		Description string `json:"description"`
	} `json:"_meta"`
	Fixtures []fixtureShape `json:"fixtures"`
}

type fixtureShape struct {
	Shape   string          `json:"shape"`
	Prompts []fixturePrompt `json:"prompts"`
}

// fixturePrompt mirrors one entry in fixtures[].prompts. Only the
// text matters for the bench; the note field is provenance the test
// suite uses.
type fixturePrompt struct {
	Text string `json:"text"`
	Note string `json:"note,omitempty"`
}

// promptResult is one parse_context invocation's measurement.
type promptResult struct {
	Shape            string `json:"shape"`
	Prompt           string `json:"prompt"`
	EnvelopeBytes    int    `json:"envelope_bytes"`
	ResolutionTimeMs int    `json:"resolution_time_ms"`
	HTTPLatencyMs    int    `json:"http_latency_ms"`
	HTTPStatus       int    `json:"http_status"`
	ErrorString      string `json:"error,omitempty"`
}

// shapeStats summarises per-intent-shape latency + envelope-size
// distribution. p50 / p99 are computed via linear interpolation on
// the sorted sample; byte counts are reported as median + max so the
// retro shows the typical-and-tail envelope budget impact.
type shapeStats struct {
	Shape            string `json:"shape"`
	N                int    `json:"n"`
	ResolutionP50Ms  int    `json:"resolution_p50_ms"`
	ResolutionP99Ms  int    `json:"resolution_p99_ms"`
	HTTPP50Ms        int    `json:"http_p50_ms"`
	HTTPP99Ms        int    `json:"http_p99_ms"`
	EnvelopeBytesP50 int    `json:"envelope_bytes_p50"`
	EnvelopeBytesMax int    `json:"envelope_bytes_max"`
}

func main() {
	httpURL := flag.String("http-url", "http://localhost:3000", "base URL of toolkit-server under test")
	fixturePath := flag.String("fixture", "go/internal/refresolve/testdata/directive_intent_fixtures.json", "path to directive_intent_fixtures.json")
	project := flag.String("project", "mcp-servers", "project parameter threaded into each parse_context call (T6 work-state surface needs this)")
	sessionID := flag.String("session-id", "", "session_id threaded into each parse_context call. Stdio MCP transport stamps this stably per connection in production; the HTTP path here doesn't, so the bench needs to thread one explicitly for the work-state filter cache to hit across the 49 prompts. Empty disables cache (matches pre-bug-866-fix behavior).")
	outJSON := flag.String("out-json", "/tmp/parse-bench.json", "per-prompt raw results")
	outMD := flag.String("out-markdown", "/tmp/parse-bench.md", "per-shape summary table (markdown)")
	outAggJSON := flag.String("out-aggregate-json", "", "flat per-shape aggregate stats keyed by \"shape.metric\" (numeric leaves) — the forge(bench) baseline shape. Unset writes nothing; \"-\" or \"/dev/stdout\" writes to stdout (what measure.bench_run captures).")
	flag.Parse()

	fixtures, err := loadFixtures(*fixturePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load fixtures: %v\n", err)
		os.Exit(1)
	}

	totalPrompts := 0
	for _, s := range fixtures {
		totalPrompts += len(s.Prompts)
	}
	fmt.Fprintf(os.Stderr, "running %d prompts across %d intent shapes against %s\n",
		totalPrompts, len(fixtures), *httpURL)

	results := make([]promptResult, 0, totalPrompts)
	for _, fx := range fixtures {
		for _, prompt := range fx.Prompts {
			results = append(results, runOnePrompt(*httpURL, *project, *sessionID, fx.Shape, prompt.Text))
		}
	}

	if err := writeJSON(*outJSON, results); err != nil {
		fmt.Fprintf(os.Stderr, "write json: %v\n", err)
		os.Exit(1)
	}
	if err := writeMarkdown(*outMD, results); err != nil {
		fmt.Fprintf(os.Stderr, "write markdown: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "wrote %s + %s\n", *outJSON, *outMD)

	if *outAggJSON != "" {
		if err := writeAggregateJSON(*outAggJSON, results); err != nil {
			fmt.Fprintf(os.Stderr, "write aggregate json: %v\n", err)
			os.Exit(1)
		}
		if *outAggJSON != "-" && *outAggJSON != "/dev/stdout" {
			fmt.Fprintf(os.Stderr, "wrote aggregate %s\n", *outAggJSON)
		}
	}
}

func loadFixtures(path string) ([]fixtureShape, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f fixtureFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	return f.Fixtures, nil
}

// runOnePrompt POSTs one parse_context call and captures bytes +
// latency. Errors flow into promptResult.ErrorString so the run
// completes even if a few prompts fail (the per-shape stats just
// have a smaller N).
func runOnePrompt(httpURL, project, sessionID, shape, prompt string) promptResult {
	// parseContextParams mirrors the typed param shape parse_context
	// accepts. message_text + session_id are the fields this bench
	// varies; the rest stay at server defaults. session_id is empty
	// in the unthread case (cache disabled) and the bench's
	// --session-id flag value otherwise (cache enabled).
	type parseContextParams struct {
		MessageText string `json:"message_text"`
		SessionID   string `json:"session_id,omitempty"`
	}
	type requestBody struct {
		Action  string             `json:"action"`
		Project string             `json:"project,omitempty"`
		Params  parseContextParams `json:"params"`
	}
	body, _ := json.Marshal(requestBody{
		Action:  "parse_context",
		Project: project,
		Params:  parseContextParams{MessageText: prompt, SessionID: sessionID},
	})

	start := time.Now()
	resp, err := http.Post(httpURL+"/mcp/knowledge", "application/json", bytes.NewReader(body))
	httpMs := int(time.Since(start).Milliseconds())
	if err != nil {
		return promptResult{Shape: shape, Prompt: prompt, HTTPLatencyMs: httpMs, ErrorString: err.Error()}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return promptResult{Shape: shape, Prompt: prompt, HTTPLatencyMs: httpMs, HTTPStatus: resp.StatusCode, ErrorString: err.Error()}
	}

	// Parse the envelope to extract resolution_time_ms. The body bytes
	// are what we measure for envelope size — exactly what the agent
	// sees on the wire.
	var env struct {
		ResolutionTimeMs int    `json:"resolution_time_ms"`
		Error            string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return promptResult{Shape: shape, Prompt: prompt, HTTPLatencyMs: httpMs, HTTPStatus: resp.StatusCode,
			ErrorString: fmt.Sprintf("unmarshal envelope: %v", err)}
	}

	return promptResult{
		Shape:            shape,
		Prompt:           prompt,
		EnvelopeBytes:    len(raw),
		ResolutionTimeMs: env.ResolutionTimeMs,
		HTTPLatencyMs:    httpMs,
		HTTPStatus:       resp.StatusCode,
		ErrorString:      env.Error,
	}
}

func writeJSON(path string, results []promptResult) error {
	out, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

// buildAggregateMap flattens the per-shape stats into a single object
// keyed by "<shape>.<metric>" with numeric leaves. The metric suffixes
// match the json tags on shapeStats so the keys stay legible against the
// markdown table's columns. It reuses computeShapeStats — no new
// percentile logic — so the aggregate file and the markdown table can
// never disagree about a shape's p50/p99.
func buildAggregateMap(results []promptResult) map[string]int {
	stats := computeShapeStats(results)
	const metricsPerShape = 7
	out := make(map[string]int, len(stats)*metricsPerShape)
	for _, s := range stats {
		out[s.Shape+".n"] = s.N
		out[s.Shape+".resolution_p50_ms"] = s.ResolutionP50Ms
		out[s.Shape+".resolution_p99_ms"] = s.ResolutionP99Ms
		out[s.Shape+".http_p50_ms"] = s.HTTPP50Ms
		out[s.Shape+".http_p99_ms"] = s.HTTPP99Ms
		out[s.Shape+".envelope_bytes_p50"] = s.EnvelopeBytesP50
		out[s.Shape+".envelope_bytes_max"] = s.EnvelopeBytesMax
	}
	return out
}

// writeAggregateJSON marshals the flat aggregate map and writes it to
// path. A path of "-" or "/dev/stdout" routes to stdout — the stream
// measure.bench_run captures from the subprocess — instead of a file.
// The 2-space indent + trailing newline match measure.bench_run's
// update_baseline writer so a checked-in baseline is byte-identical to
// one minted by an update_baseline run.
func writeAggregateJSON(path string, results []promptResult) error {
	out, err := json.MarshalIndent(buildAggregateMap(results), "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if path == "-" || path == "/dev/stdout" {
		_, err := os.Stdout.Write(out)
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

func writeMarkdown(path string, results []promptResult) error {
	stats := computeShapeStats(results)
	var b strings.Builder
	fmt.Fprintf(&b, "# parse_context bench — %s\n\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "Prompts: %d  |  Shapes: %d\n\n", len(results), len(stats))
	fmt.Fprintln(&b, "| Shape | n | resolution p50 ms | resolution p99 ms | http p50 ms | http p99 ms | env bytes p50 | env bytes max |")
	fmt.Fprintln(&b, "|---|--:|--:|--:|--:|--:|--:|--:|")
	for _, s := range stats {
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d | %d | %d |\n",
			s.Shape, s.N, s.ResolutionP50Ms, s.ResolutionP99Ms, s.HTTPP50Ms, s.HTTPP99Ms, s.EnvelopeBytesP50, s.EnvelopeBytesMax)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## All-shapes aggregate")
	all := aggregateAll(results)
	fmt.Fprintf(&b, "- prompts: %d  errors: %d\n", all.N, all.errorCount)
	fmt.Fprintf(&b, "- resolution_time_ms p50: %d, p99: %d\n", all.ResolutionP50Ms, all.ResolutionP99Ms)
	fmt.Fprintf(&b, "- http_latency_ms p50: %d, p99: %d\n", all.HTTPP50Ms, all.HTTPP99Ms)
	fmt.Fprintf(&b, "- envelope_bytes p50: %d, max: %d\n", all.EnvelopeBytesP50, all.EnvelopeBytesMax)
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

type aggregateStats struct {
	shapeStats
	errorCount int
}

func aggregateAll(results []promptResult) aggregateStats {
	stats := computeStats("ALL", results)
	errors := 0
	for _, r := range results {
		if r.ErrorString != "" {
			errors++
		}
	}
	return aggregateStats{shapeStats: stats, errorCount: errors}
}

func computeShapeStats(results []promptResult) []shapeStats {
	byShape := map[string][]promptResult{}
	for _, r := range results {
		byShape[r.Shape] = append(byShape[r.Shape], r)
	}
	out := make([]shapeStats, 0, len(byShape))
	for shape, rs := range byShape {
		out = append(out, computeStats(shape, rs))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Shape < out[j].Shape })
	return out
}

func computeStats(shape string, rs []promptResult) shapeStats {
	if len(rs) == 0 {
		return shapeStats{Shape: shape}
	}
	resTimes := make([]int, 0, len(rs))
	httpTimes := make([]int, 0, len(rs))
	bytesArr := make([]int, 0, len(rs))
	for _, r := range rs {
		resTimes = append(resTimes, r.ResolutionTimeMs)
		httpTimes = append(httpTimes, r.HTTPLatencyMs)
		bytesArr = append(bytesArr, r.EnvelopeBytes)
	}
	maxBytes := 0
	for _, b := range bytesArr {
		if b > maxBytes {
			maxBytes = b
		}
	}
	return shapeStats{
		Shape:            shape,
		N:                len(rs),
		ResolutionP50Ms:  percentile(resTimes, 50),
		ResolutionP99Ms:  percentile(resTimes, 99),
		HTTPP50Ms:        percentile(httpTimes, 50),
		HTTPP99Ms:        percentile(httpTimes, 99),
		EnvelopeBytesP50: percentile(bytesArr, 50),
		EnvelopeBytesMax: maxBytes,
	}
}

// percentile computes the p-th percentile via linear interpolation
// on the sorted sample. p in [0,100]. Returns 0 for empty input;
// returns the single value for one-element input.
func percentile(in []int, p int) int {
	if len(in) == 0 {
		return 0
	}
	xs := make([]int, len(in))
	copy(xs, in)
	sort.Ints(xs)
	if len(xs) == 1 {
		return xs[0]
	}
	pos := float64(p) / 100.0 * float64(len(xs)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return xs[lo]
	}
	frac := pos - float64(lo)
	return int(math.Round(float64(xs[lo])*(1-frac) + float64(xs[hi])*frac))
}
