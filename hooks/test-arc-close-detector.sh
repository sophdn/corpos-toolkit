#!/usr/bin/env bash
# test-arc-close-detector.sh — regression scaffold for arc-close-detector.sh.
#
# Eight cases:
#   1. Empty session_id → no counter file, no trigger output.
#   2. First Stop on a fresh session → counter=1, no trigger.
#   3. Stop at threshold (5th Stop) → counter resets, trigger fires.
#   4. stop_hook_active=true → no-op (anti-loop guard).
#   5. User-shape "thanks" trigger fires regardless of counter.
#   6. User-shape "that's all" trigger fires (multi-word match).
#   7. Counter + user-shape both fire → both triggers in payload.
#   8. Old counter files (>7 days) cleaned up on invocation.
#
# Usage:
#   bash hooks/test-arc-close-detector.sh
#
# Output:
#   PASS / FAIL lines per case + final summary.
#
# Test isolation: each case uses a fresh tmp dir as TOOLKIT_ARC_REVIEW_DIR.

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DETECTOR="$SCRIPT_DIR/arc-close-detector.sh"

if [ ! -x "$DETECTOR" ]; then
    echo "FAIL: detector script not found or not executable: $DETECTOR"
    exit 1
fi

PASS=0
FAIL=0

assert_eq() {
    local name="$1"
    local expected="$2"
    local actual="$3"
    if [ "$expected" = "$actual" ]; then
        echo "PASS $name"
        PASS=$((PASS + 1))
    else
        echo "FAIL $name"
        echo "  expected: $expected"
        echo "  actual:   $actual"
        FAIL=$((FAIL + 1))
    fi
}

assert_contains() {
    local name="$1"
    local needle="$2"
    local haystack="$3"
    if echo "$haystack" | grep -q "$needle"; then
        echo "PASS $name"
        PASS=$((PASS + 1))
    else
        echo "FAIL $name"
        echo "  needle:   $needle"
        echo "  haystack: $haystack"
        FAIL=$((FAIL + 1))
    fi
}

assert_empty() {
    local name="$1"
    local actual="$2"
    if [ -z "$actual" ]; then
        echo "PASS $name"
        PASS=$((PASS + 1))
    else
        echo "FAIL $name (expected empty)"
        echo "  actual: $actual"
        FAIL=$((FAIL + 1))
    fi
}

run_detector() {
    # Args: $1=tmp_dir, $2=input_json, $3=threshold (optional)
    local tmp_dir="$1"
    local input="$2"
    local threshold="${3:-5}"
    TOOLKIT_ARC_REVIEW_DIR="$tmp_dir" \
        TOOLKIT_ARC_REVIEW_TURN_THRESHOLD="$threshold" \
        TOOLKIT_HOOK_DRIFT_LOG=/dev/null \
        bash "$DETECTOR" <<< "$input"
}

write_transcript() {
    # Write a single-user-message transcript.
    local path="$1"
    local user_text="$2"
    cat > "$path" <<EOF
{"role":"user","content":"$user_text"}
{"role":"assistant","content":"ok"}
EOF
}

# ----- case 1: empty session_id ----------------------------------------
TMP=$(mktemp -d)
OUT=$(run_detector "$TMP" '{"stop_hook_active":false}')
assert_empty "case1_empty_session_id_no_output" "$OUT"
COUNT=$(ls "$TMP" 2>/dev/null | wc -l)
assert_eq "case1_empty_session_id_no_counter_file" "0" "$COUNT"
rm -rf "$TMP"

# ----- case 2: first Stop on fresh session -----------------------------
TMP=$(mktemp -d)
OUT=$(run_detector "$TMP" '{"session_id":"s2","stop_hook_active":false}')
assert_empty "case2_first_stop_no_trigger" "$OUT"
if [ -f "$TMP/s2.json" ]; then
    TURNS=$(jq -r '.user_turns_since_review' "$TMP/s2.json")
    assert_eq "case2_first_stop_counter_is_1" "1" "$TURNS"
else
    echo "FAIL case2_first_stop_counter_file_created"
    FAIL=$((FAIL + 1))
fi
rm -rf "$TMP"

# ----- case 3: stop at threshold fires counter trigger -----------------
TMP=$(mktemp -d)
for i in 1 2 3 4; do
    run_detector "$TMP" '{"session_id":"s3","stop_hook_active":false}' >/dev/null
