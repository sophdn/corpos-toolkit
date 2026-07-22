#!/usr/bin/env bash
# arc-close-detector.sh — trigger detection library for arc-close filing review.
#
# Reads a Stop event JSON on stdin; updates per-session counter state at
# ~/.claude/.arc-review/<session_id>.json; emits a trigger payload on
# stdout (single JSON object, one line) when any trigger fires; emits
# nothing (empty stdout) when no trigger fires. Always exit 0; never
# block the Stop event.
#
# Triggers (inclusive per docs/ARC_CLOSE_FILING_REVIEW.md §A + §C):
#
#   A. counter_user_turns_<N> — user-turn counter >= TURN_THRESHOLD
#      (default 5; override via TOOLKIT_ARC_REVIEW_TURN_THRESHOLD).
#      Counts Stop events; one Stop = one completed user turn.
#
#   C. user_shape_<phrase> — last user message matches an arc-close
#      shape (done / thanks / wrapping / that's-all / looks-good /
#      /clear / session-end / any-else). Inclusive list; case-
#      insensitive substring + word-boundary regex.
#
# Trigger B (substrate event listener) lives substrate-side in T4 —
# this hook is the harness-side detector only.
#
# This script is the DETECTOR LIBRARY. T5's Stop hook installer wraps
# it: stdin → arc-close-detector.sh → (if output non-empty) call
# work.review_arc_for_filing MCP action → handle filing_decisions →
# auto-execute high-confidence + surface medium-confidence for confirm.
#
# Counter file shape (per docs/ARC_CLOSE_FILING_REVIEW.md §Counter-
# persistence):
#   {
#     "session_id": "...",
#     "created_at": "ISO-8601",
#     "updated_at": "ISO-8601",
#     "user_turns_since_review": 3,
#     "last_fire_at": null | "ISO-8601",
#     "last_fire_trigger": null | "<trigger_signal>"
#   }
#
# Trigger payload on stdout (one line, JSON):
#   {
#     "session_id": "...",
#     "fired_at": "ISO-8601",
#     "triggers": ["counter_user_turns_5", "user_shape_done"],
#     "user_turns_since_review": 5,
#     "transcript_path": "..."
#   }
#
# Cleanup: counter files older than 7 days are removed on every
# invocation (cheap stat + rm).

set -u

# ---- config knobs (env vars) ------------------------------------------
TURN_THRESHOLD="${TOOLKIT_ARC_REVIEW_TURN_THRESHOLD:-5}"
COUNTER_DIR="${TOOLKIT_ARC_REVIEW_DIR:-$HOME/.claude/.arc-review}"
CLEANUP_DAYS="${TOOLKIT_ARC_REVIEW_CLEANUP_DAYS:-7}"
DRIFT_LOG="${TOOLKIT_HOOK_DRIFT_LOG:-/tmp/toolkit-hook-drift.log}"

# ---- helpers ----------------------------------------------------------

drift_log() {
    # Single-line append to the drift log. Zero lines is steady state;
    # non-zero indicates a malformed input, missing jq, etc.
    local msg="$1"
    printf '%s\tarc-close-detector\t%s\n' "$(date -Is 2>/dev/null || echo unknown)" "$msg" >> "$DRIFT_LOG" 2>/dev/null || true
}

iso_now() {
    date -Is 2>/dev/null || date -u +'%Y-%m-%dT%H:%M:%S+00:00'
}

counter_file() {
    local session_id="$1"
    printf '%s/%s.json' "$COUNTER_DIR" "$session_id"
}

# Cleanup counter files older than CLEANUP_DAYS. Best-effort; never
# blocks.
cleanup_old_counters() {
    [ -d "$COUNTER_DIR" ] || return 0
    find "$COUNTER_DIR" -maxdepth 1 -name '*.json' -mtime "+$CLEANUP_DAYS" -delete 2>/dev/null || true
}

# Read counter state. Echoes JSON; on missing file, echoes the initial
# shape with user_turns_since_review=0.
read_counter() {
    local session_id="$1"
    local file
    file=$(counter_file "$session_id")
    if [ -f "$file" ]; then
        cat "$file"
    else
        # Initial state.
        printf '{"session_id":"%s","created_at":"%s","updated_at":"%s","user_turns_since_review":0,"last_fire_at":null,"last_fire_trigger":null}\n' \
            "$session_id" "$(iso_now)" "$(iso_now)"
    fi
}

# Write counter state. Atomic via tmpfile + mv.
write_counter() {
    local session_id="$1"
    local content="$2"
    local file
    file=$(counter_file "$session_id")
    mkdir -p "$COUNTER_DIR" 2>/dev/null || true
    local tmp="${file}.tmp.$$"
    printf '%s\n' "$content" > "$tmp" 2>/dev/null || return 1
    mv "$tmp" "$file" 2>/dev/null || rm -f "$tmp" 2>/dev/null || true
}

