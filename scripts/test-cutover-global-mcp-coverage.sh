#!/usr/bin/env bash
# Test harness for scripts/cutover-canonical-db.sh global / non-fleet MCP-config
# coverage (bug 986: t7-flip-misses-global-mcpservers-in-claude-json).
#
# The T7 flip only rewrote ~/dev/*/.mcp.json fleet configs, so the GLOBAL
# top-level mcpServers.toolkit-server in ~/.claude.json (and a ~/dev/.mcp.json one
# level up) kept launching the native binary post-flip — a second writer on the
# canonical DB, silently breaking the single-writer invariant. This drives the
# `check`, `migrate-configs`, and `restore-configs` subcommands against a
# throwaway sandbox HOME and asserts the global + dev-level configs are reported
# and swapped surgically. Mirrors the other scripts/test-*.sh gate self-tests
# (subprocess + env overrides; nothing touches the real system).
set -euo pipefail

SCRIPT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/cutover-canonical-db.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fails=0

PROXY="$tmp/bin/toolkit-proxy"
mkdir -p "$tmp/bin"; printf '#!/usr/bin/env bash\n:\n' >"$PROXY"; chmod +x "$PROXY"

CLAUDE_JSON="$tmp/.claude.json"
DEV_ROOT="$tmp/dev/.mcp.json"
PROJ_MCP="$tmp/dev/proj1/.mcp.json"
mkdir -p "$tmp/dev/proj1"

# native_cfg <path> <default-project> [extra-json-to-deep-merge]
# Writes a config whose toolkit-server entry runs the NATIVE binary.
native_cfg() {
	local path="$1" proj="$2" extra="${3:-{\}}"
	jq -n --arg proj "$proj" --argjson extra "$extra" \
		'{mcpServers:{"toolkit-server":{command:"/usr/local/bin/toolkit-server",args:["--db","/x/toolkit.db","--default-project",$proj,"--http-port","3000"]}}} * $extra' \
		>"$path"
}
# The global ~/.claude.json carries a decoy top-level key + a decoy second
# mcpServer to prove the edit is surgical (only toolkit-server changes).
native_cfg "$CLAUDE_JSON" "mcp-servers" '{"numStartups":42,"mcpServers":{"other-server":{"command":"/usr/bin/foo"}}}'
native_cfg "$DEV_ROOT" "dev-root"
native_cfg "$PROJ_MCP" "seed-packet"

# run_cut <subcommand...> — every path sandboxed so the real system is untouched.
run_cut() {
	env \
		CLAUDE_JSON="$CLAUDE_JSON" \
		DEV_ROOT_MCP="$DEV_ROOT" \
		MCP_GLOB="$tmp/dev/*/.mcp.json" \
		PROXY_BIN="$PROXY" \
		CANON_DB="$tmp/toolkit.db" \
		SNAP_DIR="$tmp/snaps" \
		HTTP_BASE="http://localhost:3001" \
		GLOBAL_DEFAULT_PROJECT="mcp-servers" \
		bash "$SCRIPT" "$@"
}

check_eq() { # <name> <got> <want>
	if [ "$2" = "$3" ]; then echo "ok   [$1]"; else echo "FAIL [$1]: got '$2' want '$3'"; fails=1; fi
}
check_contains() { # <name> <haystack> <needle>
	if grep -qF "$3" <<<"$2"; then echo "ok   [$1]"; else echo "FAIL [$1]: missing '$3'"; echo "$2" | sed 's/^/    /'; fails=1; fi
}
jq_get() { jq -r "$2" "$1"; }
arg_after() { # <file> <flag> -> the arg following <flag> in the toolkit-server args
	jq -r --arg f "$2" '.mcpServers["toolkit-server"].args as $a | ($a|index($f)) as $i | if $i then $a[$i+1] else "" end' "$1"
}

# ── 1. check REPORTS the global + dev-level coverage (the gap the bug names) ──
out="$(run_cut check 2>&1)" || true
check_contains "check-global-label"  "$out" "global ~/.claude.json"
check_contains "check-global-native" "$out" "NATIVE - GAP"
check_contains "check-devroot-label" "$out" "dev/.mcp.json"

# ── 2. migrate-configs --yes swaps ALL three to the proxy, surgically ─────────
run_cut migrate-configs --yes >/dev/null 2>&1 || { echo "FAIL [migrate-exit]"; fails=1; }
for pair in "claude:$CLAUDE_JSON" "devroot:$DEV_ROOT" "proj:$PROJ_MCP"; do
	name="${pair%%:*}"; f="${pair#*:}"
	check_eq "swap-$name-cmd"  "$(jq_get "$f" '.mcpServers["toolkit-server"].command')" "$PROXY"
	check_eq "swap-$name-base" "$(arg_after "$f" '--http-base')" "http://localhost:3001"
	if [ -f "${f}.pre-t7-flip" ]; then echo "ok   [backup-$name]"; else echo "FAIL [backup-$name]"; fails=1; fi
done
# default-project preserved per file (extracted from the native original)
check_eq "claude-proj-preserved" "$(arg_after "$CLAUDE_JSON" '--default-project')" "mcp-servers"
check_eq "proj-proj-preserved"   "$(arg_after "$PROJ_MCP" '--default-project')"   "seed-packet"
check_eq "devroot-proj-preserved" "$(arg_after "$DEV_ROOT" '--default-project')"  "dev-root"
# surgical: unrelated keys in the big ~/.claude.json are untouched
check_eq "claude-decoy-key"    "$(jq_get "$CLAUDE_JSON" '.numStartups')" "42"
check_eq "claude-other-server" "$(jq_get "$CLAUDE_JSON" '.mcpServers["other-server"].command')" "/usr/bin/foo"

# ── 3. re-running migrate-configs is an idempotent no-op (no backup clobber) ──
run_cut migrate-configs --yes >/dev/null 2>&1 || true
check_eq "idempotent-still-proxy" "$(jq_get "$CLAUDE_JSON" '.mcpServers["toolkit-server"].command')" "$PROXY"

# ── 4. restore-configs --yes brings the native originals back from backup ─────
run_cut restore-configs --yes >/dev/null 2>&1 || { echo "FAIL [restore-exit]"; fails=1; }
check_eq "restore-claude-native" "$(jq_get "$CLAUDE_JSON" '.mcpServers["toolkit-server"].command')" "/usr/local/bin/toolkit-server"
if [ -f "${CLAUDE_JSON}.pre-t7-flip" ]; then echo "FAIL [backup-consumed-claude]"; fails=1; else echo "ok   [backup-consumed-claude]"; fi

if [ "$fails" = 0 ]; then echo "PASS: cutover global MCP-config coverage"; else echo "FAIL: cutover global MCP-config coverage"; exit 1; fi
