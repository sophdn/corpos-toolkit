#!/usr/bin/env bash
# pending-decisions-drain-hook.sh — Claude Code UserPromptSubmit hook.
# Resolves bug `arcreview-dispatch-claims-pending-decisions-but-system-reminder-never-surfaces-in-agent-conversation`
# (bug id 1479).
#
# Why UserPromptSubmit and not Stop:
#   Stop-hook stdout is shown in transcript mode but does NOT inject into
#   the next user-prompt turn's context. The previous design assumed it
#   would surface as a system-reminder; it doesn't. UserPromptSubmit's
#   documented `additionalContext` JSON envelope is the actual mechanism
#   for adding context the next turn sees (verified against the
#   PostToolUse pattern in
#   ~/.claude/vault/reference/2026-05-16_claude-code-posttooluse-hook-surface.md
#   and the spike at scripts/spike/action_describe_suggest_hook.sh).
#
# FLOW:
#   stdin (UserPromptSubmit event JSON: session_id, prompt, cwd, ...)
#     │
#     ▼  POST work.pending_decisions_claim(project, session_id)
#   typed PendingDecisionsClaimResult
#     │
#     ▼  if claimed[] non-empty:
#         format each row into a system-reminder text block
#         emit { hookSpecificOutput: { hookEventName: "UserPromptSubmit",
#                                      additionalContext: <text> } }
#     │
#     ▼  if empty / unreachable / malformed:
#         drift-log + exit 0 (steady-state silent return)
#
# FAIL-OPEN DISCIPLINE: every failure mode (toolkit-server down, missing
# jq/curl, empty input, malformed response) logs to
# /tmp/toolkit-hook-drift.log and exits 0. UserPromptSubmit must NEVER
# be blocked by this hook — a hung daemon shouldn't break user input.

set -u

DRIFT_LOG="${TOOLKIT_HOOK_DRIFT_LOG:-/tmp/toolkit-hook-drift.log}"
PORT="${TOOLKIT_HTTP_PORT:-3000}"
PROJECT="${TOOLKIT_PROJECT:-mcp-servers}"

drift_log() {
    local msg="$1"
    printf '%s\tpending-decisions-drain-hook\t%s\n' \
        "$(date -Is 2>/dev/null || date -u +'%Y-%m-%dT%H:%M:%S+00:00')" \
        "$msg" >> "$DRIFT_LOG" 2>/dev/null || true
}

INPUT=$(cat)
if [ -z "$INPUT" ]; then
    exit 0
fi

if ! command -v jq >/dev/null 2>&1; then
    drift_log "jq missing — fail-open"
    exit 0
fi
if ! command -v curl >/dev/null 2>&1; then
    drift_log "curl missing — fail-open"
    exit 0
fi

SESSION_ID=$(printf '%s' "$INPUT" | jq -r '.session_id // empty' 2>/dev/null)
if [ -z "$SESSION_ID" ]; then
    drift_log "session_id missing from payload — fail-open"
    exit 0
fi

MCP_URL="http://127.0.0.1:${PORT}/mcp/work"

CLAIM_BODY=$(jq -n \
    --arg project "$PROJECT" \
    --arg session "$SESSION_ID" \
    '{action: "pending_decisions_claim", project: $project, params: {session_id: $session}}')

CLAIM_RESP=$(curl -sS --max-time 5 \
    -X POST -H 'content-type: application/json' \
    -d "$CLAIM_BODY" "$MCP_URL" 2>/dev/null)
CURL_RC=$?
if [ $CURL_RC -ne 0 ] || [ -z "$CLAIM_RESP" ]; then
    drift_log "pending_decisions_claim unreachable at $MCP_URL (rc=$CURL_RC, session=$SESSION_ID) — fail-open"
    exit 0
fi

# HTTP MCP surface wraps typed result in CallToolResult ({content:[{text:"..."}]});
# direct stdio bypass returns the bare action result. Detect and normalize.
HAS_CONTENT=$(printf '%s' "$CLAIM_RESP" | jq -r 'has("content")' 2>/dev/null)
if [ "$HAS_CONTENT" = "true" ]; then
    CLAIMED_JSON=$(printf '%s' "$CLAIM_RESP" | jq -r '.content[0].text // empty' 2>/dev/null)
