#!/usr/bin/env bash
# scripts/test-worktree-merge.sh — hermetic multi-"agent" dry-run for the
# worktree-merge.sh capstone (chain worktree-multi-agent-orchestration-support
# T8). Builds throwaway git repos in /tmp whose linked worktrees stand in for
# parallel subagents, then drives the full spawn → conflict-check → merge-back
# → cleanup cycle and asserts it completes with ZERO manual intervention:
# core.bare is auto-reset, disjoint work merges, the conflict surface is
# detected (overlapping files + duplicate migration numbers), and the
# worktrees/branches are reaped.
#
# Hermetic and repeatable: it never touches this repo, its main branch, or any
# remote. Exit 0 on all-pass, 1 on any failure.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HELPER="$REPO_ROOT/scripts/worktree-merge.sh"
PASS=0
FAIL=0
SCRATCH=()

cleanup() { for d in "${SCRATCH[@]:-}"; do [[ -n "$d" && -d "$d" ]] && rm -rf "$d"; done; }
trap cleanup EXIT

assert() {
    local desc="$1"; shift
    if "$@"; then echo "  PASS  $desc"; PASS=$((PASS + 1));
    else echo "  FAIL  $desc"; FAIL=$((FAIL + 1)); fi
}

# new_repo creates a throwaway repo with a seed commit + the migrations dir,
# and prints its path.
new_repo() {
    local d; d="$(mktemp -d)"; SCRATCH+=("$d")
    git -C "$d" init -q
    git -C "$d" config user.email t@example.com
    git -C "$d" config user.name test
    git -C "$d" config commit.gpgsign false
    mkdir -p "$d/go/internal/db/migrations"
    echo seed > "$d/README.md"
    git -C "$d" add -A
    git -C "$d" commit -q -m seed
    echo "$d"
}

# spawn_agent adds a worktree+branch and commits the given file::content pairs,
# simulating a parallel subagent's disjoint work.
spawn_agent() {
    local d="$1" b="$2"; shift 2
    git -C "$d" worktree add -q "$d/.wt/$b" -b "$b" >/dev/null 2>&1
    local fc f c
    for fc in "$@"; do
        f="${fc%%::*}"; c="${fc#*::}"
        mkdir -p "$(dirname "$d/.wt/$b/$f")"
        printf '%s\n' "$c" > "$d/.wt/$b/$f"
    done
    git -C "$d/.wt/$b" add -A
    git -C "$d/.wt/$b" commit -q -m "$b work"
}

echo "── Scenario 1: clean disjoint work (+ distinct migration numbers) ──"
d1="$(new_repo)"
spawn_agent "$d1" agent-a "featureA.txt::A" "go/internal/db/migrations/080_alpha.sql::-- a"
spawn_agent "$d1" agent-b "featureB.txt::B" "go/internal/db/migrations/081_beta.sql::-- b"
# Simulate the Agent-tool's core.bare flip on the shared config.
git -C "$d1" config core.bare true
rc=0
( cd "$d1" && "$HELPER" --no-gate agent-a agent-b ) > "$d1/out.log" 2>&1 || rc=$?
cat "$d1/out.log" | sed 's/^/    │ /'
assert "clean: helper exits 0" test "$rc" -eq 0
assert "clean: core.bare reset to false" test "$(git -C "$d1" config core.bare)" = "false"
assert "clean: featureA merged onto main" test -f "$d1/featureA.txt"
assert "clean: featureB merged onto main" test -f "$d1/featureB.txt"
assert "clean: 080 migration merged" test -f "$d1/go/internal/db/migrations/080_alpha.sql"
assert "clean: 081 migration merged" test -f "$d1/go/internal/db/migrations/081_beta.sql"
assert "clean: branch agent-a reaped" bash -c "! git -C '$d1' rev-parse --verify agent-a >/dev/null 2>&1"
assert "clean: branch agent-b reaped" bash -c "! git -C '$d1' rev-parse --verify agent-b >/dev/null 2>&1"
assert "clean: worktree dirs reaped" bash -c "! test -d '$d1/.wt/agent-a' && ! test -d '$d1/.wt/agent-b'"

echo "── Scenario 2: duplicate migration number is flagged by --check-only ──"
d2="$(new_repo)"
spawn_agent "$d2" agent-a "x.txt::A" "go/internal/db/migrations/080_alpha.sql::-- a"
spawn_agent "$d2" agent-b "y.txt::B" "go/internal/db/migrations/080_beta.sql::-- b"
rc=0
( cd "$d2" && "$HELPER" --check-only agent-a agent-b ) > "$d2/out.log" 2>&1 || rc=$?
cat "$d2/out.log" | sed 's/^/    │ /'
assert "dup-migration: --check-only exits 1" test "$rc" -eq 1
assert "dup-migration: report names number 080" grep -q "number 080" "$d2/out.log"
assert "dup-migration: nothing merged (x.txt absent on main)" bash -c "! test -f '$d2/x.txt'"

echo "── Scenario 3: overlapping file is flagged by --check-only ──"
d3="$(new_repo)"
spawn_agent "$d3" agent-a "shared.txt::from A"
spawn_agent "$d3" agent-b "shared.txt::from B"
rc=0
( cd "$d3" && "$HELPER" --check-only agent-a agent-b ) > "$d3/out.log" 2>&1 || rc=$?
cat "$d3/out.log" | sed 's/^/    │ /'
assert "overlap: --check-only exits 1" test "$rc" -eq 1
assert "overlap: report names shared.txt" grep -q "shared.txt" "$d3/out.log"

echo ""
echo "dry-run: $PASS pass, $FAIL fail"
[[ "$FAIL" -eq 0 ]]
