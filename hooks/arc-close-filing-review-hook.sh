#!/usr/bin/env bash
# arc-close-filing-review-hook.sh — Claude Code Stop hook (chain
# arc-close-filing-review T5). Wires T3's detector + T4's MCP action
# into a single Stop-event flow per docs/ARC_CLOSE_FILING_REVIEW.md
# §Filing-dispatch Claude-Code-path.
#
# FLOW (steady state):
#   stdin (Stop event JSON)
#     │
#     ▼  pipe through arc-close-detector.sh
#   trigger payload on stdout (or empty)
#     │  if empty → exit 0 silently
#     ▼
#   POST http://127.0.0.1:$TOOLKIT_HTTP_PORT/mcp/work
#     with action="review_arc_for_filing", project=<resolved>,
#          params=<detector trigger payload>
#     │
#     ▼  parse typed response (status discriminator)
#   case status:
#     "fired"           → dispatch partition:
#                         - auto_execute: high-conf forge_bug /
#                           forge_suggestion forged directly (NOT
#                           vault_note/memory — those now stage, below).
#                         - staged_for_authoring (chain
#                           arc-close-decision-authoring-split): in-scope
#                           body-heavy kinds (vault_note / memory_write) in
#                           the auto band emit an AUTHORING prompt (Qwen as
#                           decider, agent authors the body); never forged
#                           here.
#                         - surface_for_confirm + skill_update emit a
#                           system-reminder text block on stdout for the
#                           next user turn.
#                         - skip just logs to drift.
#     "debounced"       → no-op (review already fired within 60s);
#                         drift-log and exit 0.
#     "skipped"         → no-op (missing field / empty snapshot);
#                         drift-log and exit 0.
#     "qwen_unreachable"→ fail-open (Qwen/llama-server down);
#                         drift-log and exit 0. Current discipline
#                         (parse_context + skill bodies) keeps working.
#
# CRITICAL CAVEAT (per design §Filing-dispatch Q5 + "Scope of
# auto-execute"): auto-execute fires forges and writes auto-memory
# entries ONLY. NEVER writes code, NEVER edits skill files. The
# action's partition already enforces this — skill_update lands in
# surface_for_confirm regardless of confidence — so the hook's
# auto-execute path is structurally bounded to filing surfaces.
#
# FAIL-OPEN DISCIPLINE: every failure mode (toolkit-server down,
# malformed response, missing jq/curl, empty trigger payload, etc.)
# logs to /tmp/toolkit-hook-drift.log and exits 0. The Stop event
# must NEVER be blocked by this hook.

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DETECTOR="${TOOLKIT_ARC_REVIEW_DETECTOR:-$SCRIPT_DIR/arc-close-detector.sh}"

DRIFT_LOG="${TOOLKIT_HOOK_DRIFT_LOG:-/tmp/toolkit-hook-drift.log}"
PORT="${TOOLKIT_HTTP_PORT:-3001}"
PROJECT="${TOOLKIT_PROJECT:-corpos-toolkit}"

# Confidence partition is settled inside the MCP action (see
# go/internal/arcreview/handler.go::partitionDecisions); this hook just
# dispatches the typed buckets. Surface thresholds live there.

drift_log() {
    local msg="$1"
    printf '%s\tarc-close-filing-review-hook\t%s\n' \
        "$(date -Is 2>/dev/null || date -u +'%Y-%m-%dT%H:%M:%S+00:00')" \
        "$msg" >> "$DRIFT_LOG" 2>/dev/null || true
}

# write_session_registry UPSERTs one row into session_registry per Stop fire.
# The substrate-side review observer (arcreview/observer.go::lookupActiveSession)
# queries this table to resolve "which transcript do I review when a substrate
# event lands for project X?" — see docs/ARC_CLOSE_FILING_REVIEW_SUBSTRATE_LISTENER.md.
#
# Routes through the container's HTTP surface (work.register_session) — it does
# NOT open the canonical DB file directly. Post-cutover the container is the sole
# writer; a host process opening the same file is the cross-mount-namespace WAL
# hazard (bug wired-stop-hooks-open-db-directly-and-target-stale-mcp-servers-path-
# and-3000). Fail-open per the hook's discipline: any error drift-logs and returns
# without blocking the Stop event. Requires jq+curl (verified present below before
# this is called).
write_session_registry() {
    local session_id="$1"
    local transcript_path="$2"
    [ -n "$session_id" ] || { drift_log "session_registry skipped — empty session_id"; return; }
    [ -n "$transcript_path" ] || { drift_log "session_registry skipped — empty transcript_path (session=$session_id)"; return; }
    local body
    body=$(jq -n --arg s "$session_id" --arg t "$transcript_path" --arg p "$PROJECT" \
        '{action: "register_session", project: $p, params: {session_id: $s, transcript_path: $t}}' 2>/dev/null) \
        || { drift_log "session_registry skipped — jq failed (session=$session_id)"; return; }
    curl -sS --max-time 5 -X POST -H 'content-type: application/json' \
        -d "$body" "http://127.0.0.1:${PORT}/mcp/work" >/dev/null 2>&1 \
        || drift_log "session_registry register_session POST failed (session=$session_id) — fail-open"
}

