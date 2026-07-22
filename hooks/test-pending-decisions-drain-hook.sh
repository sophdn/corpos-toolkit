#!/usr/bin/env bash
# test-pending-decisions-drain-hook.sh — regression scaffold for the
# UserPromptSubmit hook that drains pending_decisions and surfaces
# claimed rows via the additionalContext envelope.
#
# Resolves bug
# `arcreview-dispatch-claims-pending-decisions-but-system-reminder-never-surfaces-in-agent-conversation`
# (id 1479). The previous Stop-hook drain path emitted stdout that
# never reached the agent; this hook uses the documented
# UserPromptSubmit `additionalContext` JSON envelope so claimed rows
# actually surface in the next turn.
#
# Cases:
#   1. claim returns one row → hook emits
#      {hookSpecificOutput:{additionalContext:"..."}} containing the
#      decision text.
#   2. claim returns empty → hook exits 0 with no stdout.
#   3. claim endpoint unreachable → hook fail-opens (exit 0, drift-log).
#   4. Empty stdin → hook exits 0 silently.
#   5. Missing session_id field → hook exits 0, drift-log.
#
# Run:  bash hooks/test-pending-decisions-drain-hook.sh

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HOOK="$SCRIPT_DIR/pending-decisions-drain-hook.sh"

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
    local name="$1" expected="$2" actual="$3"
    if [ "$expected" = "$actual" ]; then
        echo "PASS $name"; PASS=$((PASS + 1))
    else
        echo "FAIL $name (expected '$expected', got '$actual')"; FAIL=$((FAIL + 1))
    fi
}

assert_contains() {
    local name="$1" needle="$2" haystack="$3"
    if printf '%s' "$haystack" | grep -qF -- "$needle"; then
        echo "PASS $name"; PASS=$((PASS + 1))
    else
        echo "FAIL $name (missing '$needle' in: $haystack)"; FAIL=$((FAIL + 1))
    fi
}

assert_empty() {
    local name="$1" actual="$2"
    if [ -z "$actual" ]; then
        echo "PASS $name"; PASS=$((PASS + 1))
    else
        echo "FAIL $name (expected empty, got: $actual)"; FAIL=$((FAIL + 1))
    fi
}

