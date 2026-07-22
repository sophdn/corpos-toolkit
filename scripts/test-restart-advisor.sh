#!/usr/bin/env bash
# scripts/test-restart-advisor.sh — regression tests for
# post-commit-restart-advisor.sh.
#
# Tests:
#   T1 — Bug 1322. A "toolkit-server" stdio process whose parent is
#        alive (a synthetic shell mimicking a Claude Code session)
#        is PRESERVED by the advisor when a Go-touching commit triggers
#        the rebuild + restart flow. The advisor's output reports the
#        preservation and the active process survives.
#   T2 — Bug 1322. An orphaned stdio process (PPID=1) IS killed by
#        the advisor — the preservation logic gates only on live
#        non-init parents.
#   T3 — TOOLKIT_PRECOMMIT_FORCE_STDIO_KILL=1 overrides T1's
#        preservation and kills the attached process anyway, restoring
#        the pre-bug-1322 behaviour for callers who want it.
#   T4 — TOOLKIT_PRECOMMIT_QUIET=1 collapses happy-path informational
#        lines into a single `advisor: ...` summary, while preserving
#        destructive-action warnings (the force-kill override message in
#        this synthetic case) and the existence of the preservation
#        behaviour itself (the stdio remains alive without the override).
#   T5 — Bug 1331. A deliberately-broken fake binary at the post-rebuild
#        path triggers the smoke gate: the advisor aborts before any
#        daemon restart and surfaces the binary's stderr in its output.
#        Asserts the smoke is a hard gate, not advisory.
#   T6 — Bug 1331. A passing fake binary that logs "starting db=..."
#        and parks lets the advisor continue to the stdio/HTTP restart
#        blocks. Asserts the smoke is non-blocking on the happy path.
#
# The advisor itself runs the rebuild step (make -C go build) and the
# bug 1331 smoke gate. We bypass the rebuild via a PATH-shim `make` (the
# rebuild is irrelevant to the kill-decision tests) and bypass the smoke
# via TOOLKIT_PRECOMMIT_SKIP_SMOKE=1 for the kill-decision tests; T5/T6
# exercise the smoke explicitly with a staged fake binary.
#
# We also PATH-shim `pgrep` (bug 1332) so the advisor's pgrep regex
# can't match the live toolkit-server processes on the host — if it did,
# T3/T4b's force-kill paths would clobber the production MCP.
#
# Exit code 0 on all-pass, 1 on any failure.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ADVISOR="$REPO_ROOT/scripts/post-commit-restart-advisor.sh"

if [[ ! -x "$ADVISOR" ]]; then
    echo "FATAL: advisor not found or not executable: $ADVISOR" >&2
    exit 1
fi

PASS=0
FAIL=0
SCRATCH_DIRS=()

cleanup() {
    # Kill any stragglers spawned by the test cases.
    for d in "${SCRATCH_DIRS[@]:-}"; do
        [[ -d "$d" ]] || continue
        for name in stdio.pid parent.pid orphan.pid; do
            local f="$d/$name"
            [[ -f "$f" ]] || continue
            pid="$(cat "$f" 2>/dev/null || true)"
            [[ -n "$pid" ]] || continue
            kill -9 "$pid" 2>/dev/null || true
        done
        rm -rf "$d"
    done
    # Mop up any toolkit-server-stdio-test / toolkit-server-orphan-test
    # processes the spawn helpers might have leaked; the advisor's
    # pgrep filter could match production binaries too, so be specific.
    pkill -9 -f 'toolkit-server-stdio-test'  2>/dev/null || true
    pkill -9 -f 'toolkit-server-orphan-test' 2>/dev/null || true
    # The marker file is a runtime artifact written by the production
    # advisor; the test should not leave a stale one behind.
    rm -f /tmp/toolkit-server-restart-needed 2>/dev/null || true
}
trap cleanup EXIT

assert() {
    local desc="$1"; shift
    if "$@"; then
        echo "  PASS  $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL  $desc"
        FAIL=$((FAIL + 1))
    fi
}

# Create a scratch git repo with a single Go-touching commit so the
# advisor classifies the diff as runtime-impacting. Override PATH so
# `make` is a no-op (the rebuild is irrelevant to the kill-decision
# tests).
setup_scratch_repo() {
    local d
    d="$(mktemp -d)"
    SCRATCH_DIRS+=("$d")
    pushd "$d" > /dev/null
    git init --quiet
    git config user.email "test@example.com"
    git config user.name  "test"
    # Seed an initial commit so HEAD~1 exists; then a Go-touching
    # commit so HEAD~1..HEAD shows a runtime-impacting diff for the
    # advisor's path classification.
    echo "seed" > README.md
    git add README.md
    git commit --quiet -m "initial"
    mkdir -p go/internal/dummy
    echo "package dummy" > go/internal/dummy/dummy.go
    git add go/internal/dummy/dummy.go
    git commit --quiet -m "go-touching commit"
    popd > /dev/null
    # Fake-`make` shim: any `make ...` invocation prints a marker and exits 0.
    local shim="$d/bin"
    mkdir -p "$shim"
    cat > "$shim/make" <<'EOF'
#!/usr/bin/env bash
echo "[test-shim] make $*" >&2
exit 0
EOF
    chmod +x "$shim/make"
    # ss may not exist in CI; provide a no-op shim that prints nothing
    # so the HTTP-restart branch finds no holder and goes idle.
    cat > "$shim/ss" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
    chmod +x "$shim/ss"
    # pgrep shim (bug 1332). The advisor calls `pgrep -af toolkit-server`
    # to find stdio MCP processes. The host runs a live toolkit-server
    # whose cmdline matches that pattern; under the T3/T4b force-kill
    # paths, the advisor would kill the real production MCP. Restrict
    # the shim to synthetic test cmdlines via a narrower pattern, then
    # delegate to the real pgrep at its standard PATH location so the
    # advisor can't recurse into the shim itself.
    cat > "$shim/pgrep" <<'EOF'
#!/usr/bin/env bash
real_pgrep="$(PATH=/usr/bin:/bin command -v pgrep)"
if [[ -z "$real_pgrep" ]]; then
    echo "test-shim pgrep: real pgrep not found on /usr/bin:/bin" >&2
    exit 2
fi
# Ignore the caller's pattern entirely — synthetic-test scope only.
exec "$real_pgrep" -af 'toolkit-server-(stdio|orphan)-test'
EOF
    chmod +x "$shim/pgrep"
    echo "$d"
}

