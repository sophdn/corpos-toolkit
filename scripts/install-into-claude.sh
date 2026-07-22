#!/usr/bin/env bash
# scripts/install-into-claude.sh — install this machine's agent runtime
# dependencies (skills, hooks) into ~/.claude/.
#
# ── What this installs, and in which DIRECTION ────────────────────────────────
#
# ~/.claude/skills is a TWO-HALVED tree (chain 444 T4), and the halves flow
# opposite ways. This script installs only the downstream half:
#
#   INSTALLED HERE (24 skills) — canonical in a repo, copied into ~/.claude,
#     gitignored there. Sources, in precedence order:
#       1. $REPO_ROOT/skills/<name>/         — toolkit-core (7). THIS repo is
#          canonical; pii-allow markers are stripped on the way out.
#       2. $CORPOS_DIR/internal/skills/library/<name>/ — everything else corpos
#          owns. (corpos's copies of the 7 are the T3 mirror of source 1, kept
#          honest by the skills-embed-drift gate, so precedence rarely bites.)
#
#   NOT INSTALLED (19 skills) — ~/.claude is CANONICAL for these and they are
#     git-tracked there: the 18 operator stack skills (corpos embeds them from
#     internal/skills/userlib/, which is gitignored in corpos, so corpos holds
#     the copy) plus corpos-swap-rehearsal, which is temporary and homed only in
#     the overlay. This script refuses to touch anything ~/.claude tracks.
#
# HOOKS are installed as SYMLINKS into this repo, deliberately: a hook IS the
# repo's script, and editing ~/.claude/hooks/<x>.sh SHOULD show up as `M hooks/
# <x>.sh` in this repo's `git status` and be committed here. (That is the
# original warning from bug editing-a-skill-via-claude-skills-name-silently-
# edits-the-git-tracked-mcp, and for hooks it is still exactly right. For SKILLS
# it inverted — the old symlink farm went stale and edits landed in untracked
# local files instead — which is why skills are copies now and ~/.claude/
# CLAUDE.md § "Skills" tells you to edit the canonical copy.)
#
# PERSONAS no longer exist. The role/persona system was retired wholesale on
# 2026-07-22 — ~/.claude/personas/, its /role-* commands, and every reference in
# these repos are gone. Nothing here installs or expects them. If a role idiom
# comes back it will be designed fresh, not restored from this shape.
#
# ── Disaster recovery ─────────────────────────────────────────────────────────
#   git clone <gitea-remote>/corpos-toolkit ~/dev/corpos-toolkit
#   git clone <gitea-remote>/corpos         ~/dev/corpos      # for the other 17
#   bash ~/dev/corpos-toolkit/scripts/install-into-claude.sh
#   # → merge the printed settings.json snippet into ~/.claude/settings.json
# The 19 overlay-canonical skills come back from the ~/.claude repo itself
# (<gitea-remote>/dotclaude.git), not from here.
#
# ── Properties ────────────────────────────────────────────────────────────────
#   - Idempotent: re-running with no underlying changes writes nothing.
#   - Never clobbers a git-tracked ~/.claude entry, and never deletes.
#   - Warns when an installed skill is not gitignored in ~/.claude — that means
#     the two halves have drifted and ~/.claude/.gitignore needs the new name.
#   - --dry-run: report what WOULD happen; make no changes.
#   - --target <path>: override the install root (default: ~/.claude).
#   - --corpos <path>: override the corpos checkout (default: ~/dev/corpos, or
#     $CORPOS_DIR). Absent corpos → the 7 toolkit-core skills still install and
#     the rest are loudly skipped.
#   - Does NOT auto-write settings.json; prints a snippet to merge by hand
#     (auto-mode self-modification guard).
#
# Exit codes:
#   0 — everything installed or already current.
#   1 — at least one conflict; install partial.
#   2 — fatal (run from a linked worktree, no source tree, bad arg).
#
# Authored by chain reference-resolution-migration T3; rebuilt manifest-free by
# chain 444 T6. The _manifest.toml this script used to read never existed in
# this repo and no consumer honors one (corpos's loader defers bucket support:
# internal/skills/skills.go), so the registry indirection is gone.

set -uo pipefail

# Scrub inherited git env FIRST. When this runs under a git hook (the pre-commit
# gate exercises it), git exports GIT_DIR / GIT_INDEX_FILE / GIT_WORK_TREE — and
# those OVERRIDE `git -C <path>`, so every query about the install target would
# silently answer about the hook's repo instead. That made `tracked_in_target`
# return true for skills tracked HERE and refuse to install them. Same class as
# bug 921/937, which precommit.sh scrubs for the same reason.
unset GIT_DIR GIT_INDEX_FILE GIT_WORK_TREE GIT_COMMON_DIR GIT_PREFIX \
      GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TARGET="$HOME/.claude"
CORPOS_DIR="${CORPOS_DIR:-$HOME/dev/corpos}"
DRY_RUN=0

