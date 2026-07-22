#!/usr/bin/env bash
# test-arc-close-filing-review-hook.sh — regression scaffold for the
# T5 Stop hook. Spawns a small mock toolkit-server HTTP listener per
# case (python3 single-shot http.server.HTTPServer), feeds the hook
# scripted responses, asserts the right behavior per partition.
#
# Cases:
#   1. No detector trigger → hook exits 0 silently, no MCP call.
#   2. Detector trigger + status=fired + auto-execute forge_bug →
#      hook makes forge POST to MCP.
#   3. Detector trigger + status=fired + auto-execute forge_vault_note →
#      hook makes forge POST.
#   4. Detector trigger + status=fired + auto-execute memory_write →
#      hook writes file under memory dir override.
#   5. Detector trigger + status=fired + surface_for_confirm
#      forge_bug at confidence=0.7 → hook emits system-reminder text
#      on stdout, NO forge POST.
#   6. Detector trigger + status=fired + surface_for_confirm
#      skill_update at confidence=0.95 → hook emits surface text;
#      auto-execute path SKIPPED (skill_update never auto-executes,
#      per design Q5).
#   7. Detector trigger + status=debounced → hook exits 0 silently,
#      drift-logged.
#   8. Detector trigger + status=qwen_unreachable → hook exits 0
#      silently, drift-logged.
#   9. MCP server unreachable (port closed) → hook exits 0 silently,
#      drift-logged.
#  10. stop_hook_active=true → hook exits 0, no detector run, no MCP
#      call.
#
# Test isolation: each case gets its own tmp dir for the detector's
# counter state and a fresh mock-server port. Drift log is redirected
# to a per-case tmp file so we can assert on its content.
#
# Run:  bash hooks/test-arc-close-filing-review-hook.sh

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HOOK="$SCRIPT_DIR/arc-close-filing-review-hook.sh"
DETECTOR="$SCRIPT_DIR/arc-close-detector.sh"

if [ ! -x "$HOOK" ]; then
    echo "FAIL: hook not executable: $HOOK"
    exit 1
fi
if ! command -v python3 >/dev/null 2>&1; then
    echo "FAIL: python3 missing (needed for mock MCP server)"
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

assert_not_contains() {
    local name="$1"
    local needle="$2"
    local haystack="$3"
    if printf '%s' "$haystack" | grep -qF -- "$needle"; then
        echo "FAIL $name (unexpectedly found '$needle' in: $haystack)"
        FAIL=$((FAIL + 1))
    else
        echo "PASS $name"
        PASS=$((PASS + 1))
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

# ----- mock server helpers --------------------------------------------

# start_mock_server writes a per-case mock-server response to a file,
# starts a single-shot HTTP listener on a free port, and echoes the
# port. The server logs request bodies to $1/requests.log so the test
# can assert what the hook POSTed.
start_mock_server() {
    local case_dir="$1"
    local response_body="$2"
    local port
    port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
    local responses_dir="$case_dir/responses"
    mkdir -p "$responses_dir"
    printf '%s' "$response_body" > "$responses_dir/review_response.json"
    # Forge responses are simple ok envelopes; we don't assert their
    # shape, only that the hook called them.
    printf '%s' '{"ok":true}' > "$responses_dir/forge_response.json"
    # Spawn the server. It records every request body and serves the
    # configured response based on the action field.
    python3 - "$port" "$case_dir" >/dev/null 2>&1 &
    local pid=$!
    # Save pid so the test can kill it.
    echo "$pid" > "$case_dir/server.pid"
    # Wait briefly for the listener to come up.
    for _ in 1 2 3 4 5; do
        if curl -sS --max-time 1 "http://127.0.0.1:$port/health" >/dev/null 2>&1; then
            break
        fi
        sleep 0.05
    done
    echo "$port"
}

stop_mock_server() {
    local case_dir="$1"
    [ -f "$case_dir/server.pid" ] || return 0
    local pid
    pid=$(cat "$case_dir/server.pid")
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
}

# The python3 - <heredoc> script that the runner spawns. Reads port from
# argv[1] and case_dir from argv[2]; serves /mcp/work returning a
# response based on the request's "action" field; logs the request body
# to <case_dir>/requests.log.
cat > "$TMP_BASE/mock_server.py" <<'PY'
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

port = int(sys.argv[1])
case_dir = sys.argv[2]
log_path = os.path.join(case_dir, "requests.log")
review_response = open(os.path.join(case_dir, "responses", "review_response.json")).read()
forge_response = open(os.path.join(case_dir, "responses", "forge_response.json")).read()


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/health":
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"ok")
            return
        self.send_response(404)
        self.end_headers()

    def do_POST(self):
        length = int(self.headers.get("content-length", "0") or 0)
        body = self.rfile.read(length).decode("utf-8")
        with open(log_path, "a") as f:
            f.write(body + "\n")
        try:
            req = json.loads(body)
            action = req.get("action", "")
        except Exception:
            action = ""
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.end_headers()
        if action == "review_arc_for_filing":
            self.wfile.write(review_response.encode("utf-8"))
        else:
            self.wfile.write(forge_response.encode("utf-8"))

    def log_message(self, *a, **k):  # silence
        pass


