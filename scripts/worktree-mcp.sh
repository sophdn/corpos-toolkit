#!/usr/bin/env bash
# scripts/worktree-mcp.sh — launch a per-worktree live MCP server so a
# spawned subagent can self-verify its branch's work-surface changes end to
# end, WITHOUT touching the canonical :3000 daemon or the shared
# data/toolkit.db.
#
# Bug/suggestion: per-worktree-live-mcp-server-for-subagent-branch-correct-
# self-verification (chain worktree-multi-agent-orchestration-support T4).
#
# The problem it solves: the stdio MCP and the :3000 daemon both load the MAIN
# checkout's binary, so a worktree's branch-correct MCP surface is never live —
# only `go test` exercises it. This script builds THIS worktree's binary and
# launches it on an ephemeral HTTP port against a PRIVATE database, exposing
# the full MCP surface over `POST /mcp/{surface}`. The subagent then exercises
# its own handlers live:
#
#   curl -sS -XPOST localhost:<port>/mcp/work -H 'content-type: application/json' \
#        -d '{"action":"chain_find","params":{"query":"my-chain"}}'
#
# Usage (run in the background; it blocks serving):
#   ./scripts/worktree-mcp.sh                 # private copy of the shared DB
#   ./scripts/worktree-mcp.sh --fresh         # empty auto-migrated DB
#   PORT=3994 ./scripts/worktree-mcp.sh       # pin the port (else auto-probed)
#
# Isolation guarantees: the port is never 3000 (auto-probed in 3990-3999, or
# your PORT), and the DB is a private file under /tmp — the canonical daemon
# and shared DB are never opened.

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

GIT_COMMON="$(git rev-parse --path-format=absolute --git-common-dir)"
case "$GIT_COMMON" in
    */.git) MAIN_ROOT="$(dirname "$GIT_COMMON")" ;;
    *)      MAIN_ROOT="$REPO_ROOT" ;;
esac

FRESH=0
for arg in "$@"; do
    case "$arg" in
        --fresh) FRESH=1 ;;
        *) echo "worktree-mcp: unknown arg: $arg" >&2; exit 2 ;;
    esac
done

branch="$(git branch --show-current 2>/dev/null || echo detached)"
# Sanitize the branch for use in a filename.
safe_branch="$(printf '%s' "$branch" | tr -c 'A-Za-z0-9._-' '_')"

# ── Build THIS worktree's binary ───────────────────────────────────────
echo "worktree-mcp: building this worktree's binary (make -C go build)..." >&2
make -C go build >&2

# ── Private DB ─────────────────────────────────────────────────────────
priv_db="/tmp/toolkit-worktree-${safe_branch}.db"
# Drop any stale private DB + its WAL/SHM sidecars so a re-launch starts clean.
rm -f "$priv_db" "$priv_db-wal" "$priv_db-shm"
shared_db="$MAIN_ROOT/data/toolkit.db"
if [[ "$FRESH" -eq 1 || ! -f "$shared_db" ]]; then
    if [[ "$FRESH" -ne 1 ]]; then
        echo "worktree-mcp: shared DB $shared_db absent — starting with a fresh DB." >&2
    fi
    echo "worktree-mcp: private DB = $priv_db (fresh; auto-migrated on open)" >&2
else
    # Point-in-time snapshot of the shared DB. `.recover`-free plain copy is
    # fine for surface self-verification; checkpoint first so WAL contents are
    # included in the copy.
    sqlite3 "$shared_db" 'PRAGMA wal_checkpoint(TRUNCATE);' >/dev/null 2>&1 || true
    cp "$shared_db" "$priv_db"
    echo "worktree-mcp: private DB = $priv_db (snapshot of $shared_db)" >&2
fi

# ── Pick a free, non-3000 port ─────────────────────────────────────────
port="${PORT:-}"
if [[ -z "$port" ]]; then
    for p in 3990 3991 3992 3993 3994 3995 3996 3997 3998 3999; do
        if ! ss -ltn "sport = :$p" 2>/dev/null | tail -n +2 | grep -q .; then
            port="$p"
            break
        fi
    done
fi
if [[ -z "$port" ]]; then
    echo "worktree-mcp: no free port in 3990-3999; pass PORT=<n> explicitly." >&2
    exit 1
fi
if [[ "$port" == "3000" ]]; then
    echo "worktree-mcp: refusing PORT=3000 — that is the canonical shared daemon." >&2
    exit 1
fi

echo "worktree-mcp: serving THIS worktree's MCP surface at http://localhost:$port" >&2
echo "worktree-mcp:   exercise it with: curl -sS -XPOST localhost:$port/mcp/<surface> \\" >&2
echo "worktree-mcp:     -H 'content-type: application/json' -d '{\"action\":\"...\",\"params\":{...}}'" >&2
echo "worktree-mcp:   surfaces: work / knowledge / measure / admin / ml. Ctrl-C to stop." >&2

# Launch THIS worktree's binary (go/launch.sh resolves bin relative to its own
# dir) against the private DB + port, with branch-correct blueprints/rubrics.
exec env \
    TOOLKIT_DB="$priv_db" \
    HTTP_PORT="$port" \
    TOOLKIT_BLUEPRINTS_DIR="$REPO_ROOT/blueprints/forge-schemas" \
    TOOLKIT_RUBRICS_DIR="$REPO_ROOT/blueprints/rubrics" \
    ./go/launch.sh
