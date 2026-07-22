package measure

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/events"
)

// BenchRunResult is the response envelope for measure.bench_run. The
// agent-visible fields are MarkdownTable (the per-metric diff rendered
// for human reading) + Metrics (the structured per-metric records). On
// error, Error names the failure mode; Metrics may still be populated
// if the subprocess completed but parse failed.
type BenchRunResult struct {
	OK              bool                  `json:"ok,omitempty"`
	Slug            string                `json:"slug,omitempty"`
	Metrics         []BenchMetricDiffWire `json:"metrics,omitempty"`
	MarkdownTable   string                `json:"markdown_table,omitempty"`
	RunLatencyMs    int                   `json:"run_latency_ms,omitempty"`
	BaselineUpdated bool                  `json:"baseline_updated,omitempty"`
	StderrLogPath   string                `json:"stderr_log_path,omitempty"`
	BaselinePath    string                `json:"baseline_path,omitempty"`
	BenchEventID    string                `json:"bench_event_id,omitempty"`
	Error           string                `json:"error,omitempty"`
	// GatePassed is the deterministic-metric gate outcome: nil when the
	// harness declared no gate_metrics (report-only), else whether every
	// matched gate metric had a zero delta. GateMetricCount is how many
	// metrics matched the gate patterns; GateFailures names the drifted
	// gate metrics (or a misconfiguration line when patterns matched none).
	GatePassed      *bool    `json:"gate_passed,omitempty"`
	GateMetricCount int      `json:"gate_metric_count,omitempty"`
	GateFailures    []string `json:"gate_failures,omitempty"`
}

// BenchMetricDiffWire is the per-metric diff in the response shape.
// Mirrors events.BenchmarkMetricDiff but uses concrete interface{}-via-
// json.RawMessage so the dispatch layer can pass through whatever the
// harness produced (number, string, null) without re-typing.
type BenchMetricDiffWire struct {
	Name     string          `json:"name"`
	Baseline json.RawMessage `json:"baseline"`
	Observed json.RawMessage `json:"observed"`
	DeltaAbs *float64        `json:"delta_abs,omitempty"`
	DeltaPct *float64        `json:"delta_pct,omitempty"`
}

// benchHarness is the in-process row shape read from bench_harnesses.
type benchHarness struct {
	id               int64
	slug             string
	binaryPath       string
	flagSetJSON      string
	baselineJSONPath string
	parseOutputAs    string
	timeoutMs        int
	gateMetrics      string
}

// benchRunParams is the typed bench_run request body — the json.Unmarshal
// target. It is also the action-doc TYPE source: measure.measureActionRegistry
// reflects it (reflect.TypeOf(benchRunParams{})) so each param's type derives
// from the struct field kind rather than being re-authored (chain
// migrate-measure-action-docs-to-derive-contract; docs/ACTION_DOC_CONTRACT.md).
// Extracted from an inline anonymous struct — same fields, json tags, and strict
// unmarshal, so the binding is byte-for-byte unchanged. OverrideFlags is a slice,
// so it derives to object[] (the documented type shifts object→object[] at the
// flip — the single enumerated blessed delta, the batch.ops-class correction).
type benchRunParams struct {
	Slug           string             `json:"slug"`
	UpdateBaseline bool               `json:"update_baseline,omitempty"`
	OverrideFlags  []benchFlagPairCLI `json:"override_flags,omitempty"`
}