HTTPServer(("127.0.0.1", port), Handler).serve_forever()
PY

# Wrap the python script in a function that we call via cat | python3
# from start_mock_server. Use `python3 - ...` instead so the script
# content lives in $TMP_BASE.
start_mock_server() {
    local case_dir="$1"
    local response_body="$2"
    local port
    port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
    local responses_dir="$case_dir/responses"
    mkdir -p "$responses_dir"
    printf '%s' "$response_body" > "$responses_dir/review_response.json"
    printf '%s' '{"ok":true}' > "$responses_dir/forge_response.json"
    python3 "$TMP_BASE/mock_server.py" "$port" "$case_dir" >/dev/null 2>&1 &
    local pid=$!
    echo "$pid" > "$case_dir/server.pid"
    for _ in 1 2 3 4 5; do
        if curl -sS --max-time 1 "http://127.0.0.1:$port/health" >/dev/null 2>&1; then
            break
        fi
        sleep 0.05
    done
    echo "$port"
}

# A trigger payload that fires the hook into the MCP call. Matches the
# shape T3's detector emits. Counter set to threshold so the detector
# actually fires; user_shape "thanks" forces a user-shape trigger too.
make_stop_event() {
    local session_id="$1"
    local transcript_path="$2"
    jq -n \
        --arg sid "$session_id" \
        --arg tpath "$transcript_path" \
        '{session_id: $sid, stop_hook_active: false, transcript_path: $tpath}'
}

make_transcript() {
    local path="$1"
    local last_msg="$2"
    cat > "$path" <<EOF
{"role":"user","content":"$last_msg"}
EOF
}

# Run the hook with environment overrides. Returns stdout; drift log
# goes to $case_dir/drift.log. Detector counter dir is $case_dir/counter.
run_hook() {
    local case_dir="$1"
    local stop_event="$2"
    local port="$3"
    mkdir -p "$case_dir/counter" "$case_dir/memory"
    TOOLKIT_HTTP_PORT="$port" \
        TOOLKIT_PROJECT="mcp-servers" \
        TOOLKIT_ARC_REVIEW_DIR="$case_dir/counter" \
        TOOLKIT_ARC_REVIEW_TURN_THRESHOLD="1" \
        TOOLKIT_HOOK_DRIFT_LOG="$case_dir/drift.log" \
        HOME="$case_dir" \
        bash "$HOOK" <<< "$stop_event"
}

