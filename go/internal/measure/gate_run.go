package measure

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/gate"
)

// gate_run.go implements the measure surface's gate_run + gate_trend actions.
//
// gate_run REUSES the corpos-gate CLI's core (internal/gate): it loads the
// repo's gate.yml (gate.Load), runs gate.Run over the real gate.OSRunner, and
// maps the []gate.Result verdict into a GateRunCompleted event. Because it
// drives the SAME gate.Run over the SAME gate.yml with the SAME default
// RunEnv (RepoRoot only; no injected Runner) and skip=nil, the verdict it
// returns is IDENTICAL to `corpos-gate run --tier=<tier>` on the same
// repo/commit — the check logic is not forked. See go/cmd/corpos-gate/main.go
// (cmdRun) for the CLI's matching call shape.
//
// The run is then persisted as EVENT-SOURCED trend data: one GateRunCompleted
// event whose fold writes proj_gate_runs + proj_gate_check_results (see
// go/internal/db/migrations/089_proj_gate_runs.sql and
// go/internal/projections/gate_runs.go). Persistence is ADDITIVE and
// DB-OPTIONAL: if no pool is configured, no project is supplied, or the emit
// fails, the action STILL returns the verdict (with persisted=false and a
// note) — a gate run must work with storage unavailable (CI).
//
// gate_trend is the read path: it queries proj_gate_runs for a project and
// returns the coverage / mutation / verdict time series.

// defaultGateTier is the tier gate_run uses when the caller omits `tier`.
// pre-push is the pre-integration gate (a superset of pre-commit); it is the
// tier a trend row most usefully snapshots.
const defaultGateTier = "pre-push"

// GateRunDeps holds dependencies for the gate_run / gate_trend handlers. Pool
// may be nil (DB-unavailable / CI mode) — gate_run then skips persistence and
// still returns the verdict; gate_trend returns an empty series. Runner is the
// gate-core seam: nil → runGateReal (the production internal/gate core over
// gate.yml). Tests inject a stub to exercise the emit+persist+verdict path
// without running a real multi-minute suite.
type GateRunDeps struct {
	Pool   *db.Pool
	Runner GateRunner
}

// GateRunOutcome is the raw verdict of running the gate core: the per-check
// results plus the aggregate ok. It is exactly what gate.Run returns, hoisted
// into a struct so the handler's payload-mapping is testable with a stubbed
// Runner.
type GateRunOutcome struct {
	Results   []gate.Result
	OverallOK bool
}

// GateRunner runs the gate core for a repo dir + tier and returns the raw
// outcome. runGateReal is the production implementation; tests substitute a
// stub via GateRunDeps.Runner.
type GateRunner func(ctx context.Context, repoDir string, tier gate.Tier) (GateRunOutcome, error)

// gateRunParams is the typed gate_run request body. The action_doc registry
// reflects it (reflect.TypeOf(gateRunParams{})) so each param's type derives:
// repo_dir/tier/project/commit_sha → string (optional strings render as
// optional_string in the corpus).
type gateRunParams struct {
	RepoDir   string `json:"repo_dir"`
	Tier      string `json:"tier,omitempty"`
	Project   string `json:"project,omitempty"`
	CommitSHA string `json:"commit_sha,omitempty"`
}

// GateCheckOutcome is one check's outcome in the gate_run response. Mirrors
// the persisted proj_gate_check_results row / the event's checks[] item.
type GateCheckOutcome struct {
	Name       string `json:"name"`
	Tier       string `json:"tier"`
	OK         bool   `json:"ok"`
	Skipped    bool   `json:"skipped"`
	DurationMS int    `json:"duration_ms"`
	Note       string `json:"note,omitempty"`
}

// GateRunResult is the gate_run response. It ALWAYS carries the verdict
// (overall_ok + checks + parsed metrics) whether or not persistence happened.
// Persisted reports whether a trend row was written; PersistNote explains a
// skip/failure. Error is set only on a param error or a gate-core infra
// failure (gate.yml missing, a check that could not run) — a check that simply
// FAILS is a verdict (overall_ok=false), not an Error.
type GateRunResult struct {
	OverallOK     bool               `json:"overall_ok"`
	Tier          string             `json:"tier,omitempty"`
	Project       string             `json:"project,omitempty"`
	CommitSHA     string             `json:"commit_sha,omitempty"`
	CoveragePct   float64            `json:"coverage_pct"`
	BranchPct     float64            `json:"branch_pct"`
	MutationScore float64            `json:"mutation_score"`
	DurationMS    int                `json:"duration_ms"`
	Checks        []GateCheckOutcome `json:"checks,omitempty"`
	Persisted     bool               `json:"persisted"`
	PersistNote   string             `json:"persist_note,omitempty"`
	Error         string             `json:"error,omitempty"`
}

// noteCap bounds the per-check note length persisted into the trend row — the
// full check output (a failed suite, a govulncheck dump) can be large, but the
// trend only needs a short explanatory snippet.
const noteCap = 1000