# Read the full Stop event payload before anything else — we'll pipe it
# through the detector AND read fields off it (session_id for logging,
# project resolution).
INPUT=$(cat)

# Dependency checks: jq + curl are load-bearing. Missing → fail-open.
if ! command -v jq >/dev/null 2>&1; then
    drift_log "jq missing — fail-open"
    exit 0
fi
if ! command -v curl >/dev/null 2>&1; then
    drift_log "curl missing — fail-open"
    exit 0
fi

SESSION_ID=$(printf '%s' "$INPUT" | jq -r '.session_id // empty' 2>/dev/null)
STOP_HOOK_ACTIVE=$(printf '%s' "$INPUT" | jq -r '.stop_hook_active // false' 2>/dev/null)
TRANSCRIPT_PATH=$(printf '%s' "$INPUT" | jq -r '.transcript_path // empty' 2>/dev/null)

# Anti-loop guard: if the hook already fired in this Stop chain, let
# Stop complete. Mirrors the detector's own guard.
if [ "$STOP_HOOK_ACTIVE" = "true" ]; then
    exit 0
fi

# Bridge harness state → substrate state. Write happens on every Stop
# event regardless of whether the detector fires; the substrate listener
# relies on this to find the freshest active session for a project.
write_session_registry "$SESSION_ID" "$TRANSCRIPT_PATH"

# MCP base URL — used for both the trigger-driven review call AND the
# pending_decisions_claim dispatch drain. Hoisted here so both call
# sites share one $PORT/$PROJECT resolution.
MCP_URL="http://127.0.0.1:${PORT}/mcp/work"

# pending_decisions draining lives in the UserPromptSubmit hook
# pending-decisions-drain-hook.sh, not here. Stop hook stdout doesn't
# inject into the next user-prompt turn's context — the previous
# in-hook drain claimed rows (flipping dispatched_at) but the
# system-reminder text never reached the agent. The UserPromptSubmit
# hook emits via the documented `additionalContext` envelope so
# claimed rows actually surface. See bug
# `arcreview-dispatch-claims-pending-decisions-but-system-reminder-never-surfaces-in-agent-conversation`.

# Run the detector. Detector emits exactly one JSON line on stdout when
# a trigger fires; empty otherwise.
if [ ! -x "$DETECTOR" ]; then
    drift_log "detector not executable at $DETECTOR — fail-open"
    exit 0
fi
TRIGGER_PAYLOAD=$(printf '%s' "$INPUT" | "$DETECTOR" 2>/dev/null)
if [ -z "$TRIGGER_PAYLOAD" ]; then
    # No trigger fired (counter below threshold + no user-shape match).
    # Steady state; exit silently.
    exit 0
fi

# Trigger fired. Call work.review_arc_for_filing on the toolkit-server.
# The MCP HTTP surface accepts dispatch.Args JSON on /mcp/<surface>.
# MCP_URL hoisted earlier for drain_pending_decisions.
REQUEST_BODY=$(jq -n \
    --argjson params "$TRIGGER_PAYLOAD" \
    --arg project "$PROJECT" \
    '{action: "review_arc_for_filing", project: $project, params: $params}')

# Tight curl timeout: the Qwen review pair runs ~1-2s on a warm
# llama-server; allow up to 20s for cold-start + occasional slowness.
# The Stop event already happened; this hook completes async-ish.
RESPONSE=$(curl -sS --max-time 20 \
    -X POST -H 'content-type: application/json' \
    -d "$REQUEST_BODY" "$MCP_URL" 2>/dev/null)
CURL_RC=$?
if [ $CURL_RC -ne 0 ] || [ -z "$RESPONSE" ]; then
    drift_log "MCP unreachable at $MCP_URL (rc=$CURL_RC, session=$SESSION_ID) — fail-open"
    exit 0
fi

