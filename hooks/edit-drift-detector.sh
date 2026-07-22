#!/usr/bin/env bash
# edit-drift-detector.sh — forensic catch for the "file edited by agent
# but reverted before the next turn" pattern (bug
# mid-session-file-edits-silently-reverted-by-unidentified-mechanism).
#
# Lifecycle:
#
#   PostToolUse on Edit / Write / MultiEdit
#     │
#     │  read tool_input.file_path from stdin
#     │  snapshot current md5 + size + mtime to
#     │  ~/.claude/.edit-watch/<session_id>/<sanitized_path>.json
#     ▼
#   ... possibly an external reverter modifies the file here ...
#     │
#     ▼
#   UserPromptSubmit (next turn)
#     │
#     │  for each watched path:
#     │    compute current md5
#     │    if differs from recorded:
#     │      append a single line to /tmp/toolkit-hook-drift.log
#     │      with path + before-hash + after-hash + delta-bytes
#     │    rm the watch entry
#     ▼
#   steady state: empty watch dir; empty drift log entries for this event
#
# Drift-log format (one line per drift):
#
#   <ISO-8601 ts>  edit-drift-detector  drift_detected  path=<absolute>
#     session=<session_id>  hash_before=<md5>  hash_after=<md5>
#     size_before=<bytes>  size_after=<bytes>
#     elapsed_seconds=<int>
#
# Always exits 0; never blocks the hook event.
#
# This hook is opt-in via settings.json; install it as BOTH a PostToolUse
# entry AND a UserPromptSubmit entry. The same script handles both —
# discriminates on the hook_event_name field of the input payload.

set -u

DRIFT_LOG="${TOOLKIT_HOOK_DRIFT_LOG:-/tmp/toolkit-hook-drift.log}"
WATCH_BASE="${TOOLKIT_EDIT_WATCH_DIR:-$HOME/.claude/.edit-watch}"

drift_log() {
    local msg="$1"
    printf '%s\tedit-drift-detector\t%s\n' \
        "$(date -Is 2>/dev/null || date -u +'%Y-%m-%dT%H:%M:%S+00:00')" \
        "$msg" >> "$DRIFT_LOG" 2>/dev/null || true
}

INPUT=$(cat)

if ! command -v jq >/dev/null 2>&1; then
    drift_log "jq missing — fail-open"
    exit 0
fi
if ! command -v md5sum >/dev/null 2>&1; then
    drift_log "md5sum missing — fail-open"
    exit 0
fi

# hook_event_name discriminates which lifecycle stage fired us.
EVENT=$(printf '%s' "$INPUT" | jq -r '.hook_event_name // empty' 2>/dev/null)
SESSION_ID=$(printf '%s' "$INPUT" | jq -r '.session_id // empty' 2>/dev/null)

if [ -z "$SESSION_ID" ]; then
    drift_log "session_id missing from input — fail-open"
    exit 0
fi

WATCH_DIR="$WATCH_BASE/$SESSION_ID"

# sanitize_path turns an absolute path into a single safe filename for
# the watch dir. Replaces '/' with '__' and strips leading slashes.
sanitize_path() {
    local p="$1"
    p="${p#/}"
    printf '%s' "${p//\//__}"
}

# Compute md5 + size of a file. Echoes "<md5> <bytes>" on success;
# empty string when file is missing.
hash_and_size() {
    local p="$1"
    [ -f "$p" ] || { echo ""; return 0; }
    local h
    h=$(md5sum "$p" 2>/dev/null | awk '{print $1}')
    [ -z "$h" ] && { echo ""; return 0; }
    local s
    s=$(stat -c%s "$p" 2>/dev/null || stat -f%z "$p" 2>/dev/null || echo 0)
    printf '%s %s' "$h" "$s"
}

