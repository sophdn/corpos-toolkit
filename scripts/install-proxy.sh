#!/usr/bin/env bash
# Build and install the stdio→HTTP proxy (cmd/toolkit-proxy) to a stable host
# path so each project's .mcp.json can invoke it as its MCP command.
#
# The proxy is what replaces the native `toolkit-server --db <canonical>` stdio
# command once the container owns the canonical DB (chain auto-startup-dev-
# services T7). It opens no DB; it forwards every MCP tool call to the
# container's POST /mcp/<surface> HTTP route. Building/installing it is SAFE at
# any time — it touches no database and changes no .mcp.json. The actual
# cutover (pointing the container at the canonical DB + swapping .mcp.json) is
# scripts/cutover-canonical-db.sh.
#
# Usage:
#   bash scripts/install-proxy.sh           # build + install to ~/.local/bin
#   PROXY_BIN=/custom/path bash scripts/install-proxy.sh
set -euo pipefail
export PATH="$PATH:/usr/local/go/bin"

ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"

PROXY_BIN="${PROXY_BIN:-$HOME/.local/bin/toolkit-proxy}"
PROXY_DIR="$(dirname "$PROXY_BIN")"

fail() { printf '[install-proxy] FAIL: %b\n' "$*" >&2; exit 1; }

command -v go >/dev/null 2>&1 || fail "go not found on PATH"

mkdir -p "$PROXY_DIR"
# Stamp the build SHA + time via -ldflags -X (mirrors go/Makefile for toolkit-server)
# so the installed proxy reports its provenance — the input to the staleness check
# (scripts/check-proxy-staleness.sh) that detects a drifted SPOF.
GIT_SHA="$(git rev-parse --short HEAD 2>/dev/null || echo unversioned)"
BUILT_AT_UNIX="$(date +%s 2>/dev/null || echo 0)"
LDFLAGS="-X main.gitSHA=$GIT_SHA -X main.builtAtUnix=$BUILT_AT_UNIX"
echo "[install-proxy] building cmd/toolkit-proxy ($GIT_SHA) → $PROXY_BIN …"
( cd go && go build -ldflags "$LDFLAGS" -o "$PROXY_BIN" ./cmd/toolkit-proxy/ ) || fail "go build"

# Smoke: --help must exit cleanly (flag.Parse on -h exits 0 after usage).
"$PROXY_BIN" -h >/dev/null 2>&1 || true
[ -x "$PROXY_BIN" ] || fail "proxy binary not executable at $PROXY_BIN"

echo "[install-proxy] OK — installed $PROXY_BIN ($("$PROXY_BIN" -version 2>/dev/null || echo "$GIT_SHA"))"
echo "[install-proxy] .mcp.json command form:"
cat <<EOF
  "toolkit-server": {
    "command": "$PROXY_BIN",
    "args": ["--default-project", "<project>", "--http-base", "http://localhost:3001"]
  }
EOF
case ":$PATH:" in
  *":$PROXY_DIR:"*) ;;
  *) echo "[install-proxy] NOTE: $PROXY_DIR is not on PATH (fine — .mcp.json uses the absolute path above)";;
esac
