#!/usr/bin/env bash
# scripts/install-hooks.sh — one-time post-clone setup.
#
# Installs this repo's git hooks as symlinks into .git/hooks/:
#   pre-commit  → .git-hooks/pre-commit                  (the precommit gate:
#                 fmt / vet / lint / build / test; blocks failing commits)
#   post-commit → scripts/post-commit-restart-advisor.sh (rebuild + smoke +
#                 :3000 daemon restart after a commit lands)
#   post-merge  → .git-hooks/post-merge                  (runs the same advisor
#                 after a merge/pull — git does NOT fire post-commit for merge
#                 commits, so without this a `git merge` to main deploys
#                 nothing; bug git-merge-to-main-skips-post-commit-advisor-deploy)
#
# Idempotent: re-running relinks any hook whose symlink is missing or stale.
# Run once from the repo root:
#   bash scripts/install-hooks.sh
set -euo pipefail
REPO_ROOT="$(git rev-parse --show-toplevel)"

# hook-name → symlink target (relative to REPO_ROOT)
declare -A HOOKS=(
    [pre-commit]=".git-hooks/pre-commit"
    [post-commit]="scripts/post-commit-restart-advisor.sh"
    [post-merge]=".git-hooks/post-merge"
)

for hook in "${!HOOKS[@]}"; do
    src="$REPO_ROOT/${HOOKS[$hook]}"
    dst="$REPO_ROOT/.git/hooks/$hook"
    if [[ ! -e "$src" ]]; then
        echo "WARN: hook source missing, skipping $hook: $src" >&2
        continue
    fi
    chmod +x "$src"
    if [[ -L "$dst" && "$(readlink "$dst")" == "$src" ]]; then
        echo "$hook hook already installed."
        continue
    fi
    ln -sf "$src" "$dst"
    echo "$hook hook installed: $dst -> $src"
done
