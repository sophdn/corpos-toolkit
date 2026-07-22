#!/usr/bin/env bash
# Shape C prototype — UserPromptSubmit hook for the action-docs corpus.
#
# Reads the user's prompt from stdin (Claude Code hook protocol),
# shells out to the spike's action-docs-suggest binary, and emits an
# additionalContext JSON envelope so the agent's next turn sees a
# contextual "you may want admin.action_describe(<surface>,<action>)"
# nudge.
#
# THIS IS THE SPIKE — not installed in ~/.claude/hooks/. If the spike's
# decision artifact recommends building Shape C, this script (or a
# polished variant) moves to ~/.claude/hooks/ and is wired into
# ~/.claude/settings.json's hooks.UserPromptSubmit array.
#
# Hook surface choice rationale (mirrors the toolsearch-rerank-hook
# pattern, captured in chain toolsearch-rerank-hook's design_decisions):
#   - PostToolUse fires too late for prompt-time guidance.
#   - UserPromptSubmit fires once before the agent's first turn —
#     right where we want the contextual nudge.
#   - additionalContext is the only mutation surface that's
#     non-destructive; tool_response mutations are ignored.
#
# Install (NOT done by the spike):
#   1. Build the spike binary: cd ~/dev/mcp-servers/go && go build \
#      -tags sqlite_fts5 -o bin/action-docs-suggest \
#      ./internal/actiondocs/spike/cmd_suggest/
#   2. Copy this script to ~/.claude/hooks/.
#   3. In ~/.claude/settings.json, append to hooks.UserPromptSubmit:
#      { "command": "~/.claude/hooks/action_describe_suggest_hook.sh" }

set -euo pipefail

CORPUS_DIR="${ACTION_DOCS_CORPUS_DIR:-$HOME/dev/mcp-servers/go/internal/actiondocs/corpus}"
SUGGEST_BIN="${ACTION_DOCS_SUGGEST_BIN:-$HOME/dev/corpos-toolkit/go/bin/action-docs-suggest}"
MIN_SCORE_THRESHOLD="${ACTION_DOCS_MIN_SCORE:-8.0}"

# Hook protocol: stdin is a JSON object with at least { prompt: string }.
# Extract the prompt field with a small jq filter; tolerate missing keys
# by returning empty (and exiting 0, so we don't break the agent's flow).
INPUT="$(cat || true)"
if [[ -z "$INPUT" ]]; then
    exit 0
fi
PROMPT="$(printf '%s' "$INPUT" | jq -r '.prompt // empty' 2>/dev/null || true)"
if [[ -z "$PROMPT" ]]; then
    exit 0
fi

# Skip if the corpus binary is not built yet — the spike intentionally
# doesn't auto-build; the install step does. Without the binary, the
# hook silently no-ops rather than breaking the prompt-submit flow.
if [[ ! -x "$SUGGEST_BIN" ]]; then
    exit 0
fi
if [[ ! -d "$CORPUS_DIR" ]]; then
    exit 0
fi

# Shell out to the keyword-match worker. The binary handles tokenization
# + scoring + top-3 selection; the hook just shapes the output into the
# additionalContext envelope.
SUGGEST_JSON="$("$SUGGEST_BIN" "$CORPUS_DIR" "$PROMPT" 2>/dev/null || true)"
if [[ -z "$SUGGEST_JSON" ]]; then
    exit 0
fi

# Only emit additionalContext when at least one suggestion clears the
# score threshold. Below-threshold scores tend to surface
# tangentially-related chunks ("you typed 'list', here's library_list")
# which is more noise than signal. Threshold is configurable via
# ACTION_DOCS_MIN_SCORE; default 8.0 is roughly where the spike's
# input-set hit data shows good vs noisy returns.
TOP_HIT="$(printf '%s' "$SUGGEST_JSON" | jq -r '.suggestions[0] // empty | "\(.surface)\t\(.action)\t\(.score)"' 2>/dev/null || true)"
if [[ -z "$TOP_HIT" ]]; then
    exit 0
fi
TOP_SCORE="$(printf '%s' "$TOP_HIT" | cut -f3)"
awk -v s="$TOP_SCORE" -v t="$MIN_SCORE_THRESHOLD" 'BEGIN { exit (s+0.0 < t+0.0) }' \
    || exit 0

# Build the additionalContext payload. Mirrors the toolsearch-rerank-hook
# pattern: a short markdown line per suggestion with the explicit
# admin.action_describe call shape so the agent can copy-paste.
ADDITIONAL_CONTEXT="$(printf '%s' "$SUGGEST_JSON" | jq -r '
  .suggestions
  | map("  - admin.action_describe(surface=\"\(.surface)\", action=\"\(.action)\")  (score=\(.score | (. * 100 | floor) / 100))")
  | "Based on your prompt, these action-docs corpus chunks may be relevant:\n" + join("\n")
')"

jq -n --arg ctx "$ADDITIONAL_CONTEXT" '{ additionalContext: $ctx }'