# spawn_synthetic_stdio creates a "toolkit-server" stdio process by
# starting a sleep wrapped in a bash script renamed to mimic the
# toolkit-server cmdline. pgrep -f matches against the full cmdline so
# the wrapper appears in the script's STDIO_PIDS list. Writes the pid
# to <dir>/stdio.pid.
spawn_synthetic_stdio() {
    local d="$1"
    # Use a here-doc to a script the synthetic parent will exec; the
    # script renames its argv via `exec -a`.
    cat > "$d/fake-stdio.sh" <<'EOF'
#!/usr/bin/env bash
# Mimic a toolkit-server stdio process. The exec -a sets argv[0] to
# `toolkit-server --some-stdio-arg` so the advisor's pgrep -af regex
# matches us. Sleep long enough for the test to finish.
exec -a "toolkit-server-stdio-test --default-project=test" sleep 60
EOF
    chmod +x "$d/fake-stdio.sh"
    # Spawn under the synthetic parent. We use setsid to give the
    # process its own session so SIGTERM to its group doesn't propagate
    # to the test runner.
    (
        # Re-parent under the synthetic parent_pid by running through
        # bash -c so the OS records parent_pid as our PPID? No — PPID
        # is the immediate parent. Easier: just record this process's
        # actual PPID (the test runner) as parent — that's a live
        # non-init PPID, which exercises the "preserve" branch.
        "$d/fake-stdio.sh" &
        echo "$!" > "$d/stdio.pid"
    )
    sleep 0.1
}

# spawn_orphan_stdio creates a "toolkit-server" stdio process whose
# parent has already exited — PPID gets reparented to 1 (init/systemd).
# Achieved by double-forking: the inner process's parent exits, so the
# kernel reparents the grandchild to PID 1.
spawn_orphan_stdio() {
    local d="$1"
    # setsid + subshell-exit: setsid forks the bash invocation into a new
    # session; the subshell `(...)` exits immediately after backgrounding
    # so the kernel reparents the inner sleep to PID 1 (init/systemd).
    # The `exec -a NAME` rename requires bash, so we invoke the inner
    # shell as bash explicitly rather than the platform's `sh`.
    (
        setsid bash -c 'exec -a "toolkit-server-orphan-test --default-project=test" sleep 60' &
        echo "$!" > "$d/orphan.pid"
    )
    # Wait long enough for setsid to re-fork and the immediate parent to
    # exit so init adopts our sleep. 500ms is generous; on busy CI this
    # might need a poll-until-PPID=1 loop, but for local runs it's fine.
    sleep 0.5
}

# is_alive checks whether a PID is still running.
is_alive() {
    local pid="$1"
    [[ -d "/proc/$pid" ]]
}

# run_advisor_in scratchdir, returns its stdout.
#
# Default invocation skips the bug 1331 smoke gate — the existing kill /
# preserve tests don't stage a real binary, so the smoke would abort
# before the stdio block runs. T5/T6 invoke the advisor directly without
# this skip so the smoke is exercised explicitly.
run_advisor_in() {
    local d="$1"
    pushd "$d" > /dev/null
    PATH="$d/bin:$PATH" TOOLKIT_PRECOMMIT_SKIP_SMOKE=1 \
        "$ADVISOR" "HEAD~1..HEAD" 2>&1 || true
    popd > /dev/null
}

# ── T1: attached stdio is preserved ───────────────────────────────────
d1="$(setup_scratch_repo)"
spawn_synthetic_stdio "$d1"
stdio_pid="$(cat "$d1/stdio.pid")"

# Sanity: process is alive before the advisor runs.
assert "T1 setup: stdio process is alive" is_alive "$stdio_pid"

out="$(run_advisor_in "$d1")"

assert "T1: advisor recognises Go diff" \
    bash -c "grep -q 'go build' <<< \"$out\" || grep -q 'go.cmd\\|go/internal' <<< \"$out\""
assert "T1: stdio process still alive after advisor" is_alive "$stdio_pid"
assert "T1: advisor message names the preservation behaviour" \
    bash -c "grep -q 'preserving stdio' <<< \"$out\""
assert "T1: advisor names TOOLKIT_PRECOMMIT_FORCE_STDIO_KILL override" \
    bash -c "grep -q 'TOOLKIT_PRECOMMIT_FORCE_STDIO_KILL' <<< \"$out\""

# Marker file written so the agent can detect the rebuild.
assert "T1: marker file written to /tmp/toolkit-server-restart-needed" \
    test -f /tmp/toolkit-server-restart-needed

# Cleanup the attached pid before next test.
kill "$stdio_pid" 2>/dev/null || true

# ── T2: orphan stdio is killed ────────────────────────────────────────
d2="$(setup_scratch_repo)"
spawn_orphan_stdio "$d2"
orphan_pid="$(cat "$d2/orphan.pid" 2>/dev/null || echo "")"

if [[ -z "$orphan_pid" ]]; then
    echo "  SKIP  T2: orphan setup did not record a pid (system may not allow double-fork test)"
elif ! is_alive "$orphan_pid"; then
    echo "  SKIP  T2: orphan setup process is not running (likely a race)"
