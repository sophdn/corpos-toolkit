# corpos-gate — reusable, stack-agnostic gate orchestrator

Chain `corpos-gate-testing-module`. This document is the deliverable for
task 1 (characterization + superset matrix) and the design doc for the
first increment (tasks 2–3: config schema + core, plus the Go adapter).

**Status: additive.** `scripts/precommit.sh` remains the authoritative
gate for `corpos-toolkit`. `corpos-gate` runs the same logic in parallel
so it can be characterized and hardened before any cutover. Nothing in
`scripts/precommit.sh` or `.git-hooks/` is modified by this increment.

---

## Part A — Characterization: the four existing gates

Four repositories each grew a bespoke gate. corpos-gate exists to
replace them with one orchestrator + per-stack adapters. The matrix
below is the union of what those gates do — the superset corpos-gate
must eventually cover.

| Repo | Stack | Format | Vet/type | Lint | Build | Test | Coverage | Vuln | Custom guards |
|------|-------|--------|----------|------|-------|------|----------|------|---------------|
| **corpos** | Go | gofmt | go vet | golangci-lint | go build | go test **-race** | **95%** over `./internal/...` | govulncheck | podman smoke |
| **corpos-toolkit** (this repo) | Go | gofmt | go vet | golangci-lint | go build-all | go test | **66%** over `./...` | go mod verify + govulncheck | **~15** inline guards (see below) + chain replay |
| **dashboard** | TS | — | tsc | eslint | (vite) | vitest | coverage **ratchet** | — | gen-types freshness, CSS-drift |
| **llama-server** | Shell | — | — | shellcheck | — | — | — | — | validate-unit golden-baseline |

### corpos-toolkit's ~15 inline guards (from `scripts/precommit.sh`)

These are the repo-specific checks the current gate runs inline. Most
are grep-based forbidden-pattern guards; three are already separate
invokable scripts (marked ✅ — those are the ones this increment wires
as `custom` checks to prove the escape hatch):

- CONVENTIONS.md top-level directory reference integrity
- retired CRUD table reference ban (post-migration-060)
- `ORDER BY event_id` ban (chronology must use `ts`)
- archived `toolkit/internal/forge` bare-import ban
- `log.Printf`/`fmt.Println` ban in agent-primary packages
- quadlet deployment-artifact integrity — `scripts/validate-quadlet-units.sh` ✅
- migrations canonical→testutil-mirror sync (mutating)
- event-schemas canonical→embed-mirror sync (mutating)
- runtime-affecting-paths manifest parity — `scripts/test-runtime-affecting-paths-parity.sh` ✅
- cmd-binary gitignore parity — `scripts/test-cmd-gitignore-parity.sh` ✅
- cutover global MCP-config coverage — `scripts/test-cutover-global-mcp-coverage.sh`
- gradient-question-guard regression — `hooks/test-gradient-question-guard.sh`
- action-docs corpus no-diff
- CODEMAP regen + doc.go four-field lint
- chain forge byte-identity replay

The two categories that don't fit the "custom command" shape yet are the
**mutating sync** stages (they rewrite the tree and re-stage) and the
**staged-blob** gofmt/quote-normalization stage. Those stay in
`precommit.sh` until a later increment models them.

---

## Part B — corpos-gate design

### gate.yml schema

```yaml
stack: go                     # go | ts | shell  (this increment: go adapter only)
units:
  - dir: go                   # module dir relative to repo root
    tags: sqlite_fts5         # build tags (optional)
checks:
  format:   { enabled: true,  tier: pre-commit }
  vet:      { enabled: true,  tier: pre-commit }
  lint:     { enabled: true,  tier: pre-commit }
  build:    { enabled: true,  tier: pre-commit }
  test:     { enabled: true,  tier: pre-push }
  coverage: { enabled: true,  tier: pre-push, floor: 66, scope: "./..." }
  vuln:     { enabled: true,  tier: pre-push }
  mutation: { enabled: false, tier: ci }         # report-only; slowest (ci) tier
custom:
  - { name: quadlet-directives, cmd: "bash scripts/validate-quadlet-units.sh", tier: pre-commit }
```

