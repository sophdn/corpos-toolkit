#!/usr/bin/env bash
# THE FLIP (chain auto-startup-dev-services T7) — make the toolkit CONTAINER the
# single owner of the canonical DB, reached by every session through the
# stdio→HTTP proxy. Highest blast radius in the chain; reversible by design.
#
#   bash scripts/cutover-canonical-db.sh check      # report state, prove readiness — NO changes
#   bash scripts/cutover-canonical-db.sh flip --yes  # perform the cutover
#   bash scripts/cutover-canonical-db.sh rollback --yes  # revert to native :3000 + stdio
#   bash scripts/cutover-canonical-db.sh migrate-configs --yes  # swap MCP configs only (no container dance)
#   bash scripts/cutover-canonical-db.sh restore-configs --yes  # undo just the config swap
#
# ───────────────────────────────────────────────────────────────────────────
# RUN THIS FROM A SHELL THAT IS **NOT** A TOOLKIT-MOUNTED CLAUDE SESSION.
# The flip stops the native stdio path; a session running through it holds the
# canonical DB open and would block the single-writer guarantee (it would abort
# the flip — by design). Open a plain terminal, not a `claude` session that
# mounts toolkit-server.
# ───────────────────────────────────────────────────────────────────────────
#
# What flip does, in order:
#   1. Preflight: proxy binary installed, canonical quadlet present, image built.
#   2. Stop the :3001 container (the tks-data/separate-DB one) + native :3000 daemon.
#   3. SINGLE-WRITER GUARD: assert no process still holds the canonical DB file.
#      If any do (lingering native stdio sessions), ABORT and list them.
#   4. Install the canonical-bind quadlet over the live unit (backup kept).
#   5. Swap every toolkit-server MCP config from the native stdio binary → the
#      proxy (backups kept): the fleet ~/dev/*/.mcp.json files, ~/dev/.mcp.json
#      one level up, AND the global mcpServers.toolkit-server in ~/.claude.json
#      (which governs any session rooted outside a fleet project dir — bug 986).
#   6. Start the container (now the sole opener of the canonical DB) + verify.
# rollback reverses 4→6 and restarts the native :3000 daemon.
set -euo pipefail
export PATH="$PATH:/usr/local/go/bin"

ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"

# ── config (override via env) ───────────────────────────────────────────────
CANON_DB="${CANON_DB:-$HOME/.local/share/toolkit/data/toolkit.db}"
PROXY_BIN="${PROXY_BIN:-$HOME/.local/bin/toolkit-proxy}"
HTTP_BASE="${HTTP_BASE:-http://localhost:3001}"
QUADLET_DIR="${QUADLET_DIR:-$HOME/.config/containers/systemd}"
LIVE_UNIT="$QUADLET_DIR/toolkit-server.container"
CANON_UNIT_SRC="$ROOT/deploy/quadlet/toolkit-server-canonical.container"
LAUNCH_SH="${LAUNCH_SH:-$ROOT/go/launch.sh}"
MCP_GLOB="${MCP_GLOB:-$HOME/dev/*/.mcp.json}"
BACKUP_SUFFIX=".pre-t7-flip"
SNAP_DIR="${SNAP_DIR:-$HOME/db-snapshots}"
# Non-fleet MCP configs the per-project glob can't reach (bug 986). The global
# top-level mcpServers.toolkit-server in ~/.claude.json governs any session
# rooted outside a fleet project dir (e.g. ~/dev itself); ~/dev/.mcp.json sits one
# level above the glob. Both keep opening the canonical DB natively post-flip — a
# second writer that silently breaks the T7 single-writer invariant — unless they
# move to the proxy too.
CLAUDE_JSON="${CLAUDE_JSON:-$HOME/.claude.json}"
DEV_ROOT_MCP="${DEV_ROOT_MCP:-$HOME/dev/.mcp.json}"
# default-project to stamp on a global/dev config whose native entry set none
# explicitly (the proxy still needs a session default).
GLOBAL_DEFAULT_PROJECT="${GLOBAL_DEFAULT_PROJECT:-mcp-servers}"

