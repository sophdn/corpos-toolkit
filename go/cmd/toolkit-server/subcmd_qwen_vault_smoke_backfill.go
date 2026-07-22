package main

// One-shot tool that reads
// seed-packet/process-docs/adhoc/qwen-vault-smoke/results.jsonl and
// inserts the 7 records into benchmark_results with task_shape +
// criterion subscores. Per chain `benchmarks-shape-criteria-reshape`
// T10 — gives the dashboard's Classify and Retrieve cards their first
// non-degenerate qwen polygons (the visual proof of the baseline).
//
// Idempotent: each record gets `scenario_id = 'qwen-vault-smoke-R{N}'`
// as a natural key; existing rows with that id are skipped.
//
// Mode → shape mapping (per scope.md § E6 → shape backfill mapping):
//
//	routing   → Classify (pick one subdir from N candidates)
//	retrieval → Retrieve (pick one note from a list)
//	honesty   → underlying shape (R7 is Retrieve-shaped — the prompt
//	            asks to pick from a vault note list); honesty subscore
//	            populated from refusal-vs-fabrication.
//
// Usage:
//
//	toolkit-server qwen-vault-smoke-backfill [--source PATH] [--db PATH]
//
// Folded into toolkit-server as `toolkit-server
// qwen-vault-smoke-backfill` by harvest-the-consolidation T4. Ported
// from benchmarks/src/bin/qwen_vault_smoke_backfill.rs per chain
// rust-retirement-and-db-hardening T5 Phase 3 (Option A).

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/events"

	_ "modernc.org/sqlite"
)

const qvsDefaultProjectID = "seed-packet"

// qvsScoreResult bundles the four outputs of qvsScore().
type qvsScoreResult struct {
	shape    string   // "Classify" | "Retrieve" | "Extract" | "Summarize"
	accuracy float64  // 0.0 or 1.0
	ranking  *float64 // nil for Classify, 0.0 or 1.0 otherwise
	honesty  *float64 // nil except for honesty mode
	err      error
}

// qvsScore maps (mode, gold, response) → (shape, accuracy, ranking,
// honesty). Mirrors the Rust score() function byte-for-byte modulo
// Go-idiomatic optional returns (*float64 vs Option<f64>).
func qvsScore(mode, gold, response string) qvsScoreResult {
	goldLower := strings.ToLower(strings.TrimSpace(gold))
	respLower := strings.ToLower(strings.TrimSpace(response))
	f := func(v float64) *float64 { return &v }

	switch mode {
	case "routing":
		acc := 0.0
		if goldLower == respLower {
			acc = 1.0
		}
		return qvsScoreResult{shape: "Classify", accuracy: acc}
	case "retrieval":
		hit := strings.Contains(respLower, goldLower)
		acc := 0.0
		if hit {
			acc = 1.0
		}
		rank := 0.0
		if firstLine := strings.SplitN(respLower, "\n", 2)[0]; strings.TrimSpace(firstLine) == goldLower {
			rank = 1.0
		}
		return qvsScoreResult{shape: "Retrieve", accuracy: acc, ranking: f(rank)}
	case "honesty":
		// Honest no-match: refusal IS the right answer. Accuracy
		// mirrors honesty; ranking_quality is 1.0 if refused (no
		// spurious ranking) else 0.0.
		refused := strings.Contains(respLower, "no match")
		honesty := 0.0
		if refused {
			honesty = 1.0
		}
		return qvsScoreResult{
			shape:    "Retrieve",
			accuracy: honesty,
			ranking:  f(honesty),
			honesty:  f(honesty),
		}
	default:
		return qvsScoreResult{err: fmt.Errorf("unknown mode %q", mode)}
	}
}

// qvsParseISO8601Z parses the strict YYYY-MM-DDTHH:MM:SSZ shape the
// qwen-vault-smoke writer emits. Returns Unix seconds.
func qvsParseISO8601Z(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if len(s) != 20 {
		return 0, fmt.Errorf("expected YYYY-MM-DDTHH:MM:SSZ (20 chars), got %q", s)
	}
	if s[10] != 'T' {
		return 0, fmt.Errorf("expected 'T' at position 10, got %q", s)
	}
	if s[19] != 'Z' {
		return 0, fmt.Errorf("expected 'Z' at position 19, got %q", s)
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", s, err)
	}
	return t.Unix(), nil
}

// qvsStripGGUFSuffix normalizes the dashboard model_name alias.
func qvsStripGGUFSuffix(model string) string {
	if strings.HasPrefix(model, "Qwen2.5-32B") {
		return "qwen2.5-32b"
	}
	return model
}

// qvsFmtOpt mirrors Rust's fmt_opt — formats Optional<f64> for display.
func qvsFmtOpt(v *float64) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprintf("%.2f", *v)
}