// HandleBenchRun implements the measure.bench_run action. Looks up the
// bench harness by slug, runs the binary with its recorded flag_set
// (plus optional override_flags), parses stdout per parse_output_as,
// compares against baseline_json_path, and emits BenchmarkDiff. With
// update_baseline=true, overwrites the baseline file BEFORE diffing
// and emits BenchmarkBaselineUpdated as a sibling event.
//
// Subprocess discipline:
//   - os/exec.CommandContext with discrete args (no shell wrapping).
//   - Wall-clock timeout from the harness row (default 60s per
//     migration 067) enforced via context.WithTimeout.
//   - stderr to /tmp/bench-<slug>-<ts>.stderr.log so the operator can
//     inspect runs that fail; path stamped on the BenchmarkDiff event.
//
// T6 of work-batching-and-forge-templates.
func HandleBenchRun(ctx context.Context, deps BenchmarkDeps, project string, params json.RawMessage) (BenchRunResult, error) {
	if project == "" {
		return BenchRunResult{Error: "project is required"}, nil
	}
	var p benchRunParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return BenchRunResult{Error: fmt.Sprintf("params: %s", err)}, nil
		}
	}
	if p.Slug == "" {
		return BenchRunResult{Error: "missing required params: slug"}, nil
	}

	h, err := loadBenchHarness(ctx, deps.Pool, project, p.Slug)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BenchRunResult{Error: fmt.Sprintf("bench %q not found in project %q", p.Slug, project)}, nil
		}
		return BenchRunResult{}, fmt.Errorf("load harness: %w", err)
	}

	args, err := buildBenchArgs(h.flagSetJSON, p.OverrideFlags)
	if err != nil {
		return BenchRunResult{Error: fmt.Sprintf("build args: %s", err)}, nil
	}

	// Resolve relative paths against the registering project's root
	// (projects.path), NOT the bench_run process cwd. The canonical stdio
	// MCP runs project-agnostic from ~/dev, so cwd-relative resolution
	// silently missed repo-relative registrations. Absolute paths pass
	// through unchanged; an unregistered project falls back to cwd.
	projectRoot := lookupProjectPath(ctx, deps.Pool, project)
	binaryPath := resolveBenchPath(h.binaryPath, projectRoot)

	stderrPath := filepath.Join(os.TempDir(),
		fmt.Sprintf("bench-%s-%d.stderr.log", h.slug, time.Now().UnixNano()))

	runStart := time.Now()
	// projectRoot is the subprocess cwd so relative flag paths (e.g.
	// --fixture go/internal/.../fixtures.json) resolve against the repo too.
	stdout, runErr := runBenchSubprocess(ctx, binaryPath, args, stderrPath, time.Duration(h.timeoutMs)*time.Millisecond, projectRoot)
	runLatencyMs := int(time.Since(runStart).Milliseconds())

	if runErr != nil {
		// Subprocess failed (timeout, non-zero exit, exec error). Emit
		// BenchmarkDiff with empty metrics + error string so the events
		// ledger records the failure shape, then return.
		_ = emitBenchDiff(ctx, deps.Pool, project, h.slug, nil, runLatencyMs, false, stderrPath, runErr.Error(), nil, nil)
		return BenchRunResult{
			Slug:          h.slug,
			RunLatencyMs:  runLatencyMs,
			StderrLogPath: stderrPath,
			BaselinePath:  h.baselineJSONPath,
			Error:         runErr.Error(),
		}, nil
	}

	observed, err := parseBenchOutput(stdout, h.parseOutputAs)
	if err != nil {
		_ = emitBenchDiff(ctx, deps.Pool, project, h.slug, nil, runLatencyMs, false, stderrPath, "parse: "+err.Error(), nil, nil)
		return BenchRunResult{
			Slug:          h.slug,
			RunLatencyMs:  runLatencyMs,
			StderrLogPath: stderrPath,
			BaselinePath:  h.baselineJSONPath,
			Error:         "parse stdout: " + err.Error(),
		}, nil
	}

	// Resolve the baseline path against the project root (same rule as the
	// binary + subprocess cwd). Absolute paths pass through unchanged; an
	// unregistered project falls back to cwd. The baseline file is
	// repo-managed; missing-on-first-run is handled below.
	baselineAbs := resolveBenchPath(h.baselineJSONPath, projectRoot)

	if p.UpdateBaseline {
		prevSHA, _ := hashFileIfExists(baselineAbs)
		buf, err := json.MarshalIndent(observed, "", "  ")
		if err != nil {
			return BenchRunResult{Error: "marshal observed for baseline: " + err.Error()}, nil
		}
		buf = append(buf, '\n')
		if err := atomicWriteBaseline(baselineAbs, buf); err != nil {
			return BenchRunResult{Error: "write baseline: " + err.Error()}, nil
		}
		newSHA := sha256Hex(buf)
		if err := emitBenchBaselineUpdated(ctx, deps.Pool, project, h.slug, h.baselineJSONPath, prevSHA, newSHA); err != nil {
			// Fail-open: file already on disk; surface via Error but
			// don't unwind.
			return BenchRunResult{
				Slug:            h.slug,
				BaselineUpdated: true,
				BaselinePath:    h.baselineJSONPath,
				RunLatencyMs:    runLatencyMs,
				Error:           "emit BenchmarkBaselineUpdated: " + err.Error(),
			}, nil
		}
	}

	baseline, baselineErr := loadBaselineJSON(baselineAbs)
	if baselineErr != nil && !errors.Is(baselineErr, os.ErrNotExist) {
		return BenchRunResult{Error: "load baseline: " + baselineErr.Error()}, nil
	}
	if errors.Is(baselineErr, os.ErrNotExist) && !p.UpdateBaseline {
		_ = emitBenchDiff(ctx, deps.Pool, project, h.slug, nil, runLatencyMs, false, stderrPath,
			"baseline_not_found: "+h.baselineJSONPath, nil, nil)
		return BenchRunResult{
			Slug:          h.slug,
			RunLatencyMs:  runLatencyMs,
			StderrLogPath: stderrPath,
			BaselinePath:  h.baselineJSONPath,
			Error:         "baseline file not found at " + h.baselineJSONPath + "; re-run with update_baseline=true to mint it",
		}, nil
	}

	metrics := diffBenchMetrics(baseline, observed)
	gate := evaluateBenchGate(h.gateMetrics, metrics)
	var gatePassed *bool
	if gate.Configured {
		gatePassed = &gate.Passed
	}
	markdown := renderBenchMarkdown(h.slug, metrics, runLatencyMs, p.UpdateBaseline, gate)

	if err := emitBenchDiff(ctx, deps.Pool, project, h.slug, metrics, runLatencyMs, p.UpdateBaseline, stderrPath, "", gatePassed, gate.Failures); err != nil {
		return BenchRunResult{
			Slug:            h.slug,
			Metrics:         toMetricsWire(metrics),
			MarkdownTable:   markdown,
			RunLatencyMs:    runLatencyMs,
			BaselineUpdated: p.UpdateBaseline,
			StderrLogPath:   stderrPath,
			BaselinePath:    h.baselineJSONPath,
			GatePassed:      gatePassed,
			GateMetricCount: gate.MatchCount,
			GateFailures:    gate.Failures,
			Error:           "emit BenchmarkDiff: " + err.Error(),
		}, nil
	}

	return BenchRunResult{
		OK:              true,
		Slug:            h.slug,
		Metrics:         toMetricsWire(metrics),
		MarkdownTable:   markdown,
		RunLatencyMs:    runLatencyMs,
		BaselineUpdated: p.UpdateBaseline,
		StderrLogPath:   stderrPath,
		BaselinePath:    h.baselineJSONPath,
		GatePassed:      gatePassed,
		GateMetricCount: gate.MatchCount,
		GateFailures:    gate.Failures,
	}, nil
}