# Extract the last user message text from a Claude Code transcript
# (.jsonl). Echo empty string if transcript missing or no user messages.
extract_last_user_message() {
    local transcript_path="$1"
    [ -n "$transcript_path" ] && [ -f "$transcript_path" ] || { echo ""; return 0; }
    if ! command -v jq >/dev/null 2>&1; then
        echo ""
        return 0
    fi
    # Walk the .jsonl in reverse, find the last entry where .role=="user".
    # The transcript's user-message text lives in .content (string) or
    # .content[].text (array). Handle both shapes defensively.
    tac "$transcript_path" 2>/dev/null | \
        jq -r '
            select(.role == "user")
            | if (.content | type) == "string"
              then .content
              else (.content // [] | map(select(.type == "text") | .text) | join(" "))
              end
        ' 2>/dev/null | \
        head -1 || echo ""
}

# User-shape regex patterns (inclusive list per design doc §C).
# Returns the matched phrase slug, or empty if no match.
detect_user_shape() {
    local text="$1"
    [ -n "$text" ] || { echo ""; return 0; }
    # Lowercase + collapse whitespace for matching.
    local norm
    norm=$(printf '%s' "$text" | tr '[:upper:]' '[:lower:]' | tr -s '[:space:]' ' ')
    # Patterns + slug labels. Ordered: more specific patterns first.
    if echo "$norm" | grep -qE '\bsession[[:space:]]+end\b'; then
        echo "session_end"; return 0
    fi
    if echo "$norm" | grep -qE '\bany(thing)?[[:space:]]+else\b'; then
        echo "any_else"; return 0
    fi
    if echo "$norm" | grep -qE "\bthat'?s[[:space:]]+all\b"; then
        echo "thats_all"; return 0
    fi
    if echo "$norm" | grep -qE '\blooks?[[:space:]]+good\b'; then
        echo "looks_good"; return 0
    fi
    if echo "$norm" | grep -qE '(^|[^/])\bwrapping\b'; then
        echo "wrapping"; return 0
    fi
    if echo "$norm" | grep -qE '\bthanks?\b'; then
        echo "thanks"; return 0
    fi
    if echo "$norm" | grep -qE '\bdone\b'; then
        echo "done"; return 0
    fi
    if echo "$norm" | grep -qE '/clear\b'; then
        echo "clear_command"; return 0
    fi
    echo ""
}

# Main: process one Stop event.
main() {
    local input
    input=$(cat)

    # Cleanup old counter files (best-effort, non-blocking).
    cleanup_old_counters

    # Parse input. Fail-open on missing jq.
    if ! command -v jq >/dev/null 2>&1; then
        drift_log "jq missing — detector cannot parse input"
        return 0
    fi

    local session_id stop_hook_active transcript_path
    session_id=$(echo "$input" | jq -r '.session_id // empty' 2>/dev/null)
    stop_hook_active=$(echo "$input" | jq -r '.stop_hook_active // false' 2>/dev/null)
    transcript_path=$(echo "$input" | jq -r '.transcript_path // empty' 2>/dev/null)

    # Anti-loop guard — if we already injected, let Stop complete.
    if [ "$stop_hook_active" = "true" ]; then
        return 0
    fi

    # Empty session_id → malformed input; bail.
    if [ -z "$session_id" ]; then
        drift_log "empty session_id in Stop event input"
        return 0
    fi

    # Read + increment counter.
    local counter_json new_counter_json
    counter_json=$(read_counter "$session_id")
    local current_turns
    current_turns=$(echo "$counter_json" | jq -r '.user_turns_since_review // 0' 2>/dev/null)
    local new_turns=$((current_turns + 1))

    # Detect triggers.
    local triggers=()
    if [ "$new_turns" -ge "$TURN_THRESHOLD" ]; then
        triggers+=("counter_user_turns_$new_turns")
    fi

    local last_user_text user_shape
    last_user_text=$(extract_last_user_message "$transcript_path")
    user_shape=$(detect_user_shape "$last_user_text")
    if [ -n "$user_shape" ]; then
        triggers+=("user_shape_$user_shape")
    fi

    if [ "${#triggers[@]}" -gt 0 ]; then
        # Fire. Reset counter; record fire.
        local fired_at
        fired_at=$(iso_now)
        local triggers_json
        triggers_json=$(printf '%s\n' "${triggers[@]}" | jq -R . | jq -s .)
        new_counter_json=$(echo "$counter_json" | jq \
            --arg now "$fired_at" \
            --argjson trigs "$triggers_json" \
            '.updated_at = $now | .user_turns_since_review = 0 | .last_fire_at = $now | .last_fire_trigger = ($trigs | join(","))')
        write_counter "$session_id" "$new_counter_json"

        # Emit trigger payload to stdout.
        jq -n \
            --arg sid "$session_id" \
            --arg fired_at "$fired_at" \
            --argjson trigs "$triggers_json" \
            --arg turns "$new_turns" \
            --arg tpath "$transcript_path" \
            '{session_id: $sid, fired_at: $fired_at, triggers: $trigs, user_turns_since_review: ($turns | tonumber), transcript_path: $tpath}'
    else
        # No trigger; persist the incremented counter.
        new_counter_json=$(echo "$counter_json" | jq \
            --arg now "$(iso_now)" \
            --argjson n "$new_turns" \
            '.updated_at = $now | .user_turns_since_review = $n')
        write_counter "$session_id" "$new_counter_json"
    fi

    return 0
}

# Allow sourcing for tests. When sourced, exposes the helper functions
# without calling main.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
    main "$@"
fi