while [[ $# -gt 0 ]]; do
    case "$1" in
        --dry-run) DRY_RUN=1; shift ;;
        --target) TARGET="$2"; shift 2 ;;
        --target=*) TARGET="${1#--target=}"; shift ;;
        --corpos) CORPOS_DIR="$2"; shift 2 ;;
        --corpos=*) CORPOS_DIR="${1#--corpos=}"; shift ;;
        --help|-h)
            echo "Usage: $0 [--dry-run] [--target=<path>] [--corpos=<path>]"
            echo ""
            echo "  --dry-run        Report what would happen; make no changes."
            echo "  --target <path>  Override install root (default: ~/.claude)."
            echo "  --corpos <path>  Override corpos checkout (default: ~/dev/corpos)."
            exit 0
            ;;
        *) echo "ERROR: unknown arg: $1" >&2; exit 2 ;;
    esac
done

# A linked worktree is a fine source for SKILLS (they are copied), but a fatal
# source for HOOKS: the symlinks would point into a tree worktree-merge.sh
# reaps, silently disabling every hook. Skip that half rather than refuse the
# run, so the script stays testable from a worktree.
IN_WORKTREE=0
if [[ "$(git -C "$REPO_ROOT" rev-parse --git-dir 2>/dev/null)" \
   != "$(git -C "$REPO_ROOT" rev-parse --git-common-dir 2>/dev/null)" ]]; then
    IN_WORKTREE=1
fi

echo "[install] repo:        $REPO_ROOT"
echo "[install] corpos:      $CORPOS_DIR"
echo "[install] target root: $TARGET"
[[ $DRY_RUN -eq 1 ]] && echo "[install] DRY-RUN mode — no changes will be made"
echo ""

CONFLICTS=0
WROTE=0
CURRENT=0
SKIPPED=0

# tracked_in_target <relpath> — true if ~/.claude's git index owns this path.
# That is the authoritative "this half is canonical HERE, hands off" signal.
tracked_in_target() {
    git -C "$TARGET" ls-files --error-unmatch "$1" >/dev/null 2>&1
}

# Strip the pii-allow markers this repo carries for its public-mirror scan.
# Same transform as sync-skills-to-corpos.sh — the overlay shouldn't get gate
# cruft injected into session prompts either.
strip_markers() { sed -E 's/[[:space:]]*<!-- pii-allow[^>]*>//g' "$1"; }

# ── skills ────────────────────────────────────────────────────────────────────
# Build the source map: name → absolute source dir. corpos first, this repo
# second so the toolkit-core canonical wins on a collision.
declare -A SKILL_SRC=()
if [[ -d "$CORPOS_DIR/internal/skills/library" ]]; then
    for dir in "$CORPOS_DIR/internal/skills/library"/*/; do
        [[ -f "$dir/SKILL.md" ]] || continue
        SKILL_SRC["$(basename "$dir")"]="${dir%/}"
    done
else
    echo "[install] WARNING: no corpos checkout at $CORPOS_DIR —" >&2
    echo "[install]          installing only this repo's toolkit-core skills." >&2
    echo "[install]          The other 17 come from corpos; clone it and re-run." >&2
fi
for dir in "$REPO_ROOT"/skills/*/; do
    [[ -f "$dir/SKILL.md" ]] || continue
    SKILL_SRC["$(basename "$dir")"]="${dir%/}"
done