func qvsDefaultSourcePath() string {
	if _, err := os.Stat("seed-packet/process-docs/adhoc/qwen-vault-smoke/results.jsonl"); err == nil {
		return "seed-packet/process-docs/adhoc/qwen-vault-smoke/results.jsonl"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return home + "/dev/seed-packet/process-docs/adhoc/qwen-vault-smoke/results.jsonl"
}

// qvsScenarioPresent returns true if a row with the given scenario_id
// already exists in proj_benchmark_results. Idempotency anchor.
func qvsScenarioPresent(ctx context.Context, pool *db.Pool, scenarioID string) (bool, error) {
	var count int
	err := pool.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM proj_benchmark_results WHERE scenario_id = ?`,
		scenarioID).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// qvsSentinelProvenance returns the "legacy backfill" provenance row.
func qvsSentinelProvenance(modelID string) events.BenchmarkProvenance {
	if modelID == "" {
		modelID = "unknown-legacy"
	}
	return events.BenchmarkProvenance{
		ModelID:             modelID,
		ModelVersion:        "legacy-backfill-no-version",
		PromptTemplateHash:  "legacy-backfill-no-prompt",
		CorpusHash:          "legacy-backfill-no-corpus",
		RetrieverVersion:    "legacy-backfill-no-retriever",
		RetrieverConfigHash: "legacy-backfill-no-config",
		Seed:                0,
		EnvHash:             "legacy-backfill-no-env",
	}
}

// qvsNewUUIDv4 generates an RFC-4122 v4 UUID string.
func qvsNewUUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// qvsSmokeRecord mirrors the JSONL shape qwen-vault-smoke writes.
type qvsSmokeRecord struct {
	ID        string           `json:"id"`
	Mode      string           `json:"mode"`
	Gold      string           `json:"gold"`
	StartedAt string           `json:"started_at"`
	Response  qvsSmokeResponse `json:"response"`
}

type qvsSmokeResponse struct {
	Choices []qvsSmokeChoice `json:"choices"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Usage   *qvsSmokeUsage   `json:"usage,omitempty"`
}

type qvsSmokeChoice struct {
	Message qvsSmokeMessage `json:"message"`
}

type qvsSmokeMessage struct {
	Content string `json:"content"`
}

type qvsSmokeUsage struct {
	PromptTokens     *int64 `json:"prompt_tokens,omitempty"`
	CompletionTokens *int64 `json:"completion_tokens,omitempty"`
}

// qvsProcessRecord emits Started + Completed events + inserts the
// provenance row for one JSONL record. Returns true if the record was
// inserted, false if skipped (already present).
func qvsProcessRecord(ctx context.Context, pool *db.Pool, line, runID string, out io.Writer) (bool, error) {
	var rec qvsSmokeRecord
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		return false, fmt.Errorf("parse jsonl: %w", err)
	}
	scenarioID := "qwen-vault-smoke-" + rec.ID

	present, err := qvsScenarioPresent(ctx, pool, scenarioID)
	if err != nil {
		return false, fmt.Errorf("check scenario_present %s: %w", scenarioID, err)
	}
	if present {
		fmt.Fprintf(out, "  %s  · already present, skipping\n", scenarioID)
		return false, nil
	}

	startedUnix, err := qvsParseISO8601Z(rec.StartedAt)
	if err != nil {
		return false, fmt.Errorf("parse started_at: %w", err)
	}

	if len(rec.Response.Choices) == 0 {
		return false, errors.New("response.choices missing or empty")
	}
	responseText := strings.TrimSpace(rec.Response.Choices[0].Message.Content)
	modelRaw := rec.Response.Model
	modelName := qvsStripGGUFSuffix(modelRaw)
	if modelName == "" {
		modelName = "qwen2.5-32b"
	}
	wallClockMS := (rec.Response.Created - startedUnix) * 1000
	if wallClockMS < 0 {
		wallClockMS = 0
	}

	sc := qvsScore(rec.Mode, rec.Gold, responseText)
	if sc.err != nil {
		return false, sc.err
	}

	var inputTokens, outputTokens *int64
	if rec.Response.Usage != nil {
		inputTokens = rec.Response.Usage.PromptTokens
		outputTokens = rec.Response.Usage.CompletionTokens
	}
	id := rec.ID
	mode := rec.Mode

	rowID := qvsNewUUIDv4()
	provenanceID := qvsNewUUIDv4()
	provenance := qvsSentinelProvenance(modelRaw)
	taskShapeStr := sc.shape
	notes := fmt.Sprintf("backfill from qwen-vault-smoke %s (mode=%s)", id, mode)
	toolName := fmt.Sprintf("qwen-vault-smoke-%s (%s)", id, mode)

	err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
		// 1. Emit BenchmarkRunStarted with the sentinel provenance.
		startedEventID, emitErr := events.Emit(ctx, tx, events.EmitArgs{
			Entity: events.NewCrossCuttingEntityRef("benchmark_run", runID),
			Payload: events.BenchmarkRunStartedPayload{
				ScenarioID: scenarioID,
				Provenance: provenance,
			},
		})
		if emitErr != nil {
			return fmt.Errorf("emit BenchmarkRunStarted: %w", emitErr)
		}
		// 2. Insert the provenance row, linked to the Started event.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO benchmark_provenance (
				id, run_id, model_id, model_version,
				prompt_template_hash, corpus_hash,
				retriever_version, retriever_config_hash,
				seed, env_hash, started_event_id
			) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			provenanceID, runID,
			provenance.ModelID, provenance.ModelVersion,
			provenance.PromptTemplateHash, provenance.CorpusHash,
			provenance.RetrieverVersion, provenance.RetrieverConfigHash,
			provenance.Seed, provenance.EnvHash, startedEventID,
		); err != nil {
			return fmt.Errorf("insert benchmark_provenance: %w", err)
		}
		// 3. Emit BenchmarkRunCompleted; projection fold inserts the
		//    proj_benchmark_results row from the payload.
		runIDCopy := runID
		projCopy := qvsDefaultProjectID
		scenarioCopy := scenarioID
		runAtCopy := startedUnix
		detected := responseText
		notesCopy := notes
		taskShapePtr := &taskShapeStr
		acc := sc.accuracy
		interpretationOKBool := acc >= 1.0
		rc := &events.BenchmarkResultColumns{
			ToolName:            toolName,
			ModelName:           modelName,
			TaskShape:           taskShapePtr,
			AccuracyScore:       &acc,
			HonestyScore:        sc.honesty,
			RankingQualityScore: sc.ranking,
			InvocationOK:        acc >= 1.0,
			DetectedTool:        &detected,
			Notes:               &notesCopy,
			InvokedContextually: true,
			InterpretationOK:    &interpretationOKBool,
		}
		var inputInt, outputInt *int
		if inputTokens != nil {
			n := int(*inputTokens)
			inputInt = &n
		}
		if outputTokens != nil {
			n := int(*outputTokens)
			outputInt = &n
		}
		provIDCopy := provenanceID
		payload := events.BenchmarkRunCompletedPayload{
			RunID:             runIDCopy,
			WallClockMS:       int(wallClockMS),
			InputTokens:       inputInt,
			OutputTokens:      outputInt,
			ResultColumns:     rc,
			BenchmarkResultID: &rowID,
			ProjectID:         &projCopy,
			ScenarioID:        &scenarioCopy,
			RunAt:             &runAtCopy,
			ProvenanceID:      &provIDCopy,
		}
		_, emitErr2 := events.Emit(ctx, tx, events.EmitArgs{
			Entity:  events.NewCrossCuttingEntityRef("benchmark_run", runID),
			Payload: payload,
		})
		return emitErr2
	})
	if err != nil {
		return false, fmt.Errorf("record %s: %w", scenarioID, err)
	}

	fmt.Fprintf(out, "  %s  · %-9s acc=%s rank=%s hon=%s (%dms)\n",
		scenarioID, taskShapeStr, qvsFmtOpt(&sc.accuracy), qvsFmtOpt(sc.ranking), qvsFmtOpt(sc.honesty), wallClockMS)
	return true, nil
}