else
    # Check the orphan actually has PPID=1; if double-fork didn't take,
    # T2 is meaningless and we should skip.
    orphan_ppid="$(awk -F') ' '{print $2}' "/proc/$orphan_pid/stat" 2>/dev/null | awk '{print $2}')"
    if [[ "$orphan_ppid" != "1" ]]; then
        echo "  SKIP  T2: orphan's PPID is $orphan_ppid not 1 (double-fork didn't reparent)"
    else
        run_advisor_in "$d2" > /dev/null
        sleep 0.2
        assert "T2: orphan stdio is killed by advisor" \
            bash -c "! [[ -d /proc/$orphan_pid ]]"
    fi
fi

# ── T3: force-kill override ───────────────────────────────────────────
d3="$(setup_scratch_repo)"
spawn_synthetic_stdio "$d3"
stdio_pid3="$(cat "$d3/stdio.pid")"
assert "T3 setup: attached stdio alive before force-kill" is_alive "$stdio_pid3"

(
    cd "$d3"
    PATH="$d3/bin:$PATH" TOOLKIT_PRECOMMIT_FORCE_STDIO_KILL=1 \
        TOOLKIT_PRECOMMIT_SKIP_SMOKE=1 \
        "$ADVISOR" "HEAD~1..HEAD" > /dev/null 2>&1 || true
)
sleep 0.2
assert "T3: TOOLKIT_PRECOMMIT_FORCE_STDIO_KILL=1 kills the attached stdio" \
    bash -c "! [[ -d /proc/$stdio_pid3 ]]"

# ── T4: TOOLKIT_PRECOMMIT_QUIET=1 collapses output ────────────────────
# Two assertions in quiet mode:
#   (a) Happy-path informational lines (`preserving stdio MCP processes`,
#       `running go build`) are SUPPRESSED.
#   (b) A single `advisor: ...` summary line is emitted at the end.
# Plus a force-kill quiet run to verify destructive warnings still fire.
d4="$(setup_scratch_repo)"
spawn_synthetic_stdio "$d4"
stdio_pid4="$(cat "$d4/stdio.pid")"

quiet_out="$(
    cd "$d4"
    PATH="$d4/bin:$PATH" TOOLKIT_PRECOMMIT_QUIET=1 \
        TOOLKIT_PRECOMMIT_SKIP_SMOKE=1 \
        "$ADVISOR" "HEAD~1..HEAD" 2>&1 || true
)"

assert "T4: quiet mode suppresses 'preserving stdio' informational text" \
    bash -c "! grep -q 'preserving stdio MCP processes' <<< \"$quiet_out\""
assert "T4: quiet mode suppresses 'running go build' informational text" \
    bash -c "! grep -q 'running go build' <<< \"$quiet_out\""
assert "T4: quiet mode emits a single advisor: summary line" \
    bash -c "[[ \$(grep -c '^advisor: ' <<< \"$quiet_out\") -eq 1 ]]"
assert "T4: summary names the preservation count" \
    bash -c "grep -q 'preserved 1 stdio session' <<< \"$quiet_out\""
assert "T4: stdio process still alive after quiet-mode run" is_alive "$stdio_pid4"

# T4b: force-kill in quiet mode — the kill warning must still surface.
d4b="$(setup_scratch_repo)"
spawn_synthetic_stdio "$d4b"
stdio_pid4b="$(cat "$d4b/stdio.pid")"

quiet_kill_out="$(
    cd "$d4b"
    PATH="$d4b/bin:$PATH" TOOLKIT_PRECOMMIT_QUIET=1 \
    TOOLKIT_PRECOMMIT_FORCE_STDIO_KILL=1 \
    TOOLKIT_PRECOMMIT_SKIP_SMOKE=1 \
        "$ADVISOR" "HEAD~1..HEAD" 2>&1 || true
)"

assert "T4b: quiet+force-kill still surfaces the kill warning" \
    bash -c "grep -q 'killing stdio MCP processes' <<< \"$quiet_kill_out\""
assert "T4b: quiet+force-kill still surfaces the override-banner line" \
    bash -c "grep -q 'TOOLKIT_PRECOMMIT_FORCE_STDIO_KILL=1 — killing' <<< \"$quiet_kill_out\""
sleep 0.2
assert "T4b: quiet+force-kill actually killed the stdio process" \
    bash -c "! [[ -d /proc/$stdio_pid4b ]]"

# Cleanup non-killed stdio from T4.
kill "$stdio_pid4" 2>/dev/null || true

# ── T5: smoke gate aborts on broken binary (bug 1331) ─────────────────
# Stage a fake "broken" binary at the smoke target path. The smoke step
# runs the binary; a non-zero exit before `starting db=` triggers the
# abort. Verify: the advisor surfaces the abort message AND skips the
# downstream stdio/HTTP restart blocks.
d5="$(setup_scratch_repo)"
broken_bin="$d5/broken-toolkit-server"
cat > "$broken_bin" <<'EOF'
#!/usr/bin/env bash
echo "toolkit-server-go: simulated migration runner panic" >&2
exit 1
EOF
chmod +x "$broken_bin"

smoke_fail_out="$(
    cd "$d5"
    PATH="$d5/bin:$PATH" TOOLKIT_SMOKE_BINARY="$broken_bin" \
        "$ADVISOR" "HEAD~1..HEAD" 2>&1 || true
)"

assert "T5: smoke surfaces failure banner" \
    bash -c "grep -q 'smoke FAILED' <<< \"$smoke_fail_out\""
assert "T5: smoke surfaces the broken binary's stderr" \
    bash -c "grep -q 'simulated migration runner panic' <<< \"$smoke_fail_out\""
assert "T5: smoke abort skips stdio-restart block" \
    bash -c "! grep -q 'preserving stdio MCP processes' <<< \"$smoke_fail_out\""
assert "T5: smoke abort skips HTTP-restart block" \
    bash -c "! grep -q 'restarting HTTP daemon\\|starting one' <<< \"$smoke_fail_out\""