// benchFlagPairCLI is the wire shape for params.override_flags entries.
type benchFlagPairCLI struct {
	Flag  string `json:"flag"`
	Value string `json:"value,omitempty"`
}

func loadBenchHarness(ctx context.Context, pool *db.Pool, project, slug string) (benchHarness, error) {
	var h benchHarness
	err := pool.DB().QueryRowContext(ctx,
		`SELECT id, slug, binary_path, flag_set_json, baseline_json_path,
		        parse_output_as, timeout_ms, gate_metrics
		 FROM bench_harnesses
		 WHERE project_id = ? AND slug = ?`,
		project, slug,
	).Scan(&h.id, &h.slug, &h.binaryPath, &h.flagSetJSON, &h.baselineJSONPath,
		&h.parseOutputAs, &h.timeoutMs, &h.gateMetrics)
	return h, err
}

// BuildBenchArgs is the exported version of buildBenchArgs for unit
// tests. Production callers go through HandleBenchRun.
func BuildBenchArgs(flagSetJSON string, overrides []BenchFlagPairCLI) ([]string, error) {
	pairs := make([]benchFlagPairCLI, len(overrides))
	for i, p := range overrides {
		pairs[i] = benchFlagPairCLI(p)
	}
	return buildBenchArgs(flagSetJSON, pairs)
}