if [[ ${#SKILL_SRC[@]} -eq 0 ]]; then
    echo "ERROR: no skill sources found in either repo." >&2
    exit 2
fi

install_skill() {
    local name="$1" src="$2"
    local dst="$TARGET/skills/$name"

    if tracked_in_target "skills/$name"; then
        echo "[install] skill $name: CONFLICT — git-tracked in $TARGET"
        echo "[install]   $TARGET is canonical for this skill; refusing to overwrite."
        echo "[install]   If ownership really moved to a repo, untrack it there first."
        CONFLICTS=$((CONFLICTS + 1))
        return
    fi
    if [[ -L "$dst" ]] || [[ -e "$dst" && ! -d "$dst" ]]; then
        echo "[install] skill $name: CONFLICT — target exists and is not a plain directory"
        echo "[install]   target: $dst"
        echo "[install]   action: mv \"$dst\" \"$dst.bak.\$(date +%s)\" and re-run"
        CONFLICTS=$((CONFLICTS + 1))
        return
    fi

    # Render the source into a staging dir, applying the marker strip to every
    # markdown file, then compare wholesale so idempotency is exact.
    local stage rel
    stage="$(mktemp -d)"
    while IFS= read -r -d '' rel; do
        mkdir -p "$stage/$(dirname "$rel")"
        case "$rel" in
            *.md) strip_markers "$src/$rel" > "$stage/$rel" ;;
            *)    cp -p "$src/$rel" "$stage/$rel" ;;
        esac
    done < <(cd "$src" && find . -type f -printf '%P\0')

    if [[ -d "$dst" ]] && diff -rq "$stage" "$dst" >/dev/null 2>&1; then
        CURRENT=$((CURRENT + 1))
        rm -rf "$stage"
        return
    fi
    if [[ $DRY_RUN -eq 1 ]]; then
        echo "[install]   would install skill: $dst  ← ${src/#$HOME/\~}"
    else
        mkdir -p "$dst"
        cp -a "$stage/." "$dst/"
        echo "[install] skill $name: installed"
    fi
    WROTE=$((WROTE + 1))
    rm -rf "$stage"
}

echo "[install] skills (${#SKILL_SRC[@]} from repos → $TARGET/skills)"
for name in $(printf '%s\n' "${!SKILL_SRC[@]}" | sort); do
    install_skill "$name" "${SKILL_SRC[$name]}"
done

# Honesty check: every skill we install must be gitignored in ~/.claude, or the
# .gitignore declaration and the actual install set have drifted apart.
if [[ -d "$TARGET/.git" ]]; then
    for name in $(printf '%s\n' "${!SKILL_SRC[@]}" | sort); do
        tracked_in_target "skills/$name" && continue
        if ! git -C "$TARGET" check-ignore -q "skills/$name/"; then
            echo "[install] WARNING: skills/$name is installed but NOT gitignored in $TARGET." >&2
            echo "[install]          Add it to $TARGET/.gitignore — otherwise it reads as" >&2
            echo "[install]          overlay-canonical and a future edit will be made in the" >&2
            echo "[install]          copy and lost. (See ~/.claude/CLAUDE.md § Skills.)" >&2
        fi
    done
fi

# ── hooks ─────────────────────────────────────────────────────────────────────
echo ""
install_hook() {
    local name="$1"
    local src="$REPO_ROOT/hooks/$name"
    local dst="$TARGET/hooks/$name"

    if [[ -L "$dst" ]]; then
        if [[ "$(readlink "$dst")" == "$src" ]]; then
            CURRENT=$((CURRENT + 1))
            return
        fi
        echo "[install] hook $name: SYMLINK DIFFERS"
        echo "[install]   current: $(readlink "$dst")"
        echo "[install]   want:    $src"
        echo "[install]   action:  rm the old link and re-run"
        CONFLICTS=$((CONFLICTS + 1))
        return
    fi
    if [[ -e "$dst" ]]; then
        echo "[install] hook $name: CONFLICT — target exists and is NOT a symlink"
        echo "[install]   action: mv \"$dst\" \"$dst.bak.\$(date +%s)\" and re-run"
        CONFLICTS=$((CONFLICTS + 1))
        return
    fi
    if [[ $DRY_RUN -eq 1 ]]; then
        echo "[install]   would link: $dst → $src"
    else
        mkdir -p "$(dirname "$dst")"
        ln -s "$src" "$dst"
        echo "[install] hook $name: linked"
    fi
    WROTE=$((WROTE + 1))
}

if [[ $IN_WORKTREE -eq 1 ]]; then
    echo "[install] hooks: SKIPPED — $REPO_ROOT is a LINKED WORKTREE."
    echo "[install]   Symlinks into it would break the moment the worktree is"
    echo "[install]   reaped, silently disabling every hook. Re-run from the"
    echo "[install]   main checkout to install hooks."
    for src in "$REPO_ROOT"/hooks/*.sh; do
        [[ "$(basename "$src")" == test-* ]] || SKIPPED=$((SKIPPED + 1))
    done
else
    echo "[install] hooks (symlinks → $REPO_ROOT/hooks)"
    for src in "$REPO_ROOT"/hooks/*.sh; do
        name="$(basename "$src")"
        # test-*.sh are this repo's hook test harnesses, not installable hooks.
        [[ "$name" == test-* ]] && { SKIPPED=$((SKIPPED + 1)); continue; }
        install_hook "$name"
    done
fi

echo ""
echo "[install] summary: $WROTE written, $CURRENT already current, $SKIPPED skipped, $CONFLICTS conflicts"

if [[ $CONFLICTS -eq 0 ]]; then
    cat <<'JSON'

[install] settings.json snippet to merge into ~/.claude/settings.json:

{
  "hooks": {
    "SessionStart": [
      { "hooks": [ { "type": "command", "command": "$HOME/.claude/hooks/materialize-memory.sh" } ] }
    ],
    "Stop": [
      { "hooks": [
          { "type": "command", "command": "$HOME/.claude/hooks/grounding-events-processor.sh" },
          { "type": "command", "command": "$HOME/.claude/hooks/arc-close-filing-review-hook.sh" }
      ] }
    ],
    "UserPromptSubmit": [
      { "hooks": [ { "type": "command", "command": "$HOME/.claude/hooks/edit-drift-detector.sh" } ] }
    ],
    "PostToolUse": [
      { "hooks": [ { "type": "command", "command": "$HOME/.claude/hooks/edit-drift-detector.sh" } ] }
    ]
  }
}
JSON
    echo ""
    echo "[install] note: this repo also ships hooks the snippet deliberately omits"
    echo "[install]       (arc-close-detector, gradient-question-guard,"
    echo "[install]       pending-decisions-drain-hook, toolkit-binary-staleness-check)."
    echo "[install]       They are installed as rollback/opt-in artifacts; wire them"
    echo "[install]       up only on purpose."
    echo ""
fi

[[ $CONFLICTS -gt 0 ]] && exit 1
exit 0