case "$EVENT" in
    PostToolUse)
        # Snapshot the file we just touched. Only fires for tools that
        # write to disk; bail on anything else.
        TOOL=$(printf '%s' "$INPUT" | jq -r '.tool_name // empty' 2>/dev/null)
        case "$TOOL" in
            Edit|Write|MultiEdit|NotebookEdit)
                : # fall through
                ;;
            *)
                exit 0
                ;;
        esac

        FILE_PATH=$(printf '%s' "$INPUT" | jq -r '.tool_input.file_path // .tool_input.path // empty' 2>/dev/null)
        if [ -z "$FILE_PATH" ]; then
            exit 0
        fi
        # Absolute-ize relative paths against cwd if Claude Code passes
        # them relative (current versions pass absolute; defensive).
        case "$FILE_PATH" in
            /*) ;;
            *) FILE_PATH="$(pwd)/$FILE_PATH" ;;
        esac

        SNAPSHOT=$(hash_and_size "$FILE_PATH")
        if [ -z "$SNAPSHOT" ]; then
            # File didn't exist post-tool — Write may have failed; skip
            # silently.
            exit 0
        fi
        HASH=$(printf '%s' "$SNAPSHOT" | awk '{print $1}')
        SIZE=$(printf '%s' "$SNAPSHOT" | awk '{print $2}')

        mkdir -p "$WATCH_DIR" 2>/dev/null || exit 0
        ENTRY="$WATCH_DIR/$(sanitize_path "$FILE_PATH").json"
        # Atomic write.
        TMP="${ENTRY}.tmp.$$"
        jq -n \
            --arg path "$FILE_PATH" \
            --arg hash "$HASH" \
            --arg size "$SIZE" \
            --arg ts "$(date -Is 2>/dev/null || date -u +'%Y-%m-%dT%H:%M:%S+00:00')" \
            --arg tool "$TOOL" \
            '{path: $path, hash: $hash, size: ($size | tonumber), recorded_at: $ts, tool: $tool}' \
            > "$TMP" 2>/dev/null && mv "$TMP" "$ENTRY" 2>/dev/null
        exit 0
        ;;

    UserPromptSubmit)
        # Walk the watch dir, compare current hashes, log drifts, then
        # clear the dir.
        [ -d "$WATCH_DIR" ] || exit 0
        DRIFT_COUNT=0
        for ENTRY in "$WATCH_DIR"/*.json; do
            [ -f "$ENTRY" ] || continue
            ENTRY_PATH=$(jq -r '.path // empty' "$ENTRY" 2>/dev/null)
            ENTRY_HASH=$(jq -r '.hash // empty' "$ENTRY" 2>/dev/null)
            ENTRY_SIZE=$(jq -r '.size // 0' "$ENTRY" 2>/dev/null)
            ENTRY_RECORDED=$(jq -r '.recorded_at // empty' "$ENTRY" 2>/dev/null)
            ENTRY_TOOL=$(jq -r '.tool // empty' "$ENTRY" 2>/dev/null)
            if [ -z "$ENTRY_PATH" ] || [ -z "$ENTRY_HASH" ]; then
                rm -f "$ENTRY"
                continue
            fi

            CURRENT=$(hash_and_size "$ENTRY_PATH")
            CURRENT_HASH=$(printf '%s' "$CURRENT" | awk '{print $1}')
            CURRENT_SIZE=$(printf '%s' "$CURRENT" | awk '{print $2}')

            if [ -z "$CURRENT_HASH" ]; then
                # File deleted between Edit and now — a different drift
                # shape (file-removed). Log it too.
                drift_log "drift_detected file_removed path=$ENTRY_PATH session=$SESSION_ID tool=$ENTRY_TOOL recorded_at=$ENTRY_RECORDED"
                DRIFT_COUNT=$((DRIFT_COUNT + 1))
            elif [ "$CURRENT_HASH" != "$ENTRY_HASH" ]; then
                DELTA=$((CURRENT_SIZE - ENTRY_SIZE))
                drift_log "drift_detected path=$ENTRY_PATH session=$SESSION_ID tool=$ENTRY_TOOL hash_before=$ENTRY_HASH hash_after=$CURRENT_HASH size_before=$ENTRY_SIZE size_after=$CURRENT_SIZE delta_bytes=$DELTA recorded_at=$ENTRY_RECORDED"
                DRIFT_COUNT=$((DRIFT_COUNT + 1))
            fi
            rm -f "$ENTRY"
        done
        # Best-effort cleanup of the (now-empty) session dir.
        rmdir "$WATCH_DIR" 2>/dev/null || true
        # Summary line only when drifts occurred — quiet in steady state.
        if [ "$DRIFT_COUNT" -gt 0 ]; then
            drift_log "session=$SESSION_ID drift_summary count=$DRIFT_COUNT"
        fi
        exit 0
        ;;

    *)
        # Unknown event — fail-open silently.
        exit 0
        ;;
esac
