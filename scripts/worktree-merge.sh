#!/usr/bin/env bash
# scripts/worktree-merge.sh — the canonical multi-agent merge-back helper.
#
# CAPSTONE of chain worktree-multi-agent-orchestration-support (T8). Codifies
# the spawn → conflict-check → merge-back → cleanup ritual so a multi-subagent
# run integrates with ZERO manual core.bare / migration / registry babysitting:
#
#   1. Reset core.bare (the Agent-tool's isolation=worktree flips it to true on
#      the shared .git/config; left flipped, the main checkout can't merge —
#      "this operation must be run in a work tree"). Idempotent.
#   2. Pre-merge CONFLICT-SURFACE CHECK across the named branches: files touched
#      by 2+ branches, and duplicate migration NNN numbers. (T3 makes migration
#      collisions improbable and T7 made the registry guards conflict-free, so
#      this is a fast safety net, not the primary defense.)
#   3. Ordered MERGE into the current checkout: fast-forward to the first branch
#      when possible, --no-ff the rest. Aborts cleanly on a real conflict.
#   4. Build/test GATE on the merged result (make -C go build && test) when a Go
#      module is present.
#   5. REAP the merged worktrees + branches (handles the Agent-tool's locks).
#
# Usage:
#   scripts/worktree-merge.sh [options] <branch|worktree-path>...
#
# Options:
#   --check-only    Run steps 1-2 (core.bare reset + conflict surface) and stop.
#                   The "dry-run" pre-spawn/pre-merge check. Exit 1 if conflicts.
#   --no-gate       Skip the build/test gate (step 4).
#   --no-reap       Merge but leave the worktrees/branches in place (step 5 off).
#   --gate-cmd CMD  Override the gate command (default: make -C go build && test).
#
# Merges into whatever the current checkout has checked out — run it from the
# integration target (normally the main checkout). It never pushes and never
# touches a remote.

set -euo pipefail

CHECK_ONLY=0
DO_GATE=1
DO_REAP=1
GATE_CMD=""
BRANCHES=()

while [[ $# -gt 0 ]]; do
    case "$1" in
        --check-only) CHECK_ONLY=1 ;;
        --no-gate)    DO_GATE=0 ;;
        --no-reap)    DO_REAP=0 ;;
        --gate-cmd)   shift; GATE_CMD="${1:-}" ;;
        --*)          echo "worktree-merge: unknown option: $1" >&2; exit 2 ;;
        *)            BRANCHES+=("$1") ;;
    esac
    shift
done

