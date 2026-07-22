package gate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// goCheckOrder is the stable execution order for the go-stack checks.
// GoChecks emits enabled checks in this order so a run is reproducible
// and the plan is legible.
var goCheckOrder = []string{
	"format", "vet", "lint", "build", "test", "coverage", "vuln", "mutation",
}

// GoChecks builds one Check per ENABLED go-stack check in cfg, in the
// canonical goCheckOrder, parameterized by each unit's dir + tags and
// (for coverage) the configured floor and scope. When more than one
// unit is configured, each check name is suffixed with the unit dir so
// results stay distinguishable.
func GoChecks(cfg *Config) []Check {
	var checks []Check
	multi := len(cfg.Units) > 1
	for _, name := range goCheckOrder {
		cc, ok := cfg.Checks[name]
		if !ok || !cc.Enabled {
			continue
		}
		tier := tierOf(cc.Tier)
		for _, unit := range cfg.Units {
			label := name
			if multi {
				label = name + ":" + unit.Dir
			}
			if c := buildGoCheck(name, label, tier, cc, unit); c != nil {
				checks = append(checks, c)
			}
		}
	}
	return checks
}

// buildGoCheck constructs the Check for one (checkName, unit) pair.
func buildGoCheck(name, label string, tier Tier, cc CheckConfig, unit Unit) Check {
	switch name {
	case "format":
		return funcCheck{label, tier, func(ctx context.Context, env RunEnv) Result {
			return runFormat(ctx, env, label, tier, unit)
		}}
	case "vet":
		return funcCheck{label, tier, func(ctx context.Context, env RunEnv) Result {
			// vet has no meaningful -race variant; always race=false.
			return runGoSimple(ctx, env, label, tier, unit, "vet", false)
		}}
	case "lint":
		return funcCheck{label, tier, func(ctx context.Context, env RunEnv) Result {
			return runLint(ctx, env, label, tier, unit)
		}}
	case "build":
		return funcCheck{label, tier, func(ctx context.Context, env RunEnv) Result {
			// build has no meaningful -race variant; always race=false.
			return runGoSimple(ctx, env, label, tier, unit, "build", false)
		}}
	case "test":
		return funcCheck{label, tier, func(ctx context.Context, env RunEnv) Result {
			// The test check honors cc.Race so a repo running tests
			// WITHOUT a coverage floor can still opt into -race.
			return runGoSimple(ctx, env, label, tier, unit, "test", cc.Race)
		}}
	case "coverage":
		return funcCheck{label, tier, func(ctx context.Context, env RunEnv) Result {
			return runCoverage(ctx, env, label, tier, unit, cc)
		}}
	case "vuln":
		return funcCheck{label, tier, func(ctx context.Context, env RunEnv) Result {
			return runVuln(ctx, env, label, tier, unit, cc)
		}}
	case "mutation":
		return funcCheck{label, tier, func(ctx context.Context, env RunEnv) Result {
			return runMutation(ctx, env, label, tier, unit, cc)
		}}
	}
	return nil
}

// unitDir resolves the absolute working directory for a unit.
func unitDir(env RunEnv, unit Unit) string {
	return filepath.Join(env.RepoRoot, unit.Dir)
}

// goTagArgs prefixes -tags <tags> when tags are configured.
func goTagArgs(tags string, rest ...string) []string {
	if tags != "" {
		return append([]string{"-tags", tags}, rest...)
	}
	return rest
}

// runFormat runs `gofmt -l .` and FAILS if any file is listed (i.e.
// stdout is non-empty), which means the tree carries gofmt drift.
func runFormat(ctx context.Context, env RunEnv, name string, tier Tier, unit Unit) Result {
	start := time.Now()
	out, code, err := env.runner()(ctx, unitDir(env, unit), "gofmt", "-l", ".")
	res := Result{Name: name, Tier: tier, Duration: time.Since(start), Output: out, Err: err}
	if err != nil {
		return res
	}
	trimmed := strings.TrimSpace(out)
	if code != 0 || trimmed != "" {
		res.OK = false
		if trimmed != "" {
			res.Output = "unformatted files:\n" + trimmed
		}
		return res
	}
	res.OK = true
	return res
}