note() { printf '[cutover] %b\n' "$*"; }
fail() { printf '[cutover] FAIL: %b\n' "$*" >&2; exit 1; }

# snapshot takes a CONSISTENT copy of the canonical DB (sqlite3 .backup works
# safely under live writers), verifies it with integrity_check, and prints the
# restore command. Aborts the flip if the snapshot can't be taken or doesn't
# verify — we never mutate the canonical access path without a good backstop.
# The flip changes who OPENS the file, not the data; this snapshot is the
# belt-and-braces against a botched WAL/ownership transition. Restore (with the
# container stopped + nothing holding the file) is a plain file copy back.
snapshot() {
  command -v sqlite3 >/dev/null 2>&1 || fail "sqlite3 not found — cannot take a pre-flip snapshot"
  mkdir -p "$SNAP_DIR"
  local stamp snap
  stamp="$(date +%Y%m%d-%H%M%S)"
  snap="$SNAP_DIR/toolkit.db.pre-t7-flip-$stamp"
  note "snapshotting canonical DB → $snap …"
  sqlite3 "$CANON_DB" ".backup '$snap'" || fail "snapshot failed (sqlite3 .backup)"
  local chk
  chk="$(sqlite3 "$snap" 'PRAGMA integrity_check;' 2>&1 | head -1)"
  [ "$chk" = "ok" ] || fail "snapshot integrity_check returned '$chk' (expected ok) — refusing to flip"
  note "snapshot verified (integrity_check ok). Restore if needed:"
  note "    systemctl --user stop toolkit-server   # nothing must hold the file"
  note "    cp -a '$snap' '$CANON_DB' && rm -f '${CANON_DB}-wal' '${CANON_DB}-shm'"
}

# holders prints the PIDs (one per line) that currently have the canonical DB
# file open, via fuser. Empty output = nobody holds it.
holders() { fuser "$CANON_DB" 2>/dev/null | tr -s ' ' '\n' | grep -E '^[0-9]+$' || true; }

# native_daemon_pid returns the PID listening on :3000 (the native HTTP daemon),
# or empty. ss output: ...users:(("toolkit-server",pid=NNNN,fd=N))
native_daemon_pid() {
  ss -ltnp 2>/dev/null | awk '/:3000 /{print}' | grep -oE 'pid=[0-9]+' | head -1 | cut -d= -f2 || true
}

# fleet_files lists the .mcp.json paths that currently mount the NATIVE
# toolkit-server stdio binary (command basename == toolkit-server). Those are
# the ones the flip rewrites to the proxy.
fleet_files() {
  local f cmd
  for f in $MCP_GLOB; do
    [ -f "$f" ] || continue
    cmd="$(jq -r '.mcpServers["toolkit-server"].command // empty' "$f" 2>/dev/null || true)"
    case "$cmd" in
      */toolkit-server) echo "$f" ;;
    esac
  done
}

require_yes() {
  case "${1:-}" in
    --yes|-y) return 0 ;;
    *) fail "refusing to mutate without --yes (run \`check\` first to preview)";;
  esac
}

# is_native_toolkit <file> — true if the file's toolkit-server entry launches the
# NATIVE binary (command basename toolkit-server), i.e. it still needs swapping.
is_native_toolkit() {
  local cmd
  cmd="$(jq -r '.mcpServers["toolkit-server"].command // empty' "$1" 2>/dev/null || true)"
  case "$cmd" in */toolkit-server) return 0 ;; *) return 1 ;; esac
}

# entry_default_project <file> — the --default-project value in the toolkit-server
# args, or empty if unset.
entry_default_project() {
  jq -r '.mcpServers["toolkit-server"].args as $a | ($a|index("--default-project")) as $i | if $i then $a[$i+1] else "" end' "$1" 2>/dev/null || true
}

