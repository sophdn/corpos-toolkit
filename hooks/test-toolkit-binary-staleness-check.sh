#!/usr/bin/env bash
# test-toolkit-binary-staleness-check.sh — unit test for the SessionStart
# binary-staleness hook. Builds a throwaway git repo + a fake toolkit-server
# stub whose --version SHA we control, and asserts the hook emits a warning
# (or stays silent) for each drift class. No network, no real binary.
set -u

HOOK_DIR="$(cd "$(dirname "$0")" && pwd)"
HOOK="$HOOK_DIR/toolkit-binary-staleness-check.sh"
FAILS=0
fail() { echo "FAIL: $1"; FAILS=$((FAILS + 1)); }

command -v jq >/dev/null 2>&1 || { echo "SKIP: jq not installed"; exit 0; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

REPO="$TMP/repo"
mkdir -p "$REPO/go/cmd/toolkit-server" "$REPO/go/bin"
: >"$REPO/go/cmd/toolkit-server/main.go" # satisfies the mcp-servers guard
git -C "$REPO" init -q
git -C "$REPO" config user.email t@t.t
git -C "$REPO" config user.name t
git -C "$REPO" commit -q --allow-empty -m c1
C1="$(git -C "$REPO" rev-parse --short=8 HEAD)"
git -C "$REPO" commit -q --allow-empty -m c2
HEAD8="$(git -C "$REPO" rev-parse --short=8 HEAD)"
# A divergent commit on a side branch (not an ancestor of HEAD).
git -C "$REPO" checkout -q -b side
git -C "$REPO" commit -q --allow-empty -m d1
D1="$(git -C "$REPO" rev-parse --short=8 HEAD)"
git -C "$REPO" checkout -q -

BIN="$REPO/go/bin/toolkit-server"
stub() { printf '#!/bin/sh\necho "%s built test"\n' "$1" >"$BIN"; chmod +x "$BIN"; }

run() { # run <expect: SILENT|substring>  (env already set by caller)
    local out
    out="$(TOOLKIT_REPO_ROOT="$REPO" TOOLKIT_SERVER_BIN="$BIN" TOOLKIT_SESSION_CWD="$REPO" \
        bash "$HOOK" </dev/null 2>/dev/null)"
    if [ "$1" = "SILENT" ]; then
        [ -z "$out" ] || fail "expected silence, got: $out"
    else
        printf '%s' "$out" | jq -e '.hookSpecificOutput.hookEventName=="SessionStart"' >/dev/null 2>&1 \
            || fail "expected SessionStart JSON, got: $out"
        printf '%s' "$out" | jq -r '.hookSpecificOutput.additionalContext' | grep -q "$1" \
            || fail "expected additionalContext to contain '$1', got: $out"
    fi
}

# 1. in sync → silent
stub "$HEAD8"; run SILENT
# 2. behind (deployed SHA is an ancestor of HEAD) → BEHIND warning
stub "$C1"; run "BEHIND"
# 3. divergent (deployed SHA not an ancestor) → DIVERGENT warning
stub "$D1"; run "DIVERGENT"
# 4. unversioned dev build → silent
stub "unversioned"; run SILENT
# 5. missing binary → silent
rm -f "$BIN"
out="$(TOOLKIT_REPO_ROOT="$REPO" TOOLKIT_SERVER_BIN="$BIN" TOOLKIT_SESSION_CWD="$REPO" bash "$HOOK" </dev/null 2>/dev/null)"
[ -z "$out" ] || fail "missing binary: expected silence, got: $out"
# 6. not an mcp-servers checkout → silent
NREPO="$TMP/notrepo"; mkdir -p "$NREPO"; git -C "$NREPO" init -q
git -C "$NREPO" config user.email t@t.t; git -C "$NREPO" config user.name t
git -C "$NREPO" commit -q --allow-empty -m x
stub "$C1"
out="$(TOOLKIT_REPO_ROOT="$REPO" TOOLKIT_SERVER_BIN="$BIN" TOOLKIT_SESSION_CWD="$NREPO" bash "$HOOK" </dev/null 2>/dev/null)"
[ -z "$out" ] || fail "non-mcp-servers session: expected silence, got: $out"

if [ "$FAILS" -eq 0 ]; then echo "PASS: toolkit-binary-staleness-check ($HEAD8 vs $C1/$D1)"; exit 0; fi
echo "FAILED: $FAILS case(s)"; exit 1
