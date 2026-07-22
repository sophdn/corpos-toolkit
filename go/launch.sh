#!/usr/bin/env bash
# launch.sh — canonical toolkit-server launcher (Go).
#
# toolkit-server is the unified MCP + HTTP observe binary: stdio-bound
# MCP transport (work / measure / knowledge / admin meta-tools) and an
# HTTP layer the dashboard hits for /chains, /tasks, /bugs, /sessions,
# /roadmap, /benchmarks/*, /inference/*, /knowledge/index-card, and the
# /events SSE stream.
#
# Canonical paths (defaults; override via TOOLKIT_DB / TOOLKIT_* env):
#   --db        $XDG_DATA_HOME/toolkit/data/toolkit.db  (~/.local/share/…)
#   --http-port 3000
#
# Per-project Claude Code sessions reach the toolkit through stdio +
# --default-project=<id> (written into the project's .mcp.json by the
# install-into helper). This script is the HTTP-only deployment
# entrypoint (dashboard + observe API), not the per-Claude stdio
# entrypoint — it passes --http-only so the binary skips stdio MCP.
#
# DO NOT `rm` "$DB_PATH" (the resolved toolkit.db) without first
# running `pgrep -af toolkit-server` to confirm no process holds it
# open — sqlite fd survives unlink and silently strands writes.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BINARY="${SCRIPT_DIR}/bin/toolkit-server"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
DB_PATH="${TOOLKIT_DB:-${XDG_DATA_HOME:-$HOME/.local/share}/toolkit/data/toolkit.db}"
DEFAULT_PROJECT="${TOOLKIT_DEFAULT_PROJECT:-corpos-toolkit}"
HTTP_PORT="${HTTP_PORT:-3000}"
BLUEPRINTS_DIR="${TOOLKIT_BLUEPRINTS_DIR:-${REPO_ROOT}/blueprints/forge-schemas}"
RUBRICS_DIR="${TOOLKIT_RUBRICS_DIR:-${REPO_ROOT}/blueprints/rubrics}"

export CGO_ENABLED=1
export CC=gcc
export PATH=$PATH:/usr/local/go/bin

# Chain 602: parse_context skill-body inlining. Default flipped to ON
# in the binary on 2026-05-21 (chain 602 T6 follow-up) — see
# go/internal/refresolve/body_inliner.go's InlineBodyEnvVar comment.
# The export below is now redundant (default-ON covers it) but stays
# as an explicit-intent marker for operators reading this script.
# Set TOOLKIT_PARSE_CONTEXT_INLINE_BODIES=0 here (or in a .mcp.json
# env block for a stdio child) to disable inlining as a kill-switch.
export TOOLKIT_PARSE_CONTEXT_INLINE_BODIES=1

# Pre-flight: refuse to launch if HTTP_PORT is already bound, so the
# operator sees the holder instead of a confusing "address already in
# use" after a release build. Mirrors the Rust launcher.
if ss -ltn "sport = :$HTTP_PORT" 2>/dev/null | tail -n +2 | grep -q .; then
    echo "ERROR: port $HTTP_PORT is already bound; toolkit-server cannot start." >&2
    echo "Holder:" >&2
    if command -v lsof >/dev/null 2>&1; then
        lsof -i ":$HTTP_PORT" -P -n 2>/dev/null | tail -n +2 >&2 || true
    else
        ss -ltnp "sport = :$HTTP_PORT" 2>/dev/null | tail -n +2 >&2 || true
    fi
    echo "" >&2
    echo "Stop the holding process before relaunching, e.g.:" >&2
    echo "  pgrep -af toolkit-server" >&2
    echo "  kill <pid>" >&2
    exit 1
fi

if [[ ! -f "$BINARY" ]]; then
    echo "toolkit-go: building binary..." >&2
    (cd "$SCRIPT_DIR" && go build -o bin/toolkit-server ./cmd/toolkit-server/)
fi

exec "$BINARY" \
    --db "$DB_PATH" \
    --default-project "$DEFAULT_PROJECT" \
    --blueprints-dir "$BLUEPRINTS_DIR" \
    --rubrics-dir "$RUBRICS_DIR" \
    --http-port "$HTTP_PORT" \
    --http-only \
    "$@"