// HandleGateRun implements the measure.gate_run action. It runs corpos-gate
// via the reused core, maps the verdict into a GateRunCompleted event
// (persisted when a pool + project are available), and returns the aggregated
// verdict. project comes from params (falling back to the dispatch envelope
// project).
func HandleGateRun(ctx context.Context, deps GateRunDeps, project string, params json.RawMessage) (GateRunResult, error) {
	var in gateRunParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &in); err != nil {
			return GateRunResult{Error: fmt.Sprintf("params: %s", err.Error())}, nil
		}
	}
	if strings.TrimSpace(in.RepoDir) == "" {
		return GateRunResult{Error: "missing required params: params.repo_dir"}, nil
	}

	tierStr := in.Tier
	if tierStr == "" {
		tierStr = defaultGateTier
	}
	tier, err := gate.ParseTier(tierStr)
	if err != nil {
		return GateRunResult{Error: fmt.Sprintf("invalid tier %q: %s", tierStr, err.Error())}, nil
	}

	proj := in.Project
	if proj == "" {
		proj = project
	}

	repoDir, err := filepath.Abs(in.RepoDir)
	if err != nil {
		return GateRunResult{Error: fmt.Sprintf("resolve repo_dir: %s", err.Error())}, nil
	}

	runner := deps.Runner
	if runner == nil {
		runner = runGateReal
	}
	outcome, runErr := runner(ctx, repoDir, tier)

	// Map the raw gate.Result verdict into the response + event-payload shapes.
	checks := make([]GateCheckOutcome, 0, len(outcome.Results))
	durationMS := 0
	for _, r := range outcome.Results {
		ms := int(r.Duration.Milliseconds())
		durationMS += ms
		checks = append(checks, GateCheckOutcome{
			Name:       r.Name,
			Tier:       r.Tier.String(),
			OK:         r.OK,
			Skipped:    r.Skipped,
			DurationMS: ms,
			Note:       capNote(r.Output),
		})
	}
	coverage, branch, mutation := extractGateMetrics(outcome.Results)

	res := GateRunResult{
		OverallOK:     outcome.OverallOK,
		Tier:          tier.String(),
		Project:       proj,
		CommitSHA:     in.CommitSHA,
		CoveragePct:   coverage,
		BranchPct:     branch,
		MutationScore: mutation,
		DurationMS:    durationMS,
		Checks:        checks,
	}

	// A gate-core infra failure (gate.yml missing, a check that could not run)
	// is a hard error, not a verdict: surface it and do NOT persist a row.
	if runErr != nil {
		res.Error = runErr.Error()
		res.PersistNote = "gate core failed to run; trend not recorded"
		return res, nil
	}

	// Persist the trend row. ADDITIVE / DB-OPTIONAL: any reason we can't
	// persist leaves the verdict intact with persisted=false + a note.
	if deps.Pool == nil {
		res.PersistNote = "no DB pool configured; trend not recorded"
		return res, nil
	}
	if proj == "" {
		res.PersistNote = "no project supplied; trend not recorded (pass params.project)"
		return res, nil
	}

	payload := events.GateRunCompletedPayload{
		Project:       proj,
		CommitSHA:     in.CommitSHA,
		Tier:          tier.String(),
		OverallOK:     outcome.OverallOK,
		CoveragePct:   coverage,
		BranchPct:     branch,
		MutationScore: mutation,
		DurationMS:    durationMS,
		Checks:        toPayloadChecks(checks),
	}
	if err := deps.Pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, emitErr := events.Emit(ctx, tx, events.EmitArgs{
			Entity:  events.NewCrossCuttingEntityRef("gate_run", newUUIDv4()),
			Payload: payload,
		})
		return emitErr
	}); err != nil {
		res.PersistNote = "persist failed (verdict still returned): " + err.Error()
		return res, nil
	}
	res.Persisted = true
	return res, nil
}

// toPayloadChecks converts the response checks into the event payload's check
// shape (identical fields; a distinct type keeps the events package free of a
// measure import).
func toPayloadChecks(checks []GateCheckOutcome) []events.GateCheckResult {
	out := make([]events.GateCheckResult, 0, len(checks))
	for _, c := range checks {
		out = append(out, events.GateCheckResult{
			Name:       c.Name,
			Tier:       c.Tier,
			OK:         c.OK,
			Skipped:    c.Skipped,
			DurationMS: c.DurationMS,
			Note:       c.Note,
		})
	}
	return out
}

// runGateReal is the production GateRunner: it loads the repo's gate.yml and
// runs the gate core with the same default RunEnv the CLI uses (RepoRoot only,
// no injected Runner → real gate.OSRunner) and skip=nil, so the verdict is
// identical to `corpos-gate run`. Progress lines are discarded — they do not
// affect the verdict.
func runGateReal(ctx context.Context, repoDir string, tier gate.Tier) (GateRunOutcome, error) {
	cfg, err := gate.Load(filepath.Join(repoDir, "gate.yml"))
	if err != nil {
		return GateRunOutcome{}, err
	}
	env := gate.RunEnv{RepoRoot: repoDir, Out: io.Discard}
	results, ok, err := gate.Run(ctx, cfg, tier, env, nil)
	if err != nil {
		return GateRunOutcome{Results: results}, err
	}
	return GateRunOutcome{Results: results, OverallOK: ok}, nil
}