if [[ ${#BRANCHES[@]} -eq 0 ]]; then
    echo "worktree-merge: name at least one branch or worktree path to merge" >&2
    echo "  usage: scripts/worktree-merge.sh [--check-only|--no-gate|--no-reap] <branch|path>..." >&2
    exit 2
fi

# ── Step 1: core.bare auto-reset (must run FIRST) ──────────────────────
# The Agent tool's isolation=worktree flips core.bare=true on the shared
# config; left flipped, `git rev-parse --show-toplevel` (and the merge) fail
# with "this operation must be run in a work tree". `git config` is plumbing
# that works regardless, so reset it BEFORE any work-tree-requiring command.
# Idempotent.
if [[ "$(git config core.bare 2>/dev/null || echo false)" == "true" ]]; then
    git config core.bare false
    echo "worktree-merge: reset core.bare true → false (Agent-tool flip)"
fi

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

target_branch="$(git branch --show-current 2>/dev/null || echo 'DETACHED')"
echo "worktree-merge: integration target = $target_branch ($REPO_ROOT)"

# Resolve each argument to a branch name. A path that is a registered worktree
# resolves to that worktree's checked-out branch; anything else is treated as a
# branch name directly. Records the worktree path (when known) for reaping.
declare -a MERGE_BRANCHES=()
declare -A WORKTREE_OF=()
for arg in "${BRANCHES[@]}"; do
    if [[ -d "$arg" ]]; then
        b="$(git -C "$arg" branch --show-current 2>/dev/null || true)"
        if [[ -z "$b" ]]; then
            echo "worktree-merge: $arg is not on a branch (detached?) — skipping" >&2
            continue
        fi
        MERGE_BRANCHES+=("$b")
        WORKTREE_OF["$b"]="$(cd "$arg" && pwd)"
    else
        MERGE_BRANCHES+=("$arg")
        # Discover the worktree hosting this branch (for reaping), if any.
        wt="$(git worktree list --porcelain | awk -v b="refs/heads/$arg" '
            /^worktree /{p=$2} /^branch /{ if($2==b) print p }')"
        [[ -n "$wt" ]] && WORKTREE_OF["$arg"]="$wt"
    fi
done

if [[ ${#MERGE_BRANCHES[@]} -eq 0 ]]; then
    echo "worktree-merge: no mergeable branches resolved" >&2
    exit 2
fi

# ── Step 2: pre-merge conflict-surface check ───────────────────────────
echo ""
echo "worktree-merge: conflict-surface check across ${#MERGE_BRANCHES[@]} branch(es)…"
conflicts=0

# (a) Files touched by 2+ branches (vs the merge base with the target).
tmp_files="$(mktemp)"
: > "$tmp_files"
for b in "${MERGE_BRANCHES[@]}"; do
    base="$(git merge-base HEAD "$b" 2>/dev/null || echo HEAD)"
    git diff --name-only "$base" "$b" | sed "s|\$|\t$b|"
done > "$tmp_files"
overlap="$(cut -f1 "$tmp_files" | sort | uniq -d || true)"
if [[ -n "$overlap" ]]; then
    echo "  ⚠ files modified by more than one branch (textual merge may conflict):"
    while IFS= read -r f; do
        [[ -z "$f" ]] && continue
        branches_for_f="$(awk -F'\t' -v f="$f" '$1==f{printf " %s",$2}' "$tmp_files")"
        echo "      $f →$branches_for_f"
        conflicts=1
    done <<< "$overlap"
else
    echo "  ✓ no file touched by more than one branch"
fi
rm -f "$tmp_files"

# (b) Duplicate migration numbers across branches (NNN_ prefix collision).
mig_tmp="$(mktemp)"
: > "$mig_tmp"
for b in "${MERGE_BRANCHES[@]}"; do
    base="$(git merge-base HEAD "$b" 2>/dev/null || echo HEAD)"
    git diff --name-only --diff-filter=A "$base" "$b" -- 'go/internal/db/migrations/*.sql' \
      | sed -E 's|.*/([0-9]+)_.*\.sql$|\1|' | grep -E '^[0-9]+$' | sed "s|\$|\t$b|" || true
done > "$mig_tmp"
dup_mig="$(cut -f1 "$mig_tmp" | sort | uniq -d || true)"
if [[ -n "$dup_mig" ]]; then
    echo "  ⚠ duplicate migration number(s) across branches — renumber before merge:"
    while IFS= read -r n; do
        [[ -z "$n" ]] && continue
        echo "      number $n added by:$(awk -F'\t' -v n="$n" '$1==n{printf " %s",$2}' "$mig_tmp")"
        conflicts=1
    done <<< "$dup_mig"
else
    echo "  ✓ no duplicate migration numbers"
fi
rm -f "$mig_tmp"

if [[ "$CHECK_ONLY" -eq 1 ]]; then
    echo ""
    if [[ "$conflicts" -eq 1 ]]; then
        echo "worktree-merge: --check-only → CONFLICTS PRESENT (resolve before merge-back)"
        exit 1
    fi
    echo "worktree-merge: --check-only → clean (safe to merge-back)"
    exit 0
fi

if [[ "$conflicts" -eq 1 ]]; then
    echo ""
    echo "worktree-merge: conflict surface non-empty — refusing to auto-merge." >&2
    echo "  Resolve the overlaps above (or renumber migrations) and re-run, or merge by hand." >&2
    exit 1
fi

# ── Step 3: ordered merge ──────────────────────────────────────────────
echo ""
first=1
for b in "${MERGE_BRANCHES[@]}"; do
    if [[ "$first" -eq 1 ]] && git merge --ff-only "$b" 2>/dev/null; then
        echo "worktree-merge: fast-forwarded to $b"
    else
        if ! git merge --no-ff -m "merge(worktree): integrate $b" "$b"; then
            echo "worktree-merge: MERGE CONFLICT on $b — aborting this merge." >&2
            git merge --abort 2>/dev/null || true
            echo "  Resolve manually; already-merged branches were kept." >&2
            exit 1
        fi
        echo "worktree-merge: merged $b (--no-ff)"
    fi
    first=0
done

# ── Step 4: build/test gate ────────────────────────────────────────────
if [[ "$DO_GATE" -eq 1 ]]; then
    if [[ -n "$GATE_CMD" ]]; then
        echo ""
        echo "worktree-merge: gate → $GATE_CMD"
        bash -c "$GATE_CMD"
    elif [[ -f go/go.mod ]]; then
        echo ""
        echo "worktree-merge: gate → make -C go build && make -C go test"
        make -C go build && make -C go test
    else
        echo "worktree-merge: no go/go.mod and no --gate-cmd — skipping gate."
    fi
else
    echo "worktree-merge: --no-gate — skipping build/test gate."
fi

# ── Step 5: reap merged worktrees + branches ───────────────────────────
if [[ "$DO_REAP" -eq 1 ]]; then
    echo ""
    for b in "${MERGE_BRANCHES[@]}"; do
        wt="${WORKTREE_OF[$b]:-}"
        if [[ -n "$wt" && -d "$wt" ]]; then
            # --force handles the Agent-tool's worktree lock; safe because the
            # branch's commits are now merged into the integration target.
            if git worktree remove --force "$wt" 2>/dev/null; then
                echo "worktree-merge: removed worktree $wt"
            else
                echo "worktree-merge: could not remove worktree $wt (left in place)" >&2
            fi
        fi
        if git branch -d "$b" 2>/dev/null; then
            echo "worktree-merge: deleted merged branch $b"
        else
            echo "worktree-merge: branch $b not fully merged or in use — left in place" >&2
        fi
    done
    git worktree prune
else
    echo "worktree-merge: --no-reap — worktrees/branches left in place."
fi

echo ""
echo "worktree-merge: done — ${#MERGE_BRANCHES[@]} branch(es) integrated into $target_branch."
