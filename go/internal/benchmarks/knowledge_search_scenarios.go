package benchmarks

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// KsCandidateMultiplier sizes the FTS5 pre-filter pool: top_k * multiplier
// candidates are sent to Qwen so the rerank has a meaningful selection.
// Matches the Rust dispatch convention (top_k * 4) and Go's
// knowledge.KnowledgeSearchCandidateMultiplier.
const KsCandidateMultiplier = 4

// KsScenario is one retrieval-accuracy scenario for knowledge-search
// regression. Mirrors Rust benchmarks::scenarios_knowledge_search::KsScenario.
type KsScenario struct {
	Slug              string `json:"slug"`
	Query             string `json:"query"`
	SourceRefContains string `json:"source_ref_contains"`
	SourceType        string `json:"source_type"` // informational, not used for grading
	HeldOut           bool   `json:"held_out"`
}

// RaScenario is one Reason/Attribute scenario for knowledge-search regression.
// Tests whether the retriever returns a pointer that grounds an attributable
// answer. Mirrors Rust RaScenario.
type RaScenario struct {
	Slug              string `json:"slug"`
	Query             string `json:"query"`
	SourceRefContains string `json:"source_ref_contains"`
	Origin            string `json:"origin"` // "real" or "synthetic"
}

// LoadKnowledgeSearchScenarios reads the retrieval-accuracy corpus from
// <dir>/knowledge-search.jsonl. Defaults dir to DefaultGoldDir() when empty.
func LoadKnowledgeSearchScenarios(dir string) ([]KsScenario, error) {
	if dir == "" {
		dir = DefaultGoldDir()
	}
	path := filepath.Join(dir, "knowledge-search.jsonl")
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	var rows []KsScenario
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row KsScenario
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("parse %s line %d: %w", path, lineNo, err)
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}
	return rows, nil
}

// LoadRaScenarios reads the Reason/Attribute corpus from
// <dir>/knowledge-search-reason-attribute.jsonl.
func LoadRaScenarios(dir string) ([]RaScenario, error) {
	if dir == "" {
		dir = DefaultGoldDir()
	}
	path := filepath.Join(dir, "knowledge-search-reason-attribute.jsonl")
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	var rows []RaScenario
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row RaScenario
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("parse %s line %d: %w", path, lineNo, err)
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}
	return rows, nil
}
