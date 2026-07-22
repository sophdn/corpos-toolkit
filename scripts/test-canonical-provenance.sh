#!/usr/bin/env bash
# scripts/test-canonical-provenance.sh — self-test for check-canonical-provenance.sh
# (finish-sophdn-repo-split T8). Proves the guard goes RED when provenance is wrong
# and GREEN against the real registry — the "demonstrated by a red test" acceptance.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GUARD="$ROOT/scripts/check-canonical-provenance.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0; fail=0
ck() { # ck <desc> <expected_exit> <actual_exit>
  if [ "$2" = "$3" ]; then printf '  ok   — %s (exit %s)\n' "$1" "$3"; pass=$((pass+1));
  else printf '  FAIL — %s (expected exit %s, got %s)\n' "$1" "$2" "$3"; fail=$((fail+1)); fi
}

echo "test-canonical-provenance: exercising the guard"

# Case 1 — real registry: should PASS (exit 0) against the live, correctly-sourced artifacts.
PROVENANCE_REGISTRY="$ROOT/deploy/canonical-sources.tsv" bash "$GUARD" >/dev/null 2>&1
ck "real registry → green" 0 $?

# Case 2 — WRONG expected source (point toolkit-server at the archived mcp-servers repo):
#          the live image's label still says sophdn/corpos-toolkit, so the guard must go RED.
printf 'toolkit-server\tlocalhost/toolkit-server:dev\thttps://example-host.local/git/sophdn/mcp-servers\timage_label\n' > "$TMP/wrong.tsv"
PROVENANCE_REGISTRY="$TMP/wrong.tsv" bash "$GUARD" >/dev/null 2>&1
ck "wrong expected source → red" 1 $?

# Case 3 — wrong image name for an image_label row that IS present-checked: a non-existent
#          image SKIPs (nothing deployed to mis-source) → guard still green overall.
printf 'ghost\tlocalhost/does-not-exist:dev\thttps://example/none\timage_label\n' > "$TMP/ghost.tsv"
PROVENANCE_REGISTRY="$TMP/ghost.tsv" bash "$GUARD" >/dev/null 2>&1
ck "absent image → skip, not fail" 0 $?

# Case 4 — proxy ancestry with a bogus canonical dir: cannot verify → SKIP (green).
printf 'toolkit-proxy\t${HOME}/.local/bin/toolkit-proxy\tsophdn/corpos-toolkit\tproxy_ancestry\n' > "$TMP/proxy.tsv"
TOOLKIT_DIR="$TMP/nope" PROVENANCE_REGISTRY="$TMP/proxy.tsv" bash "$GUARD" >/dev/null 2>&1
ck "proxy ancestry, missing canonical checkout → skip" 0 $?

# Case 5 — proxy ancestry against the real canonical checkout: the installed proxy's
#          stamped SHA must be reachable from HEAD → green.
TOOLKIT_DIR="$ROOT" PROVENANCE_REGISTRY="$TMP/proxy.tsv" bash "$GUARD" >/dev/null 2>&1
ck "proxy ancestry, real checkout → green" 0 $?

# Case 6 — malformed row (missing columns) → red.
printf 'broken-row-no-tabs\n' > "$TMP/broken.tsv"
PROVENANCE_REGISTRY="$TMP/broken.tsv" bash "$GUARD" >/dev/null 2>&1
ck "malformed registry row → red" 1 $?

# Case 7 — missing registry file → red.
PROVENANCE_REGISTRY="$TMP/absent.tsv" bash "$GUARD" >/dev/null 2>&1
ck "missing registry → red" 1 $?

echo "test-canonical-provenance: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