done
# 5th call should fire.
OUT=$(run_detector "$TMP" '{"session_id":"s3","stop_hook_active":false}')
assert_contains "case3_threshold_fires_counter_trigger" '"counter_user_turns_5"' "$OUT"
# After fire, counter should reset to 0.
TURNS=$(jq -r '.user_turns_since_review' "$TMP/s3.json")
assert_eq "case3_threshold_fires_counter_resets" "0" "$TURNS"
# last_fire_at should be set.
LAST_FIRE=$(jq -r '.last_fire_at' "$TMP/s3.json")
if [ "$LAST_FIRE" = "null" ] || [ -z "$LAST_FIRE" ]; then
    echo "FAIL case3_last_fire_at_set"
    FAIL=$((FAIL + 1))
else
    echo "PASS case3_last_fire_at_set"
    PASS=$((PASS + 1))
fi
rm -rf "$TMP"

# ----- case 4: stop_hook_active=true → no-op ---------------------------
TMP=$(mktemp -d)
OUT=$(run_detector "$TMP" '{"session_id":"s4","stop_hook_active":true}')
assert_empty "case4_anti_loop_no_output" "$OUT"
COUNT=$(ls "$TMP" 2>/dev/null | wc -l)
assert_eq "case4_anti_loop_no_counter_file" "0" "$COUNT"
rm -rf "$TMP"

# ----- case 5: user-shape 'thanks' triggers regardless of counter ------
TMP=$(mktemp -d)
TRANSCRIPT="$TMP/transcript.jsonl"
write_transcript "$TRANSCRIPT" "thanks!"
OUT=$(run_detector "$TMP" "{\"session_id\":\"s5\",\"stop_hook_active\":false,\"transcript_path\":\"$TRANSCRIPT\"}")
assert_contains "case5_user_shape_thanks_fires" '"user_shape_thanks"' "$OUT"
rm -rf "$TMP"

# ----- case 6: user-shape "that's all" (multi-word) --------------------
TMP=$(mktemp -d)
TRANSCRIPT="$TMP/transcript.jsonl"
write_transcript "$TRANSCRIPT" "ok that's all for now"
OUT=$(run_detector "$TMP" "{\"session_id\":\"s6\",\"stop_hook_active\":false,\"transcript_path\":\"$TRANSCRIPT\"}")
assert_contains "case6_user_shape_thats_all_fires" '"user_shape_thats_all"' "$OUT"
rm -rf "$TMP"

# ----- case 7: counter + user-shape both fire --------------------------
# Setup: 4 neutral turns (counter accumulates, user-shape no-match), then
# a 5th turn with a user-shape phrase (both triggers fire).
TMP=$(mktemp -d)
TRANSCRIPT="$TMP/transcript.jsonl"
write_transcript "$TRANSCRIPT" "implementation question about the algorithm"
for i in 1 2 3 4; do
    run_detector "$TMP" "{\"session_id\":\"s7\",\"stop_hook_active\":false,\"transcript_path\":\"$TRANSCRIPT\"}" >/dev/null
done
# Sanity: after 4 neutral turns, counter should be 4, no fires.
TURNS_BEFORE_FIFTH=$(jq -r '.user_turns_since_review' "$TMP/s7.json")
assert_eq "case7_pre_fifth_counter_is_4" "4" "$TURNS_BEFORE_FIFTH"
# Now rewrite transcript with user-shape on the 5th turn.
write_transcript "$TRANSCRIPT" "looks good"
OUT=$(run_detector "$TMP" "{\"session_id\":\"s7\",\"stop_hook_active\":false,\"transcript_path\":\"$TRANSCRIPT\"}")
assert_contains "case7_dual_trigger_counter" '"counter_user_turns_5"' "$OUT"
assert_contains "case7_dual_trigger_shape" '"user_shape_looks_good"' "$OUT"
rm -rf "$TMP"

# ----- case 8: cleanup of old counter files ----------------------------
TMP=$(mktemp -d)
# Create a counter file with mtime in the distant past.
echo '{"session_id":"old","user_turns_since_review":0}' > "$TMP/old.json"
touch -d "10 days ago" "$TMP/old.json"
# Create a fresh counter file.
echo '{"session_id":"new","user_turns_since_review":0}' > "$TMP/new.json"
# Invoke detector (any session) → cleanup runs.
run_detector "$TMP" '{"session_id":"trigger_cleanup","stop_hook_active":false}' >/dev/null
if [ ! -f "$TMP/old.json" ]; then
    echo "PASS case8_cleanup_removes_old_file"
    PASS=$((PASS + 1))
else
    echo "FAIL case8_cleanup_removes_old_file"
    FAIL=$((FAIL + 1))
fi
if [ -f "$TMP/new.json" ]; then
    echo "PASS case8_cleanup_preserves_fresh_file"
    PASS=$((PASS + 1))
else
    echo "FAIL case8_cleanup_preserves_fresh_file"
    FAIL=$((FAIL + 1))
fi
rm -rf "$TMP"

# ----- summary ---------------------------------------------------------
echo
echo "== $PASS passed, $FAIL failed =="

if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
exit 0
