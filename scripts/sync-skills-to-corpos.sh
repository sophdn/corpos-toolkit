#!/usr/bin/env bash
# sync-skills-to-corpos.sh — copy this repo's canonical toolkit-owned skills
# (skills/<name>/SKILL.md) into a corpos checkout's embed mirror
# (internal/skills/library/<name>/SKILL.md), so a headless corpos loads the
# corrected, canonical text.
#
# corpos-toolkit is canonical for these disciplines (chain 444 T1/T2). corpos
# embeds a COMMITTED real-copy mirror because `//go:embed` needs the files at
# build time and rejects symlinks — the same discipline as this repo's
# migrations mirror (CONVENTIONS.md §Migrations). The mirror is kept honest by
# scripts/check-skills-embed-drift.sh, wired as the `skills-embed-drift` gate.
#
# TRANSFORM: strip inline `<!-- pii-allow ... -->` markers. Those markers waive
# THIS repo's commit-time pii-scan (corpos-toolkit publishes a public mirror and
# keeps real infra strings that publish-public.sh scrubs). corpos has no such
# gate and never publishes publicly, so it takes the real text — but WITHOUT the
# markers, which would otherwise be injected into session prompts as cruft.
#
# COPY-ONLY: never deletes. The synced set is exactly this repo's skills/*/ —
# corpos-OWNED skills (library/userlib general craft) are never touched. If a
# skill LEAVES canonical ownership, remove its corpos copy by hand (rare; the
# drift check will not flag a corpos-only skill).
#
# Usage:
#   [CORPOS_DIR=/path/to/corpos] scripts/sync-skills-to-corpos.sh
# Default CORPOS_DIR=$HOME/dev/corpos. Point it at a corpos worktree to sync
# there and commit via corpos's worktree workflow.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
CANON="$REPO_ROOT/skills"
CORPOS_DIR="${CORPOS_DIR:-$HOME/dev/corpos}"
DEST="$CORPOS_DIR/internal/skills/library"

[ -d "$CANON" ] || { echo "sync-skills: no canonical skills/ at $CANON" >&2; exit 1; }
[ -d "$DEST" ]  || { echo "sync-skills: corpos embed dir not found: $DEST (set CORPOS_DIR)" >&2; exit 1; }

# Strip only pii-allow HTML-comment markers (leading whitespace + the comment,
# up to and including its closing '>'). The markers contain no interior '>'.
strip_markers() { sed -E 's/[[:space:]]*<!-- pii-allow[^>]*>//g' "$1"; }

changed=0
for dir in "$CANON"/*/; do
    name="$(basename "$dir")"
    src="$dir/SKILL.md"
    [ -f "$src" ] || continue
    dst="$DEST/$name/SKILL.md"
    tmp="$(mktemp)"
    strip_markers "$src" > "$tmp"
    if [ ! -f "$dst" ] || ! cmp -s "$tmp" "$dst"; then
        mkdir -p "$DEST/$name"
        cp "$tmp" "$dst"
        echo "sync-skills: wrote $name"
        changed=$((changed + 1))
    fi
    rm -f "$tmp"
done
echo "sync-skills: done ($changed skill(s) written) → $DEST"