// runGoSimple runs `go <sub> [-tags <tags>] [-race] <scope>` in the unit
// dir and FAILS on non-zero exit. Covers vet, build, and test. race is
// only ever set for the test sub (see buildGoCheck); -race roughly
// doubles suite runtime, so it is opt-in per gate.yml.
//
// When race is enabled AND the check runs at the pre-commit tier, the
// scope is narrowed to only the CHANGED Go packages (raceScope) so the
// race detector stays off the whole-tree hot path — keeping pre-commit
// fast. If nothing Go changed, the check SKIPs. At pre-push/ci (or with
// race off) the scope is the full ./... as before.
func runGoSimple(ctx context.Context, env RunEnv, name string, tier Tier, unit Unit, sub string, race bool) Result {
	start := time.Now()
	scope, skip, err := raceScope(ctx, env, tier, unit, race, []string{"./..."})
	if err != nil {
		return Result{Name: name, Tier: tier, Duration: time.Since(start), Err: err}
	}
	if skip {
		return Result{
			Name: name, Tier: tier, Duration: time.Since(start),
			OK: true, Skipped: true, Output: noChangedGoNote,
		}
	}
	rest := []string{}
	if race {
		rest = append(rest, "-race")
	}
	rest = append(rest, scope...)
	args := append([]string{sub}, goTagArgs(unit.Tags, rest...)...)
	out, code, err := env.runner()(ctx, unitDir(env, unit), "go", args...)
	res := Result{Name: name, Tier: tier, Duration: time.Since(start), Output: out, Err: err}
	res.OK = err == nil && code == 0
	return res
}

// noChangedGoNote is the SKIP explanation a pre-commit race run emits
// when git reports no changed Go packages.
const noChangedGoNote = "no changed Go packages — skipping race run (pre-commit scope)"

// raceScope decides the package scope for a possibly-race check. When
// race is enabled and the tier is pre-commit, it resolves the CHANGED Go
// packages (fast scope) and reports skip=true when none changed;
// otherwise it returns defaultScope unchanged (full-tree ./... or the
// coverage-configured scope). This is the single place the "scope race
// to what changed on pre-commit" rule lives, shared by the test and
// coverage checks.
func raceScope(ctx context.Context, env RunEnv, tier Tier, unit Unit, race bool, defaultScope []string) (scope []string, skip bool, err error) {
	if !race || tier != TierPreCommit {
		return defaultScope, false, nil
	}
	pkgs, err := changedGoPackages(ctx, env, unit)
	if err != nil {
		return nil, false, err
	}
	if len(pkgs) == 0 {
		return nil, true, nil
	}
	return pkgs, false, nil
}

// runLint locates golangci-lint (PATH → GOPATH/bin → ~/go/bin), runs
// `golangci-lint config verify` (bug 1315 guard), then `golangci-lint run
// ./...`. If the binary is absent it SKIPs with a warning rather than
// hard-failing, mirroring scripts/precommit.sh.
func runLint(ctx context.Context, env RunEnv, name string, tier Tier, unit Unit) Result {
	start := time.Now()
	bin, found := env.lookup("golangci-lint")
	if !found {
		return Result{
			Name: name, Tier: tier, Duration: time.Since(start),
			OK: true, Skipped: true,
			Output: "golangci-lint not installed — skipping (install: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest)",
		}
	}
	dir := unitDir(env, unit)
	// config verify FIRST (bug 1315): a v2-syntax key in a v1.62 binary
	// silently falls back to flag-every-identifier behavior, so verify
	// fails loudly on the actionable config error rather than the
	// misleading downstream `run` output.
	vOut, vCode, vErr := env.runner()(ctx, dir, bin, "config", "verify")
	if vErr != nil {
		return Result{Name: name, Tier: tier, Duration: time.Since(start), Output: vOut, Err: vErr}
	}
	if vCode != 0 {
		return Result{
			Name: name, Tier: tier, Duration: time.Since(start),
			OK: false, Output: "golangci-lint config verify failed:\n" + vOut,
		}
	}
	out, code, err := env.runner()(ctx, dir, bin, "run", "./...")
	res := Result{Name: name, Tier: tier, Duration: time.Since(start), Output: out, Err: err}
	res.OK = err == nil && code == 0
	return res
}