# ── T6: smoke gate passes through on healthy binary (bug 1331) ────────
# Stage a fake "passing" binary that logs the `starting db=` line the
# smoke greps for, then parks. The smoke times it out after ~4s with
# SIGTERM. The advisor proceeds to the stdio-restart block.
d6="$(setup_scratch_repo)"
good_bin="$d6/good-toolkit-server"
cat > "$good_bin" <<'EOF'
#!/usr/bin/env bash
echo "toolkit-server-go: starting db=$2 project=test rubrics=" >&2
# Park so the timeout has to kill us — mirrors `select {}` in main.go.
sleep 30
EOF
chmod +x "$good_bin"

smoke_pass_out="$(
    cd "$d6"
    PATH="$d6/bin:$PATH" TOOLKIT_SMOKE_BINARY="$good_bin" \
        "$ADVISOR" "HEAD~1..HEAD" 2>&1 || true
)"

assert "T6: smoke logs ok" \
    bash -c "grep -q 'smoke ok' <<< \"$smoke_pass_out\""
assert "T6: smoke pass-through still reaches stdio-restart block" \
    bash -c "grep -q 'No stdio processes found\\|preserving stdio MCP processes\\|killing stdio MCP processes' <<< \"$smoke_pass_out\""

# ── T7: an action-docs corpus chunk is runtime-affecting ──────────────
# The corpus is go:embed'd into the binary (chain single-source-action-
# describe T6) from go/internal/actiondocs/corpus/, so a chunk add/edit
# changes the compiled binary and DOES affect runtime
# (admin.action_describe, auto-generated meta-tool descriptions). It is
# caught by the advisor's go/internal/* arm — NEED_GO_REBUILD + both
# restarts. (Pre-T6 the corpus lived at blueprints/action-docs/ and was
# disk-loaded; the dedicated arm + the bug it fixed,
# advisor-misclassifies-action-docs-chunks-as-docs-only, are retired now
# that the go/internal arm covers it with the correct rebuild disposition.)
#
# Setup mirrors setup_scratch_repo but with a corpus chunk as the only
# changed file. Sibling negative (T7b) confirms a chunk under a
# different blueprints subdir still classifies docs-only.
setup_action_docs_repo() {
    local d
    d="$(mktemp -d)"
    SCRATCH_DIRS+=("$d")
    pushd "$d" > /dev/null
    git init --quiet
    git config user.email "test@example.com"
    git config user.name  "test"
    echo "seed" > README.md
    git add README.md
    git commit --quiet -m "initial"
    mkdir -p go/internal/actiondocs/corpus/knowledge
    cat > go/internal/actiondocs/corpus/knowledge/synthetic_action.toml <<'CHUNK_EOF'
surface = "knowledge"
action = "synthetic_action"
purpose = "Test fixture."
CHUNK_EOF
    git add go/internal/actiondocs/corpus/knowledge/synthetic_action.toml
    git commit --quiet -m "action-docs corpus chunk only"
    popd > /dev/null
    # Reuse the same shim binaries (make / ss / pgrep) the other tests use.
    local shim="$d/bin"
    mkdir -p "$shim"
    cat > "$shim/make" <<'EOF'
#!/usr/bin/env bash
echo "[test-shim] make $*" >&2
exit 0
EOF
    chmod +x "$shim/make"
    cat > "$shim/ss" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
    chmod +x "$shim/ss"
    cat > "$shim/pgrep" <<'EOF'
#!/usr/bin/env bash
real_pgrep="$(PATH=/usr/bin:/bin command -v pgrep)"
if [[ -z "$real_pgrep" ]]; then exit 2; fi
exec "$real_pgrep" -af 'toolkit-server-(stdio|orphan)-test'
EOF
    chmod +x "$shim/pgrep"
    echo "$d"
}

d7="$(setup_action_docs_repo)"
out7="$(run_advisor_in "$d7")"

assert "T7: action-docs chunk does NOT classify as docs-only" \
    bash -c "! grep -q 'no runtime-affecting changes' <<< \"$out7\""
assert "T7: action-docs chunk triggers HTTP daemon restart line" \
    bash -c "grep -q 'HTTP daemon relaunched\\|restarting HTTP daemon\\|no HTTP daemon running' <<< \"$out7\""
assert "T7: action-docs chunk hits the stdio-restart marker branch" \
    bash -c "grep -q 'stdio processes\\|No stdio processes\\|preserving stdio MCP processes' <<< \"$out7\""

# A corpus chunk now lives under go/internal, so the go rebuild step is
# required to re-embed it AND to refresh the main.gitSHA ldflag (bug
# advisor-restart-without-rebuild-leaves-stale-compile-time-sha-trips-
# drift-banner). The freshly-linked binary then carries HEAD's SHA so the
# dashboard drift banner doesn't fire falsely.
assert "T7: action-docs corpus chunk triggers the go rebuild step" \
    bash -c "grep -q 'running go build' <<< \"$out7\""

# T7b: a non-runtime-affecting blueprints path stays docs-only
# (regression guard so the restart-needed arms for forge-schemas /
# rubrics — added in bug 857's fix — don't accidentally widen to all
# blueprints/*). Uses blueprints/events/ because event schemas are
# embedded into the binary at build time via go/internal/events/
# schemas/ rather than read from disk at startup — a blueprints/events/
# edit is spec-only.
setup_other_blueprints_repo() {
    local d
    d="$(mktemp -d)"
    SCRATCH_DIRS+=("$d")
    pushd "$d" > /dev/null
    git init --quiet
    git config user.email "test@example.com"
    git config user.name  "test"
    echo "seed" > README.md
    git add README.md
    git commit --quiet -m "initial"
    mkdir -p blueprints/events
    echo '{"$id": "https://synthetic"}' > blueprints/events/synthetic.json
    git add blueprints/events/synthetic.json
    git commit --quiet -m "events blueprint subdir only"
    popd > /dev/null
    local shim="$d/bin"
    mkdir -p "$shim"
    cat > "$shim/make" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
    chmod +x "$shim/make"
    echo "$d"
}