# swap_one_to_proxy <file> <fallback-default-project> — surgically rewrite ONE
# config's toolkit-server entry to the proxy form (jq + atomic mv), keeping a
# *.pre-t7-flip backup. No-op if the file is absent or already on the proxy
# (idempotent — protects an existing native backup from being clobbered). Same jq
# path works for fleet, ~/dev/.mcp.json, and the global ~/.claude.json block;
# ~/.claude.json is large + continuously rewritten by Claude Code, so the atomic
# mv matters and any clobber race is resolved by the session's next /mcp reconnect.
swap_one_to_proxy() {
  local f="$1" fallback="${2:-}" proj tmp
  [ -f "$f" ] || return 0
  is_native_toolkit "$f" || return 0
  proj="$(entry_default_project "$f")"
  [ -n "$proj" ] || proj="$fallback"
  cp -a "$f" "${f}${BACKUP_SUFFIX}"
  tmp="$(mktemp)"
  jq --arg cmd "$PROXY_BIN" --arg proj "$proj" --arg base "$HTTP_BASE" \
     '.mcpServers["toolkit-server"] = {command:$cmd, args:(["--default-project",$proj,"--http-base",$base])}' \
     "$f" > "$tmp" && mv "$tmp" "$f"
  note "    $f  → proxy (default-project: ${proj:-<none>})"
}

# swap_all_configs — move EVERY toolkit-server MCP config to the proxy: the fleet
# per-project files, ~/dev/.mcp.json, and the global ~/.claude.json block. Shared
# by `flip` and the standalone `migrate-configs` subcommand.
swap_all_configs() {
  note "swapping MCP configs → proxy (backups: *${BACKUP_SUFFIX})…"
  local f
  for f in $(fleet_files); do swap_one_to_proxy "$f" ""; done
  swap_one_to_proxy "$DEV_ROOT_MCP" "$GLOBAL_DEFAULT_PROJECT"
  swap_one_to_proxy "$CLAUDE_JSON"  "$GLOBAL_DEFAULT_PROJECT"
}

# restore_all_configs — move every *.pre-t7-flip backup back over its config
# (fleet glob + ~/dev/.mcp.json + global ~/.claude.json). Shared by `rollback`
# and the standalone `restore-configs` subcommand.
restore_all_configs() {
  note "restoring MCP configs from *${BACKUP_SUFFIX} backups…"
  local b f
  # shellcheck disable=SC2086  # intentional glob expansion of the fleet backup pattern
  for b in ${MCP_GLOB}${BACKUP_SUFFIX} "${DEV_ROOT_MCP}${BACKUP_SUFFIX}" "${CLAUDE_JSON}${BACKUP_SUFFIX}"; do
    [ -e "$b" ] || continue
    f="${b%"$BACKUP_SUFFIX"}"
    mv "$b" "$f"
    note "    restored $f"
  done
}

# report_config_coverage <file> <label> — one check-line classifying a non-fleet
# config: NATIVE (the gap — would be swapped), proxy (covered), no entry, or absent.
report_config_coverage() {
  local f="$1" label="$2" cmd
  if [ ! -f "$f" ]; then note "    $label: absent (no config file)"; return; fi
  cmd="$(jq -r '.mcpServers["toolkit-server"].command // empty' "$f" 2>/dev/null || true)"
  case "$cmd" in
    "")               note "    $label: no toolkit-server entry" ;;
    */toolkit-server) note "    $label: NATIVE - GAP (would be swapped by flip)" ;;
    *)                note "    $label: proxy (covered)" ;;
  esac
}