// runCoverage runs `go test [-tags] [-race] -coverprofile=<tmp> <scope>`,
// then `go tool cover -func=<tmp>`, parses the `total:` percentage, and
// FAILS if it is below the configured floor. The temp profile is
// cleaned up before return. When cc.Race is set, `-race` is added to the
// SAME run — Go supports `-race -coverprofile` together, so one pass does
// both race detection and coverage. -race roughly doubles suite runtime,
// so it is opt-in per gate.yml.
func runCoverage(ctx context.Context, env RunEnv, name string, tier Tier, unit Unit, cc CheckConfig) (res Result) {
	start := time.Now()
	res = Result{Name: name, Tier: tier}
	// Named return so this deferred Duration stamp reaches the RETURNED
	// value (an unnamed return copies res before the defer mutates it).
	defer func() { res.Duration = time.Since(start) }()

	scope := cc.Scope
	if scope == "" {
		scope = "./..."
	}
	// A pre-commit race run scopes coverage to only the changed packages
	// (keeps pre-commit fast); at pre-push/ci it uses the configured
	// scope. If race is set on pre-commit and nothing Go changed, SKIP.
	scopePkgs, skip, serr := raceScope(ctx, env, tier, unit, cc.Race, []string{scope})
	if serr != nil {
		res.Err = serr
		return res
	}
	if skip {
		res.OK = true
		res.Skipped = true
		res.Output = noChangedGoNote
		return res
	}

	tmp, err := os.CreateTemp("", "corpos-gate-cover-*.out")
	if err != nil {
		res.Err = fmt.Errorf("create coverage temp file: %w", err)
		return res
	}
	profile := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(profile) }()

	dir := unitDir(env, unit)
	rest := []string{}
	if cc.Race {
		rest = append(rest, "-race")
	}
	rest = append(rest, "-coverprofile="+profile)
	rest = append(rest, scopePkgs...)
	testArgs := append([]string{"test"}, goTagArgs(unit.Tags, rest...)...)
	out, code, err := env.runner()(ctx, dir, "go", testArgs...)
	if err != nil {
		res.Err = err
		res.Output = out
		return res
	}
	if code != 0 {
		res.OK = false
		res.Output = "coverage test run failed:\n" + out
		return res
	}

	funcOut, code, err := env.runner()(ctx, dir, "go", "tool", "cover", "-func="+profile)
	if err != nil {
		res.Err = err
		res.Output = funcOut
		return res
	}
	if code != 0 {
		res.OK = false
		res.Output = "go tool cover failed:\n" + funcOut
		return res
	}

	measured, ok := parseCoverageTotal(funcOut)
	if !ok {
		res.OK = false
		res.Output = "could not parse coverage total from:\n" + funcOut
		return res
	}
	floor := float64(cc.Floor)
	if measured >= floor {
		res.OK = true
		res.Output = fmt.Sprintf("coverage %.1f%% >= %g%% floor", measured, floor)
	} else {
		res.OK = false
		res.Output = fmt.Sprintf("coverage %.1f%% < %g%% floor", measured, floor)
	}
	return res
}

// parseCoverageTotal extracts the aggregate percentage from the last
// `total:` line of `go tool cover -func` output. Returns (pct, true) on
// success. Uses Fields (no awk) to grab the trailing `NN.N%` token.
func parseCoverageTotal(out string) (float64, bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "total:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		tok := strings.TrimSuffix(fields[len(fields)-1], "%")
		if v, err := strconv.ParseFloat(tok, 64); err == nil {
			return v, true
		}
	}
	return 0, false
}