Parsing is **strict** (`yaml/v3` `KnownFields(true)`): an unknown key is
an error. `Load` also validates a known `stack`, at least one `unit`,
known check names, and a valid tier on every check and custom entry.

### Tier model

**Three** tiers with a strict **superset** relation — `ci ⊃ pre-push ⊃
pre-commit`:

- `pre-commit` — the **fast** gate (format, vet, lint, build, custom
  guards). Target: keep it well under **~20s** so it never discourages
  committing. A race-enabled check on this tier scopes to only the
  **changed** packages (see below) to stay fast.
- `pre-push` — a **superset**: every pre-commit check **plus** the
  pre-push-tiered checks (the full test suite, coverage floor, vuln scan).
- `ci` — the slowest gate: everything in `pre-push` **plus** the ci-only
  checks too slow even for a pre-push (e.g. **mutation** testing, which
  re-runs the suite once per mutant). Wire `corpos-gate run --tier=ci`
  into a CI job; nothing runs a ci-tier check on `pre-commit`/`pre-push`.

`Tier.Includes(check)` is `run >= check` (iota order `pre-commit=0 <
pre-push=1 < ci=2`), so a `ci` run selects everything, a `pre-push` run
selects pre-commit + pre-push, and a `pre-commit` run selects only
pre-commit checks. Proof from this repo's `gate.yml` (with `mutation`
enabled to make the ci-only tier visible):

```
$ corpos-gate plan --tier=pre-push        $ corpos-gate plan --tier=ci
  1. format          [pre-commit]           1. format          [pre-commit]
  2. vet             [pre-commit]           2. vet             [pre-commit]
  3. lint            [pre-commit]           3. lint            [pre-commit]
  4. build           [pre-commit]           4. build           [pre-commit]
  5. coverage        [pre-push]             5. coverage        [pre-push]
  6. vuln            [pre-push]             6. vuln            [pre-push]
  7. quadlet-directives    [pre-commit]     7. mutation        [ci]   ← ci-only tier adds
  8. cmd-gitignore-parity  [pre-commit]     8. quadlet-directives    [pre-commit]
  9. runtime-affecting-... [pre-commit]     9. cmd-gitignore-parity  [pre-commit]
                                           10. runtime-affecting-... [pre-commit]
```

`mutation` appears **only** at `ci`, never at `pre-push`. In this repo's
committed `gate.yml`, `mutation` is `enabled: false` (feature available,
run on demand), so it's omitted from every live plan until enabled.

### Adapter command mapping (go stack)

Each check runs in the unit's `dir`; `<tags>` is the unit's build tags.

| Check | Command(s) | Fail condition |
|-------|-----------|----------------|
| format | `gofmt -l .` | stdout non-empty (unformatted files listed) |
| vet | `go vet -tags <tags> ./...` | non-zero exit |
| lint | `golangci-lint run ./...` | non-zero exit; **SKIP** (warn) if binary absent |
| build | `go build -tags <tags> ./...` | non-zero exit |
| test | `go test -tags <tags> [-race] ./...` | non-zero exit |
| coverage | `go test -tags <tags> [-race] -coverprofile=<tmp> <scope>` then `go tool cover -func=<tmp>` | parsed `total:` % below `floor` |
| vuln | `go mod verify` then `go tool govulncheck -tags <tags> ./...` — OR, when `govulncheck_version:` is set on the check, `go run golang.org/x/vuln/cmd/govulncheck@<version> -tags <tags> ./...` | either non-zero (matches `go/Makefile` `vuln`) |

`go tool govulncheck` requires govulncheck to be a `go.mod` tool directive.
For repos that keep `go.mod` dependency-free (dev tools run via `go run
<tool>@<ver>`), set `govulncheck_version:` on the vuln check — it runs the
pinned module directly via `go run …@<version>`, needing no `go.mod` entry:

