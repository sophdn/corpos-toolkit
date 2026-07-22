SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c

export CGO_ENABLED := 1
export CC := gcc
export PATH := $(PATH):/usr/local/go/bin

BINARY := bin/toolkit-server
GROUNDING_BINARY := bin/grounding-events-processor
COMMIT_LANDED_BINARY := bin/commit-landed-emit
PCBENCH_BINARY := bin/parse-context-bench
CORPOS_GATE_BINARY := bin/corpos-gate
DB_PATH ?= ../data/toolkit.db
DEFAULT_PROJECT ?= mcp-servers
TAGS := sqlite_fts5

# go-mutesting (mutation testing) is pinned to an exact pseudo-version of the
# maintained avito-tech fork — the zimmski original is dead (issue #100), and
# the fork has no semver tags. Installed STANDALONE (mutest-install),
# deliberately NOT a go.mod tool directive: a tool directive drags the fork's
# ~15 transitive deps — including the archived gopkg.in/yaml.v3 v3.0.1 that
# chain harden-go-deps T2 removed — into the production go.sum, for a tool that
# never links into the shipped binary. Vetted + pinned per skill
# dependency-vetting-discipline.
GO_MUTESTING_VERSION := v0.0.0-20251226130216-48d0401f00fb
GOPATH_BIN := $(shell go env GOPATH)/bin

# Build-time stamps injected via -ldflags -X so the running binary
# carries the git SHA + build timestamp it was compiled from. Surfaced
# through admin.server_version (MCP) and GET /version (HTTP) so the
# dashboard can detect a daemon that's drifted behind committed source
# (bug 1415). Falls back to "unversioned" when git isn't available
# (containers, tarball builds) — same sentinel admin.Deps used pre-1415.
GIT_SHA := $(shell git rev-parse --short HEAD 2>/dev/null || echo unversioned)
BUILT_AT_UNIX := $(shell date +%s)
LDFLAGS := -X main.gitSHA=$(GIT_SHA) -X main.builtAtUnix=$(BUILT_AT_UNIX)

.PHONY: build build-grounding-processor corpos-gate corpos-gate-install test coverage coverage-html cover-floor fmt vet build-all run smoke clean replay-verify vuln mutest mutest-install

