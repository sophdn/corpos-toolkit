package construct

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// BenchFields is the validated, canonicalized bench field set that BOTH
// forge's native createBenchInTx AND construct's bench arm build from, so the
// two paths emit a byte-identical BenchmarkForged payload + write an identical
// bench_harnesses row. Re-home seam (chain 311 T7 Stage 6 P2-A, 2026-05-29):
// the field truth lives here once instead of being duplicated in construct.
type BenchFields struct {
	BinaryPath       string
	FlagSetJSON      string
	BaselineJSONPath string
	ParseOutputAs    string
	TimeoutMs        int
	GateMetrics      string
}

// ResolveBenchFields validates the bench schema fields and canonicalizes them
// (flag_set → normalized flag_set_json, parse_output_as default + closed-enum
// check, timeout_ms parse + default, gate_metrics passthrough). Pure (no DB /
// tx) so the forge create path and the construct bench arm share one source of
// field truth — guaranteeing parity. Mirrors the pre-P2-A inline validation
// that lived in createBenchInTx.
func ResolveBenchFields(fields map[string]FieldValue) (BenchFields, error) {
	binaryPath := stringField(fields, "binary_path")
	if binaryPath == "" {
		return BenchFields{}, fmt.Errorf("forge(bench): binary_path is required")
	}
	baselinePath := stringField(fields, "baseline_json_path")
	if baselinePath == "" {
		return BenchFields{}, fmt.Errorf("forge(bench): baseline_json_path is required")
	}

	flagSet := stringField(fields, "flag_set")
	if flagSet == "" {
		return BenchFields{}, fmt.Errorf("forge(bench): flag_set is required")
	}
	flagSetJSON, err := normalizeFlagSet(flagSet)
	if err != nil {
		return BenchFields{}, fmt.Errorf("forge(bench): flag_set: %w", err)
	}

	parseOutputAs := stringField(fields, "parse_output_as")
	if parseOutputAs == "" {
		parseOutputAs = "json"
	}
	if parseOutputAs != "json" {
		return BenchFields{}, fmt.Errorf("forge(bench): parse_output_as=%q not supported in v1 (json only)", parseOutputAs)
	}

	timeoutMs := 60000
	if tStr := stringField(fields, "timeout_ms"); tStr != "" {
		v, err := strconv.Atoi(tStr)
		if err != nil || v < 1 {
			return BenchFields{}, fmt.Errorf("forge(bench): timeout_ms must be a positive integer, got %q", tStr)
		}
		timeoutMs = v
	}

	// gate_metrics is optional — empty means report-only (no pass/fail gate).
	return BenchFields{
		BinaryPath:       binaryPath,
		FlagSetJSON:      flagSetJSON,
		BaselineJSONPath: baselinePath,
		ParseOutputAs:    parseOutputAs,
		TimeoutMs:        timeoutMs,
		GateMetrics:      stringField(fields, "gate_metrics"),
	}, nil
}

// normalizeFlagSet accepts either a JSON array of {flag,value} pairs OR
// a single command-line string and returns the canonical JSON array
// form that gets stored in flag_set_json. The bench_run handler reads
// this back and tokenizes for os/exec.
//
// JSON array form is preferred; single-string is a convenience for
// one-liner registrations. Tokenization on the single-string form is
// whitespace-split with no shell-quote handling — values with spaces
// MUST use the array form.
func normalizeFlagSet(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("empty flag_set")
	}
	if strings.HasPrefix(trimmed, "[") {
		var entries []struct {
			Flag  string `json:"flag"`
			Value string `json:"value"`
		}
		if err := json.Unmarshal([]byte(trimmed), &entries); err != nil {
			return "", fmt.Errorf("parse JSON array: %w", err)
		}
		for i, e := range entries {
			if e.Flag == "" {
				return "", fmt.Errorf("entry[%d]: flag is required", i)
			}
		}
		// Re-marshal canonically so the stored form is stable.
		buf, err := json.Marshal(entries)
		if err != nil {
			return "", fmt.Errorf("re-marshal: %w", err)
		}
		return string(buf), nil
	}
	// Single-string form: whitespace-split into discrete args. Each
	// token becomes a single arg; pairs like `--flag value` produce
	// two tokens (two args). We synthesize the pairs as {flag,value=""}
	// for storage and rely on bench_run to flatten back to discrete
	// args at run time.
	tokens := strings.Fields(trimmed)
	entries := make([]map[string]string, 0, len(tokens))
	for i := 0; i < len(tokens); i++ {
		entries = append(entries, map[string]string{"flag": tokens[i], "value": ""})
	}
	buf, err := json.Marshal(entries)
	if err != nil {
		return "", fmt.Errorf("re-marshal split tokens: %w", err)
	}
	return string(buf), nil
}
