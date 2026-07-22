// Command corpos-gate is the CLI front-end for the stack-agnostic gate
// orchestrator (internal/gate). It loads gate.yml, resolves the check
// plan for a tier, and either prints it (`plan`) or runs it (`run`).
//
// Usage:
//
//	corpos-gate run  --tier=pre-commit|pre-push|ci [--config <path>] [--skip <a,b>] [--list]
//	corpos-gate plan --tier=pre-commit|pre-push|ci [--config <path>] [--skip <a,b>]
//	corpos-gate init [dir]
//
// With no --config, gate.yml is found by walking up from the current
// directory, then falling back to `git rev-parse --show-toplevel`.
// `run` exits 0 when every selected check passes, 1 on any failure.
// `init` wires gate-only hooks + a starter gate.yml into a repo.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"toolkit/internal/gate"
)

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}

// runCLI is the testable entry point: it takes the arg slice (without
// argv[0]) and the output writers, and returns the process exit code.
func runCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		usage(stderr)
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "run":
		return cmdRun(rest, stdout, stderr)
	case "plan":
		return cmdPlan(rest, stdout, stderr)
	case "init":
		return cmdInit(rest, stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "corpos-gate: unknown subcommand %q\n", sub)
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `corpos-gate — stack-agnostic gate orchestrator

Usage:
  corpos-gate run  --tier=pre-commit|pre-push|ci [--config <path>] [--skip <a,b>] [--list]
  corpos-gate plan --tier=pre-commit|pre-push|ci [--config <path>] [--skip <a,b>]
  corpos-gate init [dir]

  run   execute the check plan for the tier; exit 1 on any failure
  plan  print the resolved ordered check plan without executing
  init  install gate-only hooks + a starter gate.yml into a repo (idempotent)

  tiers are a superset: ci ⊃ pre-push ⊃ pre-commit
  --skip  comma-separated check names to omit from the plan (e.g. --skip=vuln)
`)
}

// cmdRun handles `corpos-gate run`.
func cmdRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tierStr := fs.String("tier", "pre-commit", "gate tier to run: pre-commit, pre-push, or ci")
	configPath := fs.String("config", "", "path to gate.yml (default: found by walking up / git root)")
	skipStr := fs.String("skip", "", "comma-separated check names to omit (e.g. vuln)")
	list := fs.Bool("list", false, "print the resolved plan without executing (same as `plan`)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	skip := parseSkip(*skipStr)

	cfg, root, tier, code := loadForCmd(*configPath, *tierStr, stderr)
	if cfg == nil {
		return code
	}
	if *list {
		printPlan(stdout, cfg, tier, skip)
		return 0
	}

	env := gate.RunEnv{RepoRoot: root, Out: stdout}
	_, ok, err := gate.Run(context.Background(), cfg, tier, env, skip)
	if err != nil {
		fmt.Fprintf(stderr, "corpos-gate: %v\n", err)
		return 1
	}
	if !ok {
		fmt.Fprintln(stdout, "[corpos-gate] FAIL — one or more checks failed")
		return 1
	}
	fmt.Fprintln(stdout, "[corpos-gate] PASS — all checks passed")
	return 0
}

// cmdPlan handles `corpos-gate plan`.
func cmdPlan(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tierStr := fs.String("tier", "pre-commit", "gate tier to plan: pre-commit, pre-push, or ci")
	configPath := fs.String("config", "", "path to gate.yml (default: found by walking up / git root)")
	skipStr := fs.String("skip", "", "comma-separated check names to omit (e.g. vuln)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	skip := parseSkip(*skipStr)
	cfg, _, tier, code := loadForCmd(*configPath, *tierStr, stderr)
	if cfg == nil {
		return code
	}
	printPlan(stdout, cfg, tier, skip)
	return 0
}

// parseSkip splits a comma-separated --skip value into a clean list of
// check names, dropping empty tokens and surrounding whitespace so that
// "", "vuln", and "vuln, build" all parse sensibly.
func parseSkip(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, tok := range strings.Split(s, ",") {
		if t := strings.TrimSpace(tok); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// loadForCmd resolves the config path, loads it, and parses the tier.
// On any error it writes to stderr and returns a nil config plus the
// exit code to use; on success it returns the config, repo root, tier,
// and 0.
func loadForCmd(configPath, tierStr string, stderr io.Writer) (*gate.Config, string, gate.Tier, int) {
	tier, err := gate.ParseTier(tierStr)
	if err != nil {
		fmt.Fprintf(stderr, "corpos-gate: %v\n", err)
		return nil, "", 0, 2
	}
	path, err := findConfig(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "corpos-gate: %v\n", err)
		return nil, "", 0, 1
	}
	cfg, err := gate.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "corpos-gate: %v\n", err)
		return nil, "", 0, 1
	}
	root, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		fmt.Fprintf(stderr, "corpos-gate: %v\n", err)
		return nil, "", 0, 1
	}
	return cfg, root, tier, 0
}

// printPlan renders the resolved plan for a tier, omitting any check
// named in skip.
func printPlan(w io.Writer, cfg *gate.Config, tier gate.Tier, skip []string) {
	entries := gate.Plan(cfg, tier, skip)
	fmt.Fprintf(w, "corpos-gate plan — tier=%s (%d checks)\n", tier, len(entries))
	for i, e := range entries {
		fmt.Fprintf(w, "  %d. %-24s [%s]\n", i+1, e.Name, e.Tier)
	}
}

// findConfig resolves the gate.yml path: an explicit --config wins;
// otherwise walk up from CWD looking for gate.yml, then fall back to the
// git repo root.
func findConfig(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if dir, err := os.Getwd(); err == nil {
		for {
			p := filepath.Join(dir, "gate.yml")
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
		p := filepath.Join(strings.TrimSpace(string(out)), "gate.yml")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("gate.yml not found (pass --config, or run from within the repo)")
}
