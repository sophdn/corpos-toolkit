#!/usr/bin/env bash
# Test harness for scripts/check-proxy-staleness.sh — drives it against a throwaway
# git repo + stub proxy binaries covering each branch. Mirrors the other
# scripts/test-*.sh gate self-tests.
set -euo pipefail

CHECK="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/check-proxy-staleness.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fails=0

# A stub proxy that answers `-version` like the real one ("toolkit-proxy <sha> built <n>").
# mode: a sha string, "unversioned", or "noflag" (a pre-T5 proxy: errors on -version).
mkstub() {
	local path="$1" mode="$2"
	if [ "$mode" = "noflag" ]; then
		printf '#!/usr/bin/env bash\necho "flag provided but not defined: -version" >&2\nexit 2\n' >"$path"
	else
		printf '#!/usr/bin/env bash\necho "toolkit-proxy %s built 0"\n' "$mode" >"$path"
	fi
	chmod +x "$path"
}

# run <name> <want_exit> <needle> <proxy_bin> [extra env KEY=VAL ...]  (cwd is $REPO)
run() {
	local name="$1" want="$2" needle="$3" bin="$4"
	shift 4
	local out rc
	out="$(cd "$REPO" && env PROXY_BIN="$bin" "$@" bash "$CHECK" 2>&1)" && rc=0 || rc=$?
	if [ "$rc" != "$want" ]; then
		echo "FAIL [$name]: exit $rc, want $want"; echo "$out" | sed 's/^/    /'; fails=1; return
	fi
	if [ -n "$needle" ] && ! grep -qF "$needle" <<<"$out"; then
		echo "FAIL [$name]: output missing: $needle"; echo "$out" | sed 's/^/    /'; fails=1; return
	fi
	echo "ok   [$name] (exit $rc)"
}

# Build a throwaway repo: c1 touches a proxy path, c2 is unrelated (so the latest
# proxy-source commit is c1, an ancestor of HEAD=c2).
REPO="$tmp/repo"; mkdir -p "$REPO/go/cmd/toolkit-proxy"
( cd "$REPO" && git init -q && git config user.email t@t && git config user.name t \
	&& echo a >go/cmd/toolkit-proxy/main.go && git add -A && git commit -qm c1 \
	&& echo x >unrelated.txt && git add -A && git commit -qm c2 )
B="$(cd "$REPO" && git rev-parse --short HEAD)"

run "missing"      1 "no installed proxy"            "$tmp/nonexistent-proxy"
mkstub "$tmp/unversioned" "unversioned"; run "unversioned" 1 "'unversioned'" "$tmp/unversioned"
mkstub "$tmp/noflag" "noflag";           run "no-version-flag" 1 "does not report -version" "$tmp/noflag"
mkstub "$tmp/ok" "$B";                   run "ok-ancestor" 0 "includes the latest proxy source" "$tmp/ok"

# c3 touches a proxy path → the latest proxy-source commit is now newer than the
# installed build B → STALE.
( cd "$REPO" && echo b >>go/cmd/toolkit-proxy/main.go && git add -A && git commit -qm c3 )
run "stale" 1 "STALE" "$tmp/ok"

# SKIP when the proxy source commit can't be resolved (run outside any git repo).
nonrepo="$tmp/nonrepo"; mkdir -p "$nonrepo"
out="$(cd "$nonrepo" && PROXY_BIN="$tmp/ok" bash "$CHECK" 2>&1)" && rc=0 || rc=$?
if [ "$rc" = 0 ] && grep -qF "SKIP" <<<"$out"; then
	echo "ok   [skip-no-src] (exit 0)"
else
	echo "FAIL [skip-no-src]: exit $rc"; echo "$out" | sed 's/^/    /'; fails=1
fi

if [ "$fails" != 0 ]; then echo "[test-proxy-staleness] FAILED"; exit 1; fi
echo "[test-proxy-staleness] all cases passed."
