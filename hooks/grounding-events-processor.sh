#!/usr/bin/env bash
# grounding-events-processor.sh — Claude Code Stop hook (user-level)
#
# Processes the just-completed session JSONL file through
# grounding-events-processor (Go binary), writing grounding_events rows
# for any vault_search/kiwix_search/knowledge_search calls made during
# the session AND emitting query_interactions rows for any detected
# click_kind tier (followed / cited / mentioned / resolved-from) per
# TT1 §5.
#
# Non-blocking: always exits 0. DB or binary errors are logged to stderr
# but do not prevent the session from ending.
#
# Binary location: go/bin/grounding-events-processor, produced by
# `make -C ~/dev/mcp-servers/go build`. The earlier Rust prototype at
# target/debug/knowledge_grounding_processor (archived 2026-05-17 to
# resolve bug knowledge-grounding-processor-misplaced-rust-binary-in-benchmarks)
# is no longer consulted — knowledge surface is Go-canonical post
# 2026-05-13 and the cross-substrate query-telemetry-substrate plug-in
# extensions land in the Go binary directly.
#
# Transcript resolution order (bug click-detection-stop-hook-unwired,
# 2026-05-18):
#  1. Prefer `transcript_path` directly from the Claude Code Stop hook
#     JSON payload when present — the canonical source of truth.
#  2. Fall back to `cwd` from the payload (Claude Code's record of the
#     session's launch directory) → ~/.claude/projects/<slug>/<session>.jsonl.
#  3. Fall back to the local shell's pwd (legacy behavior; breaks when
#     the user has cd'd elsewhere mid-session).
#  4. Last-ditch: glob every ~/.claude/projects/*/ subdir for a file
#     named <session_id>.jsonl. Catches the "hook fires from a project
#     subdir not matching the launch dir" case that left query_interactions
#     empty for the entire query-telemetry-substrate chain.

set -u

INPUT=$(cat)

SESSION_ID=$(echo "$INPUT" | jq -r '.session_id // empty' 2>/dev/null) || exit 0
[ -z "$SESSION_ID" ] && exit 0

# Layer 1: explicit transcript_path from the hook payload.
TRANSCRIPT=$(echo "$INPUT" | jq -r '.transcript_path // empty' 2>/dev/null)

# Layer 2: build from payload's cwd if transcript_path absent or stale.
if [ -z "$TRANSCRIPT" ] || [ ! -f "$TRANSCRIPT" ]; then
  PAYLOAD_CWD=$(echo "$INPUT" | jq -r '.cwd // empty' 2>/dev/null)
  if [ -n "$PAYLOAD_CWD" ]; then
    PAYLOAD_SLUG=$(echo "$PAYLOAD_CWD" | sed 's|/|-|g')
    CAND="$HOME/.claude/projects/$PAYLOAD_SLUG/$SESSION_ID.jsonl"
    [ -f "$CAND" ] && TRANSCRIPT="$CAND"
  fi
fi

# Layer 3: shell pwd (legacy behavior). Kept for backward compatibility
# but doesn't resolve the launch-dir mismatch that motivated the rewrite.
if [ -z "$TRANSCRIPT" ] || [ ! -f "$TRANSCRIPT" ]; then
  PROJECT_SLUG=$(pwd | sed 's|/|-|g')
  CAND="$HOME/.claude/projects/$PROJECT_SLUG/$SESSION_ID.jsonl"
  [ -f "$CAND" ] && TRANSCRIPT="$CAND"
fi

# Layer 4: glob fallback. ~/.claude/projects/*/<session>.jsonl is unique
# per session — session UUIDs don't collide across projects — so the
# first match (if any) is the right transcript regardless of which
# directory the session was launched from.
if [ -z "$TRANSCRIPT" ] || [ ! -f "$TRANSCRIPT" ]; then
  for CAND in "$HOME"/.claude/projects/*/"$SESSION_ID".jsonl; do
    if [ -f "$CAND" ]; then
      TRANSCRIPT="$CAND"
      break
    fi
  done
fi

[ -z "$TRANSCRIPT" ] && exit 0
[ -f "$TRANSCRIPT" ] || exit 0

BINARY="$HOME/dev/corpos-toolkit/go/bin/grounding-events-processor"
[ -f "$BINARY" ] || exit 0

PROJECT_CWD=$(echo "$INPUT" | jq -r '.cwd // empty' 2>/dev/null)
[ -z "$PROJECT_CWD" ] && PROJECT_CWD=$(pwd)
PROJECT_ID=$(echo "$PROJECT_CWD" | sed 's|.*/dev/||' | sed 's|/.*||')
[ -z "$PROJECT_ID" ] && PROJECT_ID="unknown"

# Route through the container's HTTP surface (POST ingest_grounding on :3001): the
# processor parses the transcript HOST-SIDE and POSTs the grounding to the container,
# which does the emit + projection fold as the SINGLE writer. It opens NO database
# (no cross-mount-namespace WAL hazard). Fail-open. The direct --db path is retained
# ONLY for a container-down single-writer one-shot, via TOOLKIT_GROUNDING_ALLOW_DIRECT_DB=1.
if [ "${TOOLKIT_GROUNDING_ALLOW_DIRECT_DB:-0}" = "1" ]; then
    DB_PATH="${TOOLKIT_DB:-$HOME/.local/share/toolkit/data/toolkit.db}"
    "$BINARY" --session "$TRANSCRIPT" --project-id "$PROJECT_ID" --db "$DB_PATH" >/dev/null 2>&1 || true
else
    HTTP_BASE="${TOOLKIT_HTTP_BASE:-http://localhost:3001}"
    "$BINARY" --session "$TRANSCRIPT" --project-id "$PROJECT_ID" --http-base "$HTTP_BASE" >/dev/null 2>&1 || true
fi

exit 0