```yaml
checks:
  vuln: { enabled: true, tier: pre-push, govulncheck_version: v1.3.0 }
```
| mutation | `go-mutesting --exec=scripts/go-mutesting-exec.sh --exec-timeout=180 <scope>` in the unit dir | **never fails** (report-only); **SKIP** if binary absent |
| custom | `sh -c "<cmd>"` in repo root | non-zero exit |

`-race` is opt-in per check via `race: true` on the `coverage` (or `test`)
check block. It roughly **doubles** suite runtime (the detector
instruments every memory access), so it is off by default. On the
`coverage` check Go runs `-race -coverprofile` in a **single pass**, so
enabling it adds race detection without a second suite run. This repo's
`gate.yml` leaves `race` unset — enabling it is a separate decision after
a race-clean verification.

#### Changed-package race scoping (keeps pre-commit fast)

When a **race-enabled** check runs at the **pre-commit** tier, `-race` is
scoped to only the **changed** Go packages instead of `./...`. The changed
set comes from `git diff --name-only HEAD` + `git diff --name-only
--cached` (unstaged + staged): the `*.go` files under the unit are mapped
to their `./…`-relative package patterns, deduped. If **nothing** Go
changed, the check **SKIPs** with a "no changed Go packages" note. At
`pre-push`/`ci`, a race check runs the full `./...` as before. This keeps
the expensive race detector off the whole-tree hot path on every commit
while still exercising it over what you actually touched.

### Go adapter check catalog

The chain's completion condition enumerates a catalog of gate concerns.
Here is the honest status of each for the **Go adapter**:

| Catalog item | Status | Notes |
|--------------|--------|-------|
| coverage floor | ✅ | `go test -coverprofile` + `go tool cover -func`, floor from gate.yml |
| static suite (vet + golangci) | ✅ | `go vet` and `golangci-lint run` |
| race | ✅ (opt-in) | `-race` on the coverage/test check; off by default (~2× runtime) |
| mutation-as-report | ✅ (report-only) | drives `go-mutesting` via `scripts/go-mutesting-exec.sh`; **never fails the gate** — surviving mutants are advisory signal for a human |
| **branch-cov** | **N/A for the Go adapter** | Go's cover tooling reports **statement** coverage only — there is no branch/condition coverage in the standard `go test`/`go tool cover` pipeline. Branch coverage is a **TypeScript-adapter concern** (vitest reports it), deferred to task 7. We do not fake a branch-cov number for Go. |
| **goleak** | **Out of adapter scope** | goleak is a **test-authoring convention** — you add `goleak.VerifyTestMain(m)` to a package's `TestMain`, and it fails that package's own test run if a goroutine leaks. It is **not a gate command an adapter can invoke**; enforcing that packages adopt it is a separate test-convention task, not a corpos-gate check. Where goleak IS wired into a `TestMain`, the adapter already exercises it transitively through the normal `test`/`coverage` run. |

The two "not applicable" rows are deliberate: branch-cov and goleak are
documented as out-of-scope **with their reason**, rather than papered over
with a stub check that would falsely report green.

`golangci-lint` is located the way `scripts/precommit.sh` does: PATH,
then `$GOBIN`, then `$(go env GOPATH)/bin`, then `~/go/bin`.

### TypeScript adapter (`stack: ts`)

The ts adapter (`internal/gate/adapter_ts.go`, `gate.TSChecks(cfg)`)
mirrors the go adapter's shape: one `Check` per enabled ts-stack check,
run in the unit's `dir` via the injected `Runner`, tier-respecting, built
in the stable `tsCheckOrder` `[static, lint, build, coverage, mutation,
race, goleak]` (fast→slow). Every tool is invoked through `npx
--no-install <tool>` so a **missing** tool FAILS loudly instead of
auto-installing — mirroring the dashboard's `precommit.sh`.

#### Command mapping (ts stack)

