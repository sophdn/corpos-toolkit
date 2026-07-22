#!/usr/bin/env bash
# scripts/check-canonical-provenance.sh — assert each running artifact was built
# from its ONE canonical source repo (finish-sophdn-repo-split T8).
#
# The structural end-state the chain promises: the half-finished monorepo split
# (toolkit-server image from mcp-servers, proxy from toolkit — split-brain) is made
# IMPOSSIBLE to reintroduce silently. This guard reads the canonical-source registry
# (deploy/canonical-sources.tsv) and, for each live artifact, compares its real
# provenance signal against the registry. ANY mismatch is a loud, exit-1 failure.
#
# Checks:
#   image_label    — the image's OCI `org.opencontainers.image.source` label must
#                    EXACTLY equal the registry's expected source URL.
#   proxy_ancestry — the installed proxy's stamped build SHA (-version) must be a
#                    commit reachable from the canonical repo's HEAD (i.e. it was
#                    built from THAT repo, not a fork). Delegates the freshness angle
#                    to check-proxy-staleness.sh; here we assert REPO IDENTITY.
#
# Absent artifacts (image not built, podman missing) SKIP with a warning — there is
# nothing deployed to mis-source. A PRESENT artifact from the WRONG repo FAILS.
#
# Overrides (used by test-canonical-provenance.sh):
#   PROVENANCE_REGISTRY=<path>   registry file (default: deploy/canonical-sources.tsv)
#   TOOLKIT_DIR=<path>           canonical sophdn/corpos-toolkit checkout (default: ~/dev/corpos-toolkit)
#
# Usage:  scripts/check-canonical-provenance.sh
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REGISTRY="${PROVENANCE_REGISTRY:-$ROOT/deploy/canonical-sources.tsv}"
TOOLKIT_DIR="${TOOLKIT_DIR:-$HOME/dev/corpos-toolkit}"

fail=0
warn=0
ok=0

say()  { printf '[provenance] %b\n' "$*"; }
red()  { printf '[provenance] \033[31mFAIL\033[0m: %b\n' "$*" >&2; fail=$((fail+1)); }
yel()  { printf '[provenance] \033[33mSKIP\033[0m: %b\n' "$*"; warn=$((warn+1)); }
grn()  { printf '[provenance] \033[32mOK\033[0m: %b\n' "$*"; ok=$((ok+1)); }

[ -f "$REGISTRY" ] || { red "registry not found: $REGISTRY"; exit 1; }
command -v podman >/dev/null 2>&1 || say "WARN: podman not found — image_label checks will SKIP"

check_image_label() {
  local artifact="$1" image="$2" expected="$3" got
  if ! command -v podman >/dev/null 2>&1; then yel "$artifact: podman absent, cannot inspect $image"; return; fi
  if ! podman image exists "$image" 2>/dev/null; then yel "$artifact: image $image not present — nothing to mis-source"; return; fi
  got="$(podman image inspect "$image" --format '{{ index .Labels "org.opencontainers.image.source" }}' 2>/dev/null)"
  if [ -z "$got" ]; then red "$artifact: image $image has NO org.opencontainers.image.source label (un-provenanced build); expected $expected"; return; fi
  if [ "$got" = "$expected" ]; then grn "$artifact: image $image source=$got"; else
    red "$artifact: image $image built from WRONG repo\n         expected: $expected\n         got:      $got"
  fi
}

check_proxy_ancestry() {
  local artifact="$1" path="$2" expected_slug="$3" bin sha
  bin="$(eval echo "$path")"   # expand ${HOME}
  if [ ! -x "$bin" ]; then yel "$artifact: $bin not present — nothing to mis-source"; return; fi
  if ! sha="$("$bin" -version 2>/dev/null | grep -oE '[0-9a-f]{7,40}' | head -1)"; then
    red "$artifact: $bin does not report a build SHA (-version) — un-provenanced (pre-T5 proxy?)"; return
  fi
  [ -n "$sha" ] || { red "$artifact: $bin -version emitted no SHA"; return; }
  if [ ! -d "$TOOLKIT_DIR/.git" ]; then yel "$artifact: canonical checkout $TOOLKIT_DIR absent — cannot verify ancestry of $sha"; return; fi
  # The stamped SHA must be a real commit in the canonical repo AND reachable from
  # HEAD — i.e. this binary was built from THIS repo's history, not a fork.
  if ! git -C "$TOOLKIT_DIR" cat-file -e "${sha}^{commit}" 2>/dev/null; then
    red "$artifact: build SHA $sha is NOT a commit in canonical $expected_slug ($TOOLKIT_DIR) — built from a foreign repo"; return
  fi
  if git -C "$TOOLKIT_DIR" merge-base --is-ancestor "$sha" HEAD 2>/dev/null; then
    grn "$artifact: $bin built from $expected_slug @ $sha (reachable from HEAD)"
  else
    # Known SHA but not ancestor of HEAD: a side branch — flag, don't hard-pass.
    red "$artifact: $bin SHA $sha exists in $expected_slug but is NOT reachable from HEAD (built off a side branch?)"
  fi
}

say "registry: $REGISTRY"
while IFS=$'\t' read -r artifact target expected check; do
  case "$artifact" in ''|\#*) continue;; esac
  [ -n "${check:-}" ] || { red "$artifact: malformed registry row (need 4 TAB columns)"; continue; }
  case "$check" in
    image_label)    check_image_label    "$artifact" "$target" "$expected" ;;
    proxy_ancestry) check_proxy_ancestry "$artifact" "$target" "$expected" ;;
    *)              red "$artifact: unknown check kind '$check'" ;;
  esac
done < "$REGISTRY"

echo
say "summary: $ok ok, $warn skipped, $fail failed"
if [ "$fail" -gt 0 ]; then
  say "→ a running artifact is built from a non-canonical repo. Rebuild it from its"
  say "  canonical source (see docs/TOPOLOGY.md §Canonical repo ownership) or fix the"
  say "  registry if the canonical repo legitimately changed (e.g. the T9 rename)."
  exit 1
fi
exit 0
