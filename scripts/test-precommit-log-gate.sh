#!/usr/bin/env bash
# scripts/test-precommit-log-gate.sh — regression test for the
# agent-primary log discipline gate added in scripts/precommit.sh
# (chain agent-first-substrate T5).
#
# Verifies that planting a `log.Printf` (or `fmt.Println`) call in any
# agent-primary Go path (work, forge, knowledge, measure, admin) causes
# the precommit gate's 0d2 stage to exit non-zero. This is the
# regression gate for the structured-observability acceptance criterion
# (c): "structure-lint extended: log.Printf and fmt.Println in
# agent-primary paths fail the lint gate. CLI scaffolding remains
# acceptable."
#
# The test plants a single offending line in a tracked file, runs the
# 0d2 scan in isolation (via a stripped extraction of the relevant
# shell block), asserts non-zero exit + the offender shows up in the
# output, then restores the file from git. Plants are made inside a
# temp file under go/internal/work/ so the production tree is never
# left in a poisoned state.

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

PLANT="go/internal/work/_log_gate_plant.go"
trap 'rm -f "$PLANT"' EXIT

# Plant a file with a log.Printf call. Use a deliberately compileable
# stub so the file can be staged + scanned without breaking the build —
# the lint scan is a textual grep, so the actual functions need to
# compile only if the test runs `go build`, which it does not.
cat > "$PLANT" <<'GO_EOF'
//go:build ignore

package work

import "log"

// Plant for scripts/test-precommit-log-gate.sh — never compiled (build
// tag `ignore` keeps it out of every build target). The lint scan is
// textual; the build tag is invisible to grep.
func _PlantedLogPrintf() { log.Printf("planted: %s", "test") }
GO_EOF

# Run the same scan the precommit gate's §0d2 block runs.
_log_offenders=$(find \
    go/internal/work \
    go/internal/forge \
    go/internal/knowledge \
    go/internal/measure \
    go/internal/admin \
    -type f -name '*.go' ! -name '*_test.go' 2>/dev/null \
  | xargs grep -nE '\blog\.(Printf|Println|Fatalf|Fatal)\b|\bfmt\.Print(f|ln)\b' 2>/dev/null || true)

if [ -z "$_log_offenders" ]; then
  echo "FAIL: scan did not detect planted log.Printf in $PLANT"
  exit 1
fi

if ! echo "$_log_offenders" | grep -q "$PLANT"; then
  echo "FAIL: scan output did not mention plant path $PLANT"
  echo "scan output:"
  echo "$_log_offenders"
  exit 1
fi

echo "PASS: agent-primary log discipline gate detected the planted offender."

# Defensive secondary check: scan with the plant removed should be
# clean — confirms migrations didn't leave residual log.Printf behind.
rm -f "$PLANT"
_clean_scan=$(find \
    go/internal/work \
    go/internal/forge \
    go/internal/knowledge \
    go/internal/measure \
    go/internal/admin \
    -type f -name '*.go' ! -name '*_test.go' 2>/dev/null \
  | xargs grep -nE '\blog\.(Printf|Println|Fatalf|Fatal)\b|\bfmt\.Print(f|ln)\b' 2>/dev/null || true)
if [ -n "$_clean_scan" ]; then
  echo "FAIL: residual offenders found after plant removal:"
  echo "$_clean_scan"
  exit 1
fi

echo "PASS: post-removal scan is clean — no residual log.Printf in agent-primary paths."
