#!/usr/bin/env bash
# Install the toolkit-server Quadlet units into the rootless systemd-user dir and
# reload. The canonical unit files live in deploy/quadlet/ (version-controlled);
# this script materializes them into ~/.config/containers/systemd/ where Quadlet's
# generator (/usr/libexec/podman/quadlet) reads them at `systemctl --user
# daemon-reload`. Re-run after editing any unit.
#
# Usage:
#   scripts/install-quadlet-units.sh          install + daemon-reload (does NOT start)
#   scripts/install-quadlet-units.sh --start  install + daemon-reload + start toolkit-server
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
SRC="$ROOT/deploy/quadlet"
DEST="${XDG_CONFIG_HOME:-$HOME/.config}/containers/systemd"

fail() { printf '[install-quadlet] FAIL: %b\n' "$*" >&2; exit 1; }

[ -d "$SRC" ] || fail "unit source $SRC missing"
command -v podman >/dev/null 2>&1 || fail "podman not found"
[ -e /usr/libexec/podman/quadlet ] || echo "[install-quadlet] WARN: Quadlet generator not at /usr/libexec/podman/quadlet — daemon-reload may not generate services"

# --- host-gateway / sibling-container landmine guard ------------------------
# A `--add-host=<name>:host-gateway` that names a SIBLING container in this unit
# set is a latent outage: once <name> is itself a container, podman resolves
# `host-gateway` to an EMPTY host-containers-internal IP and refuses to start the
# container (exit 126 → crash-loop → systemd "start request repeated too
# quickly"). This is exactly the 2026-06-07 toolkit-server outage: the unit kept
# the native-llama-era `--add-host=llama-server:host-gateway` after llama moved
# into the llama-server container. Catch it at install time, not at 2am — once a
# dependency is a sibling container, address it by its corpos-net DNS name (drop
# the --add-host). Override (rare, genuine host-gateway use to reach the actual
# host): TOOLKIT_QUADLET_ALLOW_HOST_GATEWAY_SIBLING=1.
guard_host_gateway_siblings() {
  local container_names hit=0 f name
  container_names="$(grep -hoP '^\s*ContainerName=\K\S+' "$SRC"/*.container 2>/dev/null | sort -u)"
  for f in "$SRC"/*.container; do
    [ -e "$f" ] || continue
    while IFS= read -r name; do
      [ -n "$name" ] || continue
      if grep -qxF "$name" <<<"$container_names"; then
        echo "[install-quadlet] LANDMINE: $(basename "$f") maps --add-host=$name:host-gateway," >&2
        echo "                  but '$name' is itself a container unit in this set. host-gateway" >&2
        echo "                  will resolve to an empty IP and the container will crash-loop" >&2
        echo "                  (exit 126). Reach '$name' by its corpos-net DNS name instead and" >&2
        echo "                  drop the --add-host line. (bug toolkit-server-quadlet-stale-add-host-*)" >&2
        hit=1
      fi
      # strip comment lines first — the explanatory comments in these units quote
      # the `--add-host=…:host-gateway` pattern on purpose, and matching those would
      # false-positive on the very units we ship. Only real directive lines count.
    done < <(grep -v '^[[:space:]]*#' "$f" | grep -oP -- '--add-host=\K[^:[:space:]]+(?=:host-gateway)' 2>/dev/null)
  done
  if [ "$hit" = 1 ] && [ -z "${TOOLKIT_QUADLET_ALLOW_HOST_GATEWAY_SIBLING:-}" ]; then
    fail "host-gateway/sibling-container landmine (see above). Override with TOOLKIT_QUADLET_ALLOW_HOST_GATEWAY_SIBLING=1 if this is genuinely a host (not sibling) target."
  fi
}
guard_host_gateway_siblings

mkdir -p "$DEST"
echo "[install-quadlet] installing units → $DEST"
for u in "$SRC"/*.network "$SRC"/*.volume "$SRC"/*.container; do
  [ -e "$u" ] || continue
  install -m 0644 "$u" "$DEST/$(basename "$u")"
  echo "  + $(basename "$u")"
done

echo "[install-quadlet] systemctl --user daemon-reload"
systemctl --user daemon-reload

# enable-linger so the units come up at boot and survive logout (rootless). May
# require privilege depending on polkit; warn rather than fail if it can't.
if [ "$(loginctl show-user "$USER" 2>/dev/null | sed -n 's/^Linger=//p')" != "yes" ]; then
  if loginctl enable-linger "$USER" 2>/dev/null; then
    echo "[install-quadlet] enabled linger for $USER (boot-time start)"
  else
    echo "[install-quadlet] NOTE: could not enable linger non-privileged — run: sudo loginctl enable-linger $USER"
    echo "                  (units still start now via systemctl --user; linger only affects boot/logout)"
  fi
fi

if [ "${1:-}" = "--start" ]; then
  echo "[install-quadlet] starting toolkit-server.service"
  systemctl --user start toolkit-server.service
  systemctl --user --no-pager status toolkit-server.service | head -8 || true
else
  echo "[install-quadlet] done. Start with: systemctl --user start toolkit-server.service"
fi
