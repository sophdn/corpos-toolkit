#!/usr/bin/env bash
# scripts/test-install-into-claude.sh — regression harness for
# scripts/install-into-claude.sh (chain 444 T6).
#
# The failure this pins: the installer was dead for weeks (it read a
# _manifest.toml that never existed in this repo) and nobody noticed, because
# nothing exercised it. Every case below runs against a TEMP target — this
# harness never writes to ~/.claude.
#
# Cases:
#   1. fresh install populates a target and reports 0 conflicts
#   2. re-running is a no-op (idempotent)
#   3. a git-tracked skill in the target is REFUSED, not clobbered, exit 1
#   4. --dry-run writes nothing
#   5. no corpos checkout → toolkit-core only, still exit 0
#   6. pii-allow markers never reach an installed skill
#   7. the overlay-canonical half (operator stack skills) is never installed
#
# Usage: bash scripts/test-install-into-claude.sh
set -uo pipefail

# The harness runs under the pre-commit gate, where git exports GIT_DIR /
# GIT_INDEX_FILE. Those would redirect the harness's OWN git calls (the case-3
# fixture repo) at the hook's repo. Scrub them so the harness is hermetic no
# matter what invokes it; case 8 re-introduces them deliberately, per-command.
unset GIT_DIR GIT_INDEX_FILE GIT_WORK_TREE GIT_COMMON_DIR GIT_PREFIX \
      GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALL="$REPO_ROOT/scripts/install-into-claude.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

FAILED=0
pass() { echo "  ok   — $1"; }
fail() { echo "  FAIL — $1" >&2; FAILED=$((FAILED + 1)); }

echo "[test-install] harness root: $TMP"

# ── case 1: fresh install ─────────────────────────────────────────────────────
out="$(bash "$INSTALL" --target="$TMP/fresh" 2>&1)"; rc=$?
[[ $rc -eq 0 ]] && pass "fresh install exits 0" || fail "fresh install exit $rc"
grep -q "0 conflicts" <<<"$out" && pass "fresh install reports 0 conflicts" \
    || fail "fresh install reported conflicts"
n_installed=$(ls "$TMP/fresh/skills" 2>/dev/null | wc -l)
[[ $n_installed -gt 0 ]] && pass "fresh install populated $n_installed skills" \
    || fail "fresh install populated nothing"

# ── case 2: idempotency ───────────────────────────────────────────────────────
out="$(bash "$INSTALL" --target="$TMP/fresh" 2>&1)"
grep -qE "summary: 0 written" <<<"$out" && pass "re-run writes nothing" \
    || fail "re-run was not idempotent: $(grep summary: <<<"$out")"

# ── case 3: refuse to clobber a git-tracked skill in the target ───────────────
# This is the load-bearing guard: ~/.claude is CANONICAL for 19 skills, and an
# installer that overwrote them would silently destroy the only copy.
victim="$(ls "$TMP/fresh/skills" | head -1)"
mkdir -p "$TMP/tracked/skills/$victim"
echo "TARGET IS CANONICAL" > "$TMP/tracked/skills/$victim/SKILL.md"
git -C "$TMP/tracked" init -q .
git -C "$TMP/tracked" -c user.email=t@t -c user.name=t add -A
git -C "$TMP/tracked" -c user.email=t@t -c user.name=t commit -qm seed
out="$(bash "$INSTALL" --target="$TMP/tracked" 2>&1)"; rc=$?
[[ $rc -eq 1 ]] && pass "tracked-skill conflict exits 1" || fail "tracked conflict exit $rc (want 1)"
grep -q "CONFLICT — git-tracked" <<<"$out" && pass "tracked-skill conflict is reported" \
    || fail "tracked-skill conflict not reported"
[[ "$(cat "$TMP/tracked/skills/$victim/SKILL.md")" == "TARGET IS CANONICAL" ]] \
    && pass "tracked skill was NOT clobbered" || fail "tracked skill was overwritten"

# ── case 4: --dry-run writes nothing ──────────────────────────────────────────
bash "$INSTALL" --dry-run --target="$TMP/dry" >/dev/null 2>&1
[[ ! -d "$TMP/dry" ]] && pass "--dry-run created no target" || fail "--dry-run wrote to disk"

# ── case 5: absent corpos checkout ────────────────────────────────────────────
out="$(bash "$INSTALL" --corpos="$TMP/nope" --target="$TMP/nocorpos" 2>&1)"; rc=$?
[[ $rc -eq 0 ]] && pass "absent corpos still exits 0" || fail "absent corpos exit $rc"
grep -q "no corpos checkout" <<<"$out" && pass "absent corpos warns loudly" \
    || fail "absent corpos warned silently"
n_core=$(ls "$TMP/nocorpos/skills" | wc -l)
n_canon=$(find "$REPO_ROOT/skills" -mindepth 2 -maxdepth 2 -name SKILL.md | wc -l)
[[ "$n_core" -eq "$n_canon" ]] && pass "absent corpos installs this repo's $n_canon canonical skills" \
    || fail "absent corpos installed $n_core, want $n_canon"

# ── case 6: pii-allow markers are stripped ────────────────────────────────────
if grep -rqI 'pii-allow' "$TMP/fresh/skills" 2>/dev/null; then
    fail "pii-allow markers leaked into an installed skill"
else
    pass "no pii-allow markers in installed skills"
fi
# ...and prove the strip is actually exercised, not vacuously true.
if grep -rqI 'pii-allow' "$REPO_ROOT/skills" 2>/dev/null; then
    pass "strip is non-vacuous (canonical source carries markers)"
else
    echo "  note — canonical source carries no pii-allow markers; case 6 is vacuous"
fi

# ── case 7: the overlay-canonical half is never installed ─────────────────────
# corpos gitignores internal/skills/userlib/*/, so those skills have no
# committed upstream and ~/.claude is their canonical home. The installer must
# never source from userlib.
leaked=0
for s in go-conventions rust-conventions python-conventions worktree-workflow \
         godot-conventions expo-conventions layout-conventions corpos-swap-rehearsal; do
    [[ -e "$TMP/fresh/skills/$s" ]] && { echo "    leaked: $s" >&2; leaked=$((leaked + 1)); }
done
[[ $leaked -eq 0 ]] && pass "no overlay-canonical skill was installed" \
    || fail "$leaked overlay-canonical skill(s) installed — would clobber their only copy"

# ── case 8: inherited git env must not redirect target queries ────────────────
# Under a git hook, git exports GIT_DIR/GIT_INDEX_FILE, and those override
# `git -C <path>`. Unscrubbed, every question about the install target gets
# answered about the HOOK's repo instead — which made the installer refuse to
# install skills that are tracked in corpos-toolkit but absent from the target.
# Caught by the pre-commit gate on this task's own commit; pinned here so it
# can't come back.
out="$(GIT_DIR="$REPO_ROOT/.git" GIT_INDEX_FILE="$REPO_ROOT/.git/index" \
       bash "$INSTALL" --target="$TMP/gitenv" 2>&1)"; rc=$?
n_env=$(ls "$TMP/gitenv/skills" 2>/dev/null | wc -l)
[[ $rc -eq 0 && "$n_env" -eq "$n_installed" ]] \
    && pass "inherited GIT_DIR/GIT_INDEX_FILE does not redirect target queries" \
    || fail "git env leaked: exit $rc, installed $n_env (want $n_installed)"

echo ""
if [[ $FAILED -gt 0 ]]; then
    echo "[test-install] FAIL — $FAILED assertion(s)" >&2
    exit 1
fi
echo "[test-install] PASS"
