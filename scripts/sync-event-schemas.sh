#!/usr/bin/env bash
# scripts/sync-event-schemas.sh — single-source mirror for event payload schemas.
#
# Canonical source-of-truth: blueprints/events/
# Mirror target:
#   - go/internal/events/schemas/  (the Go events-package embed root)
#
# Sibling to scripts/sync-migrations.sh — same shape, different file
# extension and a single mirror target instead of two. Kept separate to
# keep each script single-purpose; the precommit gate runs both.
#
# Why a script and not symlinks: Go's //go:embed rejects symlinked
# directories AND symlinked files with `cannot embed irregular file`,
# so the canonical and embed dirs have to be real on-disk copies.
#
# scripts/precommit.sh runs this BEFORE the Rust + Go test stages and
# re-stages any mirror-dir files the sync touched, so a freshly-edited
# event schema in canonical reaches the working binaries and the commit
# without the developer touching the mirror dir at all.
#
# Standalone invocation: `bash scripts/sync-event-schemas.sh` mirrors and
# exits 0 on success, 1 if any post-sync drift remains.

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

CANONICAL="blueprints/events"
MIRROR="go/internal/events/schemas"

if [[ ! -d "$CANONICAL" ]]; then
    echo "ERROR: canonical event-schemas directory missing: $CANONICAL" >&2
    exit 1
fi

mkdir -p "$MIRROR"

# Copy / overwrite canonical files into mirror when content differs.
while IFS= read -r -d '' src; do
    name="$(basename "$src")"
    dst="$MIRROR/$name"
    if [[ ! -f "$dst" ]] || ! cmp -s "$src" "$dst"; then
        cp "$src" "$dst"
    fi
done < <(find "$CANONICAL" -maxdepth 1 -name '*.json' -print0)

# Delete mirror files that are not in canonical.
while IFS= read -r -d '' dst; do
    name="$(basename "$dst")"
    if [[ ! -f "$CANONICAL/$name" ]]; then
        rm -- "$dst"
    fi
done < <(find "$MIRROR" -maxdepth 1 -name '*.json' -print0)

# Drift check.
if ! diff -rq "$CANONICAL" "$MIRROR" >/dev/null 2>&1; then
    echo "ERROR: drift remains between $CANONICAL and $MIRROR after sync:" >&2
    diff -rq "$CANONICAL" "$MIRROR" >&2 || true
    exit 1
fi

exit 0