// BenchFlagPairCLI is the exported wire shape (matches benchFlagPairCLI
// 1:1). Tests construct override slices in this shape; production
// callers receive the same JSON shape via params decode.
type BenchFlagPairCLI = benchFlagPairCLI

// buildBenchArgs decodes the harness's flag_set_json into discrete args
// for os/exec, applying any per-call overrides. Override semantics: an
// override entry's `flag` matches a registered entry's `flag` →
// replaces its value (and keeps order); a new flag → appended.
func buildBenchArgs(flagSetJSON string, overrides []benchFlagPairCLI) ([]string, error) {
	var entries []benchFlagPairCLI
	if err := json.Unmarshal([]byte(flagSetJSON), &entries); err != nil {
		return nil, fmt.Errorf("decode flag_set_json: %w", err)
	}
	// Apply overrides by flag-name match; preserve original order.
	overrideIdx := map[string]int{}
	for i, o := range overrides {
		overrideIdx[o.Flag] = i
	}
	used := make([]bool, len(overrides))
	args := make([]string, 0, len(entries)*2)
	for _, e := range entries {
		if i, hit := overrideIdx[e.Flag]; hit {
			args = append(args, e.Flag)
			if overrides[i].Value != "" {
				args = append(args, overrides[i].Value)
			}
			used[i] = true
			continue
		}
		args = append(args, e.Flag)
		if e.Value != "" {
			args = append(args, e.Value)
		}
	}
	for i, o := range overrides {
		if used[i] {
			continue
		}
		args = append(args, o.Flag)
		if o.Value != "" {
			args = append(args, o.Value)
		}
	}
	return args, nil
}

// runBenchSubprocess executes binaryPath with args, captures stdout in
// memory, writes stderr to stderrPath, enforces wall-clock timeout via
// context. Returns stdout + a structured error on failure.
//
// binaryPath is treated as a literal path (no shell). `go run ./...`
// style targets are deliberately NOT supported — register the compiled
// binary or use a wrapper script if shell semantics are needed.
//
// dir, when non-empty, is the subprocess working directory (the
// registering project's root) so any relative paths the harness reads
// from its flags resolve against the repo rather than the MCP server's
// cwd. Empty dir inherits the parent process cwd (unregistered project).
func runBenchSubprocess(ctx context.Context, binaryPath string, args []string, stderrPath string, timeout time.Duration, dir string) ([]byte, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stderrFile, err := os.Create(stderrPath)
	if err != nil {
		return nil, fmt.Errorf("create stderr log: %w", err)
	}
	defer stderrFile.Close()

	var stdout bytes.Buffer
	cmd := exec.CommandContext(runCtx, binaryPath, args...)
	cmd.Dir = dir
	cmd.Stdout = &stdout
	cmd.Stderr = stderrFile
	if err := cmd.Run(); err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return stdout.Bytes(), fmt.Errorf("timeout after %s: %w", timeout, err)
		}
		return stdout.Bytes(), fmt.Errorf("subprocess: %w", err)
	}
	return stdout.Bytes(), nil
}

// parseBenchOutput dispatches stdout decoding by the harness's
// parse_output_as enum. v1 supports `json` only. Returns
// map[metric_name]json.RawMessage so callers can preserve the
// per-metric JSON shape (number / string / null / nested object)
// without flattening to a single Go type — keeps the package's
// `any`-free boundary discipline (forbidigo gate).
func parseBenchOutput(stdout []byte, parseAs string) (map[string]json.RawMessage, error) {
	switch parseAs {
	case "json":
		return parseBenchJSON(stdout)
	default:
		return nil, fmt.Errorf("parse_output_as=%q not supported in v1", parseAs)
	}
}