| Check | Command(s) | Fail condition |
|-------|-----------|----------------|
| static (typecheck) | `npx --no-install tsc -p <cfg> --noEmit` per `unit.tsconfig` entry, or a single `npx --no-install tsc --noEmit` when none configured | any non-zero exit (names the failing tsconfig) |
| lint | `npx --no-install eslint .` | non-zero exit |
| build | `npm run build` | non-zero exit (optional; enable per gate.yml) |
| coverage | vitest (default): `npx --no-install vitest run --coverage --coverage.reporter=json-summary --coverage.reportsDirectory=<tmp>`  •  jest: `npx --no-install jest --coverage --coverageReporters=json-summary --coverageDirectory=<tmp>` — then read `<tmp>/coverage-summary.json` | any configured metric below its threshold |
| mutation | `npx --no-install stryker run` | **never fails** (report-only); **SKIP** if stryker not resolvable |
| race / goleak | — | **SKIP** with reason "not applicable to the TypeScript stack" |
| custom | `sh -c "<cmd>"` in repo root | non-zero exit (stack-agnostic) |

#### Four-metric coverage thresholds

The ts coverage check gates on the **four** istanbul metrics vitest and
jest both report — `statements`, `branches`, `functions`, `lines` — via a
per-metric `thresholds:` map on the coverage check (NOT the scalar
`floor:` the Go adapter uses). It runs the configured runner with a
json-summary reporter pointed at a temp dir, reads
`<tmp>/total.{statements,branches,functions,lines}.pct` from
`coverage-summary.json`, and FAILS if **any** configured metric is below
its threshold (the verdict names which). A metric with **no** threshold
entry is **not** gated. Unknown metric keys in `thresholds:` are rejected
at `Load` time. A missing or malformed summary file is a check failure
with a clear message. The temp dir is cleaned up after the read.

**Test runner is selectable** via `runner:` on the coverage check —
`vitest` (the default when omitted) or `jest`. Both emit the identical
istanbul-shaped `coverage-summary.json`, so the parse + four-metric
compare is shared; only the command differs (vitest's
`--coverage.reporter`/`--coverage.reportsDirectory` vs jest's
`--coverageReporters`/`--coverageDirectory`). An unknown `runner:` value
is rejected at `Load` time. (Phase B: `campaign-settings` is vitest,
`dm-manager` is jest.)

#### Mutation is report-only; race/goleak skip with reason

Stryker follows the **same graduation model** as the Go adapter's
go-mutesting: the `mutation` check captures output and returns OK even on
surviving mutants, a non-zero exit, or a tool that couldn't start — it
**never fails the gate**. If stryker isn't resolvable the check SKIPs.
`race` and `goleak` are go-only concerns; if a ts unit names them they
SKIP with the reason "not applicable to the TypeScript stack" rather than
a misleading pass or a hard error.

#### Sample `stack: ts` gate.yml

```yaml
stack: ts
units:
  - dir: app
    tsconfig: [tsconfig.json, tsconfig.node.json]   # static runs tsc -p each
checks:
  static:   { enabled: true,  tier: pre-commit }    # tsc --noEmit
  lint:     { enabled: true,  tier: pre-commit }     # eslint .
  build:    { enabled: false, tier: pre-commit }     # npm run build (optional)
  coverage:
    enabled: true
    tier: pre-push
    runner: vitest                                   # vitest (default) | jest
    thresholds: { statements: 80, branches: 70, functions: 80, lines: 80 }
  mutation: { enabled: false, tier: ci }             # stryker; report-only
```

**Phase B (separate follow-up)** migrates `campaign-settings` (vitest) and
`dm-manager` (jest) onto this adapter. It is NOT part of this task and
touches neither repo.

### Python adapter (`stack: py`)

