package gate

import (
	"bytes"
	"fmt"
	"os"

	yaml "go.yaml.in/yaml/v3"
)

// Config is the parsed gate.yml — the stack-agnostic description of what
// the gate should run for a repository.
type Config struct {
	Stack  string                 `yaml:"stack"`
	Units  []Unit                 `yaml:"units"`
	Checks map[string]CheckConfig `yaml:"checks"`
	Custom []Custom               `yaml:"custom"`
}

// Unit is one build unit within the repo — a module directory plus its
// build tags. The go adapter runs its checks inside Dir.
type Unit struct {
	Dir  string `yaml:"dir"`
	Tags string `yaml:"tags,omitempty"`
	// Tsconfig lists the tsconfig files the TS-stack `static` (typecheck)
	// check runs `tsc -p <cfg> --noEmit` against, one per entry. Empty →
	// a single bare `tsc --noEmit`. Only meaningful for the ts adapter.
	Tsconfig []string `yaml:"tsconfig,omitempty"`
}

// CheckConfig is the per-check toggle block. Floor and Scope are only
// meaningful for the coverage check. Race is meaningful for the coverage
// and test checks (it enables the data-race detector).
type CheckConfig struct {
	Enabled bool   `yaml:"enabled"`
	Tier    string `yaml:"tier"`
	Floor   int    `yaml:"floor,omitempty"`
	Scope   string `yaml:"scope,omitempty"`
	// Race enables `go test -race` for the coverage/test checks. It
	// roughly DOUBLES suite runtime (the race detector instruments every
	// memory access), so it is opt-in per gate.yml rather than on by
	// default. Go runs `-race -coverprofile` together in a single pass,
	// so enabling it on the coverage check adds race detection without a
	// second suite run.
	Race bool `yaml:"race,omitempty"`
	// Thresholds carries the per-metric coverage floors for the TS-stack
	// coverage check — keys are the four vitest/jest metrics (statements,
	// branches, functions, lines), values are minimum percentages. A
	// metric with no entry here is not gated. The Go coverage check
	// ignores this and uses the scalar Floor instead.
	Thresholds map[string]float64 `yaml:"thresholds,omitempty"`
	// Runner selects the TS coverage test runner: "vitest" (the default
	// when empty) or "jest". Both emit the same istanbul-shaped
	// coverage-summary.json, so only the command differs. Ignored by the
	// Go adapter.
	Runner string `yaml:"runner,omitempty"`
	// GovulncheckVersion, when set on the Go vuln check, makes it run
	// govulncheck via `go run golang.org/x/vuln/cmd/govulncheck@<version>`
	// instead of `go tool govulncheck`. Use it for repos that keep go.mod
	// dependency-free (no govulncheck tool directive) — the go-run path
	// needs no go.mod entry. Empty → the default `go tool govulncheck`.
	GovulncheckVersion string `yaml:"govulncheck_version,omitempty"`
}

// Custom is a repo-specific escape-hatch check: an arbitrary shell
// command run in the repo root, failing on non-zero exit.
type Custom struct {
	Name string `yaml:"name"`
	Cmd  string `yaml:"cmd"`
	Tier string `yaml:"tier"`
}

// knownStacks and knownChecks bound the config's vocabulary. `go`, `ts`,
// and `py` have adapters (GoChecks / TSChecks / PyChecks); `shell` parses
// but is custom-only (no adapter checks).
var (
	knownStacks = map[string]bool{"go": true, "ts": true, "shell": true, "py": true}
	knownChecks = map[string]bool{
		"format": true, "vet": true, "lint": true, "build": true,
		"test": true, "coverage": true, "vuln": true, "mutation": true,
		// TS-stack check names: `static` (typecheck). `race`/`goleak` are
		// go-only concerns that SKIP-with-reason if named on a ts unit, so
		// they parse here too. The py adapter reuses `format`, `lint`,
		// `static` (mypy), `coverage` (pytest-cov), and `mutation` (mutmut)
		// — all already present above — and SKIPs `race`/`goleak`.
		"static": true, "race": true, "goleak": true,
	}
	// coverageMetrics bounds the keys allowed in a coverage check's
	// `thresholds:` map — the four istanbul/vitest/jest metrics.
	coverageMetrics = map[string]bool{
		"statements": true, "branches": true, "functions": true, "lines": true,
	}
	// coverageRunners bounds the TS coverage `runner:` values.
	coverageRunners = map[string]bool{"vitest": true, "jest": true}
)

// Load reads and validates gate.yml at path. Parsing is strict: unknown
// keys are an error (KnownFields). Validation enforces a known stack, at
// least one unit, known check names, and valid tiers on every check and
// custom entry.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read gate config %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse gate config %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid gate config %s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if !knownStacks[c.Stack] {
		return fmt.Errorf("unknown stack %q (want go, ts, py, or shell)", c.Stack)
	}
	if len(c.Units) == 0 {
		return fmt.Errorf("at least one unit is required")
	}
	for i, u := range c.Units {
		if u.Dir == "" {
			return fmt.Errorf("unit %d: dir is required", i)
		}
	}
	for name, cc := range c.Checks {
		if !knownChecks[name] {
			return fmt.Errorf("unknown check %q", name)
		}
		if _, err := ParseTier(cc.Tier); err != nil {
			return fmt.Errorf("check %q: %w", name, err)
		}
		for metric := range cc.Thresholds {
			if !coverageMetrics[metric] {
				return fmt.Errorf("check %q: unknown coverage metric %q in thresholds (want statements, branches, functions, or lines)", name, metric)
			}
		}
		if cc.Runner != "" && !coverageRunners[cc.Runner] {
			return fmt.Errorf("check %q: unknown coverage runner %q (want vitest or jest)", name, cc.Runner)
		}
	}
	for i, cu := range c.Custom {
		if cu.Name == "" {
			return fmt.Errorf("custom %d: name is required", i)
		}
		if cu.Cmd == "" {
			return fmt.Errorf("custom %q: cmd is required", cu.Name)
		}
		if _, err := ParseTier(cu.Tier); err != nil {
			return fmt.Errorf("custom %q: %w", cu.Name, err)
		}
	}
	return nil
}