// parseBenchJSON decodes stdout as JSON. Accepts either an object
// (treated as the metric map directly) or an array of objects (each
// item's fields contribute to the map, prefixed by the item's
// "name"/"shape"/"id" field if present, else by its 0-indexed position).
// Returns map[name]RawMessage where the RawMessage is whatever JSON
// shape was at that key (number, string, null, nested).
func parseBenchJSON(stdout []byte) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty stdout")
	}
	if trimmed[0] == '{' {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &m); err != nil {
			return nil, fmt.Errorf("decode object: %w", err)
		}
		return flattenMetricsMap("", m), nil
	}
	if trimmed[0] == '[' {
		var arr []map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, fmt.Errorf("decode array: %w", err)
		}
		out := map[string]json.RawMessage{}
		for i, item := range arr {
			prefix := preferStringField(item, "shape", "name", "id")
			if prefix == "" {
				prefix = fmt.Sprintf("[%d]", i)
			}
			for k, v := range item {
				if k == "shape" || k == "name" || k == "id" {
					continue
				}
				out[prefix+"."+k] = v
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("stdout not JSON object or array (first byte = %q)", string(trimmed[0]))
}

// flattenMetricsMap turns a nested {a: {b: 1, c: 2}} into a flat
// {a.b: 1, a.c: 2}. A RawMessage is recognized as an object by its
// first non-whitespace byte; '{' triggers recursion. Anything else
// (numbers, strings, arrays, null) lands at the current key without
// further flattening — so a percentile-array metric like [8, 12, 25]
// round-trips intact for the eyeball diff path.
func flattenMetricsMap(prefix string, m map[string]json.RawMessage) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, v := range m {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		// Peek the first non-whitespace byte to decide whether to
		// recurse. Avoids a second Unmarshal pass for non-object leaves.
		trimmed := bytes.TrimSpace(v)
		if len(trimmed) > 0 && trimmed[0] == '{' {
			var sub map[string]json.RawMessage
			if err := json.Unmarshal(trimmed, &sub); err == nil {
				for sk, sv := range flattenMetricsMap(key, sub) {
					out[sk] = sv
				}
				continue
			}
		}
		out[key] = v
	}
	return out
}

func preferStringField(m map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			var s string
			if err := json.Unmarshal(v, &s); err == nil && s != "" {
				return s
			}
		}
	}
	return ""
}

func loadBaselineJSON(path string) (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseBenchJSON(data)
}