# Mock MCP HTTP server: responds to pending_decisions_claim with a
# canned JSON body. Body is read from $case_dir/responses/claim_response.json
# at startup. Per-case caller writes that file before starting the server.
start_mock_server() {
    local case_dir="$1"
    cat > "$case_dir/mock_responder.py" <<'PY'
import json, os, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
port = int(sys.argv[1])
case_dir = sys.argv[2]
log_path = os.path.join(case_dir, "requests.log")
claim_resp = open(os.path.join(case_dir, "responses", "claim_response.json")).read()
class H(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/health":
            self.send_response(200); self.end_headers(); self.wfile.write(b"ok"); return
        self.send_response(404); self.end_headers()
    def do_POST(self):
        length = int(self.headers.get("content-length","0") or 0)
        body = self.rfile.read(length).decode("utf-8")
        with open(log_path, "a") as f:
            f.write(body + "\n")
        self.send_response(200); self.send_header("content-type","application/json"); self.end_headers()
        self.wfile.write(claim_resp.encode("utf-8"))
    def log_message(self,*a,**k): pass
HTTPServer(("127.0.0.1",port), H).serve_forever()
PY
    local port
    port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
    python3 "$case_dir/mock_responder.py" "$port" "$case_dir" >/dev/null 2>&1 &
    echo "$!" > "$case_dir/server.pid"
    local i
    for i in 1 2 3 4 5; do
        if curl -sS --max-time 1 "http://127.0.0.1:$port/health" >/dev/null 2>&1; then
            break
        fi
        sleep 0.05
    done
    printf '%s' "$port"
}

stop_mock_server() {
    local case_dir="$1"
    local pid
    pid=$(cat "$case_dir/server.pid" 2>/dev/null || echo "")
    [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
}

make_prompt_event() {
    local session="$1"
    jq -n --arg s "$session" '{
        session_id: $s,
        transcript_path: "/tmp/fake.jsonl",
        cwd: "/home/user/dev/mcp-servers",
        hook_event_name: "UserPromptSubmit",
        prompt: "regular work message"
    }'
}

# ----- case 1: claim returns one row → additionalContext emitted -----
TMP=$TMP_BASE/case1
mkdir -p "$TMP/responses"
CLAIM_RESPONSE=$(jq -n '{
    claimed: [{
        id: 1,
        event_id: "evt-corpus-1",
        target_session_id: "s1",
        decisions: [
            {action: "forge_bug", payload: {title: "x"}, confidence: 0.91, reasoning: "real friction"}
        ],
        triggers: ["event_bug_resolved"],
        arc_summary: "fixed a tricky null-deref",
        created_at: "2026-05-20T12:00:00Z"
    }]
}')
printf '%s' "$CLAIM_RESPONSE" > "$TMP/responses/claim_response.json"
PORT=$(start_mock_server "$TMP")
EVENT=$(make_prompt_event "s1")
OUT=$(TOOLKIT_HTTP_PORT="$PORT" \
    TOOLKIT_PROJECT="mcp-servers" \
    TOOLKIT_HOOK_DRIFT_LOG="$TMP/drift.log" \
    bash "$HOOK" <<< "$EVENT")
assert_contains "case1_additionalContext_envelope" '"additionalContext"' "$OUT"
assert_contains "case1_hookSpecificOutput_envelope" '"hookSpecificOutput"' "$OUT"
assert_contains "case1_hookEventName_userpromptsubmit" '"UserPromptSubmit"' "$OUT"
assert_contains "case1_names_action" 'forge_bug' "$OUT"
assert_contains "case1_names_confidence" '0.91' "$OUT"
assert_contains "case1_names_trigger" 'event_bug_resolved' "$OUT"
assert_contains "case1_names_arc_summary" 'null-deref' "$OUT"
REQUESTS=$(cat "$TMP/requests.log" 2>/dev/null || echo "")
assert_contains "case1_calls_pending_decisions_claim" '"pending_decisions_claim"' "$REQUESTS"
stop_mock_server "$TMP"

# ----- case 1b: staged-for-authoring row → authoring prompt, NOT verbatim forge -----
# Chain arc-close-decision-authoring-split T4. A staged vault_note must
# render with Qwen-as-DECIDER framing + the seed (kind+title) + an
# "author the body" directive, and must NOT instruct a verbatim forge of
# Qwen's draft body.
TMP=$TMP_BASE/case1b
mkdir -p "$TMP/responses"
CLAIM_RESPONSE=$(jq -n '{
    claimed: [{
        id: 7,
        event_id: "evt-corpus-staged",
        target_session_id: "s1b",
        decisions: [
            {action: "forge_vault_note", staged_for_authoring: true,
             payload: {note_kind: "decision", title: "seed title here", body: "qwen draft body"},
             confidence: 0.93, reasoning: "this arc settled an architecture choice"}
        ],
        triggers: ["event_commit_landed"],
        arc_summary: "decided to split decider from author",
        created_at: "2026-05-26T12:00:00Z"
    }]
}')
printf '%s' "$CLAIM_RESPONSE" > "$TMP/responses/claim_response.json"
PORT=$(start_mock_server "$TMP")
EVENT=$(make_prompt_event "s1b")
OUT=$(TOOLKIT_HTTP_PORT="$PORT" \
    TOOLKIT_PROJECT="mcp-servers" \
    TOOLKIT_HOOK_DRIFT_LOG="$TMP/drift.log" \
    bash "$HOOK" <<< "$EVENT")
assert_contains "case1b_names_qwen_decider" 'DECIDER=Qwen' "$OUT"
assert_contains "case1b_surfaces_seed_kind" 'note_kind=decision' "$OUT"
assert_contains "case1b_surfaces_seed_title" 'seed title here' "$OUT"
assert_contains "case1b_asks_agent_to_author" 'YOU author the body' "$OUT"
# The draft body must NOT be handed over as a forge-this payload dump.
if printf '%s' "$OUT" | grep -q 'qwen draft body'; then
    echo "FAIL case1b_does_not_dump_draft_body (Qwen's draft body leaked into the authoring prompt)"; FAIL=$((FAIL + 1))
else
    echo "PASS case1b_does_not_dump_draft_body"; PASS=$((PASS + 1))
fi
stop_mock_server "$TMP"

# ----- case 1c: enrich-existing decision → enrich prompt, not new-file -----
# Chain arc-close-decision-authoring-split T6. A decision the agent already
# filed this session renders with an ENRICH directive naming the existing
# artifact, not a file-a-new-one directive.
TMP=$TMP_BASE/case1c
mkdir -p "$TMP/responses"
CLAIM_RESPONSE=$(jq -n '{
    claimed: [{
        id: 9,
        event_id: "evt-enrich",
        target_session_id: "s1c",
        decisions: [
            {action: "forge_vault_note",
             enrich_existing: {kind: "vault_note", slug: "decider-author-split", title: "decider author split"},
             payload: {note_kind: "decision", title: "decider author split", body: "dup"},
             confidence: 0.93, reasoning: "same arc as earlier"}
        ],
        triggers: ["event_commit_landed"],
        arc_summary: "",
        created_at: "2026-05-26T12:00:00Z"
    }]
}')
printf '%s' "$CLAIM_RESPONSE" > "$TMP/responses/claim_response.json"
PORT=$(start_mock_server "$TMP")
EVENT=$(make_prompt_event "s1c")
OUT=$(TOOLKIT_HTTP_PORT="$PORT" \
    TOOLKIT_PROJECT="mcp-servers" \
    TOOLKIT_HOOK_DRIFT_LOG="$TMP/drift.log" \
    bash "$HOOK" <<< "$EVENT")
assert_contains "case1c_names_already_filed" 'ALREADY FILED THIS SESSION' "$OUT"
assert_contains "case1c_names_existing_slug" 'decider-author-split' "$OUT"
assert_contains "case1c_says_enrich" 'ENRICH' "$OUT"
stop_mock_server "$TMP"

# ----- case 2: empty drain → silent -----
TMP=$TMP_BASE/case2
mkdir -p "$TMP/responses"
printf '%s' '{"claimed":[]}' > "$TMP/responses/claim_response.json"
PORT=$(start_mock_server "$TMP")
EVENT=$(make_prompt_event "s2")
OUT=$(TOOLKIT_HTTP_PORT="$PORT" \
    TOOLKIT_PROJECT="mcp-servers" \
    TOOLKIT_HOOK_DRIFT_LOG="$TMP/drift.log" \
    bash "$HOOK" <<< "$EVENT")
assert_empty "case2_empty_claim_no_stdout" "$OUT"
stop_mock_server "$TMP"

# ----- case 3: unreachable endpoint → fail-open -----
TMP=$TMP_BASE/case3
mkdir -p "$TMP"
FREE_PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
EVENT=$(make_prompt_event "s3")
OUT=$(TOOLKIT_HTTP_PORT="$FREE_PORT" \
    TOOLKIT_PROJECT="mcp-servers" \
    TOOLKIT_HOOK_DRIFT_LOG="$TMP/drift.log" \
    bash "$HOOK" <<< "$EVENT"; echo "exit=$?")
EXIT_LINE=$(printf '%s' "$OUT" | tail -1)
assert_eq "case3_unreachable_hook_exits_zero" "exit=0" "$EXIT_LINE"
DRIFT=$(cat "$TMP/drift.log" 2>/dev/null || echo "")
assert_contains "case3_unreachable_drift_logged" "pending_decisions_claim unreachable" "$DRIFT"

# ----- case 4: empty stdin → silent exit -----
TMP=$TMP_BASE/case4
mkdir -p "$TMP"
OUT=$(TOOLKIT_HOOK_DRIFT_LOG="$TMP/drift.log" bash "$HOOK" < /dev/null; echo "exit=$?")
# Hook should produce no stdout; $(...) strips trailing newlines, so the
# only content left should be the appended "exit=0" marker.
assert_eq "case4_empty_stdin_no_stdout" "exit=0" "$OUT"

# ----- case 5: missing session_id → fail-open -----
TMP=$TMP_BASE/case5
mkdir -p "$TMP"
EVENT='{"transcript_path":"/tmp/fake.jsonl","prompt":"hi"}'
OUT=$(TOOLKIT_HOOK_DRIFT_LOG="$TMP/drift.log" bash "$HOOK" <<< "$EVENT"; echo "exit=$?")
EXIT_LINE=$(printf '%s' "$OUT" | tail -1)
assert_eq "case5_missing_session_id_exits_zero" "exit=0" "$EXIT_LINE"
DRIFT=$(cat "$TMP/drift.log" 2>/dev/null || echo "")
assert_contains "case5_missing_session_id_drift_logged" "session_id missing" "$DRIFT"

echo
echo "== $PASS passed, $FAIL failed =="
[ "$FAIL" -eq 0 ]