d7b="$(setup_other_blueprints_repo)"
out7b="$(
    cd "$d7b"
    PATH="$d7b/bin:$PATH" TOOLKIT_PRECOMMIT_SKIP_SMOKE=1 \
        "$ADVISOR" "HEAD~1..HEAD" 2>&1 || true
)"

assert "T7b: blueprints/events chunk still classifies as docs-only (spec-only, embedded copy is runtime)" \
    bash -c "grep -q 'no runtime-affecting changes' <<< \"$out7b\""

# T7c: blueprints/forge-schemas/* IS restart-needed. Bug 857. The forge
# schema registry loads from disk at toolkit-server startup via
# registry.Load(blueprintsDir) at go/cmd/toolkit-server/main.go:378.
# A schema add / edit doesn't take effect until restart (or an explicit
# admin.schema_reload, which the advisor can't assume).
setup_forge_schemas_repo() {
    local d
    d="$(mktemp -d)"
    SCRATCH_DIRS+=("$d")
    pushd "$d" > /dev/null
    git init --quiet
    git config user.email "test@example.com"
    git config user.name  "test"
    echo "seed" > README.md
    git add README.md
    git commit --quiet -m "initial"
    mkdir -p blueprints/forge-schemas
    cat > blueprints/forge-schemas/synthetic.toml <<'CHUNK_EOF'
supported_ops = ["create"]
[schema]
name = "synthetic"
prefix = "SYN"
output_dir = "process-docs/synthetic"
filename_pattern = "{prefix}_{slug}_{date}.md"
[storage]
target = "fs"
[[fields]]
name = "title"
type = "string"
CHUNK_EOF
    git add blueprints/forge-schemas/synthetic.toml
    git commit --quiet -m "forge schema chunk"
    popd > /dev/null
    local shim="$d/bin"
    mkdir -p "$shim"
    cat > "$shim/make" <<'EOF'
#!/usr/bin/env bash
echo "[test-shim] make $*" >&2
exit 0
EOF
    chmod +x "$shim/make"
    cat > "$shim/ss" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
    chmod +x "$shim/ss"
    cat > "$shim/pgrep" <<'EOF'
#!/usr/bin/env bash
real_pgrep="$(PATH=/usr/bin:/bin command -v pgrep)"
if [[ -z "$real_pgrep" ]]; then exit 2; fi
exec "$real_pgrep" -af 'toolkit-server-(stdio|orphan)-test'
EOF
    chmod +x "$shim/pgrep"
    echo "$d"
}

d7c="$(setup_forge_schemas_repo)"
out7c="$(run_advisor_in "$d7c")"

assert "T7c (bug 857): forge-schemas chunk does NOT classify as docs-only" \
    bash -c "! grep -q 'no runtime-affecting changes' <<< \"$out7c\""
assert "T7c (bug 857): forge-schemas chunk does NOT classify as dashboard-only" \
    bash -c "! grep -q 'dashboard-only diff' <<< \"$out7c\""
assert "T7c (bug 857): forge-schemas chunk triggers HTTP daemon restart line" \
    bash -c "grep -q 'HTTP daemon relaunched\\|restarting HTTP daemon\\|no HTTP daemon running' <<< \"$out7c\""

# T7d: blueprints/rubrics/* IS restart-needed. Bug 857 sibling — same
# load-at-startup pattern (rubric registry loaded via --rubrics-dir at
# go/cmd/toolkit-server/main.go).
setup_rubrics_repo() {
    local d
    d="$(mktemp -d)"
    SCRATCH_DIRS+=("$d")
    pushd "$d" > /dev/null
    git init --quiet
    git config user.email "test@example.com"
    git config user.name  "test"
    echo "seed" > README.md
    git add README.md
    git commit --quiet -m "initial"
    mkdir -p blueprints/rubrics
    cat > blueprints/rubrics/synthetic.toml <<'CHUNK_EOF'
name = "synthetic-rubric"
[[criteria]]
key = "stub"
weight = 1
CHUNK_EOF
    git add blueprints/rubrics/synthetic.toml
    git commit --quiet -m "rubric chunk"
    popd > /dev/null
    local shim="$d/bin"
    mkdir -p "$shim"
    cat > "$shim/make" <<'EOF'
#!/usr/bin/env bash
echo "[test-shim] make $*" >&2
exit 0
EOF
    chmod +x "$shim/make"
    cat > "$shim/ss" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
    chmod +x "$shim/ss"
    cat > "$shim/pgrep" <<'EOF'
#!/usr/bin/env bash
real_pgrep="$(PATH=/usr/bin:/bin command -v pgrep)"
if [[ -z "$real_pgrep" ]]; then exit 2; fi
exec "$real_pgrep" -af 'toolkit-server-(stdio|orphan)-test'
EOF
    chmod +x "$shim/pgrep"
    echo "$d"
}

d7d="$(setup_rubrics_repo)"
out7d="$(run_advisor_in "$d7d")"

assert "T7d (bug 857): rubrics chunk does NOT classify as docs-only" \
    bash -c "! grep -q 'no runtime-affecting changes' <<< \"$out7d\""
assert "T7d (bug 857): rubrics chunk does NOT classify as dashboard-only" \
    bash -c "! grep -q 'dashboard-only diff' <<< \"$out7d\""
assert "T7d (bug 857): rubrics chunk triggers HTTP daemon restart line" \
    bash -c "grep -q 'HTTP daemon relaunched\\|restarting HTTP daemon\\|no HTTP daemon running' <<< \"$out7d\""

