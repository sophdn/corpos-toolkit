#!/usr/bin/env bash
# verify-publish-manifest.sh — ci-tier manifest-rot guard (chain 438).
#
# Fails if the publish manifest has drifted from reality in a way that could leak:
#   1. any manifest pattern that matches NOTHING (a listed private file was
#      renamed/moved — the old path no longer drops it, so it would now publish);
#   2. any secret surviving into the scrubbed tree (fail-closed secret scan).
#
# Run at corpos-gate ci tier (not pre-commit/pre-push — it materializes a tree).
# Usage: verify-publish-manifest.sh [--repo <path>]
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
repo="$PWD"
[ "${1:-}" = "--repo" ] && { repo="$2"; shift 2 || true; }
repo="$(cd "$repo" && git rev-parse --show-toplevel)"

if [ ! -f "$repo/.publish-manifest" ]; then
  echo "verify-publish-manifest: no .publish-manifest in $repo — nothing to verify."; exit 0
fi

dry="$(bash "$here/publish-public.sh" --repo "$repo" --dry-run)"
echo "$dry"
if printf '%s\n' "$dry" | grep -q "matching NOTHING"; then
  echo "verify-publish-manifest: FAIL — a manifest pattern matches no file." >&2
  echo "  A private file was renamed/moved; its old path no longer drops it and it" >&2
  echo "  would now publish. Update .publish-manifest before the next mirror push." >&2
  exit 1
fi

out="$(mktemp -d)/tree"
trap 'rm -rf "$(dirname "$out")"' EXIT
bash "$here/publish-public.sh" --repo "$repo" --out "$out" >/dev/null
bash "$here/secret-scan-tree.sh" "$out"
echo "verify-publish-manifest: OK ($repo)"
