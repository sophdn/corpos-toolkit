#!/usr/bin/env bash
# scripts/worktree-setup.sh — bootstrap a linked git worktree for the full gate.
#
# Bug 938 (chain worktree-multi-agent-orchestration-support T5). Because
# core.hooksPath is set in the SHARED config to the main checkout's .git/hooks,
# a worktree commit fires the post-commit advisor, which restarts the shared
# :3000 daemon with the worktree's binary (bug 936). This script makes a
# worktree turnkey: run it ONCE from inside the worktree and `git commit` there
# is gate-only.
#
#   cd <worktree> && ./scripts/worktree-setup.sh
#
# What it does (idempotent; the main checkout's state is never touched):
#   - Installs a GATE-ONLY hooks dir (pre-commit -> scripts/precommit.sh; NO
#     post-commit advisor) in this worktree's private git dir, and points THIS
#     worktree's core.hooksPath at it via per-worktree config. The full quality
#     gate still runs on every commit; the advisor never does, so a worktree
#     commit never hijacks the shared :3000 daemon.
#
# (The old apps/dashboard/node_modules symlink step was dropped when the
# frontend split out to sophdn/corpos-toolkit-dashboard — this is a Go-only repo, so a
# worktree needs no JS deps. See docs/MULTI_AGENT_WORKTREE_WORKFLOW.md §4.)

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

GIT_COMMON="$(git rev-parse --path-format=absolute --git-common-dir)"
case "$GIT_COMMON" in
    */.git) MAIN_ROOT="$(dirname "$GIT_COMMON")" ;;
    *)      MAIN_ROOT="" ;;
esac

if [[ -z "$MAIN_ROOT" || "$MAIN_ROOT" == "$REPO_ROOT" ]]; then
    echo "worktree-setup: this looks like the MAIN checkout ($REPO_ROOT), not a" >&2
    echo "                linked worktree. Run it from inside a 'git worktree add'" >&2
    echo "                checkout — the main checkout already has its hooks wired." >&2
    exit 1
fi

echo "worktree-setup: main checkout = $MAIN_ROOT"
echo "worktree-setup: this worktree = $REPO_ROOT"

# ── gate-only hooks dir + per-worktree core.hooksPath ─────────────────
# The hooks dir lives in this worktree's PRIVATE git dir
# (.git/worktrees/<name>/), so it is invisible to the main checkout and to
# version control, and is removed with the worktree.
GIT_DIR="$(git rev-parse --absolute-git-dir)"
hooks_dir="$GIT_DIR/gate-only-hooks"
mkdir -p "$hooks_dir"
cat > "$hooks_dir/pre-commit" <<EOF
#!/usr/bin/env bash
# Gate-only pre-commit for a linked worktree (installed by
# scripts/worktree-setup.sh). Runs the full quality gate; there is
# deliberately NO post-commit hook here, so a worktree commit does not run
# the advisor that would restart the shared :3000 daemon (bug 936).
exec "$REPO_ROOT/scripts/precommit.sh" "\$@"
EOF
chmod +x "$hooks_dir/pre-commit"

# Point THIS worktree's hooks at the gate-only dir WITHOUT disturbing the main
# checkout. extensions.worktreeConfig (a benign, idempotent repo flag) lets
# core.hooksPath be overridden per worktree; `git config --worktree` writes to
# this worktree's private config.worktree only. The shared core.hooksPath that
# the main checkout reads is left exactly as-is.
git config extensions.worktreeConfig true
git config --worktree core.hooksPath "$hooks_dir"

echo "worktree-setup: gate-only hooks installed at $hooks_dir"
echo "worktree-setup: 'git commit' in this worktree now runs the pre-commit gate"
echo "                only (no post-commit advisor, no :3000 restart)."
echo "worktree-setup: done."