# Toolkit's HTTP MCP surface wraps the typed result in a CallToolResult
# envelope ({content: [{text: "<stringified JSON>"}], ...}); the bare
# action result has top-level {status, partition, ...}. Detect which
# shape arrived and normalize to RESULT_JSON.
HAS_CONTENT=$(printf '%s' "$RESPONSE" | jq -r 'has("content")' 2>/dev/null)
if [ "$HAS_CONTENT" = "true" ]; then
    RESULT_JSON=$(printf '%s' "$RESPONSE" | jq -r '.content[0].text // empty' 2>/dev/null)
else
    RESULT_JSON="$RESPONSE"
fi
if [ -z "$RESULT_JSON" ]; then
    drift_log "malformed MCP response (session=$SESSION_ID) — fail-open"
    exit 0
fi

STATUS=$(printf '%s' "$RESULT_JSON" | jq -r '.status // empty' 2>/dev/null)
if [ -z "$STATUS" ]; then
    drift_log "malformed MCP response (session=$SESSION_ID) — no status field — fail-open"
    exit 0
fi
case "$STATUS" in
    debounced)
        drift_log "debounced (session=$SESSION_ID): $(printf '%s' "$RESULT_JSON" | jq -r '.reason // ""')"
        exit 0
        ;;
    skipped)
        drift_log "skipped (session=$SESSION_ID): $(printf '%s' "$RESULT_JSON" | jq -r '.reason // ""')"
        exit 0
        ;;
    qwen_unreachable)
        drift_log "qwen_unreachable (session=$SESSION_ID): $(printf '%s' "$RESULT_JSON" | jq -r '.reason // ""')"
        exit 0
        ;;
    fired)
        : # fall through to dispatch
        ;;
    *)
        drift_log "unknown status '$STATUS' (session=$SESSION_ID) — fail-open"
        exit 0
        ;;
esac

# ----- Dispatch the partition -----------------------------------------

# Helper: forge one row via curl POST. Used for forge_bug + forge_vault_note + memory.
# Slug derivation: payload .name wins (memory uses kebab-name as slug);
# else payload .title is kebab-cased; else falls back to "arc-review-<ts>".
forge_row() {
    local schema_name="$1"
    local fields_json="$2"
    local slug
    slug=$(printf '%s' "$fields_json" | jq -r '
        if .name then .name
        elif .title then (.title | ascii_downcase | gsub("[^a-z0-9]+"; "-") | gsub("^-+|-+$"; ""))
        else "arc-review-" + (now | tostring)
        end' 2>/dev/null)
    [ -z "$slug" ] && slug="arc-review-$(date +%s)"
    # Cap slug length at 80 chars to avoid forge schema rejections.
    slug=$(printf '%s' "$slug" | cut -c1-80)
    local rationale="arc-close-filing-review fired ($STATUS); auto-executed via Stop hook based on Qwen review of session $SESSION_ID"
    local body
    body=$(jq -n \
        --arg schema "$schema_name" \
        --arg slug "$slug" \
        --argjson fields "$fields_json" \
        --arg project "$PROJECT" \
        --arg rationale "$rationale" \
        '{action: "forge", project: $project, rationale: $rationale, params: {schema_name: $schema, slug: $slug, fields: $fields}}')
    curl -sS --max-time 10 \
        -X POST -H 'content-type: application/json' \
        -d "$body" "$MCP_URL" >/dev/null 2>&1
}

# NOTE: the auto-memory forge helper (write_memory) was removed with the
# decision/authoring split (chain arc-close-decision-authoring-split T4):
# memory_write is now a body-heavy kind that stages for agent authoring
# (see the staged_for_authoring section below) rather than auto-forging
# Qwen's draft, so the Stop hook no longer forges memory entries directly.

# Auto-execute path. Iterate the partition.auto_execute array.
AUTO_COUNT=$(printf '%s' "$RESULT_JSON" | jq -r '.partition.auto_execute | length' 2>/dev/null)
AUTO_COUNT=${AUTO_COUNT:-0}
if [ "$AUTO_COUNT" -gt 0 ]; then
    EXECUTED=0
    for i in $(seq 0 $((AUTO_COUNT - 1))); do
        ACTION=$(printf '%s' "$RESULT_JSON" | jq -r ".partition.auto_execute[$i].action" 2>/dev/null)
        PAYLOAD=$(printf '%s' "$RESULT_JSON" | jq -c ".partition.auto_execute[$i].payload" 2>/dev/null)
        case "$ACTION" in
            forge_bug)
                # Stamp originating qwen task ID on the payload so the
                # filed bug carries machine-readable attribution.
                # Claude-in-session forge calls leave qwen_task_id NULL;
                # arc-review-originated calls stamp the qwen task whose
                # output produced the filing decision. The decisions
                # task is 'arc-review-decisions' per
                # go/internal/arcreview/handler.go; the summary task
                # doesn't file.
                STAMPED=$(printf '%s' "$PAYLOAD" | jq -c '. + {qwen_task_id: "arc-review-decisions"}' 2>/dev/null)
                [ -z "$STAMPED" ] && STAMPED="$PAYLOAD"
                forge_row "bug" "$STAMPED" && EXECUTED=$((EXECUTED + 1))
                ;;
            forge_vault_note|memory_write)
                # Contract guard (chain arc-close-decision-authoring-split):
                # body-heavy kinds must NEVER auto-forge Qwen's draft body —
                # they belong in partition.staged_for_authoring (authored by
                # the in-session agent). If one lands in auto_execute the Go
                # partition regressed; REFUSE to forge and log, rather than
                # silently re-introducing the verbatim-forge behavior the
                # split removed.
                drift_log "REFUSED auto-forge of body-heavy '$ACTION' in auto_execute — must be staged_for_authoring (session=$SESSION_ID)"
                ;;
            *)
                drift_log "unexpected auto_execute action '$ACTION' (session=$SESSION_ID)"
                ;;
        esac
    done
    drift_log "auto_execute fired $EXECUTED of $AUTO_COUNT decisions (session=$SESSION_ID)"
