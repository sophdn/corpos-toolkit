package construct

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/forge/fieldvalue"
	"toolkit/internal/work"
)

// ── bench arm (chain 311 T7 Stage 6 P2-A) ───────────────────────────────────
//
// bench is an event-sourced create whose projection row (bench_harnesses) is
// materialized by the projections/bench_harnesses.go fold off BenchmarkForged.
// Unlike bug/chain/task, bench is IDEMPOTENT rather than dup-rejected: a
// re-forge of an existing slug returns the existing identity untouched (the
// fold's ON CONFLICT DO NOTHING), so bench is deliberately NOT in
// shouldDupCheck. It is also not Indexed, so it's not in needsIndexSync.
//
// Field validation + canonicalization is re-homed to ResolveBenchFields
// (the exported seam), shared with createBenchInTx, so the construct
// path and the forge path emit a byte-identical BenchmarkForged payload.

// BenchInput carries forge(bench)'s field surface (schema field names, not the
// flag_set→flag_set_json column rename). ParseOutputAs / TimeoutMs / GateMetrics
// are optional (empty = forge's defaults: json / 60000 / report-only).
type BenchInput struct {
	Slug             string
	BinaryPath       string
	FlagSet          string
	BaselineJSONPath string
	ParseOutputAs    string
	TimeoutMs        string
	GateMetrics      string
}

// fieldMap renders BenchInput into the fieldvalue.FieldValue map ResolveBenchFields
// reads — keyed by the bench schema's field names. The three required fields
// are always present (so ResolveBenchFields produces forge's required-field
// errors); the optionals are set verbatim ("" is handled as absent inside
// ResolveBenchFields, applying the same defaults forge applies).
func (in BenchInput) fieldMap() map[string]fieldvalue.FieldValue {
	return map[string]fieldvalue.FieldValue{
		"binary_path":        fieldvalue.SingleValue(in.BinaryPath),
		"flag_set":           fieldvalue.SingleValue(in.FlagSet),
		"baseline_json_path": fieldvalue.SingleValue(in.BaselineJSONPath),
		"parse_output_as":    fieldvalue.SingleValue(in.ParseOutputAs),
		"timeout_ms":         fieldvalue.SingleValue(in.TimeoutMs),
		"gate_metrics":       fieldvalue.SingleValue(in.GateMetrics),
	}
}

// buildBench returns a BenchmarkForged event byte-identical to
// createBenchInTx's for equivalent input, plus the routing note. It
// probes bench_harnesses for an existing row to set the payload's idempotent
// flag + the note's verb exactly as forge does — read-only, on the supplied
// Queryer (the umbrella's pool). The fold is the writer; this only emits.
func buildBench(ctx context.Context, q db.Queryer, project string, in BenchInput) (work.RecordEvent, string, error) {
	if strings.TrimSpace(in.Slug) == "" {
		return work.RecordEvent{}, "", fmt.Errorf("bench: slug is required")
	}
	bf, err := ResolveBenchFields(in.fieldMap())
	if err != nil {
		return work.RecordEvent{}, "", err
	}

	// Probe idempotency (mirrors createBenchInTx): an existing (project,
	// slug) row means this is a re-forge that returns the existing identity.
	idempotent := false
	var existingID int64
	probeErr := q.QueryRowContext(ctx,
		`SELECT id FROM bench_harnesses WHERE project_id = ? AND slug = ?`,
		project, in.Slug,
	).Scan(&existingID)
	if probeErr == nil {
		idempotent = true
	} else if !errors.Is(probeErr, sql.ErrNoRows) {
		return work.RecordEvent{}, "", fmt.Errorf("probe existing bench: %w", probeErr)
	}

	timeoutPtr := bf.TimeoutMs
	payload, err := json.Marshal(events.BenchmarkForgedPayload{
		Slug:             in.Slug,
		BinaryPath:       bf.BinaryPath,
		BaselineJSONPath: bf.BaselineJSONPath,
		ParseOutputAs:    bf.ParseOutputAs,
		TimeoutMs:        &timeoutPtr,
		Idempotent:       idempotent,
		FlagSetJSON:      bf.FlagSetJSON,
		GateMetrics:      bf.GateMetrics,
	})
	if err != nil {
		return work.RecordEvent{}, "", fmt.Errorf("marshal BenchmarkForged payload: %w", err)
	}

	verb := "registered"
	if idempotent {
		verb = "re-resolved (idempotent — existing row returned)"
	}
	gateNote := "report-only (no gate_metrics)"
	if bf.GateMetrics != "" {
		gateNote = "gate_metrics=" + bf.GateMetrics
	}
	note := fmt.Sprintf("bench %q %s; binary=%s baseline=%s parse_output_as=%s timeout_ms=%d %s",
		in.Slug, verb, bf.BinaryPath, bf.BaselineJSONPath, bf.ParseOutputAs, bf.TimeoutMs, gateNote)

	pid := project
	return work.RecordEvent{
		Type: "BenchmarkForged",
		// EntityKind is set explicitly: events.EntityKindForType does not infer
		// "bench" from BenchmarkForged (forge's native path sets it via
		// events.NewEntityRef), so the record submit + bench_harnesses fold
		// (which filters on entity_kind='bench') need it stamped here.
		EntityKind:      "bench",
		EntitySlug:      in.Slug,
		EntityProjectID: &pid,
		Payload:         payload,
	}, note, nil
}