# T7e: hooks/* is not dashboard-only. Bug 857. Claude Code reads hook
# scripts at Stop / SessionStart / etc. — no daemon impact, so no
# rebuild/restart. But labeling a hook edit as "dashboard-only diff —
# vite HMR handles it" is misleading (and was the second of bug 857's
# two failure cases). Correct label is "no daemon restart needed".
setup_hooks_repo() {
    local d
    d="$(mktemp -d)"
    SCRATCH_DIRS+=("$d")
    pushd "$d" > /dev/null
    git init --quiet
    git config user.email "test@example.com"
    git config user.name  "test"
    echo "seed" > README.md
    git add README.md
    git commit --quiet -m "initial"
    mkdir -p hooks
    cat > hooks/synthetic-hook.sh <<'CHUNK_EOF'
#!/usr/bin/env bash
exit 0
CHUNK_EOF
    chmod +x hooks/synthetic-hook.sh
    git add hooks/synthetic-hook.sh
    git commit --quiet -m "hook chunk"
    popd > /dev/null
    local shim="$d/bin"
    mkdir -p "$shim"
    cat > "$shim/make" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
    chmod +x "$shim/make"
    echo "$d"
}

d7e="$(setup_hooks_repo)"
out7e="$(
    cd "$d7e"
    PATH="$d7e/bin:$PATH" TOOLKIT_PRECOMMIT_SKIP_SMOKE=1 \
        "$ADVISOR" "HEAD~1..HEAD" 2>&1 || true
)"

assert "T7e (bug 857): hooks chunk does NOT classify as dashboard-only" \
    bash -c "! grep -q 'dashboard-only diff' <<< \"$out7e\""

# ── T8: pipefail-fragile pipeline audit (advisor extension) ──────────
# Mirrors the commit 154fc51 STDIO_PIDS fix on the remaining
# command-substitution pipelines that can legitimately produce empty
# results under set -euo pipefail:
#
#   - HTTP_PID  (line ~528): `ss | sed | head` aborts if ss is missing.
#   - HTTP_TTY  (line ~534): `ps -p $PID | tr` aborts if the daemon
#                             exited between the ss read and ps.
#   - HTTP_PGID (line ~549): same race as HTTP_TTY.
#
# Each is exercised below with a PATH-shim that triggers the failure
# mode; the assertion is that the advisor does NOT abort and proceeds
# to the relaunch line.

# T8a: missing ss shim (exit 127) — the HTTP_PID pipeline's left side
# fails. Without `|| true` the substitution propagates the failure and
# the advisor aborts before reaching the relaunch.
setup_missing_ss_repo() {
    local d
    d="$(mktemp -d)"
    SCRATCH_DIRS+=("$d")
    pushd "$d" > /dev/null
    git init --quiet
    git config user.email "test@example.com"
    git config user.name  "test"
    echo "seed" > README.md
    git add README.md
    git commit --quiet -m "initial"
    mkdir -p go/internal/dummy
    echo "package dummy" > go/internal/dummy/dummy.go
    git add go/internal/dummy/dummy.go
    git commit --quiet -m "go-touching commit"
    popd > /dev/null
    local shim="$d/bin"
    mkdir -p "$shim"
    cat > "$shim/make" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
    chmod +x "$shim/make"
    # ss shim that simulates a missing-iproute2 host: exit 127, no stdout.
    cat > "$shim/ss" <<'EOF'
#!/usr/bin/env bash
exit 127
EOF
    chmod +x "$shim/ss"
    cat > "$shim/pgrep" <<'EOF'
#!/usr/bin/env bash
real_pgrep="$(PATH=/usr/bin:/bin command -v pgrep)"
if [[ -z "$real_pgrep" ]]; then exit 2; fi
exec "$real_pgrep" -af 'toolkit-server-(stdio|orphan)-test'
EOF
    chmod +x "$shim/pgrep"
    echo "$d"
}

d8a="$(setup_missing_ss_repo)"
out8a="$(
    cd "$d8a"
    PATH="$d8a/bin:$PATH" TOOLKIT_PRECOMMIT_SKIP_SMOKE=1 \
        "$ADVISOR" "HEAD~1..HEAD" 2>&1 || true
)"

assert "T8a: missing ss does not abort the advisor mid-script" \
    bash -c "grep -q 'HTTP daemon relaunched\\|no HTTP daemon running' <<< \"$out8a\""
assert "T8a: missing ss treats empty HTTP_PID as 'no daemon running'" \
    bash -c "grep -q 'no HTTP daemon running' <<< \"$out8a\""

# T8b: ps returning empty for a stale HTTP_PID — the race where the
# daemon exited between the ss read and the ps invocation. Shim ss to
# return a synthetic PID for sed to extract, then shim ps to exit 1
# (mimicking 'no such process'). The advisor must continue past both
# the HTTP_TTY and HTTP_PGID lines and reach the relaunch.
setup_ps_empty_repo() {
    local d
    d="$(mktemp -d)"
    SCRATCH_DIRS+=("$d")
    pushd "$d" > /dev/null
    git init --quiet
    git config user.email "test@example.com"
    git config user.name  "test"
    echo "seed" > README.md
    git add README.md
    git commit --quiet -m "initial"
    mkdir -p go/internal/dummy
    echo "package dummy" > go/internal/dummy/dummy.go
    git add go/internal/dummy/dummy.go
    git commit --quiet -m "go-touching commit"
    popd > /dev/null
    local shim="$d/bin"
    mkdir -p "$shim"
    cat > "$shim/make" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
    chmod +x "$shim/make"
    # ss prints a fake match so HTTP_PID parses to 999999 (a PID the
    # kernel won't allocate during the test). The advisor's sed regex
    # 's/.*pid=\([0-9]*\).*/\1/p' extracts the digits.
    cat > "$shim/ss" <<'EOF'
#!/usr/bin/env bash
case "${1:-}" in
    -ltnp)
        echo 'LISTEN 0 4096 *:3000 *:* users:(("toolkit-server",pid=999999,fd=10))'
        ;;
    *)
        ;;
