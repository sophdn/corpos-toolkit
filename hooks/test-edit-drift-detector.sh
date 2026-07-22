#!/usr/bin/env bash
# test-edit-drift-detector.sh — regression scaffold for the
# edit-drift-detector forensic guard.
#
# Cases:
#   1. PostToolUse on Edit → watch entry created; matches file's hash.
#   2. PostToolUse on a non-edit tool (Bash) → no watch entry.
#   3. UserPromptSubmit with no drift → watch entry cleared; drift log empty.
#   4. UserPromptSubmit with content-drift → drift_detected line logged.
#   5. UserPromptSubmit with file-removed drift → drift_detected file_removed logged.
#   6. UserPromptSubmit clears watch entries even when nothing drifted.
#   7. PostToolUse without file_path → no watch entry created.
#   8. Empty session_id → fail-open, no watch entry, no log spam.
#
# Run:  bash hooks/test-edit-drift-detector.sh

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HOOK="$SCRIPT_DIR/edit-drift-detector.sh"

if [ ! -x "$HOOK" ]; then
    echo "FAIL: hook not executable: $HOOK"
    exit 1
fi

PASS=0
FAIL=0
TMP_BASE=$(mktemp -d)
trap 'rm -rf "$TMP_BASE"' EXIT

assert_eq() {
    local name="$1"
    local expected="$2"
    local actual="$3"
    if [ "$expected" = "$actual" ]; then
        echo "PASS $name"
        PASS=$((PASS + 1))
    else
        echo "FAIL $name (expected '$expected', got '$actual')"
        FAIL=$((FAIL + 1))
    fi
}

assert_contains() {
    local name="$1"
    local needle="$2"
    local haystack="$3"
    if printf '%s' "$haystack" | grep -qF -- "$needle"; then
        echo "PASS $name"
        PASS=$((PASS + 1))
    else
        echo "FAIL $name (missing '$needle' in: $haystack)"
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
        echo "FAIL $name (expected empty, got: $actual)"
        FAIL=$((FAIL + 1))
    fi
}

run_hook() {
    local case_dir="$1"
    local input="$2"
    TOOLKIT_EDIT_WATCH_DIR="$case_dir/watch" \
        TOOLKIT_HOOK_DRIFT_LOG="$case_dir/drift.log" \
        HOME="$case_dir" \
        bash "$HOOK" <<< "$input"
}

# ----- case 1: PostToolUse on Edit creates a watch entry -------------
TMP=$TMP_BASE/case1
mkdir -p "$TMP"
TARGET="$TMP/target.txt"
printf 'original\n' > "$TARGET"
INPUT=$(jq -n \
    --arg sid "s1" \
    --arg path "$TARGET" \
    '{hook_event_name: "PostToolUse", session_id: $sid, tool_name: "Edit", tool_input: {file_path: $path}}')