else
    CLAIMED_JSON="$CLAIM_RESP"
fi
if [ -z "$CLAIMED_JSON" ]; then
    drift_log "pending_decisions_claim malformed response (session=$SESSION_ID) — fail-open"
    exit 0
fi

CLAIMED_COUNT=$(printf '%s' "$CLAIMED_JSON" | jq -r '.claimed | length' 2>/dev/null)
CLAIMED_COUNT=${CLAIMED_COUNT:-0}
if [ "$CLAIMED_COUNT" -le 0 ]; then
    # Steady state — nothing pending. Silent return, no additionalContext.
    exit 0
fi

# Format the claimed rows into a single system-reminder text block. The
# harness wraps additionalContext text in <system-reminder>...</system-reminder>
# automatically — we emit the inner text only.
# Per-decision rendering branches on .staged_for_authoring (chain
# arc-close-decision-authoring-split T4): a staged body-heavy decision
# carries Qwen's seed (kind + title) and Qwen-as-DECIDER attribution, and
# asks the agent to AUTHOR the body — it does NOT hand Qwen's draft body
# over to forge verbatim. Non-staged decisions keep the original
# dispatch-the-payload shape.
REMINDER_TEXT=$(printf '%s' "$CLAIMED_JSON" | jq -r '
    def seed:
        if .action == "forge_vault_note" then "note_kind=\(.payload.note_kind // "?"), title=\(.payload.title // "?")"
        elif .action == "memory_write" then "memory_kind=\(.payload.memory_kind // "?"), name=\(.payload.name // "?")"
        else (.payload // {} | tostring) end;
    def render:
        if (.enrich_existing != null)
        then "  - [confidence=\(.confidence)] \(.action) — ALREADY FILED THIS SESSION: you filed \"\(.enrich_existing.title // "")\" (\(.enrich_existing.slug // "")). ENRICH that existing artifact (append the new angle) instead of filing a duplicate, or skip if already covered. \(.reasoning // "")"
        elif (.staged_for_authoring // false)
        then "  - [DECIDER=Qwen, confidence=\(.confidence)] \(.action) — Qwen decided this arc is worth filing: \(.reasoning // "")\n"
             + "    seed (refine freely): " + seed + "\n"
             + "    → YOU author the body from full conversational context, then forge once. Qwen chose WHETHER + kind + seed title; the body is yours. Skip if it does not warrant a note after all."
        else "  - [confidence=\(.confidence)] \(.action) — \(.reasoning // "")\n    payload: \(.payload // null | tostring)"
        end;
    (.claimed | map(.decisions[] | (.staged_for_authoring // false)) | flatten | any) as $has_staged
    | (.claimed
        | map(
            "Arc-close filing review fired on triggers: \(.triggers | join(", ")).\n"
            + (if (.arc_summary // "") != "" then "Arc summary: \(.arc_summary)\n" else "" end)
            + "\nQwen returned \(.decisions | length) typed filing decision(s):\n\n"
            + ([.decisions[] | render] | join("\n"))
          )
        | join("\n\n---\n\n"))
    + "\n\n"
    + (if $has_staged then "STAGED items: Qwen is the DECIDER (whether + kind + seed title); YOU are the AUTHOR — write the body from full conversational context, then forge(vault-note, ...) / forge(memory, memory_kind=...). An unauthored staged item is captured by the unreviewed-fallback at session end, so authoring beats skipping silently.\n" else "" end)
    + "Non-staged: dispatch via mcp__toolkit-server__work.forge(...) for forge_bug / forge_vault_note, forge(memory, memory_kind=...) for memory_write (NOT a direct dir write — that orphans), or review + apply for skill_update. Decisions with confidence ≥ 0.85 auto-execute; 0.50-0.85 confirm explicitly; <0.50 skip."
' 2>/dev/null)

if [ -z "$REMINDER_TEXT" ]; then
    drift_log "claimed $CLAIMED_COUNT rows but reminder text empty after format (session=$SESSION_ID) — fail-open"
    exit 0
fi

jq -n \
    --arg ctx "$REMINDER_TEXT" \
    '{hookSpecificOutput: {hookEventName: "UserPromptSubmit", additionalContext: $ctx}}'

drift_log "drained $CLAIMED_COUNT pending_decisions row(s) (session=$SESSION_ID)"
exit 0