esac
exit 0
EOF
    chmod +x "$shim/ss"
    # ps returns 1 for any -p invocation, mimicking process gone.
    cat > "$shim/ps" <<'EOF'
#!/usr/bin/env bash
# ps -p <missing-pid>: exit 1 per Linux ps. Print nothing.
case "$*" in
    *-p*)
        exit 1
        ;;
    *)
        exec /bin/ps "$@"
        ;;
esac
EOF
    chmod +x "$shim/ps"
    cat > "$shim/pgrep" <<'EOF'
#!/usr/bin/env bash
real_pgrep="$(PATH=/usr/bin:/bin command -v pgrep)"
if [[ -z "$real_pgrep" ]]; then exit 2; fi
exec "$real_pgrep" -af 'toolkit-server-(stdio|orphan)-test'
EOF
    chmod +x "$shim/pgrep"
    # Also need a no-op `kill` so the simulated 999999 kill doesn't
    # accidentally signal an unrelated host process if one happens to
    # carry that pid. Unlikely but worth pinning.
    cat > "$shim/kill" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
    chmod +x "$shim/kill"
    # And a no-op launch.sh-style relaunch — `nohup ./go/launch.sh` would
    # fail in the scratch repo. Provide a stub launch.sh so the relaunch
    # marker prints.
    mkdir -p "$d/go"
    cat > "$d/go/launch.sh" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
    chmod +x "$d/go/launch.sh"
    echo "$d"
}

d8b="$(setup_ps_empty_repo)"
out8b="$(
    cd "$d8b"
    PATH="$d8b/bin:$PATH" TOOLKIT_PRECOMMIT_SKIP_SMOKE=1 \
        "$ADVISOR" "HEAD~1..HEAD" 2>&1 || true
)"

assert "T8b: ps-empty race does not abort the advisor mid-script" \
    bash -c "grep -q 'HTTP daemon relaunched' <<< \"$out8b\""
assert "T8b: advisor still emits the 'restarting HTTP daemon' banner under ps-empty" \
    bash -c "grep -q 'restarting HTTP daemon (pid 999999)' <<< \"$out8b\""

# ── T9: sequencing-operation guard (bug 920) ──────────────────────────
# During git rebase / cherry-pick, the post-commit hook fires once per
# replayed commit. The advisor must detect the in-progress sequencing
# state (worktree-aware, via `git rev-parse --git-path`) and no-op with a
# single advisory line instead of rebuilding + restarting per pick.

# T9a: a synthetic .git/rebase-merge dir makes the advisor treat the
# commit as mid-rebase. setup_scratch_repo's diff is go-touching, so
# WITHOUT the guard the advisor would run the go build — the assertions
# below prove it skips instead.
d9a="$(setup_scratch_repo)"
# Create the state dir INSIDE the scratch repo. The cd + mkdir must share
# one subshell — `mkdir "$(cd $d && git rev-parse --git-path …)"` would
# resolve the relative .git path in the subshell but run mkdir in the
# caller's CWD, planting state in the wrong repo.
( cd "$d9a" && mkdir -p "$(git rev-parse --git-path rebase-merge)" )
out9a="$(run_advisor_in "$d9a")"

assert "T9a: mid-rebase advisor prints the skip line" \
    bash -c "grep -q 'skipping per-commit rebuild' <<< \"$out9a\""
assert "T9a: mid-rebase advisor does NOT run the go build" \
    bash -c "! grep -q 'running go build' <<< \"$out9a\""
assert "T9a: mid-rebase advisor does NOT reach the stdio/HTTP restart blocks" \
    bash -c "! grep -qE 'preserving stdio|restarting HTTP daemon|HTTP daemon relaunched|No stdio processes' <<< \"$out9a\""

# T9b: a synthetic CHERRY_PICK_HEAD file triggers the same skip via the
# cherry-pick arm.
d9b="$(setup_scratch_repo)"
( cd "$d9b" && : > "$(git rev-parse --git-path CHERRY_PICK_HEAD)" )
out9b="$(run_advisor_in "$d9b")"

assert "T9b: mid-cherry-pick advisor prints the skip line" \
    bash -c "grep -q 'skipping per-commit rebuild' <<< \"$out9b\""
assert "T9b: mid-cherry-pick advisor does NOT run the go build" \
    bash -c "! grep -q 'running go build' <<< \"$out9b\""

# T9c: end-to-end — a real 2-commit rebase with the advisor wired as the
# post-commit hook must invoke `make` ZERO times (no per-pick rebuild).
# T9d (below) is the control: a normal commit on the same wired repo DOES
# invoke make, proving the hook+make wiring works and the guard is
# specific to sequencing ops. (Acceptance: rebasing a >=2-commit branch,
# observing no per-pick rebuild; normal single-commit path unchanged.)
setup_rebase_repo() {
    local d
    d="$(mktemp -d)"
    SCRATCH_DIRS+=("$d")
    pushd "$d" > /dev/null
    git init --quiet
    git config user.email "test@example.com"
    git config user.name  "test"
    git config core.hooksPath "$d/.git/hooks"  # pin hooks to this repo
    echo "seed" > README.md
    git add README.md && git commit --quiet -m "initial"
    echo "main-advance" > main.txt
    git add main.txt && git commit --quiet -m "main advance"
    git tag rebase-target            # tag the upstream tip (default-branch-name agnostic)
    git checkout --quiet -b feature HEAD~1   # branch off pre-advance base
    mkdir -p go/internal/dummy
    echo "package dummy // c1" > go/internal/dummy/dummy.go
    git add go/internal/dummy/dummy.go && git commit --quiet -m "feat c1"
    echo "package dummy // c2" > go/internal/dummy/dummy2.go
    git add go/internal/dummy/dummy2.go && git commit --quiet -m "feat c2"
    popd > /dev/null
    local shim="$d/bin"
    mkdir -p "$shim"
    # make shim records every invocation; $d expands now, $* stays literal.
    cat > "$shim/make" <<EOF
#!/usr/bin/env bash
echo "make \$*" >> "$d/make-calls.log"
exit 0
EOF
    chmod +x "$shim/make"
    cat > "$shim/ss" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
    chmod +x "$shim/ss"
    cat > "$shim/pgrep" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF
    chmod +x "$shim/pgrep"
    # Wire the advisor as the post-commit hook AFTER the setup commits so
    # the setup above doesn't fire it.
    ln -sf "$ADVISOR" "$d/.git/hooks/post-commit"
    echo "$d"
}