The py adapter (`internal/gate/adapter_py.go`, `gate.PyChecks(cfg)`)
mirrors the go/ts adapters' shape: one `Check` per enabled py-stack check,
run in the unit's `dir` via the injected `Runner`, tier-respecting, built
in the stable `pyCheckOrder` `[format, lint, static, coverage, mutation,
race, goleak]` (fast→slow). Each tool is resolved via `env.lookup` and a
**missing** tool SKIPs-with-reason (mirroring the go adapter's
golangci-lint handling) rather than hard-failing.

#### Command mapping (py stack)

Each check runs in the unit's `dir`.

| Check | Command(s) | Fail condition |
|-------|-----------|----------------|
| format | `ruff format --check .` | non-zero exit (a file would be reformatted — formatting drift); **SKIP** if `ruff` absent |
| lint | `ruff check .` | non-zero exit; **SKIP** if `ruff` absent |
| static | `mypy .` | non-zero exit; **SKIP** if `mypy` not resolvable (ml-training has no mypy yet, so this check typically stays disabled or skips) |
| coverage | `python -m pytest --cov=<target> [--cov=<target>...] --cov-report=json:<tmp>/coverage.json` then read `<tmp>/coverage.json` | `totals.percent_covered` below the scalar `floor`; **SKIP** if `pytest` not resolvable |
| mutation | `mutmut run` | **never fails** (report-only); **SKIP** if `mutmut` not resolvable |
| race / goleak | — | **SKIP** with reason "not applicable to the Python stack" |
| custom | `sh -c "<cmd>"` in repo root | non-zero exit (stack-agnostic) |

#### Coverage is a scalar line-coverage floor

Unlike the ts adapter's four istanbul metrics, coverage.py reports a
**single** aggregate line-coverage percentage, so the py coverage check
reuses the **scalar** `floor:` field (like the Go adapter), NOT the
per-metric `thresholds:` map (that stays ts-only). It runs pytest with
coverage.py's json reporter pointed at a temp dir, reads
`totals.percent_covered` (a float) from `coverage.json`, and FAILS if it
is below `floor` — printing measured vs floor (`coverage 85.0% >= 80%
floor`). The coverage target(s) come from the check's `scope:` (default
`.`); a `scope:` holding several whitespace- and/or comma-separated
targets emits one `--cov=X` per target. A missing or malformed json report
is a check failure with a clear message. The temp dir is cleaned up after
the read. **`pytest-cov` must be a dev dependency** for this check —
`--cov` is a pytest-cov plugin flag, not core pytest.

#### Mutation is report-only; race/goleak skip with reason

`mutmut` follows the **same graduation model** as the go/ts mutation
checks: the `mutation` check captures output and returns OK even on
surviving mutants, a non-zero exit, or a tool that couldn't start — it
**never fails the gate**. If `mutmut` isn't resolvable the check SKIPs.
`race` and `goleak` are go-only concerns; a py unit naming them SKIPs with
the reason "not applicable to the Python stack" rather than a misleading
pass or a hard error.

#### Sample `stack: py` gate.yml

```yaml
stack: py
units:
  - dir: training                                   # module dir relative to repo root
checks:
  format:   { enabled: true,  tier: pre-commit }    # ruff format --check .
  lint:     { enabled: true,  tier: pre-commit }     # ruff check .
  static:   { enabled: false, tier: pre-commit }     # mypy . (off until mypy adopted)
  coverage:
    enabled: true
    tier: pre-push
    floor: 80                                        # scalar percent_covered floor
    scope: "training export eval data"               # one --cov= per target
  mutation: { enabled: false, tier: ci }             # mutmut; report-only
```

**ml-training migration is a separate follow-up.** Applying this adapter
to `sophdn/ml-training` (ruff + pytest, packages `training/export/eval/
data`) is NOT part of this task and touches neither repo — it only builds
the adapter. That repo has no mypy and no coverage tool yet, so its
coverage check runs pytest-cov and `pytest-cov` must be added as a dev
dependency before the coverage check can measure anything.

### Architecture (sans-IO)

