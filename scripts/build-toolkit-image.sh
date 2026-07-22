#!/usr/bin/env bash
# Build and smoke-test the toolkit-server BACKEND container image (rootless Podman).
#
# Builds the multi-stage deploy/toolkit-server/Containerfile (context = repo
# root), tags the result, asserts the runtime stage runs as non-root, and runs
# `toolkit-server version` inside the container as an end-to-end proof that the
# CGo-free static binary actually executes on distroless. Mirrors corpos's
# scripts/build-image.sh. Runnable standalone or as a precommit stage (skippable
# via TOOLKIT_PRECOMMIT_SKIP_IMAGE=1, since it needs podman + network).
#
# Usage:
#   scripts/build-toolkit-image.sh                 build + smoke-test
#   scripts/build-toolkit-image.sh --refresh-bases pull floating base tags + print digests to pin
set -euo pipefail
export PATH="$PATH:/usr/local/go/bin"

ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"

IMAGE="${TOOLKIT_IMAGE:-toolkit-server}"
CONTAINERFILE="deploy/toolkit-server/Containerfile"
GOLANG_TAG="docker.io/library/golang:1.26.3"
DISTROLESS_TAG="gcr.io/distroless/static-debian12:nonroot"
SIZE_BUDGET_MB=40   # whole substrate + modernc's pure-Go SQLite; report-only

fail() { printf '[build-toolkit-image] FAIL: %b\n' "$*" >&2; exit 1; }

command -v podman >/dev/null 2>&1 || fail "podman not found (chain mandates rootless podman; \`sudo apt install podman\`)"

# --refresh-bases: re-pull the floating tags and print their current digests so
# they can be pasted back into the Containerfile's pinned FROM lines.
if [ "${1:-}" = "--refresh-bases" ]; then
  echo "[build-toolkit-image] pulling floating base tags to read current digests…"
  podman pull "$GOLANG_TAG" >/dev/null
  podman pull "$DISTROLESS_TAG" >/dev/null
  echo "[build-toolkit-image] pin these digests in $CONTAINERFILE:"
  podman image inspect "$GOLANG_TAG"     --format '  builder : {{index .RepoDigests 0}}'
  podman image inspect "$DISTROLESS_TAG" --format '  runtime : {{index .RepoDigests 0}}'
  exit 0
fi

# Build stamps injected via --build-arg so /version + admin.server_version
# report the real SHA in the container (matches the Makefile's -ldflags -X).
GIT_SHA="$(git rev-parse --short HEAD 2>/dev/null || echo unversioned)"
BUILT_AT_UNIX="$(date +%s)"

echo "[build-toolkit-image] building ${IMAGE}:dev (+ ${IMAGE}:${GIT_SHA}) [git ${GIT_SHA}]…"
podman build \
  --build-arg "GIT_SHA=${GIT_SHA}" \
  --build-arg "BUILT_AT_UNIX=${BUILT_AT_UNIX}" \
  -t "${IMAGE}:dev" -t "${IMAGE}:${GIT_SHA}" \
  -f "$CONTAINERFILE" . \
  || fail "podman build"

# Non-root guarantee: distroless has no shell, so assert via the image config.
user="$(podman image inspect "${IMAGE}:dev" --format '{{.Config.User}}')"
case "$user" in
  nonroot:nonroot|nonroot|65532*) echo "[build-toolkit-image]   runs as non-root user: ${user}" ;;
  ""|root|0|0:0) fail "image runs as root (User=${user:-<empty>}) — non-root invariant broken" ;;
  *) echo "[build-toolkit-image]   WARN: unexpected User=${user} (expected nonroot)" ;;
esac

# End-to-end proof the static binary executes on distroless. `version` runs
# before flag parsing (main.go), so it works despite the server-flag default CMD.
echo "[build-toolkit-image] running \`${IMAGE}:dev version\` in the container…"
out="$(podman run --rm "${IMAGE}:dev" version)"
echo "[build-toolkit-image]   -> ${out}"
case "$out" in
  "${GIT_SHA} built "*) echo "[build-toolkit-image]   version reports the baked SHA ✓" ;;
  *) fail "container version %q did not start with baked SHA %q" "$out" "${GIT_SHA}" ;;
esac

# Size is a target (not a hard contract): report it, warn if over budget.
bytes="$(podman image inspect "${IMAGE}:dev" --format '{{.Size}}')"
mb=$(( bytes / 1024 / 1024 ))
if [ "$mb" -le "$SIZE_BUDGET_MB" ]; then
  echo "[build-toolkit-image]   image size: ${mb}MB (budget ${SIZE_BUDGET_MB}MB) ✓"
else
  echo "[build-toolkit-image]   WARN: image size ${mb}MB exceeds ${SIZE_BUDGET_MB}MB budget"
fi

echo "[build-toolkit-image] OK — ${IMAGE}:dev built, non-root, runs."
