package benchmarks

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GoldCorpusEnv is the env var that overrides the gold-corpus directory.
// Mirrors Rust scenarios_e4_pre_commit_failure's A1_RUBRIC_SMOKE_GOLD_DIR
// override, generalized to every rubric.
const GoldCorpusEnv = "A1_RUBRIC_SMOKE_GOLD_DIR"

// DefaultGoldDir is the canonical on-disk location for the e4 gold
// corpora. Each rubric's scenarios live at <DefaultGoldDir>/<rubric>.jsonl
// with one JSON row per scenario: {"slug": ..., "label": ..., "text": ...}.
// Honors A1_RUBRIC_SMOKE_GOLD_DIR when set.
func DefaultGoldDir() string {
	if d := os.Getenv(GoldCorpusEnv); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, "dev/seed-packet/process-docs/adhoc/a1-rubric-smoke/gold")
}

// goldRow is the on-disk JSONL row shape. Each line is one row.
type goldRow struct {
	Slug  string `json:"slug"`
	Label string `json:"label"`
	Text  string `json:"text"`
}

// LoadGoldScenarios reads <dir>/<rubric>.jsonl and returns the parsed
// ClassifyScenarios. If dir is empty, DefaultGoldDir() is used.
//
// The label "unclassifiable" maps to ClassifyGold{Kind: GoldUnclassifiable};
// labels containing "|" map to ClassifyGold{Kind: GoldMultiClass, MultiLabel: split-on-|};
// any other label maps to ClassifyGold{Kind: GoldSingleClass, SingleLabel: label}.
//
// Mirrors Rust scenarios_e4_pre_commit_failure::seed_scenarios + analogous
// inline-scenario seeders across the other 9 rubrics, now collapsed to
// one generic loader.
func LoadGoldScenarios(rubric, dir string) ([]ClassifyScenario, error) {
	if dir == "" {
		dir = DefaultGoldDir()
	}
	path := filepath.Join(dir, rubric+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open gold corpus %s: %w", path, err)
	}
	defer f.Close()

	var scenarios []ClassifyScenario
	scanner := bufio.NewScanner(f)
	// Some scenarios (chain-assessment, artifact-review) carry multi-paragraph
	// text. Default 64 KiB buffer comfortably covers, but raise to 1 MiB to
	// future-proof against extra-long extract-review fixtures.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row goldRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("parse %s line %d: %w", path, lineNo, err)
		}
		scenarios = append(scenarios, ClassifyScenario{
			Slug:       row.Slug,
			Text:       row.Text,
			RubricSlug: rubric,
			Gold:       parseGoldLabel(row.Label),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}
	return scenarios, nil
}

// parseGoldLabel maps an on-disk label string into a ClassifyGold value.
// "unclassifiable" → Unclassifiable; "a|b|c" → MultiClass; otherwise SingleClass.
func parseGoldLabel(label string) ClassifyGold {
	if label == "unclassifiable" {
		return ClassifyGold{Kind: GoldUnclassifiable}
	}
	if strings.Contains(label, "|") {
		return ClassifyGold{
			Kind:       GoldMultiClass,
			MultiLabel: strings.Split(label, "|"),
		}
	}
	return ClassifyGold{Kind: GoldSingleClass, SingleLabel: label}
}

// KnownE4Rubrics is the set of rubrics that have gold corpora under
// DefaultGoldDir(). Used by smoke-classify-rubric to validate the
// --rubric flag and by the dashboard to enumerate available rubrics.
//
// Adding a new rubric port = adding a new <rubric>.jsonl + adding the
// slug here. No new binary, no new dispatch arm.
var KnownE4Rubrics = []string{
	"agentic-audit",
	"artifact-review",
	"chain-assessment",
	"content-routing-antipattern",
	"docstring-drift",
	"pre-commit-failure",
	"refactoring-heuristics",
	"retirement-signal",
	"session-routing",
	"tiered-context",
}

// IsKnownE4Rubric reports whether rubric is in KnownE4Rubrics.
func IsKnownE4Rubric(rubric string) bool {
	for _, r := range KnownE4Rubrics {
		if r == rubric {
			return true
		}
	}
	return false
}
