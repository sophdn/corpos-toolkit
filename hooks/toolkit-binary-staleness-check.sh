#!/usr/bin/env bash
# toolkit-binary-staleness-check.sh — Claude Code SessionStart hook (user-level)
#
# Surfaces a discoverable in-session signal when the DEPLOYED toolkit-server
# binary (the one .mcp.json's stdio command + go/launch.sh actually load — the
# MAIN checkout's go/bin/toolkit-server) does not match the current session
# checkout's git HEAD. Closes suggestion `session-start-staleness-structural-
# signal` and the in-session-signal half of bug `worktree-commits-dont-deploy-
# to-stdio-binary-path-no-staleness-signal`: the "binary lags HEAD; run
# admin.server_version vs git rev-parse HEAD; /mcp reconnect" reflex was a
# procedural convention invisible to a fresh agent — this makes it structural.
#
# Two drift classes are distinguished via the git ancestry of the deployed
# binary's SHA relative to the session HEAD:
#   - BEHIND   (deployed SHA is an ancestor of HEAD): the binary predates your
#              checkout — a normal rebuild + /mcp reconnect deploys HEAD.
#   - DIVERGENT (deployed SHA is NOT an ancestor): the deployed binary is from
#              a different branch than your checkout HEAD — the worktree /
#              concurrent-agent case where a commit+reconnect will NOT make your
#              Go change live (the main checkout must build your branch first).
#
# What it does NOT cover (deliberate): WITHIN-session staleness after you commit
# during the session — the post-commit advisor's own message + admin.server_version
# handle that. This hook fires at session START, catching HEAD-moved-but-binary-
# not-rebuilt (branch switch, worktree, rebase, prior-session commit w/o rebuild).
#
# Mechanism: emits a warning as SessionStart hookSpecificOutput.additionalContext
# (the harness wraps it in <system-reminder> and folds it into context). Silent
# (no emission) when in sync, when not an mcp-servers session, when the binary is
# an unversioned dev build, or on any error. Always exits 0 (non-blocking).
#
# Test-injectable: TOOLKIT_REPO_ROOT (the canonical main checkout), TOOLKIT_SERVER_BIN
# (the deployed binary), and TOOLKIT_SESSION_CWD (override the SessionStart cwd).

set -u

DRIFT_LOG=/tmp/toolkit-binary-staleness-drift.log
drift_log() { printf '[%s] %s\n' "$(date -u +%FT%TZ)" "$1" >>"$DRIFT_LOG" 2>/dev/null || true; }

# jq parses the SessionStart payload; without it, fail-open silent.
if ! command -v jq >/dev/null 2>&1; then
    drift_log "jq missing — fail-open"
    exit 0
fi

REPO_ROOT="${TOOLKIT_REPO_ROOT:-$HOME/dev/corpos-toolkit}"
DEPLOYED_BIN="${TOOLKIT_SERVER_BIN:-$REPO_ROOT/go/bin/toolkit-server}"

# Session cwd: from the SessionStart JSON payload on stdin, overridable for tests,
# falling back to $PWD. The payload is small; read it without blocking forever.
PAYLOAD=""
if [ -z "${TOOLKIT_SESSION_CWD:-}" ]; then
    PAYLOAD="$(cat 2>/dev/null || true)"
fi
SESSION_CWD="${TOOLKIT_SESSION_CWD:-}"
if [ -z "$SESSION_CWD" ] && [ -n "$PAYLOAD" ]; then
    SESSION_CWD="$(printf '%s' "$PAYLOAD" | jq -r '.cwd // empty' 2>/dev/null || true)"
fi
[ -n "$SESSION_CWD" ] || SESSION_CWD="$PWD"

# Resolve the session's checkout root (handles worktrees — they share the object
# store with the main checkout). Only proceed if this session is in an
# mcp-servers checkout (identified by the toolkit-server main package).
SESSION_REPO="$(git -C "$SESSION_CWD" rev-parse --show-toplevel 2>/dev/null || true)"
if [ -z "$SESSION_REPO" ] || [ ! -f "$SESSION_REPO/go/cmd/toolkit-server/main.go" ]; then
    drift_log "not an mcp-servers session (cwd=$SESSION_CWD) — no-op"
    exit 0
fi

# The deployed binary must exist and report a versioned SHA. --version is a
# pre-flag-parse subcommand that prints "<gitSHA> built <date>" and exits without
# opening the DB or starting the server, so this is a cheap, side-effect-free read.
if [ ! -x "$DEPLOYED_BIN" ]; then
    drift_log "deployed binary not found/executable at $DEPLOYED_BIN — no-op"
    exit 0
fi
BIN_SHA="$("$DEPLOYED_BIN" --version 2>/dev/null | awk 'NR==1{print $1}')"
if [ -z "$BIN_SHA" ] || [ "$BIN_SHA" = "unversioned" ]; then
    drift_log "deployed binary unversioned/unreadable — no-op"
    exit 0
fi

HEAD_SHA="$(git -C "$SESSION_REPO" rev-parse --short=8 HEAD 2>/dev/null || true)"
[ -n "$HEAD_SHA" ] || { drift_log "session HEAD unreadable — no-op"; exit 0; }

# Compare on a common short length so an 8-char ldflag SHA matches a longer
# rev-parse without a false mismatch.
shortlen=${#BIN_SHA}
HEAD_CMP="$(git -C "$SESSION_REPO" rev-parse --short="$shortlen" HEAD 2>/dev/null || echo "$HEAD_SHA")"
if [ "$BIN_SHA" = "$HEAD_CMP" ]; then
    drift_log "in sync (bin=$BIN_SHA head=$HEAD_CMP) — no-op"
    exit 0
fi

# Drift detected — classify via ancestry of the deployed SHA vs session HEAD.
if git -C "$SESSION_REPO" merge-base --is-ancestor "$BIN_SHA" HEAD 2>/dev/null; then
    klass="BEHIND"
    detail="The deployed toolkit-server binary (gitSHA $BIN_SHA) is an ANCESTOR of this checkout's HEAD ($HEAD_SHA) — it predates your code. Rebuild and reconnect to deploy HEAD:  make -C go build  then  /mcp reconnect."
else
    klass="DIVERGENT"
    detail="The deployed toolkit-server binary (gitSHA $BIN_SHA) is NOT an ancestor of this checkout's HEAD ($HEAD_SHA) — it was built from a DIFFERENT branch (worktree / concurrent-agent case). A commit + /mcp reconnect will NOT make your Go change live: the deployed binary at $DEPLOYED_BIN is loaded by .mcp.json + go/launch.sh and is built by whichever checkout last ran the post-commit advisor. To go live you must build YOUR branch into that path (or merge to the main checkout and rebuild there). See bug worktree-commits-dont-deploy-to-stdio-binary-path-no-staleness-signal."
fi

CTX="$(printf '%s\n\n%s' \
    "⚠️ toolkit-server binary staleness ($klass): the running MCP may be serving stale code." \
    "$detail")"

jq -n --arg ctx "$CTX" \
    '{hookSpecificOutput: {hookEventName: "SessionStart", additionalContext: $ctx}}'

drift_log "drift=$klass bin=$BIN_SHA head=$HEAD_SHA repo=$SESSION_REPO"
exit 0