# ----- case 1: no detector trigger ------------------------------------
# To get NO trigger, raise threshold above the count of Stop events the
# detector will see (we send one Stop event; threshold=99 means counter
# stays at 1, no fire). User message is bland so no user-shape match.
TMP=$TMP_BASE/case1
mkdir -p "$TMP/counter"
PORT=$(start_mock_server "$TMP" '{"status":"fired","decisions":[],"partition":{"auto_execute":[],"surface_for_confirm":[],"skip":[]},"triggers":[]}')
TRANSCRIPT="$TMP/transcript.jsonl"
make_transcript "$TRANSCRIPT" "regular work message"
STOP_EVENT=$(make_stop_event "s1" "$TRANSCRIPT")
OUT=$(TOOLKIT_HTTP_PORT="$PORT" TOOLKIT_ARC_REVIEW_DIR="$TMP/counter" \
    TOOLKIT_ARC_REVIEW_TURN_THRESHOLD="99" \
    TOOLKIT_HOOK_DRIFT_LOG="$TMP/drift.log" \
    HOME="$TMP" bash "$HOOK" <<< "$STOP_EVENT")
assert_empty "case1_no_trigger_no_output" "$OUT"
# Drain moved to UserPromptSubmit hook (bug 1479); Stop hook no longer
# calls pending_decisions_claim. When the detector doesn't fire there
# should be no MCP traffic at all.
REQUESTS=$(cat "$TMP/requests.log" 2>/dev/null || echo "")
assert_not_contains "case1_no_trigger_no_review_call" '"review_arc_for_filing"' "$REQUESTS"
assert_not_contains "case1_no_trigger_no_claim_call" '"pending_decisions_claim"' "$REQUESTS"
stop_mock_server "$TMP"

# ----- case 2: auto-execute forge_bug ---------------------------------
TMP=$TMP_BASE/case2
mkdir -p "$TMP"
REVIEW_RESPONSE=$(jq -n '{
    status: "fired",
    decisions: [{action: "forge_bug", payload: {title: "wrapper swallows exit code", problem_statement: "noticed", severity: "low"}, confidence: 0.92, reasoning: "real bug"}],
    partition: {
        auto_execute: [{action: "forge_bug", payload: {title: "wrapper swallows exit code", problem_statement: "noticed", severity: "low"}, confidence: 0.92, reasoning: "real bug"}],
        surface_for_confirm: [],
        skip: []
    },
    triggers: ["user_shape_thanks"]
}')
PORT=$(start_mock_server "$TMP" "$REVIEW_RESPONSE")
TRANSCRIPT="$TMP/transcript.jsonl"
make_transcript "$TRANSCRIPT" "thanks that worked"
STOP_EVENT=$(make_stop_event "s2" "$TRANSCRIPT")
OUT=$(run_hook "$TMP" "$STOP_EVENT" "$PORT")
assert_empty "case2_auto_execute_no_stdout" "$OUT"
# Two requests should land in the log: the review call + the forge call.
REQUESTS=$(cat "$TMP/requests.log" 2>/dev/null || echo "")
assert_contains "case2_review_call_posted" '"review_arc_for_filing"' "$REQUESTS"
assert_contains "case2_forge_call_posted" '"action": "forge"' "$REQUESTS"
assert_contains "case2_forge_schema_bug" '"schema_name": "bug"' "$REQUESTS"
stop_mock_server "$TMP"