// diffBenchMetrics computes per-metric diff entries from baseline +
// observed. Metric names are the union of both maps; missing-on-either-
// side carries the present value with the other side as JSON null.
// Numeric pairs get delta_abs + delta_pct (when baseline non-zero);
// non-numeric or null-side metrics get nil deltas.
func diffBenchMetrics(baseline, observed map[string]json.RawMessage) []events.BenchmarkMetricDiff {
	names := map[string]struct{}{}
	for k := range baseline {
		names[k] = struct{}{}
	}
	for k := range observed {
		names[k] = struct{}{}
	}
	sorted := make([]string, 0, len(names))
	for k := range names {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	out := make([]events.BenchmarkMetricDiff, 0, len(sorted))
	for _, name := range sorted {
		b, bok := baseline[name]
		o, ook := observed[name]
		entry := events.BenchmarkMetricDiff{Name: name}
		entry.Baseline = jsonValueOrNull(b, bok)
		entry.Observed = jsonValueOrNull(o, ook)
		if bFloat, bIsNum := rawMessageToFloat(b); bIsNum {
			if oFloat, oIsNum := rawMessageToFloat(o); oIsNum {
				delta := oFloat - bFloat
				entry.DeltaAbs = &delta
				if bFloat != 0 {
					pct := (oFloat - bFloat) / bFloat * 100.0
					if !math.IsNaN(pct) && !math.IsInf(pct, 0) {
						entry.DeltaPct = &pct
					}
				}
			}
		}
		out = append(out, entry)
	}
	return out
}

func jsonValueOrNull(v json.RawMessage, present bool) json.RawMessage {
	if !present || len(v) == 0 {
		return json.RawMessage("null")
	}
	return v
}

// rawMessageToFloat unmarshals a RawMessage into a float64 if it parses
// as a JSON number. Strings / nulls / objects / arrays return (0, false).
func rawMessageToFloat(v json.RawMessage) (float64, bool) {
	if len(v) == 0 {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(v, &f); err != nil {
		return 0, false
	}
	return f, true
}

func toMetricsWire(in []events.BenchmarkMetricDiff) []BenchMetricDiffWire {
	out := make([]BenchMetricDiffWire, len(in))
	for i, m := range in {
		out[i] = BenchMetricDiffWire{
			Name:     m.Name,
			Baseline: m.Baseline,
			Observed: m.Observed,
			DeltaAbs: m.DeltaAbs,
			DeltaPct: m.DeltaPct,
		}
	}
	return out
}

// renderGateSummary produces the gate verdict block above the diff
// table. A non-gated harness reads as report-only; a gated one shows
// PASS/FAIL over the matched deterministic metrics, listing any drift.
func renderGateSummary(gate benchGateResult) string {
	if !gate.Configured {
		return "gate: not configured (report-only — set gate_metrics on the harness to enable a pass/fail)\n\n"
	}
	var b strings.Builder
	verdict := "PASS"
	if !gate.Passed {
		verdict = "FAIL"
	}
	fmt.Fprintf(&b, "gate: %s — %d deterministic metric(s) checked for zero-delta\n", verdict, gate.MatchCount)
	for _, f := range gate.Failures {
		fmt.Fprintf(&b, "  - drift: %s\n", f)
	}
	b.WriteString("\n")
	return b.String()
}

// renderBenchMarkdown produces the human-readable diff table for the
// MarkdownTable response field + the operator's stdout view. Numeric
// metrics show baseline, observed, abs delta, pct delta; non-numeric
// show baseline, observed, equal/diff flag.
func renderBenchMarkdown(slug string, metrics []events.BenchmarkMetricDiff, runLatencyMs int, baselineUpdated bool, gate benchGateResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# bench %s — %s\n\n", slug, time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "run_latency_ms: %d  |  baseline_updated: %v  |  metrics: %d\n\n",
		runLatencyMs, baselineUpdated, len(metrics))
	b.WriteString(renderGateSummary(gate))
	b.WriteString("| Metric | Baseline | Observed | Δ | Δ% |\n")
	b.WriteString("|---|---|---|---:|---:|\n")
	for _, m := range metrics {
		baselineStr := string(m.Baseline)
		observedStr := string(m.Observed)
		deltaStr := "—"
		pctStr := "—"
		if m.DeltaAbs != nil {
			deltaStr = formatDelta(*m.DeltaAbs)
		}
		if m.DeltaPct != nil {
			pctStr = formatPct(*m.DeltaPct)
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n",
			m.Name, baselineStr, observedStr, deltaStr, pctStr)
	}
	return b.String()
}

func formatDelta(v float64) string {
	sign := ""
	if v > 0 {
		sign = "+"
	}
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return fmt.Sprintf("%s%d", sign, int64(v))
	}
	return fmt.Sprintf("%s%.3f", sign, v)
}

func formatPct(v float64) string {
	sign := ""
	if v > 0 {
		sign = "+"
	}
	return fmt.Sprintf("%s%.2f%%", sign, v)
}

// emitBenchDiff lands the BenchmarkDiff event inside a write tx.
// gatePassed is nil for non-gated harnesses + the error paths (gate not
// computed); gateFailures names the drifted gate metrics.
func emitBenchDiff(ctx context.Context, pool *db.Pool, project, slug string, metrics []events.BenchmarkMetricDiff, runLatencyMs int, baselineUpdated bool, stderrPath, errStr string, gatePassed *bool, gateFailures []string) error {
	if pool == nil {
		return nil
	}
	return pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity: events.NewEntityRef("bench", slug, project),
			Payload: events.BenchmarkDiffPayload{
				Slug:            slug,
				Metrics:         metrics,
				RunLatencyMs:    runLatencyMs,
				BaselineUpdated: baselineUpdated,
				StderrLogPath:   stderrPath,
				Error:           errStr,
				GatePassed:      gatePassed,
				GateFailures:    gateFailures,
			},
		})
		return err
	})
}

func emitBenchBaselineUpdated(ctx context.Context, pool *db.Pool, project, slug, path, prevSHA, newSHA string) error {
	if pool == nil {
		return nil
	}
	return pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity: events.NewEntityRef("bench", slug, project),
			Payload: events.BenchmarkBaselineUpdatedPayload{
				Slug:                   slug,
				BaselineJSONPath:       path,
				PreviousBaselineSHA256: prevSHA,
				NewBaselineSHA256:      newSHA,
			},
		})
		return err
	})
}

func atomicWriteBaseline(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bench-baseline-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func hashFileIfExists(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return sha256Hex(data), nil
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