// runQwenVaultSmokeBackfill drives the `qwen-vault-smoke-backfill`
// subcommand. Returns 0 on success; 1 on any error.
func runQwenVaultSmokeBackfill(args []string) int {
	fs := flag.NewFlagSet("qwen-vault-smoke-backfill", flag.ContinueOnError)
	source := fs.String("source", qvsDefaultSourcePath(), "path to qwen-vault-smoke results.jsonl")
	dbPath := fs.String("db", defaultToolkitDBPath(), "path to toolkit.db")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: toolkit-server qwen-vault-smoke-backfill [--source PATH] [--db PATH]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}
	fmt.Printf("→ source: %s\n", *source)
	fmt.Printf("→ db:     %s\n", *dbPath)

	raw, err := os.ReadFile(*source)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read source %s: %v\n", *source, err)
		return 1
	}
	installProjectionsFoldHook()
	pool, err := db.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db %s: %v\n", *dbPath, err)
		return 1
	}
	defer pool.Close()

	runID := qvsNewUUIDv4()
	fmt.Printf("→ run_id: %s\n", runID)

	ctx := context.Background()
	var inserted, skipped int
	for lineNo, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		ins, err := qvsProcessRecord(ctx, pool, line, runID, os.Stdout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "line %d: %v\n", lineNo+1, err)
			return 1
		}
		if ins {
			inserted++
		} else {
			skipped++
		}
	}
	fmt.Printf("→ inserted: %d, skipped: %d\n", inserted, skipped)
	return 0
}