# build produces the canonical executables: the production daemons
# (toolkit-server = MCP + HTTP daemon; grounding-events-processor = the
# Stop hook invoked from ~/.claude/hooks/grounding-events-processor.sh on
# every session-end; commit-landed-emit) plus parse-context-bench, the
# registered bench harness measure.bench_run(slug='parse-context')
# os/exec's at an absolute go/bin/ path. The bench binary builds here
# (not in build-all, which emits nothing) so the post-commit advisor
# keeps it present + current for on-demand regression runs. Always
# invokes `go build` and lets Go's own build cache handle "did anything
# change" — file-target deps would only fire on first build, masking
# source changes (bug 1370).
build:
	go build -tags $(TAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/toolkit-server/
	go build -tags $(TAGS) -o $(GROUNDING_BINARY) ./cmd/grounding-events-processor/
	go build -tags $(TAGS) -o $(COMMIT_LANDED_BINARY) ./cmd/commit-landed-emit/
	go build -tags $(TAGS) -o $(PCBENCH_BINARY) ./cmd/parse-context-bench/

# Phony alias so callers can rebuild the hook binary explicitly without
# rebuilding toolkit-server (faster iteration during hook development).
build-grounding-processor:
	go build -tags $(TAGS) -o $(GROUNDING_BINARY) ./cmd/grounding-events-processor/

# corpos-gate builds the stack-agnostic gate orchestrator CLI to
# bin/corpos-gate (gitignored). scripts/precommit.sh builds this before
# invoking it to drive the Go CHECK stages (format/vet/lint/build/coverage/
# vuln + the custom guards) from gate.yml. Same sqlite_fts5 tag + ldflags
# stamp style as `build`, so the gate binary is versioned like the daemons.
corpos-gate:
	go build -tags $(TAGS) -ldflags "$(LDFLAGS)" -o $(CORPOS_GATE_BINARY) ./cmd/corpos-gate/

# corpos-gate-install builds + installs the gate binary ONTO $PATH (GOBIN
# if set, else $(go env GOPATH)/bin) via `go install`, so a plain
# `corpos-gate` resolves in ANY repo. This is the one-time prerequisite
# for `corpos-gate init` in another repo: the hooks it writes call
# `corpos-gate` from PATH. Mirrors the corpos-gate target's tag + ldflags
# so the installed binary is versioned like the daemons (a -X against the
# unused main.gitSHA/builtAtUnix symbols is a harmless no-op here).
corpos-gate-install:
	go install -tags $(TAGS) -ldflags "$(LDFLAGS)" ./cmd/corpos-gate/
	@echo "corpos-gate installed to $$(go env GOBIN 2>/dev/null || echo $(GOPATH_BIN))/corpos-gate — now on PATH for 'corpos-gate init' in any repo"

# build-all compiles every package (not just cmd/toolkit-server) so build
# errors in test-less packages surface at gate time. Mirrors the precommit
# build stage's coverage; `make build` alone misses non-binary packages.
build-all:
	go build -tags $(TAGS) ./...

test:
	go test -tags $(TAGS) ./...

# coverage runs the full suite with a statement-coverage profile written to
# go/bin/cover.out (gitignored), then prints the aggregate total — the
# statement-weighted % that `go tool cover -func` reports on its last line.
# Same sqlite_fts5 tag as `test` so the FTS5-gated paths compile in; without
# it those packages fall out of the profile and the total skews. Drop the
# `| tail -1` for the full per-function table. `make coverage-html` opens the
# line-level HTML view of the same profile.
COVERPROFILE := bin/cover.out

coverage:
	go test -tags $(TAGS) -coverprofile=$(COVERPROFILE) ./...
	@echo "--- aggregate ---"
	@go tool cover -func=$(COVERPROFILE) | tail -1

coverage-html: coverage
	go tool cover -html=$(COVERPROFILE)

# cover-floor runs the suite ONCE with a coverage profile and enforces an
# aggregate statement-coverage floor — it IS the test run (same -tags, same
# ./... scope) plus a coverage-regression gate, so it subsumes the plain
# `test` target's work in a single ~5-min pass (never double-run the suite).
#
# STOPGAP: the pre-commit gate historically ran fmt/vet/lint/build/test/vuln
# but NO coverage; this target closes that gap until the
# corpos-gate-testing-module chain replaces it with a unified tool.
#
# COVERAGE_FLOOR is a RATCHET baseline (measured aggregate was 67.0%; floor set
# one point below for margin). Raise it over time as coverage climbs; NEVER
# lower it. The aggregate is measured over ./... — this INCLUDES the cmd/
# entrypoints, which drag the total down, UNLIKE corpos's internal-only
# (./internal/...) scope. That divergence is deliberate here and minor; chain
# 434 (corpos-gate-testing-module) will unify the scope. The awk compare idiom
# mirrors corpos scripts/gate.sh's coverage stage.
COVERAGE_FLOOR ?= 66
cover-floor:
	@mkdir -p $(dir $(COVERPROFILE))
	go test -tags $(TAGS) -coverprofile=$(COVERPROFILE) ./...
	@cov="$$(go tool cover -func=$(COVERPROFILE) | awk '/^total:/{gsub("%","",$$3);print $$3}')"; \
	 awk -v c="$$cov" -v m="$(COVERAGE_FLOOR)" 'BEGIN{exit !(c+0>=m+0)}' \
	   && echo "[cover-floor] aggregate coverage: $${cov}% >= $(COVERAGE_FLOOR)% floor — PASS" \
	   || { echo "[cover-floor] aggregate coverage: $${cov}% < $(COVERAGE_FLOOR)% floor — FAIL"; exit 1; }

# replay-verify re-forges the work-batching-and-forge-templates chain via
# the forge(chain, full-objects) path into a temp DB and asserts the
# chain row + task rows are byte-identical to the live production rows
# (modulo ids/timestamps/lifecycle) — closing T7 acceptance (f)'s
# strictest reading. SKIPs (exit 0) when the live DB or chain is absent,
# so a fresh checkout stays green. Wired into scripts/precommit.sh.
replay-verify:
	go run -tags $(TAGS) ./cmd/chain-replay-verify

# vuln is the Go dependency vulnerability gate (skill dependency-vetting-
# discipline). Two checks:
#   - go mod verify  : go.sum hashes haven't drifted (pinning integrity).
#   - govulncheck    : the authoritative Go vuln scanner (Go-team-maintained,
#                      reachability-aware). Module-managed via the tool
#                      directive (go tool govulncheck), so it runs
#                      UNCONDITIONALLY — no "tool missing → silent skip" hole
#                      (go-conventions §drop-silent-skip-once-module-managed).
#                      Exits non-zero (blocks) on a CALLED vuln; prints
#                      present-but-unreachable vulns as a non-blocking warning
#                      (the x/sys GO-2026-5024 case — surface it, bump manually).
#
# osv-scanner is deliberately NOT in this gate: v2.3.8 false-cleans Go
# advisories (bug osv-scanner-v2-silently-false-cleans-go-advisories — it
# extracted x/text@0.3.0 with the advisory in its local DB yet matched 0).
# A false-clean scanner in a gate gives false confidence. osv-scanner stays
# the breadth layer for NON-Go ecosystems (npm/PyPI/cargo) per the discipline
# skill, where it is proven. For Go: govulncheck + the manual web scan.
#
# Wired into scripts/precommit.sh; bypass offline (govulncheck needs
# vuln.go.dev) via TOOLKIT_PRECOMMIT_SKIP_VULN=1.
vuln:
	go mod verify
	go tool govulncheck -tags $(TAGS) ./...

# mutest-install installs the pinned go-mutesting binary to GOPATH/bin.
# Standalone (not module-tracked) — see GO_MUTESTING_VERSION rationale above.
mutest-install:
	go install github.com/avito-tech/go-mutesting/cmd/go-mutesting@$(GO_MUTESTING_VERSION)

# mutest runs mutation testing on PKG (a file or package path). go-mutesting
# has no native build-tag support, so it drives the -tags sqlite_fts5 suite
# through scripts/go-mutesting-exec.sh. On-demand analysis ONLY — NOT in the
# precommit gate (it re-runs the suite once per mutant; minutes per file).
# Surviving mutants on covered lines = test-net gaps worth pinning. Example:
#   make -C go mutest PKG=internal/refresolve/discipline_intent.go
#
# RECOVERY after a hard kill: go-mutesting mutates the target file IN PLACE
# (scripts/go-mutesting-exec.sh moves <file> → <file>.tmp, copies the mutant
# over <file>, then restores via a SIGHUP/SIGINT/SIGTERM trap). A SIGKILL
# (OOM, kill -9, terminal close) is untrappable and strands the mutant in
# <file> with the pristine original in <file>.tmp. It reads like uncommitted
# source corruption. Recover with: `git checkout <file> && rm <file>.tmp`
# (the .tmp is byte-identical to HEAD) plus `git checkout go/go.sum` to revert
# the tool's transitive-dep pollution. The pre-run guard below refuses to run
# while a *.go.tmp is stranded under PKG so a mutant can't compound.
PKG ?= internal/refresolve
mutest:
	@test -x $(GOPATH_BIN)/go-mutesting || { echo "go-mutesting not installed — run: make -C go mutest-install"; exit 1; }
	@_pkgdir=$$(case "$(PKG)" in *.go) dirname "$(PKG)";; *) echo "$(PKG)";; esac); strand=$$(find "$$_pkgdir" -name '*.go.tmp' 2>/dev/null); if [ -n "$$strand" ]; then echo "ERROR: stranded go-mutesting backup(s) found — a prior run was hard-killed (SIGKILL); the matching .go file may hold an applied mutant:"; echo "$$strand" | sed 's/^/    /'; echo "  recover: git checkout <file> && rm <file>.tmp   (the .tmp is byte-identical to HEAD)"; echo "           git checkout go/go.sum                  (revert tool dep pollution)"; echo "  refusing to run so the stranded mutant cannot compound."; exit 1; fi
	MUTATE_TIMEOUT=120 $(GOPATH_BIN)/go-mutesting \
		--exec="$$(git rev-parse --show-toplevel)/scripts/go-mutesting-exec.sh" \
		--exec-timeout=180 $(PKG)

fmt:
	gofmt -w .

vet:
	go vet -tags $(TAGS) ./...

run: build
	./$(BINARY) --db $(DB_PATH) --default-project $(DEFAULT_PROJECT)

smoke: build
	TOOLKIT_SERVER_BINARY=$(CURDIR)/$(BINARY) \
	TOOLKIT_SMOKE_DB=$(CURDIR)/$(DB_PATH) \
	go test -tags $(TAGS) -v -run TestSmoke ./smoketest/...

clean:
	rm -f $(BINARY) $(GROUNDING_BINARY) $(COMMIT_LANDED_BINARY) $(PCBENCH_BINARY)

# The gen-types target (Go→TS codegen via tygo, output to
# apps/dashboard/src/api/types.gen.ts) was dropped when toolkit split out
# of the monorepo (chain auto-startup-dev-services T2, 2026-06-02). The
# dashboard — and the tygo config + its precommit freshness gate — moved
# to the sophdn/frontend repo.
