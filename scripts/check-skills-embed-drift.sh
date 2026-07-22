#!/usr/bin/env bash
# check-skills-embed-drift.sh — read-only drift gate (corpos-gate custom guard
# `skills-embed-drift`). Every canonical toolkit-owned skill (this repo's
# skills/<name>/SKILL.md) must be byte-identical — AFTER pii-allow-marker
# stripping — to corpos's embedded mirror (internal/skills/library/<name>).
# Fails naming the drifted skill(s) and the sync command.
#
# Editing a canonical skill without running sync-skills-to-corpos.sh (and
# committing the corpos side) trips this guard on the next corpos-toolkit commit.
#
# ENFORCEMENT SCOPE: this is a DEV-MACHINE guard — it can only compare against a
# corpos checkout that is present on disk. In CI / the container (no sibling
# corpos), it LOUDLY skips rather than failing an unrelated build. Canonical
# edits + commits happen on the dev box where both repos live, which is exactly
# where the guard bites.
#
# Usage: [CORPOS_DIR=/path/to/corpos] scripts/check-skills-embed-drift.sh
# Default CORPOS_DIR=$HOME/dev/corpos.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
CANON="$REPO_ROOT/skills"
CORPOS_DIR="${CORPOS_DIR:-$HOME/dev/corpos}"
DEST="$CORPOS_DIR/internal/skills/library"

[ -d "$CANON" ] || { echo "[skills-embed-drift] no canonical skills/ — nothing to check."; exit 0; }
if [ ! -d "$DEST" ]; then
    echo "[skills-embed-drift] corpos checkout not found at $CORPOS_DIR — SKIP (set CORPOS_DIR to enforce)." >&2
    exit 0
fi

strip_markers() { sed -E 's/[[:space:]]*<!-- pii-allow[^>]*>//g' "$1"; }

drift=()
for dir in "$CANON"/*/; do
    name="$(basename "$dir")"
    src="$dir/SKILL.md"
    [ -f "$src" ] || continue
    dst="$DEST/$name/SKILL.md"
    if [ ! -f "$dst" ] || ! cmp -s <(strip_markers "$src") "$dst"; then
        drift+=("$name")
    fi
done

if [ "${#drift[@]}" -gt 0 ]; then
    {
        echo "[skills-embed-drift] corpos embed is STALE vs canonical skills/ for:"
        for n in "${drift[@]}"; do echo "  - $n"; done
        echo "Fix: CORPOS_DIR=$CORPOS_DIR scripts/sync-skills-to-corpos.sh   then commit the corpos change."
    } >&2
    exit 1
fi
echo "[skills-embed-drift] OK — corpos embed matches canonical (skills/, ${#drift[@]} drift)."
exit 0
