#!/usr/bin/env bash
# scripts/validate-quadlet-units.sh — assert the deploy/quadlet/*.container units
# carry the directives the containerized stack depends on at runtime.
#
# WHY THIS EXISTS: the T7 canonical-DB flip moved the toolkit WRITER into a
# container, and two directives that the native :3000 daemon never needed became
# load-bearing — and were initially MISSING, silently breaking real features:
#   - the host vault bind: without it, memory/vault-note/retrospective artifacts
#     write to the container's EPHEMERAL fs (/home/nonroot/.claude/vault), so the
#     bodies are lost on restart and never materialize into MEMORY.md.
#     (bug containerized-toolkit-writes-vault-artifacts-to-ephemeral-container-fs-not-host-vault)
#   - TOOLKIT_LOCAL_URL / LLAMA_CPP_BASE_URL: default localhost:8081 is the
#     toolkit container ITSELF, not the llama-server container — so rerank /
#     vault_search / arc-review all connection-refused.
#     (bug containerized-toolkit-reaches-llama-at-localhost-8081-not-llama-server-container)
#
# These are deployment-artifact regressions a Go unit test can't see, so this
# pure-bash validator is wired into scripts/precommit.sh (local gate) AND
# .gitea/workflows/ci.yaml (CI) — it needs no systemd/podman, only the files.
# Mirrors sophdn/llama-server's scripts/validate-unit.sh (golden-directive assert).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
Q="$ROOT/deploy/quadlet"

fails=0
pass=0
err()  { printf '  ✗ %s\n' "$*" >&2; fails=$((fails + 1)); }
ok()   { printf '  ✓ %s\n' "$*"; pass=$((pass + 1)); }

# need FILE REGEX MESSAGE — assert REGEX matches a line in FILE.
need() { if grep -Eq "$2" "$1"; then ok "$3"; else err "$3 [expected /$2/ in $(basename "$1")]"; fi; }
# deny FILE REGEX MESSAGE — assert REGEX does NOT match any line in FILE.
deny() { if grep -Eq "$2" "$1"; then err "$3 [forbidden /$2/ present in $(basename "$1")]"; else ok "$3"; fi; }

echo "[validate-quadlet] $Q"

# ── toolkit-server-canonical.container — the flip target (canonical-DB owner) ──
CANON="$Q/toolkit-server-canonical.container"
if [ ! -f "$CANON" ]; then
  err "missing $CANON"
else
  echo "toolkit-server-canonical.container:"
  need "$CANON" '^Volume=.*:/data$'                                   "binds the host canonical-DB dir → /data"
  need "$CANON" '^Volume=.*:/home/nonroot/\.claude/vault$'            "binds the host vault → /home/nonroot/.claude/vault (else vault artifacts hit ephemeral fs)"
  need "$CANON" '^UserNS=keep-id'                                     "keep-id uid map (nonroot → host user) for host-file writes"
  need "$CANON" '^Environment=TOOLKIT_LOCAL_URL=https?://'            "sets TOOLKIT_LOCAL_URL (llama base — default localhost is unreachable in-container)"
  deny "$CANON" '^Environment=TOOLKIT_LOCAL_URL=https?://(localhost|127\.0\.0\.1)' "TOOLKIT_LOCAL_URL is NOT localhost/127.0.0.1 (that's this container, not llama-server)"
  deny "$CANON" '^Environment=LLAMA_CPP_BASE_URL=https?://(localhost|127\.0\.0\.1)' "LLAMA_CPP_BASE_URL (if set) is NOT localhost/127.0.0.1"
  need "$CANON" '^PublishPort=3001:3000$'                             "publishes 3001:3000"
  need "$CANON" '^Network=corpos\.network$'                           "joins corpos.network (DNS to llama-server)"
  need "$CANON" '^WantedBy=default\.target$'                          "boot-starts (WantedBy=default.target)"
fi

# ── toolkit-dashboard.container (optional — present once T5 landed) ────────────
DASH="$Q/toolkit-dashboard.container"
if [ -f "$DASH" ]; then
  echo "toolkit-dashboard.container:"
  need "$DASH" '^PublishPort=8082:8080$'      "publishes 8082:8080"
  need "$DASH" '^Network=corpos\.network$'    "joins corpos.network"
  need "$DASH" '^WantedBy=default\.target$'   "boot-starts"
fi

# ── llama-server-container.container (optional — present once T6 landed) ───────
LLAMA="$Q/llama-server-container.container"
if [ -f "$LLAMA" ]; then
  echo "llama-server-container.container:"
  need "$LLAMA" '^AddDevice=nvidia\.com/gpu'  "GPU passthrough via CDI (AddDevice=nvidia.com/gpu)"
  need "$LLAMA" '^PublishPort=8081:8081$'     "publishes 8081:8081"
  need "$LLAMA" '^Network=corpos\.network$'   "joins corpos.network"
  need "$LLAMA" '^WantedBy=default\.target$'  "boot-starts"
fi

echo "[validate-quadlet] $pass checks passed, $fails failed."
[ "$fails" -eq 0 ] || { echo "[validate-quadlet] FAIL — fix deploy/quadlet/ before committing." >&2; exit 1; }