# ----- case 3: staged-for-authoring forge_vault_note ------------------
# Chain arc-close-decision-authoring-split T4: a body-heavy vault_note in
# the auto band is STAGED, not auto-forged. The hook must emit an
# authoring prompt (Qwen as decider + seed kind/title) and make NO forge
# POST, and must NOT dump Qwen's draft body.
TMP=$TMP_BASE/case3
mkdir -p "$TMP"
REVIEW_RESPONSE=$(jq -n '{
    status: "fired",
    event_id: "evt-staged-vault",
    partition: {
        auto_execute: [],
        staged_for_authoring: [{action: "forge_vault_note", staged_for_authoring: true, payload: {note_kind: "learning", title: "exit-code propagation", body: "QWEN DRAFT BODY SHOULD NOT BE DUMPED", tags: "shell,gotcha"}, confidence: 0.91, reasoning: "cross-project gotcha"}],
        surface_for_confirm: [],
        skip: []
    },
    triggers: ["user_shape_thanks"]
}')
PORT=$(start_mock_server "$TMP" "$REVIEW_RESPONSE")
TRANSCRIPT="$TMP/transcript.jsonl"
make_transcript "$TRANSCRIPT" "thanks"
STOP_EVENT=$(make_stop_event "s3" "$TRANSCRIPT")
OUT=$(run_hook "$TMP" "$STOP_EVENT" "$PORT")
REQUESTS=$(cat "$TMP/requests.log" 2>/dev/null || echo "")
assert_not_contains "case3_staged_makes_no_forge" '"action": "forge"' "$REQUESTS"
assert_contains "case3_staged_emits_authoring_prompt" 'AUTHOR the body' "$OUT"
assert_contains "case3_staged_names_qwen_decider" 'Qwen' "$OUT"
assert_contains "case3_staged_surfaces_seed_title" 'exit-code propagation' "$OUT"
assert_not_contains "case3_staged_does_not_dump_draft_body" 'QWEN DRAFT BODY SHOULD NOT BE DUMPED' "$OUT"
stop_mock_server "$TMP"

# ----- case 4: staged-for-authoring memory_write ----------------------
# memory_write is also body-heavy (chain arc-close-decision-authoring-
# split T4): staged for authoring, NOT auto-forged. Assert authoring
# prompt + no forge POST.
TMP=$TMP_BASE/case4
mkdir -p "$TMP"
REVIEW_RESPONSE=$(jq -n '{
    status: "fired",
    event_id: "evt-staged-mem",
    partition: {
        auto_execute: [],
        staged_for_authoring: [{action: "memory_write", staged_for_authoring: true, payload: {memory_kind: "feedback", name: "test-memory-seed", description: "seed desc", body: "QWEN MEMORY DRAFT NOT DUMPED"}, confidence: 0.90, reasoning: "user pattern"}],
        surface_for_confirm: [],
        skip: []
    },
    triggers: ["user_shape_thanks"]
}')
PORT=$(start_mock_server "$TMP" "$REVIEW_RESPONSE")
TRANSCRIPT="$TMP/transcript.jsonl"
make_transcript "$TRANSCRIPT" "thanks"
STOP_EVENT=$(make_stop_event "s4" "$TRANSCRIPT")
OUT=$(run_hook "$TMP" "$STOP_EVENT" "$PORT")
REQUESTS=$(cat "$TMP/requests.log" 2>/dev/null || echo "")
assert_not_contains "case4_staged_memory_makes_no_forge" '"action": "forge"' "$REQUESTS"
assert_contains "case4_staged_memory_emits_authoring_prompt" 'memory_kind=feedback' "$OUT"
assert_contains "case4_staged_memory_seed_name" 'test-memory-seed' "$OUT"
assert_not_contains "case4_staged_memory_no_draft_dump" 'QWEN MEMORY DRAFT NOT DUMPED' "$OUT"
stop_mock_server "$TMP"

