#!/usr/bin/env bash
# scripts/test-cmd-gitignore-parity.sh — assert go/.gitignore covers every
# cmd binary (bug 736).
#
# `go build ./cmd/<name>/` invoked from go/ WITHOUT `-o` names the output after
# the package's directory and drops it at go/<name> (~14 MB). Those land as
# untracked `??` entries forever and risk an accidental `git add .`. The fix
# (05febd1) added per-cmd ignore lines to go/.gitignore, but a hand-maintained
# "one entry per cmd" list DRIFTS — every new cmd/<name>/ needs a remembered
# gitignore edit, and ~18 had been missed by the time this gate was added.
#
# This is the ratchet that kills the drift: it fails when any go/cmd/<name>/ has
# no matching root-anchored ignore entry in go/.gitignore, so a new cmd dir
# without coverage can't land. Mirrors the runtime-affecting-paths parity check;
# wired into scripts/precommit.sh.
#
# Matching honors globs: an entry like `/curate-*` or `/*-audit-emit` covers
# every cmd whose name matches the pattern, so cmd families don't need a line
# each.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GITIGNORE="$REPO_ROOT/go/.gitignore"
CMD_DIR="$REPO_ROOT/go/cmd"

if [[ ! -f "$GITIGNORE" ]]; then
    echo "FATAL: go/.gitignore not found: $GITIGNORE" >&2
    exit 1
fi
if [[ ! -d "$CMD_DIR" ]]; then
    echo "FATAL: go/cmd not found: $CMD_DIR" >&2
    exit 1
fi

# Root-anchored, single-segment ignore patterns (lines like `/toolkit-server`,
# `/curate-*`, `/*-audit-emit`) with the leading slash stripped. These are the
# stray-binary patterns; `bin/`, `*.test`, `/report.json` etc. are collected too
# but simply won't match any cmd name.
mapfile -t patterns < <(grep -E '^/[^/]+$' "$GITIGNORE" | sed 's#^/##')

shopt -s extglob
covered() {
    local name="$1"
    for p in "${patterns[@]}"; do
        # shellcheck disable=SC2053  # intentional glob match of name vs pattern
        [[ "$name" == $p ]] && return 0
    done
    return 1
}

FAIL=0
count=0
for d in "$CMD_DIR"/*/; do
    name="$(basename "$d")"
    count=$((count + 1))
    if ! covered "$name"; then
        echo "FAIL: go/cmd/$name/ has no matching entry in go/.gitignore — add '/$name' (a stray 'go build ./cmd/$name/' from go/ leaves go/$name untracked, bug 736)" >&2
        FAIL=1
    fi
done

if [[ $FAIL -eq 0 ]]; then
    echo "go/.gitignore covers all $count cmd stray-binary paths"
fi
exit $FAIL