# ── check ────────────────────────────────────────────────────────────────────
do_check() {
  note "── readiness check (no changes) ──"
  note "canonical DB:        $CANON_DB"
  note "proxy binary:        $PROXY_BIN $( [ -x "$PROXY_BIN" ] && echo '(installed ✓)' || echo '(MISSING — run scripts/install-proxy.sh)')"
  note "canonical quadlet:   $CANON_UNIT_SRC $( [ -f "$CANON_UNIT_SRC" ] && echo '✓' || echo 'MISSING')"
  note "live quadlet unit:   $LIVE_UNIT $( [ -f "$LIVE_UNIT" ] && echo '(present)' || echo '(absent)')"

  local img
  img="$(podman image inspect localhost/toolkit-server:dev --format '{{index .RepoDigests 0}}{{.Id}}' 2>/dev/null || true)"
  if [ -n "$img" ]; then
    note "image localhost/toolkit-server:dev present"
  else
    note "image localhost/toolkit-server:dev MISSING — build with scripts/build-toolkit-image.sh"
  fi
  note "  (ensure the image includes the X-MCP header support — rebuild from this branch before flipping)"

  local nd; nd="$(native_daemon_pid)"
  if [ -n "$nd" ]; then
    note "native :3000 daemon: RUNNING (pid $nd)"
  else
    note "native :3000 daemon: not listening"
  fi

  note "DB holders right now:"
  holders | while read -r p; do
    note "    pid $p  $(ps -o comm= -p "$p" 2>/dev/null || echo '?')"
  done
  [ -z "$(holders)" ] && note "    (none)"

  note "fleet .mcp.json on the NATIVE stdio binary (would be swapped):"
  fleet_files | while read -r f; do
    note "    $f  (default-project: $(jq -r '.mcpServers["toolkit-server"].args as $a | ($a|index("--default-project")) as $i | if $i then $a[$i+1] else "?" end' "$f"))"
  done
  [ -z "$(fleet_files)" ] && note "    (none — already on the proxy?)"

  note "non-fleet MCP configs the per-project glob can't reach (bug 986):"
  report_config_coverage "$DEV_ROOT_MCP" "the ~/dev/.mcp.json one level above the fleet glob"
  report_config_coverage "$CLAUDE_JSON"  "global ~/.claude.json mcpServers.toolkit-server"

  note "snapshot dir: $SNAP_DIR (flip auto-snapshots here first)"
  local latest
  # shellcheck disable=SC2012  # snapshot names are date-stamped (no special chars); ls -t is the simple mtime sort
  latest="$(ls -1t "$SNAP_DIR"/toolkit.db.pre-t7-flip-* 2>/dev/null | head -1 || true)"
  if [ -n "$latest" ]; then
    note "  latest snapshot: $latest ($(date -r "$latest" '+%Y-%m-%d %H:%M' 2>/dev/null || echo '?'))"
  else
    note "  (none yet — the flip will create one)"
  fi
  note "── end check ──"
}

# ── flip ───────────────────────────────────────────────────────────────────
do_flip() {
  require_yes "${1:-}"
  [ -x "$PROXY_BIN" ] || fail "proxy not installed at $PROXY_BIN — run scripts/install-proxy.sh first"
  [ -f "$CANON_UNIT_SRC" ] || fail "canonical quadlet missing: $CANON_UNIT_SRC"

  # Belt-and-braces backstop BEFORE touching anything (the DB is still
  # consistent + live; .backup works under writers). Aborts on any snapshot
  # failure — no flip without a verified backup.
  snapshot

  note "stopping the :3001 container (tks-data DB)…"
  systemctl --user stop toolkit-server 2>/dev/null || note "  (container unit not active — ok)"

  local nd; nd="$(native_daemon_pid)"
  if [ -n "$nd" ]; then
    note "stopping native :3000 daemon (pid $nd)…"
    kill "$nd" 2>/dev/null || true
    sleep 2
  fi

  # SINGLE-WRITER GUARD — non-negotiable.
  local remaining; remaining="$(holders)"
  if [ -n "$remaining" ]; then
    note "STILL-OPEN holders of $CANON_DB:"
    echo "$remaining" | while read -r p; do note "    pid $p  $(ps -o comm=,args= -p "$p" 2>/dev/null || echo '?')"; done
    fail "canonical DB is still held (likely live native stdio sessions). Close those Claude sessions and re-run.\n      The container must be the SOLE opener before it binds the canonical file."
  fi
  note "single-writer precondition met: nothing holds $CANON_DB ✓"

  note "installing canonical-bind quadlet (backup: ${LIVE_UNIT}${BACKUP_SUFFIX})…"
  [ -f "$LIVE_UNIT" ] && cp -a "$LIVE_UNIT" "${LIVE_UNIT}${BACKUP_SUFFIX}"
  cp -a "$CANON_UNIT_SRC" "$LIVE_UNIT"
  systemctl --user daemon-reload

  swap_all_configs

  note "starting the container on the canonical DB…"
  systemctl --user start toolkit-server
  sleep 3

  # verify single-writer + a real read
  note "post-flip holders of $CANON_DB:"
  holders | while read -r p; do note "    pid $p  $(ps -o comm= -p "$p" 2>/dev/null || echo '?')"; done
  if curl -fsS -m 8 -X POST "$HTTP_BASE/mcp/work" -H 'Content-Type: application/json' \
       -d '{"action":"chain_status"}' >/dev/null 2>&1; then
    note "container serves the canonical ledger over $HTTP_BASE ✓"
  else
    note "WARN: could not read $HTTP_BASE/mcp/work — check \`podman logs toolkit-server\`; rollback available."
  fi
  note "FLIP COMPLETE. New Claude sessions use the proxy → container (canonical DB)."
  note "This shell's own toolkit access (if any) is unchanged until you reconnect/relaunch."
}