# ----- case 5: surface-for-confirm forge_bug at medium confidence -----
TMP=$TMP_BASE/case5
mkdir -p "$TMP"
REVIEW_RESPONSE=$(jq -n '{
    status: "fired",
    partition: {
        auto_execute: [],
        surface_for_confirm: [{action: "forge_bug", payload: {title: "maybe a bug", problem_statement: "uncertain"}, confidence: 0.70, reasoning: "tentative"}],
        skip: []
    },
    triggers: ["user_shape_thanks"]
}')
PORT=$(start_mock_server "$TMP" "$REVIEW_RESPONSE")
TRANSCRIPT="$TMP/transcript.jsonl"
make_transcript "$TRANSCRIPT" "thanks"
STOP_EVENT=$(make_stop_event "s5" "$TRANSCRIPT")
OUT=$(run_hook "$TMP" "$STOP_EVENT" "$PORT")
assert_contains "case5_surface_emits_system_reminder_open_tag" '<system-reminder>' "$OUT"
assert_contains "case5_surface_emits_system_reminder_close_tag" '</system-reminder>' "$OUT"
assert_contains "case5_surface_names_action" 'forge_bug' "$OUT"
assert_contains "case5_surface_names_confidence" '0.7' "$OUT"
REQUESTS=$(cat "$TMP/requests.log" 2>/dev/null || echo "")
assert_not_contains "case5_no_forge_post" '"action": "forge"' "$REQUESTS"
stop_mock_server "$TMP"

# ----- case 5b: enrich-existing surfaces as enrich, no forge ----------
# Chain arc-close-decision-authoring-split T6: a decision the agent already
# filed this session lands in surface_for_confirm with enrich_existing set;
# the hook renders an ENRICH directive naming the existing artifact and
# makes no forge POST.
TMP=$TMP_BASE/case5b
mkdir -p "$TMP"
REVIEW_RESPONSE=$(jq -n '{
    status: "fired",
    partition: {
        auto_execute: [],
        staged_for_authoring: [],
        surface_for_confirm: [{action: "forge_vault_note", enrich_existing: {kind: "vault_note", slug: "decider-author-split", title: "decider author split"}, payload: {note_kind: "decision", title: "decider author split", body: "dup"}, confidence: 0.93, reasoning: "same arc"}],
        skip: []
    },
    triggers: ["user_shape_thanks"]
}')
PORT=$(start_mock_server "$TMP" "$REVIEW_RESPONSE")
TRANSCRIPT="$TMP/transcript.jsonl"
make_transcript "$TRANSCRIPT" "thanks"
STOP_EVENT=$(make_stop_event "s5b" "$TRANSCRIPT")
OUT=$(run_hook "$TMP" "$STOP_EVENT" "$PORT")
REQUESTS=$(cat "$TMP/requests.log" 2>/dev/null || echo "")
assert_contains "case5b_says_already_filed" 'ALREADY FILED THIS SESSION' "$OUT"
assert_contains "case5b_names_existing_slug" 'decider-author-split' "$OUT"
assert_not_contains "case5b_no_forge_post" '"action": "forge"' "$REQUESTS"
stop_mock_server "$TMP"

# ----- case 6: skill_update surfaces even at high confidence ----------
TMP=$TMP_BASE/case6
mkdir -p "$TMP"
# The action's partitionDecisions puts skill_update in
# surface_for_confirm regardless of confidence. The hook trusts the
# partition shape and just dispatches per bucket.
REVIEW_RESPONSE=$(jq -n '{
    status: "fired",
    partition: {
        auto_execute: [],
        surface_for_confirm: [{action: "skill_update", payload: {skill_slug: "vault-filing-discipline", patch_kind: "add_section", content: "## New section"}, confidence: 0.95, reasoning: "skill gap"}],
        skip: []
    },
    triggers: ["user_shape_thanks"]
}')
PORT=$(start_mock_server "$TMP" "$REVIEW_RESPONSE")
TRANSCRIPT="$TMP/transcript.jsonl"
make_transcript "$TRANSCRIPT" "thanks"
STOP_EVENT=$(make_stop_event "s6" "$TRANSCRIPT")
OUT=$(run_hook "$TMP" "$STOP_EVENT" "$PORT")
assert_contains "case6_skill_update_surfaces" 'skill_update' "$OUT"
REQUESTS=$(cat "$TMP/requests.log" 2>/dev/null || echo "")
assert_not_contains "case6_skill_update_never_auto_executes" '"action": "forge"' "$REQUESTS"
stop_mock_server "$TMP"