fi

# ----- Staged-for-authoring path -------------------------------------
# Chain arc-close-decision-authoring-split T4. In-scope body-heavy
# decisions (forge_vault_note / memory_write) in the auto-execute band are
# NOT auto-forged with Qwen's draft body. Qwen is the DECIDER (whether +
# kind + seed title); the in-session agent — which holds the full
# conversational arc — is the AUTHOR. Emit an authoring prompt; NEVER forge
# here. Qwen's draft body stays server-side on the pending row for the T5
# unreviewed-fallback if the agent never authors.
STAGED_COUNT=$(printf '%s' "$RESULT_JSON" | jq -r '.partition.staged_for_authoring | length' 2>/dev/null)
STAGED_COUNT=${STAGED_COUNT:-0}
if [ "$STAGED_COUNT" -gt 0 ]; then
    TRIGGERS_CSV=$(printf '%s' "$RESULT_JSON" | jq -r '.triggers | join(", ")' 2>/dev/null)
    EVENT_ID=$(printf '%s' "$RESULT_JSON" | jq -r '.event_id // ""' 2>/dev/null)
    {
        printf '<system-reminder>\n'
        printf 'Arc-close filing review fired (triggers: %s). Qwen (arc-close review) decided %d high-value item(s) are worth filing and chose the kind + a seed title for each. YOU have the full conversation in context — AUTHOR the body; do not just rubber-stamp Qwen'\''s draft.\n\n' \
            "$TRIGGERS_CSV" "$STAGED_COUNT"
        for i in $(seq 0 $((STAGED_COUNT - 1))); do
            ACTION=$(printf '%s' "$RESULT_JSON" | jq -r ".partition.staged_for_authoring[$i].action" 2>/dev/null)
            REASONING=$(printf '%s' "$RESULT_JSON" | jq -r ".partition.staged_for_authoring[$i].reasoning" 2>/dev/null)
            case "$ACTION" in
                forge_vault_note)
                    NK=$(printf '%s' "$RESULT_JSON" | jq -r ".partition.staged_for_authoring[$i].payload.note_kind // \"?\"" 2>/dev/null)
                    TITLE=$(printf '%s' "$RESULT_JSON" | jq -r ".partition.staged_for_authoring[$i].payload.title // \"?\"" 2>/dev/null)
                    printf '%d. forge_vault_note (note_kind=%s) — Qwen decider rationale: %s\n' "$((i + 1))" "$NK" "$REASONING"
                    printf '   seed title (refine freely): %s\n' "$TITLE"
                    printf '   → forge(vault-note, note_kind=%s, title=..., body=<your synthesis>). Qwen chose the kind+title; the body is yours.\n' "$NK"
                    ;;
                memory_write)
                    MK=$(printf '%s' "$RESULT_JSON" | jq -r ".partition.staged_for_authoring[$i].payload.memory_kind // \"?\"" 2>/dev/null)
                    NM=$(printf '%s' "$RESULT_JSON" | jq -r ".partition.staged_for_authoring[$i].payload.name // \"?\"" 2>/dev/null)
                    printf '%d. memory_write (memory_kind=%s) — Qwen decider rationale: %s\n' "$((i + 1))" "$MK" "$REASONING"
                    printf '   seed name (refine freely): %s\n' "$NM"
                    printf '   → forge(memory, memory_kind=%s, name=..., description=..., body=<your synthesis>).\n' "$MK"
                    ;;
                *)
                    drift_log "unexpected staged action '$ACTION' (session=$SESSION_ID)"
                    ;;
            esac
        done
        printf '\nQwen decided WHETHER; you have final say with full context — skip an item if it does not warrant a note. '
        printf 'An unauthored staged item is captured by the unreviewed-fallback at session end, so authoring beats skipping silently.\n'
        printf '(staged from arc-review event %s)\n' "$EVENT_ID"
        printf '</system-reminder>\n'
    }
    drift_log "staged_for_authoring surfaced $STAGED_COUNT decisions (session=$SESSION_ID)"