run_hook "$TMP" "$INPUT"
ENTRY="$TMP/watch/s1"
ENTRY_COUNT=$(ls "$ENTRY" 2>/dev/null | wc -l)
assert_eq "case1_one_watch_entry_created" "1" "$ENTRY_COUNT"
ENTRY_FILE=$(ls "$ENTRY"/*.json 2>/dev/null | head -1)
RECORDED_HASH=$(jq -r '.hash' "$ENTRY_FILE" 2>/dev/null)
ACTUAL_HASH=$(md5sum "$TARGET" | awk '{print $1}')
assert_eq "case1_recorded_hash_matches_disk" "$ACTUAL_HASH" "$RECORDED_HASH"

# ----- case 2: PostToolUse on Bash creates no watch entry ------------
TMP=$TMP_BASE/case2
mkdir -p "$TMP"
INPUT=$(jq -n \
    --arg sid "s2" \
    '{hook_event_name: "PostToolUse", session_id: $sid, tool_name: "Bash", tool_input: {command: "echo hi"}}')
run_hook "$TMP" "$INPUT"
ENTRY_COUNT=$(ls "$TMP/watch/s2" 2>/dev/null | wc -l)
assert_eq "case2_no_watch_entry_for_bash" "0" "$ENTRY_COUNT"

# ----- case 3: UserPromptSubmit with no drift ------------------------
TMP=$TMP_BASE/case3
mkdir -p "$TMP"
TARGET="$TMP/target.txt"
printf 'stable content\n' > "$TARGET"
# Seed the PostToolUse watch entry.
INPUT=$(jq -n \
    --arg sid "s3" \
    --arg path "$TARGET" \
    '{hook_event_name: "PostToolUse", session_id: $sid, tool_name: "Edit", tool_input: {file_path: $path}}')
run_hook "$TMP" "$INPUT"
# Now fire UserPromptSubmit — file unchanged.
INPUT=$(jq -n --arg sid "s3" '{hook_event_name: "UserPromptSubmit", session_id: $sid}')
run_hook "$TMP" "$INPUT"
DRIFT_LOG=$(cat "$TMP/drift.log" 2>/dev/null || echo "")
assert_empty "case3_no_drift_no_log" "$DRIFT_LOG"
ENTRY_COUNT=$(ls "$TMP/watch/s3" 2>/dev/null | wc -l)
assert_eq "case3_watch_entry_cleared" "0" "$ENTRY_COUNT"

# ----- case 4: UserPromptSubmit with content drift -------------------
TMP=$TMP_BASE/case4
mkdir -p "$TMP"
TARGET="$TMP/target.txt"
printf 'agent wrote this\n' > "$TARGET"
INPUT=$(jq -n \
    --arg sid "s4" \
    --arg path "$TARGET" \
    '{hook_event_name: "PostToolUse", session_id: $sid, tool_name: "Edit", tool_input: {file_path: $path}}')
run_hook "$TMP" "$INPUT"
# Simulate an external reverter changing the file.
printf 'something else clobbered it\n' > "$TARGET"
INPUT=$(jq -n --arg sid "s4" '{hook_event_name: "UserPromptSubmit", session_id: $sid}')
run_hook "$TMP" "$INPUT"
DRIFT_LOG=$(cat "$TMP/drift.log" 2>/dev/null || echo "")
assert_contains "case4_drift_logged" "drift_detected" "$DRIFT_LOG"
assert_contains "case4_drift_includes_path" "$TARGET" "$DRIFT_LOG"
assert_contains "case4_drift_includes_session" "session=s4" "$DRIFT_LOG"
assert_contains "case4_drift_includes_before_hash" "hash_before=" "$DRIFT_LOG"
assert_contains "case4_drift_includes_after_hash" "hash_after=" "$DRIFT_LOG"

# ----- case 5: UserPromptSubmit with file-removed drift --------------
TMP=$TMP_BASE/case5
mkdir -p "$TMP"
TARGET="$TMP/target.txt"
printf 'will be deleted\n' > "$TARGET"
INPUT=$(jq -n \
    --arg sid "s5" \
    --arg path "$TARGET" \
    '{hook_event_name: "PostToolUse", session_id: $sid, tool_name: "Write", tool_input: {file_path: $path}}')
run_hook "$TMP" "$INPUT"
rm -f "$TARGET"
INPUT=$(jq -n --arg sid "s5" '{hook_event_name: "UserPromptSubmit", session_id: $sid}')
run_hook "$TMP" "$INPUT"
DRIFT_LOG=$(cat "$TMP/drift.log" 2>/dev/null || echo "")
assert_contains "case5_file_removed_drift_logged" "file_removed" "$DRIFT_LOG"

# ----- case 6: UserPromptSubmit clears entries even on clean run -----
# Already covered by case3, but verify the dir gets cleaned up.
TMP=$TMP_BASE/case6
mkdir -p "$TMP"
TARGET="$TMP/target.txt"
printf 'a\n' > "$TARGET"
INPUT=$(jq -n \
    --arg sid "s6" \
    --arg path "$TARGET" \
    '{hook_event_name: "PostToolUse", session_id: $sid, tool_name: "Edit", tool_input: {file_path: $path}}')
run_hook "$TMP" "$INPUT"
INPUT=$(jq -n --arg sid "s6" '{hook_event_name: "UserPromptSubmit", session_id: $sid}')
run_hook "$TMP" "$INPUT"
if [ -d "$TMP/watch/s6" ]; then
    echo "FAIL case6_session_dir_cleaned (dir still exists)"
    FAIL=$((FAIL + 1))
else
    echo "PASS case6_session_dir_cleaned"
    PASS=$((PASS + 1))
fi

# ----- case 7: PostToolUse with missing file_path → no entry ---------
TMP=$TMP_BASE/case7
mkdir -p "$TMP"
INPUT=$(jq -n \
    --arg sid "s7" \
    '{hook_event_name: "PostToolUse", session_id: $sid, tool_name: "Edit", tool_input: {}}')
run_hook "$TMP" "$INPUT"
ENTRY_COUNT=$(ls "$TMP/watch/s7" 2>/dev/null | wc -l)
assert_eq "case7_no_entry_without_file_path" "0" "$ENTRY_COUNT"

# ----- case 8: Empty session_id → fail-open --------------------------
TMP=$TMP_BASE/case8
mkdir -p "$TMP"
INPUT='{"hook_event_name":"PostToolUse","tool_name":"Edit","tool_input":{"file_path":"/tmp/foo"}}'
run_hook "$TMP" "$INPUT"
WATCH_DIRS=$(ls "$TMP/watch" 2>/dev/null | wc -l)
assert_eq "case8_no_watch_dir_without_session_id" "0" "$WATCH_DIRS"
DRIFT=$(cat "$TMP/drift.log" 2>/dev/null || echo "")
assert_contains "case8_fail_open_logged" "session_id missing" "$DRIFT"

# ----- summary --------------------------------------------------------
echo
echo "== $PASS passed, $FAIL failed =="
[ "$FAIL" -eq 0 ]