# ----- case 7: status=debounced is a no-op ----------------------------
TMP=$TMP_BASE/case7
mkdir -p "$TMP"
REVIEW_RESPONSE=$(jq -n '{
    status: "debounced",
    last_fire_at: "2026-05-19T22:00:00.000Z",
    reason: "previous fire 30s ago",
    triggers: ["user_shape_thanks"]
}')
PORT=$(start_mock_server "$TMP" "$REVIEW_RESPONSE")
TRANSCRIPT="$TMP/transcript.jsonl"
make_transcript "$TRANSCRIPT" "thanks"
STOP_EVENT=$(make_stop_event "s7" "$TRANSCRIPT")
OUT=$(run_hook "$TMP" "$STOP_EVENT" "$PORT")
assert_empty "case7_debounced_no_stdout" "$OUT"
DRIFT=$(cat "$TMP/drift.log" 2>/dev/null || echo "")
assert_contains "case7_debounced_drift_logged" 'debounced' "$DRIFT"
stop_mock_server "$TMP"

# ----- case 8: status=qwen_unreachable is a no-op ---------------------
TMP=$TMP_BASE/case8
mkdir -p "$TMP"
REVIEW_RESPONSE=$(jq -n '{
    status: "qwen_unreachable",
    reason: "router generate: connection refused",
    triggers: ["user_shape_thanks"]
}')
PORT=$(start_mock_server "$TMP" "$REVIEW_RESPONSE")
TRANSCRIPT="$TMP/transcript.jsonl"
make_transcript "$TRANSCRIPT" "thanks"
STOP_EVENT=$(make_stop_event "s8" "$TRANSCRIPT")
OUT=$(run_hook "$TMP" "$STOP_EVENT" "$PORT")
assert_empty "case8_qwen_unreachable_no_stdout" "$OUT"
DRIFT=$(cat "$TMP/drift.log" 2>/dev/null || echo "")
assert_contains "case8_qwen_unreachable_drift_logged" 'qwen_unreachable' "$DRIFT"
stop_mock_server "$TMP"

# ----- case 9: MCP server unreachable (no server) ---------------------
TMP=$TMP_BASE/case9
mkdir -p "$TMP/counter"
# Find a free port; do NOT start a server on it.
FREE_PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
TRANSCRIPT="$TMP/transcript.jsonl"
make_transcript "$TRANSCRIPT" "thanks"
STOP_EVENT=$(make_stop_event "s9" "$TRANSCRIPT")
OUT=$(TOOLKIT_HTTP_PORT="$FREE_PORT" \
    TOOLKIT_ARC_REVIEW_DIR="$TMP/counter" \
    TOOLKIT_ARC_REVIEW_TURN_THRESHOLD="1" \
    TOOLKIT_HOOK_DRIFT_LOG="$TMP/drift.log" \
    HOME="$TMP" bash "$HOOK" <<< "$STOP_EVENT")
assert_empty "case9_mcp_unreachable_no_stdout" "$OUT"
DRIFT=$(cat "$TMP/drift.log" 2>/dev/null || echo "")
assert_contains "case9_mcp_unreachable_drift_logged" 'MCP unreachable' "$DRIFT"

# ----- case 10: stop_hook_active=true → no-op -------------------------
TMP=$TMP_BASE/case10
mkdir -p "$TMP/counter"
# stop_hook_active=true means the hook exits before running the
# detector. We don't even need a mock server; the request log should
# stay empty.
PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
STOP_EVENT='{"session_id":"s10","stop_hook_active":true,"transcript_path":"/tmp/none"}'
OUT=$(TOOLKIT_HTTP_PORT="$PORT" \
    TOOLKIT_ARC_REVIEW_DIR="$TMP/counter" \
    TOOLKIT_ARC_REVIEW_TURN_THRESHOLD="1" \
    TOOLKIT_HOOK_DRIFT_LOG="$TMP/drift.log" \
    HOME="$TMP" bash "$HOOK" <<< "$STOP_EVENT")