fi

# Surface-for-confirm path. Emit a system-reminder text block on stdout
# for the agent to act on in the next user turn.
SURFACE_COUNT=$(printf '%s' "$RESULT_JSON" | jq -r '.partition.surface_for_confirm | length' 2>/dev/null)
SURFACE_COUNT=${SURFACE_COUNT:-0}
if [ "$SURFACE_COUNT" -gt 0 ]; then
    TRIGGERS_CSV=$(printf '%s' "$RESULT_JSON" | jq -r '.triggers | join(", ")' 2>/dev/null)
    {
        printf '<system-reminder>\n'
        printf 'Arc-close filing review fired (triggers: %s). Qwen returned %d filing decision(s) for confirm:\n\n' \
            "$TRIGGERS_CSV" "$SURFACE_COUNT"
        for i in $(seq 0 $((SURFACE_COUNT - 1))); do
            ACTION=$(printf '%s' "$RESULT_JSON" | jq -r ".partition.surface_for_confirm[$i].action" 2>/dev/null)
            CONFIDENCE=$(printf '%s' "$RESULT_JSON" | jq -r ".partition.surface_for_confirm[$i].confidence" 2>/dev/null)
            REASONING=$(printf '%s' "$RESULT_JSON" | jq -r ".partition.surface_for_confirm[$i].reasoning" 2>/dev/null)
            # T6 enrich-existing (chain arc-close-decision-authoring-split):
            # a decision the agent already filed this session surfaces as
            # "enrich existing X", not "file a new one".
            ENRICH_SLUG=$(printf '%s' "$RESULT_JSON" | jq -r ".partition.surface_for_confirm[$i].enrich_existing.slug // empty" 2>/dev/null)
            if [ -n "$ENRICH_SLUG" ]; then
                ENRICH_TITLE=$(printf '%s' "$RESULT_JSON" | jq -r ".partition.surface_for_confirm[$i].enrich_existing.title // empty" 2>/dev/null)
                printf '%d. [confidence=%s] %s — ALREADY FILED THIS SESSION: you filed "%s" (%s). ENRICH that existing artifact instead of filing a duplicate, or skip if covered. %s\n' \
                    "$((i + 1))" "$CONFIDENCE" "$ACTION" "$ENRICH_TITLE" "$ENRICH_SLUG" "$REASONING"
            else
                PAYLOAD_PREVIEW=$(printf '%s' "$RESULT_JSON" | jq -c ".partition.surface_for_confirm[$i].payload" 2>/dev/null)
                printf '%d. [confidence=%s] %s — %s\n' "$((i + 1))" "$CONFIDENCE" "$ACTION" "$REASONING"
                printf '   payload: %s\n' "$PAYLOAD_PREVIEW"
            fi
        done
        printf '\n'
        printf 'Execute via mcp__toolkit-server__work.forge(...) with the matching schema (bug / vault-note / memory) per the typed payloads. '
        printf 'skill_update decisions edit live skill files; review the proposed patch before applying.\n'
        printf '</system-reminder>\n'
    }
    drift_log "surface_for_confirm emitted $SURFACE_COUNT decisions (session=$SESSION_ID)"
fi

# Skip count — just log for telemetry.
SKIP_COUNT=$(printf '%s' "$RESULT_JSON" | jq -r '.partition.skip | length' 2>/dev/null)
SKIP_COUNT=${SKIP_COUNT:-0}
if [ "$SKIP_COUNT" -gt 0 ]; then
    drift_log "skipped $SKIP_COUNT low-confidence decisions (session=$SESSION_ID)"
fi

exit 0