// runVuln runs `go mod verify` then govulncheck over the unit. FAILS if
// either command exits non-zero.
//
// By default govulncheck runs via `go tool govulncheck` (matching
// go/Makefile's `vuln` target), which requires govulncheck to be a go.mod
// tool directive. When cc.GovulncheckVersion is set, it instead runs the
// pinned module directly via `go run golang.org/x/vuln/cmd/govulncheck@<v>`
// — the path for repos that keep go.mod dependency-free (no tool directive),
// so the scan needs no go.mod entry.
func runVuln(ctx context.Context, env RunEnv, name string, tier Tier, unit Unit, cc CheckConfig) (res Result) {
	start := time.Now()
	res = Result{Name: name, Tier: tier}
	// Named return so this deferred Duration stamp reaches the RETURNED
	// value (an unnamed return copies res before the defer mutates it).
	defer func() { res.Duration = time.Since(start) }()
	dir := unitDir(env, unit)

	verifyOut, code, err := env.runner()(ctx, dir, "go", "mod", "verify")
	if err != nil {
		res.Err = err
		res.Output = verifyOut
		return res
	}
	if code != 0 {
		res.OK = false
		res.Output = "go mod verify failed:\n" + verifyOut
		return res
	}

	var vulnArgs []string
	if cc.GovulncheckVersion != "" {
		// Dependency-free path: run the pinned module directly, no go.mod
		// tool directive required.
		vulnArgs = append([]string{"run", "golang.org/x/vuln/cmd/govulncheck@" + cc.GovulncheckVersion}, goTagArgs(unit.Tags, "./...")...)
	} else {
		vulnArgs = append([]string{"tool", "govulncheck"}, goTagArgs(unit.Tags, "./...")...)
	}
	vulnOut, code, err := env.runner()(ctx, dir, "go", vulnArgs...)
	res.Output = verifyOut + vulnOut
	if err != nil {
		res.Err = err
		return res
	}
	res.OK = code == 0
	return res
}

// mutationDefaultScope is the package go-mutesting runs against when the
// coverage `scope` isn't repurposed for mutation. It mirrors the repo's
// `make -C go mutest` default (PKG=internal/refresolve).
const mutationDefaultScope = "internal/refresolve"

// runMutation is REPORT-ONLY: it runs go-mutesting for signal but NEVER
// fails the gate. Whatever the tool reports — surviving mutants, a
// non-zero exit, even a run that couldn't start — the check returns
// Result{OK: true}. Mutation testing surfaces test-net gaps to a human;
// it is not a pass/fail contract.
//
// go-mutesting has no native build-tag support and this module needs
// `-tags sqlite_fts5`, so the run is driven through
// scripts/go-mutesting-exec.sh (the avito default exec wrapper + that one
// tag), exactly like `make -C go mutest`. The tool mutates files IN PLACE
// under the unit dir and re-runs the suite once per mutant, so it is slow
// and on-demand: keep it `enabled: false` in gate.yml and turn it on
// deliberately. If the binary is absent the check SKIPs (like runLint).
func runMutation(ctx context.Context, env RunEnv, name string, tier Tier, unit Unit, cc CheckConfig) Result {
	start := time.Now()
	bin, found := env.lookup("go-mutesting")
	if !found {
		return Result{
			Name: name, Tier: tier, Duration: time.Since(start),
			OK: true, Skipped: true,
			Output: "go-mutesting not installed — skipping mutation report (install: make -C go mutest-install)",
		}
	}

	scope := cc.Scope
	if scope == "" {
		scope = mutationDefaultScope
	}
	execScript := filepath.Join(env.RepoRoot, "scripts", "go-mutesting-exec.sh")
	args := []string{"--exec=" + execScript, "--exec-timeout=180", scope}
	out, code, err := env.runner()(ctx, unitDir(env, unit), bin, args...)

	res := Result{
		Name: name, Tier: tier, Duration: time.Since(start),
		OK: true, // report-only: NEVER fails the gate.
	}
	switch {
	case err != nil:
		// Couldn't even start the tool — capture it, but still OK.
		res.Output = fmt.Sprintf("mutation report (could not run go-mutesting, reporting only): %v\n%s", err, out)
	case code != 0:
		// go-mutesting exits non-zero when mutants survive — that's
		// signal for a human, NOT a gate failure.
		res.Output = fmt.Sprintf("mutation report (go-mutesting exit %d — report-only, surviving mutants are advisory):\n%s", code, out)
	default:
		res.Output = "mutation report (go-mutesting: no surviving mutants):\n" + out
	}
	return res
}