assert_empty "case10_anti_loop_no_stdout" "$OUT"
COUNTERS=$(ls "$TMP/counter" 2>/dev/null | wc -l)
assert_eq "case10_anti_loop_no_counter_file" "0" "$COUNTERS"

# ----- case 11: session_registry UPSERT on every Stop event -----------
# Build a fresh sqlite DB with just the session_registry schema (we don't
# need the full migration set; the hook only cares about this one table)
# and run the hook with TOOLKIT_DB pointed at it. Even without the
# detector firing (threshold raised), the UPSERT must land.
TMP=$TMP_BASE/case11
mkdir -p "$TMP/counter"
TEST_DB="$TMP/toolkit.db"
sqlite3 "$TEST_DB" <<'SQL'
CREATE TABLE session_registry (
    session_id      TEXT PRIMARY KEY,
    project_id      TEXT,
    transcript_path TEXT NOT NULL,
    last_active_at  TEXT NOT NULL,
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
SQL
TRANSCRIPT="$TMP/transcript.jsonl"
make_transcript "$TRANSCRIPT" "regular work"
STOP_EVENT=$(make_stop_event "case11-session" "$TRANSCRIPT")
OUT=$(TOOLKIT_HTTP_PORT="9" \
    TOOLKIT_DB="$TEST_DB" \
    TOOLKIT_PROJECT="mcp-servers" \
    TOOLKIT_ARC_REVIEW_DIR="$TMP/counter" \
    TOOLKIT_ARC_REVIEW_TURN_THRESHOLD="99" \
    TOOLKIT_HOOK_DRIFT_LOG="$TMP/drift.log" \
    HOME="$TMP" bash "$HOOK" <<< "$STOP_EVENT")
ROW=$(sqlite3 "$TEST_DB" "SELECT session_id || '|' || project_id || '|' || transcript_path FROM session_registry WHERE session_id = 'case11-session';" 2>/dev/null)
assert_eq "case11_session_registry_upsert_landed" "case11-session|mcp-servers|$TRANSCRIPT" "$ROW"

# ----- case 12: session_registry write failure is fail-open -----------
# Point TOOLKIT_DB at a path that does NOT exist. The hook's
# write_session_registry should drift-log and return without blocking
# the Stop event. The hook overall must still exit 0 and behave normally
# (no trigger here — threshold raised — so we expect silent exit).
TMP=$TMP_BASE/case12
mkdir -p "$TMP/counter"
TRANSCRIPT="$TMP/transcript.jsonl"
make_transcript "$TRANSCRIPT" "regular work"
STOP_EVENT=$(make_stop_event "case12-session" "$TRANSCRIPT")
OUT=$(TOOLKIT_HTTP_PORT="9" \
    TOOLKIT_DB="/tmp/toolkit-hook-test-nonexistent-$$.db" \
    TOOLKIT_PROJECT="mcp-servers" \
    TOOLKIT_ARC_REVIEW_DIR="$TMP/counter" \
    TOOLKIT_ARC_REVIEW_TURN_THRESHOLD="99" \
    TOOLKIT_HOOK_DRIFT_LOG="$TMP/drift.log" \
    HOME="$TMP" bash "$HOOK" <<< "$STOP_EVENT"; echo "exit=$?")
EXIT_CODE=$(printf '%s' "$OUT" | tail -1)
assert_eq "case12_db_missing_hook_still_exits_zero" "exit=0" "$EXIT_CODE"
DRIFT=$(cat "$TMP/drift.log" 2>/dev/null || echo "")
assert_contains "case12_db_missing_drift_logged" "session_registry skipped — db missing" "$DRIFT"

# ----- case 13: re-firing same session_id UPDATEs not duplicates ------
# Same session_id fires the hook twice (e.g. two Stop events in one
# session); the table must hold exactly one row with last_active_at
# updated on the second fire.
TMP=$TMP_BASE/case13
mkdir -p "$TMP/counter"
TEST_DB="$TMP/toolkit.db"
sqlite3 "$TEST_DB" <<'SQL'
CREATE TABLE session_registry (
    session_id      TEXT PRIMARY KEY,
    project_id      TEXT,
    transcript_path TEXT NOT NULL,
    last_active_at  TEXT NOT NULL,
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
SQL
TRANSCRIPT1="$TMP/transcript1.jsonl"
TRANSCRIPT2="$TMP/transcript2.jsonl"
make_transcript "$TRANSCRIPT1" "first turn"
make_transcript "$TRANSCRIPT2" "second turn"
# First fire.
STOP_EVENT=$(make_stop_event "case13-session" "$TRANSCRIPT1")
TOOLKIT_HTTP_PORT="9" \
    TOOLKIT_DB="$TEST_DB" \
    TOOLKIT_PROJECT="mcp-servers" \
    TOOLKIT_ARC_REVIEW_DIR="$TMP/counter" \
    TOOLKIT_ARC_REVIEW_TURN_THRESHOLD="99" \
    TOOLKIT_HOOK_DRIFT_LOG="$TMP/drift.log" \
    HOME="$TMP" bash "$HOOK" <<< "$STOP_EVENT" >/dev/null
LAST1=$(sqlite3 "$TEST_DB" "SELECT last_active_at FROM session_registry WHERE session_id = 'case13-session';" 2>/dev/null)
# Sleep so the second fire's datetime('now') is provably later.
sleep 1.1
# Second fire — different transcript path, same session_id.
STOP_EVENT=$(make_stop_event "case13-session" "$TRANSCRIPT2")
TOOLKIT_HTTP_PORT="9" \
    TOOLKIT_DB="$TEST_DB" \
    TOOLKIT_PROJECT="mcp-servers" \
    TOOLKIT_ARC_REVIEW_DIR="$TMP/counter" \
    TOOLKIT_ARC_REVIEW_TURN_THRESHOLD="99" \
    TOOLKIT_HOOK_DRIFT_LOG="$TMP/drift.log" \
    HOME="$TMP" bash "$HOOK" <<< "$STOP_EVENT" >/dev/null
ROW_COUNT=$(sqlite3 "$TEST_DB" "SELECT COUNT(*) FROM session_registry WHERE session_id = 'case13-session';" 2>/dev/null)
assert_eq "case13_no_duplicate_rows" "1" "$ROW_COUNT"
LAST2=$(sqlite3 "$TEST_DB" "SELECT last_active_at FROM session_registry WHERE session_id = 'case13-session';" 2>/dev/null)
NEW_PATH=$(sqlite3 "$TEST_DB" "SELECT transcript_path FROM session_registry WHERE session_id = 'case13-session';" 2>/dev/null)
assert_eq "case13_transcript_path_overwritten" "$TRANSCRIPT2" "$NEW_PATH"
if [ "$LAST2" != "$LAST1" ] && [ -n "$LAST2" ] && [ -n "$LAST1" ]; then
    echo "PASS case13_last_active_at_advanced"
    PASS=$((PASS + 1))
else
    echo "FAIL case13_last_active_at_advanced (LAST1='$LAST1' LAST2='$LAST2')"
    FAIL=$((FAIL + 1))
fi

# pending_decisions drain regression cases live in
# test-pending-decisions-drain-hook.sh — bug 1479 moved the drain off
# Stop-hook stdout (which doesn't surface to the agent) onto a
# UserPromptSubmit hook that emits via the documented additionalContext
# envelope.

# ----- summary --------------------------------------------------------
echo
echo "== $PASS passed, $FAIL failed =="
[ "$FAIL" -eq 0 ]
