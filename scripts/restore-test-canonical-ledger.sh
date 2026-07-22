#!/usr/bin/env bash
# Prove the newest ledger backup is a database, not a rumor.
#
# WHY THIS EXISTS
#   The backup half can succeed nightly for months while producing files that
#   are unusable. A copy you have never opened and queried is an assumption.
#   This runs on a cadence so the assumption is tested by a machine rather than
#   by the disaster.
#
# WHAT IT ASSERTS
#   - the newest local backup is < 48h old. A backup job that silently STOPS is
#     the failure that actually happens (a peer's ran dead for 22 days); without
#     an age assertion this test would keep passing against an ancient file and
#     report health while nothing was being backed up.
#   - it opens, and PRAGMA integrity_check returns ok
#   - it carries real content, cross-checked against the LIVE ledger: bugs and
#     chains are non-empty, and not absurdly short of live. Exact equality would
#     be flaky (the backup predates subsequent writes), so the assertion is
#     "plausibly complete", not "identical".
#   - the event log — the thing everything else is rebuilt from — is present and
#     non-empty. A projection-only copy would restore into a ledger that cannot
#     be replayed.
#
# WHAT IT DOESN'T PROVE
#   That restic can hand the file back. restic has its own restore drill
#   (AUDIT.md on the backup host); this covers the half that lives on this box.
#
# LOUD FAILURE
#   set -euo pipefail, nothing swallowed. A failure fails the unit.

set -euo pipefail

resolve_db() {
  if [ -n "${TOOLKIT_DB:-}" ]; then printf '%s' "$TOOLKIT_DB"; return; fi
  local data_home="${XDG_DATA_HOME:-$HOME/.local/share}"
  printf '%s' "$data_home/toolkit/data/toolkit.db"
}

DB="$(resolve_db)"
BACKUP_DIR="${TOOLKIT_BACKUP_DIR:-$(dirname "$DB")/../backups}"
MAX_AGE_H="${TOOLKIT_BACKUP_MAX_AGE_H:-48}"

log() { echo "$(date -Is) ledger-restore-test: $1"; }

[ -d "$BACKUP_DIR" ] || { log "FATAL: no backup dir at $BACKUP_DIR"; exit 2; }

NEWEST="$(find "$BACKUP_DIR" -maxdepth 1 -name 'toolkit-*.db' -printf '%T@ %p\n' 2>/dev/null \
  | sort -rn | head -1 | cut -d' ' -f2-)"
[ -n "$NEWEST" ] || { log "FATAL: no backup found in $BACKUP_DIR — nothing to restore-test"; exit 3; }

AGE_H=$(( ( $(date +%s) - $(stat -c %Y "$NEWEST") ) / 3600 ))
log "testing $(basename "$NEWEST") (age=${AGE_H}h)"
if [ "$AGE_H" -gt "$MAX_AGE_H" ]; then
  log "FATAL: newest backup is ${AGE_H}h old (>${MAX_AGE_H}h) — the backup job has stopped"
  exit 4
fi

# Compare the restored copy against live in one place, so the check can't drift
# between "what we back up" and "what we verify".
python3 - "$NEWEST" "$DB" <<'PY'
import re, sqlite3, sys

backup_path, live_path = sys.argv[1], sys.argv[2]
b = sqlite3.connect(f"file:{backup_path}?mode=ro", uri=True)
live = sqlite3.connect(f"file:{live_path}?mode=ro", uri=True)

# FTS5 inverted-index complaints are non-fatal — see the long note in
# backup-canonical-ledger.sh. Short version: python's SQLite 3.45.1 calls this
# ledger's FTS5 index malformed while 3.50.6 and the live modernc engine call
# it fine, and 3.45.1 still MATCH-queries it successfully. And an FTS5 index is
# derived data, rebuildable from the content tables, so it is not data loss.
# Anything else integrity_check reports IS fatal.
rows = [r[0] for r in b.execute("pragma integrity_check").fetchall()]
real = [r for r in rows if r != "ok" and not re.search(r"inverted index for FTS5 table", r)]
noted = [r for r in rows if r != "ok" and r not in real]
for r in noted:
    print(f"verify note (non-fatal, derived index): {r}")
if real:
    for r in real:
        print(f"VERIFY FAIL: integrity_check: {r}")
    sys.exit(5)
print("verify ok: integrity_check clean (ignoring derived FTS5 index)")

# The FTS index may be derived, but prove it actually answers — a rebuild is
# cheap, discovering at restore time that search is dead is not.
try:
    b.execute("select count(*) from knowledge_pointers_fts where knowledge_pointers_fts match 'backup'").fetchone()
    print("verify ok: FTS5 MATCH query answers on the restored copy")
except sqlite3.Error as e:
    print(f"verify note (non-fatal, rebuildable): FTS5 MATCH failed: {e}")

def count(conn, table):
    try:
        return conn.execute(f"select count(*) from {table}").fetchone()[0]
    except sqlite3.Error:
        return None

failed = False
# events is load-bearing: it is what the byte-identity replay rebuilds from.
for table in ("events", "proj_current_bugs", "proj_chain_status"):
    got = count(b, table)
    want = count(live, table)
    if got is None:
        print(f"VERIFY FAIL: table {table} missing from the backup")
        failed = True
        continue
    if got == 0 and (want or 0) > 0:
        print(f"VERIFY FAIL: {table} restored 0 rows but live has {want}")
        failed = True
        continue
    # The backup is a point in time before now, so it may trail live — but not
    # by much. A copy holding a small fraction of live means it was taken from
    # a truncated or half-migrated database.
    if want and got < want * 0.5:
        print(f"VERIFY FAIL: {table} restored {got} vs live {want} — implausibly short")
        failed = True
        continue
    print(f"verify ok: {table} backup={got} live={want}")

sys.exit(6 if failed else 0)
PY

log "restore test PASSED for $(basename "$NEWEST")"