- **`internal/gate`** — pure orchestration. `RunEnv` injects a `Runner`
  (command execution), a tool locator, and an output sink, so every
  check's command **construction** is unit-tested with a fake `Runner`
  that records `(dir, name, args)` — no real process is spawned in the
  hermetic tests. `OSRunner` is the production `Runner`.
- **`cmd/corpos-gate`** — stdlib `flag` CLI (matches `cmd/codemap-gen`):
  `run` and `plan` subcommands, `--tier`, `--config`, `--list`.
  gate.yml is found by walking up from CWD, then `git rev-parse
  --show-toplevel`.

### CLI

```
corpos-gate run  --tier=pre-commit|pre-push|ci [--config <path>] [--skip <a,b>] [--list]
corpos-gate plan --tier=pre-commit|pre-push|ci [--config <path>] [--skip <a,b>]
corpos-gate init [dir]
```

`run` exits 0 when every selected check passes, 1 on any failure.
Fail-fast by default (stops at the first failing non-skipped check);
`RunEnv.KeepGoing` runs them all.

### `corpos-gate init` — wire the gate into any repo

`init` bootstraps a repo to be gated by corpos-gate: it writes a starter
`gate.yml` (if none exists) and installs gate-only git hooks. It only
**wires**; it never runs the gate.

**One-time prerequisite — install the binary on `$PATH`:**

```
make -C go corpos-gate-install     # go install → $GOBIN (or $(go env GOPATH)/bin)
```

The hooks call a bare `corpos-gate` from `$PATH`, so this makes the tool
available cross-repo. Then, in any repo:

```
corpos-gate init            # gates the repo containing the current dir
corpos-gate init <dir>      # gates the repo containing <dir>
```

What it does, **idempotently** (re-running rewrites the managed hooks and
never clobbers an existing `gate.yml`):

- **Starter `gate.yml`.** Auto-detects the stack — `go.mod` (at the root
  or one dir down) → `go`; `package.json` → `ts`; else `shell` — and
  writes a minimal config with format/vet/build at pre-commit and test at
  pre-push (go), or custom build/test checks (ts/shell). If a `gate.yml`
  already exists it is **kept untouched** ("gate.yml exists, keeping it").
- **Hooks.** A `pre-commit` hook running `corpos-gate run
  --tier=pre-commit` and a `pre-push` hook running `... --tier=pre-push`.

**Worktree-aware** (mirrors `scripts/worktree-setup.sh`). If the target is
a **linked worktree**, init installs gate-only hooks into that worktree's
**private** git dir (`.git/worktrees/<name>/gate-only-hooks`) and points a
**per-worktree** `core.hooksPath` at them — invisible to the main
checkout, removed with the worktree, and it never writes a **shared**
`core.hooksPath` that a main-checkout worktree-discipline guard could
collide with. If the target is a **main checkout**, init writes the hook
files straight into the repo's hooks dir (`.git/hooks`), setting no shared
config. Greenfield works from a standing start: `git init` a fresh repo,
`corpos-gate init` it, and it's gated.

**`--no-verify` still works.** The hooks rely on git's native bypass and
do nothing to defeat it. Each hook's header documents the escape:

```
# Emergency bypass is git-native:
#   git commit --no-verify     (or: git push --no-verify)
# This hook does nothing to defeat that.
```

So the gate is bypass-**resistant** (you have to opt out explicitly) but
not un-bypassable. If `corpos-gate` isn't on `$PATH` when a hook fires,
the hook prints the `make -C go corpos-gate-install` remedy and exits
non-zero (or bypass with `--no-verify`).

### Not in this increment

- The shell adapter (config parses the stack; custom-only, no adapter
  checks wired).
- **Phase B** — migrating `campaign-settings` (vitest) + `dm-manager`
  (jest) onto the ts adapter is a separate follow-up task; this increment
  only builds the adapter.
- The mutating sync stages and staged-blob gofmt stage of
  `scripts/precommit.sh`.
- **Cutover.** `scripts/precommit.sh` stays authoritative; retiring it
  is a later task in the chain.