d9c="$(setup_rebase_repo)"
(
    cd "$d9c"
    PATH="$d9c/bin:$PATH" TOOLKIT_PRECOMMIT_SKIP_SMOKE=1 \
        git -c advice.detachedHead=false rebase rebase-target > /dev/null 2>&1 || true
)
assert "T9c: real rebase did NOT invoke make (no per-pick rebuild)" \
    bash -c "! [[ -s '$d9c/make-calls.log' ]]"
assert "T9c: rebase actually replayed feature onto main (main.txt present)" \
    test -f "$d9c/main.txt"

# T9d: control — a normal single commit (hook wired, NOT mid-sequence)
# DOES invoke make. Confirms the normal post-commit path is unchanged.
(
    cd "$d9c"
    rm -f make-calls.log
    echo "package dummy // c3" > go/internal/dummy/dummy3.go
    git add go/internal/dummy/dummy3.go
    PATH="$d9c/bin:$PATH" TOOLKIT_PRECOMMIT_SKIP_SMOKE=1 \
        git commit --quiet -m "normal tip commit" > /dev/null 2>&1 || true
)
assert "T9d: a normal (non-sequencing) commit still invokes make (rebuild path intact)" \
    bash -c "[[ -s '$d9c/make-calls.log' ]]"

# ── T10: linked-worktree deploy warning ───────────────────────────────
# Bug worktree-commits-dont-deploy-to-stdio-binary-path-no-staleness-signal:
# a Go-touching commit built from a LINKED WORKTREE must warn that the build
# won't reach the MAIN checkout's deployed go/bin. Control (T10b): the same
# rebuild in the main checkout stays silent on that warning.
d10="$(setup_scratch_repo)"
wt10="$(mktemp -d)/wt"
SCRATCH_DIRS+=("$(dirname "$wt10")")
git -C "$d10" worktree add --quiet -b wt-branch "$wt10" > /dev/null 2>&1
(
    cd "$wt10"
    mkdir -p go/internal/dummy
    echo "package dummy // wt" > go/internal/dummy/dummy_wt.go
    git add go/internal/dummy/dummy_wt.go
    git commit --quiet -m "go change in worktree"
)
out10="$(cd "$wt10" && PATH="$d10/bin:$PATH" TOOLKIT_PRECOMMIT_SKIP_SMOKE=1 "$ADVISOR" "HEAD~1..HEAD" 2>&1 || true)"
assert "T10: advisor warns when a Go commit is built in a linked worktree" \
    bash -c 'printf "%s" "$1" | grep -q "LINKED WORKTREE"' _ "$out10"
assert "T10: warning names the MAIN checkout's deployed binary path" \
    bash -c 'printf "%s" "$1" | grep -qF "'"$d10"'/go/bin/toolkit-server"' _ "$out10"

out10b="$(run_advisor_in "$d10")"
assert "T10b: a main-checkout build does NOT emit the worktree warning" \
    bash -c '! printf "%s" "$1" | grep -q "LINKED WORKTREE"' _ "$out10b"
assert "T10b: a main-checkout build STILL restarts :3000 (default unchanged)" \
    bash -c 'printf "%s" "$1" | grep -q "HTTP daemon relaunched\|no HTTP daemon running"' _ "$out10b"

# ── T10c: a linked-worktree commit SKIPS the shared :3000 restart (bug 936) ──
# Reuses out10 (the advisor run inside the linked worktree wt10). Before the
# fix the advisor restarted :3000 from the worktree binary unconditionally;
# now it detects the linked worktree and skips, pointing at the private-port
# path instead.
assert "T10c: a linked-worktree commit skips the :3000 daemon restart" \
    bash -c 'printf "%s" "$1" | grep -q "skipping :3000 daemon restart"' _ "$out10"
assert "T10c: a linked-worktree commit does NOT relaunch :3000" \
    bash -c '! printf "%s" "$1" | grep -q "HTTP daemon relaunched\|restarting HTTP daemon"' _ "$out10"

# ── T11: TOOLKIT_PRECOMMIT_NO_DAEMON_RESTART=1 opt-out, any checkout (bug 936) ──
d11="$(setup_scratch_repo)"
out11="$(cd "$d11" && PATH="$d11/bin:$PATH" TOOLKIT_PRECOMMIT_SKIP_SMOKE=1 \
    TOOLKIT_PRECOMMIT_NO_DAEMON_RESTART=1 "$ADVISOR" "HEAD~1..HEAD" 2>&1 || true)"
assert "T11: NO_DAEMON_RESTART=1 skips the :3000 restart in a main checkout" \
    bash -c 'printf "%s" "$1" | grep -q "TOOLKIT_PRECOMMIT_NO_DAEMON_RESTART=1"' _ "$out11"
assert "T11: NO_DAEMON_RESTART=1 does NOT relaunch :3000" \
    bash -c '! printf "%s" "$1" | grep -q "HTTP daemon relaunched\|restarting HTTP daemon"' _ "$out11"

# ── Summary ───────────────────────────────────────────────────────────
echo ""
echo "tests: $PASS pass, $FAIL fail"
if [[ "$FAIL" -gt 0 ]]; then
    exit 1
fi