# ── rollback ─────────────────────────────────────────────────────────────────
do_rollback() {
  require_yes "${1:-}"
  note "stopping the canonical-bind container…"
  systemctl --user stop toolkit-server 2>/dev/null || true

  if [ -f "${LIVE_UNIT}${BACKUP_SUFFIX}" ]; then
    note "restoring the pre-flip quadlet…"
    mv "${LIVE_UNIT}${BACKUP_SUFFIX}" "$LIVE_UNIT"
    systemctl --user daemon-reload
  else
    note "no quadlet backup found — leaving $LIVE_UNIT as-is"
  fi

  restore_all_configs

  note "restarting the native :3000 daemon…"
  if [ -x "$LAUNCH_SH" ]; then
    HTTP_PORT=3000 TOOLKIT_DB="$CANON_DB" nohup "$LAUNCH_SH" >/tmp/toolkit-native-3000.log 2>&1 &
    sleep 2
    note "  native daemon relaunched (log: /tmp/toolkit-native-3000.log)"
  else
    note "  launch.sh not executable at $LAUNCH_SH — start native manually: HTTP_PORT=3000 TOOLKIT_DB=$CANON_DB go/launch.sh"
  fi
  note "ROLLBACK COMPLETE. Native :3000 + stdio fleet restored. (Restart the tks-data container if you want it back: systemctl --user start toolkit-server — note the quadlet is back to the separate-volume form.)"
}

# ── migrate-configs / restore-configs (companion to flip; bug 986) ───────────
# Move every toolkit-server MCP config to/from the proxy WITHOUT the full
# container/daemon dance. Lets you remediate the config layer on an
# already-flipped system (e.g. a global ~/.claude.json the original flip missed)
# or undo just the config swap, without re-running the whole cutover.
do_migrate_configs() {
  require_yes "${1:-}"
  [ -x "$PROXY_BIN" ] || fail "proxy not installed at $PROXY_BIN — run scripts/install-proxy.sh first"
  swap_all_configs
  note "MCP config migration complete. New sessions from ANY cwd use the proxy → container."
  note "Active sessions keep their current binary until /mcp reconnect or relaunch."
}

do_restore_configs() {
  require_yes "${1:-}"
  restore_all_configs
  note "MCP configs restored from *${BACKUP_SUFFIX} backups."
}

case "${1:-check}" in
  check)           do_check ;;
  flip)            shift; do_flip "$@" ;;
  rollback)        shift; do_rollback "$@" ;;
  migrate-configs) shift; do_migrate_configs "$@" ;;
  restore-configs) shift; do_restore_configs "$@" ;;
  *) fail "unknown subcommand '${1}'. Use: check | flip --yes | rollback --yes | migrate-configs --yes | restore-configs --yes" ;;
esac
