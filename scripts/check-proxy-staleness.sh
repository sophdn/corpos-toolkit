#!/usr/bin/env bash
# Pre-flight staleness check for the hand-installed stdio proxy.
#
# The toolkit-proxy (cmd/toolkit-proxy) is a SPOF: it is built by hand into
# ~/.local/bin/toolkit-proxy (scripts/install-proxy.sh) and every project's
# .mcp.json invokes it, so a stale/missing proxy silently breaks all Claude Code
# MCP. This mirrors toolkit-server's server_version vs git-HEAD ritual for the
# proxy: it reads the INSTALLED proxy's ldflags-stamped build SHA (toolkit-proxy
# -version) and asks whether that build includes the latest proxy SOURCE.
#
# "Stale" = the most recent commit touching the proxy's source (cmd/toolkit-proxy
# or a package it embeds: internal/dispatch, internal/actiondocs) is NOT an
# ancestor of the installed build. Using ancestry (not SHA==HEAD) means an
# unrelated commit does not false-positive, and reinstalling at a newer HEAD stays
# green.
#
#   exit 0  in sync (or cannot resolve the repo — SKIP)
#   exit 1  missing / un-stamped / no -version / drifted — refresh with install-proxy.sh
#
# Overrides for testing: PROXY_BIN (installed binary), PROXY_SRC_SHA (the latest
# proxy-source commit; default: git log over the proxy paths).
set -euo pipefail

PROXY_BIN="${PROXY_BIN:-$HOME/.local/bin/toolkit-proxy}"
FALLBACK="refresh it with: scripts/install-proxy.sh"

if [ ! -x "$PROXY_BIN" ]; then
	echo "[proxy-staleness] FAIL: no installed proxy at $PROXY_BIN — every .mcp.json depends on it. $FALLBACK" >&2
	exit 1
fi

ver_line="$("$PROXY_BIN" -version 2>/dev/null || true)" # "toolkit-proxy <sha> built <unix>"
installed_sha="$(awk '{print $2}' <<<"$ver_line")"
if [ -z "$installed_sha" ]; then
	echo "[proxy-staleness] FAIL: $PROXY_BIN does not report -version (a pre-T5 proxy). $FALLBACK" >&2
	exit 1
fi
if [ "$installed_sha" = "unversioned" ]; then
	echo "[proxy-staleness] FAIL: installed proxy is 'unversioned' (built without the ldflags stamp — a bare go build, not install-proxy.sh). $FALLBACK" >&2
	exit 1
fi

src_sha="${PROXY_SRC_SHA:-$(git log -1 --format=%H -- go/cmd/toolkit-proxy go/internal/dispatch go/internal/actiondocs 2>/dev/null || true)}"
if [ -z "$src_sha" ]; then
	echo "[proxy-staleness] SKIP: cannot resolve the proxy source commit (not in the toolkit repo); installed proxy is $installed_sha."
	exit 0
fi

if git merge-base --is-ancestor "$src_sha" "$installed_sha" 2>/dev/null; then
	echo "[proxy-staleness] OK: installed proxy ($installed_sha) includes the latest proxy source ($(git rev-parse --short "$src_sha" 2>/dev/null || echo "$src_sha"))."
	exit 0
fi

echo "[proxy-staleness] STALE: installed proxy ($installed_sha) predates the latest proxy-source commit ($(git rev-parse --short "$src_sha" 2>/dev/null || echo "$src_sha")). $FALLBACK" >&2
exit 1