var (
	coverageRE = regexp.MustCompile(`coverage ([0-9]+(?:\.[0-9]+)?)%`)
	mutationRE = regexp.MustCompile(`mutation score is ([0-9]+(?:\.[0-9]+)?)`)
)

// extractGateMetrics parses the coverage percentage and mutation score out of
// their checks' summary output. Returns -1 for any metric whose check did not
// run, was skipped, or produced no parseable value. branch_pct is always -1:
// Go's cover tool reports statement coverage only.
func extractGateMetrics(results []gate.Result) (coverage, branch, mutation float64) {
	coverage, branch, mutation = -1, -1, -1
	for _, r := range results {
		if r.Skipped {
			continue
		}
		switch r.Name {
		case "coverage":
			if v, ok := parseFirstFloat(coverageRE, r.Output); ok {
				coverage = v
			}
		case "mutation":
			if v, ok := parseFirstFloat(mutationRE, r.Output); ok {
				mutation = v
			}
		}
	}
	return coverage, branch, mutation
}

// parseFirstFloat returns the first capture group of re in s parsed as a
// float, or (0, false) when there is no match or it does not parse.
func parseFirstFloat(re *regexp.Regexp, s string) (float64, bool) {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// capNote trims a check's output to a short trend-friendly snippet.
func capNote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= noteCap {
		return s
	}
	return s[:noteCap] + "…"
}

// ── gate_trend read path ─────────────────────────────────────────────────

// gateTrendParams is the typed gate_trend request body (reflected in the
// action_doc registry). project → string; metric → optional_string; limit →
// int.
type gateTrendParams struct {
	Project string `json:"project"`
	Metric  string `json:"metric,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

// GateTrendPoint is one run in a project's gate trend. Most-recent runs first
// (ran_at DESC).
type GateTrendPoint struct {
	RanAt         string  `json:"ran_at"`
	CommitSHA     string  `json:"commit_sha"`
	Tier          string  `json:"tier"`
	OverallOK     bool    `json:"overall_ok"`
	CoveragePct   float64 `json:"coverage_pct"`
	MutationScore float64 `json:"mutation_score"`
}

// GateTrendResult is the gate_trend response: the ordered series plus the echo
// of the query. Error is set on a param error.
type GateTrendResult struct {
	Project string           `json:"project"`
	Metric  string           `json:"metric,omitempty"`
	Points  []GateTrendPoint `json:"points"`
	Error   string           `json:"error,omitempty"`
}

// HandleGateTrend implements the measure.gate_trend action — the read path over
// proj_gate_runs. It returns the coverage/mutation/verdict time series for a
// project, most-recent first. The optional `metric` filter narrows to rows that
// actually carry that metric (coverage / mutation), or 'verdict' for the full
// series. project comes from params (falling back to the dispatch envelope).
func HandleGateTrend(ctx context.Context, deps GateRunDeps, project string, params json.RawMessage) (GateTrendResult, error) {
	var in gateTrendParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &in); err != nil {
			return GateTrendResult{Error: fmt.Sprintf("params: %s", err.Error())}, nil
		}
	}
	proj := in.Project
	if proj == "" {
		proj = project
	}
	if proj == "" {
		return GateTrendResult{Error: "missing required params: params.project"}, nil
	}
	switch in.Metric {
	case "", "coverage", "mutation", "verdict":
	default:
		return GateTrendResult{Error: fmt.Sprintf("invalid metric %q: want coverage, mutation, or verdict", in.Metric)}, nil
	}

	result := GateTrendResult{Project: proj, Metric: in.Metric, Points: []GateTrendPoint{}}
	if deps.Pool == nil {
		return result, nil
	}

	var sb strings.Builder
	sb.WriteString(`SELECT ran_at, commit_sha, tier, overall_ok, coverage_pct, mutation_score
	                FROM proj_gate_runs WHERE project_id = ?`)
	binds := db.NewArgs()
	binds.AddString(proj)
	switch in.Metric {
	case "coverage":
		sb.WriteString(" AND coverage_pct >= 0")
	case "mutation":
		sb.WriteString(" AND mutation_score >= 0")
	}
	sb.WriteString(" ORDER BY ran_at DESC, id DESC")
	if in.Limit > 0 {
		fmt.Fprintf(&sb, " LIMIT %d", in.Limit)
	}

	rows, err := deps.Pool.DB().QueryContext(ctx, sb.String(), binds.Slice()...)
	if err != nil {
		return GateTrendResult{}, fmt.Errorf("query gate trend: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var pt GateTrendPoint
		var overallOK int
		if err := rows.Scan(&pt.RanAt, &pt.CommitSHA, &pt.Tier, &overallOK, &pt.CoveragePct, &pt.MutationScore); err != nil {
			return GateTrendResult{}, fmt.Errorf("scan gate trend row: %w", err)
		}
		pt.OverallOK = overallOK != 0
		result.Points = append(result.Points, pt)
	}
	if err := rows.Err(); err != nil {
		return GateTrendResult{}, fmt.Errorf("iterate gate trend rows: %w", err)
	}
	return result, nil
}
