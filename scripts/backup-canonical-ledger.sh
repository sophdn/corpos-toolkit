#!/usr/bin/env bash
# Back up the canonical toolkit ledger, and ship it somewhere that isn't this
# machine.
#
# WHY THIS EXISTS
#   Until 2026-07-15 the canonical ledger had NO backup. Verified: no restic,
#   borg, timeshift or duplicity installed on this box, no backup timer, no
#   cron. The only copies were ~/db-snapshots/toolkit.db.pre-t7-flip-* — hand
#   -taken one-offs from 2026-06-02, made as a pre-migration safety copy and
#   never re-taken. That file holds every chain, task, bug, suggestion, the
#   roadmap, and the append-only event history — none of it re-derivable.
#
#   Event-sourcing does not help here. The replay gate proves the projections
#   can be rebuilt FROM THE EVENT LOG; it does not give you a second copy of
#   the event log. Once the file is gone, there is nothing to replay.
#
# WHY NOT `cp`
#   The ledger is WAL mode, and there is routinely a multi-MB -wal beside it
#   (20 MB at the time of writing). A plain copy of toolkit.db captures the
#   main file WITHOUT the committed-but-not-yet-checkpointed pages in the WAL,
#   so it restores as a torn database or silently loses recent work. This uses
#   SQLite's online-backup API, which is WAL-correct and safe against a live
#   writer — the same guarantee restic/scripts/backup.sh gets from
#   `sqlite3 .backup` on the backup host.
#
# WHY python3 AND NOT sqlite3
#   There is no system sqlite3 on this box; the only one on PATH belongs to the
#   Android SDK, which is not a thing a backup should depend on continuing to
#   exist. python3's stdlib sqlite3 ships Connection.backup() (the same online
#   -backup C API) and is already a hard dependency of the hooks here. No new
#   package, nothing to install.
#
# WHERE IT GOES
#   Local timestamped copies (fast rollback, e.g. after a bad migration), plus
#   an rsync of the newest to a path on the backup host that its restic run
#   ALREADY covers — so this script never learns about restic. It just puts a
#   consistent file where a proven regime will find it: a repo on a separate
#   physical NVMe, 7d/4w/6m retention, with a performed restore drill. That is
#   the whole point — this box has no backup regime, and that one is proven.
#
#   Scheduled to run before the backup host's nightly restic run, so the copy
#   it captures is tonight's rather than last night's. See
#   deploy/systemd/README.md; keep the two times in that order if either moves.
#
# LOUD FAILURE
#   set -euo pipefail, nothing wrapped in `|| true`. A failure fails the unit.
#   A backup script that swallows its own errors is how you discover, months
#   later, that you have no backups.

set -euo pipefail

# Resolve the ledger the way go/launch.sh does, rather than hardcoding a path
# that outlives the tree it named (see bug
# chain-replay-verify-gate-stage-has-silently-skipped-since-the-repo-rename).
resolve_db() {
  if [ -n "${TOOLKIT_DB:-}" ]; then printf '%s' "$TOOLKIT_DB"; return; fi
  local data_home="${XDG_DATA_HOME:-$HOME/.local/share}"
  printf '%s' "$data_home/toolkit/data/toolkit.db"
}

DB="$(resolve_db)"
BACKUP_DIR="${TOOLKIT_BACKUP_DIR:-$(dirname "$DB")/../backups}"
KEEP_DAYS="${TOOLKIT_BACKUP_KEEP_DAYS:-14}"
# HARD FLOOR — pruning can never take the directory below this many copies,
# whatever their age. A cleanup that can delete to zero eventually will.
KEEP_MIN="${TOOLKIT_BACKUP_KEEP_MIN:-3}"

# Off-box target: <user>@<host>:<path>. Deliberately has NO default — a backup
# host is deployment config, not source, and hardcoding one would put real infra
# identifiers in a repo that publishes a public mirror (the commit-time pii-scan
# rejects them, correctly). Set it via the unit's EnvironmentFile; see
# deploy/systemd/README.md and deploy/systemd/ledger-backup.env.example.
#
# Unset is a HARD FAILURE rather than a quiet local-only fallback: local copies
# share the disk whose death is the entire scenario, so "backed up" without a
# ship is a comforting lie. Set TOOLKIT_BACKUP_SHIP_TO=local to say local-only
# out loud (used by the tests).
SHIP_TO="${TOOLKIT_BACKUP_SHIP_TO:-}"
if [ -z "$SHIP_TO" ]; then
  echo "$(date -Is) ledger-backup: FATAL: TOOLKIT_BACKUP_SHIP_TO is unset." >&2
  echo "  The ledger would be copied only to this disk — the one whose failure this exists to survive." >&2
  echo "  Set it in ~/.config/corpos-toolkit/ledger-backup.env (see deploy/systemd/ledger-backup.env.example)," >&2
  echo "  or pass TOOLKIT_BACKUP_SHIP_TO=local to accept local-only copies deliberately." >&2
  exit 6
fi

log() { echo "$(date -Is) ledger-backup: $1"; }

[ -f "$DB" ] || { log "FATAL: ledger not found at $DB"; exit 2; }

mkdir -p "$BACKUP_DIR"
BACKUP_DIR="$(cd "$BACKUP_DIR" && pwd)"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
TARGET="$BACKUP_DIR/toolkit-${STAMP}.db"

# Write to .partial and rename only after the integrity check passes, so an
# interrupted or corrupt run can never leave a file that looks like a good
# backup — and so the restore test never picks a half-written one.
log "backup start db=$DB -> $TARGET"
python3 - "$DB" "$TARGET.partial" <<'PY'
import sqlite3, sys
src, dst = sys.argv[1], sys.argv[2]
# mode=ro: never write the canonical file. The online-backup API copes with a
# concurrent writer by design; it does not need the container stopped.
s = sqlite3.connect(f"file:{src}?mode=ro", uri=True)
d = sqlite3.connect(dst)
with d:
    s.backup(d)
d.close()
s.close()
PY

# A backup you have not opened is a rumor even before you try to restore it.
#
# FTS5 complaints are NOT fatal here, deliberately. python's bundled SQLite
# (3.45.1) reports "malformed inverted index for FTS5 table
# knowledge_pointers_fts" on this ledger, while sqlite 3.50.6 reports ok, the
# live modernc/sqlite_fts5 engine serves the FTS surface fine, and 3.45.1
# itself MATCH-queries the table successfully — it contradicts its own checker.
# So it is a cross-version checker disagreement, not corruption.
#
# Even if it were real: an FTS5 index is DERIVED data, rebuildable from the
# content tables with INSERT INTO <fts>(<fts>) VALUES('rebuild'). It is not
# data loss, and it must not be allowed to veto a backup — a backup script that
# refuses to back up because of an index-checker quirk produces no backups,
# which is precisely the failure this script exists to end. Any NON-FTS5
# complaint is still fatal.
BAD="$(python3 - "$TARGET.partial" <<'PY'
import re, sqlite3, sys
c = sqlite3.connect(f"file:{sys.argv[1]}?mode=ro", uri=True)
rows = [r[0] for r in c.execute("pragma integrity_check").fetchall()]
if rows == ["ok"]:
    sys.exit(0)
# Keep only complaints that are not the known FTS5 inverted-index disagreement.
real = [r for r in rows if not re.search(r"inverted index for FTS5 table", r)]
for r in rows:
    if r not in real:
        print(f"NOTE(non-fatal, derived index): {r}", file=sys.stderr)
print("\n".join(real))
PY
)"
[ -z "$BAD" ] || { log "FATAL: integrity_check found real corruption: $BAD"; rm -f "$TARGET.partial"; exit 3; }

# Content check: an empty-but-valid database passes integrity_check. Assert the
# copy actually carries the ledger — and specifically that `events` is present,
# since that is what everything else is rebuilt from.
read -r BUGS EVENTS <<<"$(python3 - "$TARGET.partial" <<'PY'
import sqlite3, sys
c = sqlite3.connect(f"file:{sys.argv[1]}?mode=ro", uri=True)
def n(t):
    try:
        return c.execute(f"select count(*) from {t}").fetchone()[0]
    except sqlite3.Error:
        return -1
print(n("proj_current_bugs"), n("events"))
PY
)"
[ "$BUGS" -gt 0 ]   || { log "FATAL: backup has $BUGS bugs — not a usable ledger"; rm -f "$TARGET.partial"; exit 4; }
[ "$EVENTS" -gt 0 ] || { log "FATAL: backup has $EVENTS events — the event log is what the replay rebuilds from"; rm -f "$TARGET.partial"; exit 4; }

mv "$TARGET.partial" "$TARGET"
SIZE="$(stat -c %s "$TARGET")"
log "backup ok bytes=$SIZE bugs=$BUGS events=$EVENTS integrity=ok"

# ─── Ship off-box ─────────────────────────────────────────────────────────────
# This is the step that actually protects the data: everything above still
# lives on the one disk whose death is the scenario.
if [ "$SHIP_TO" != "local" ]; then
  REMOTE_HOST="${SHIP_TO%%:*}"
  REMOTE_PATH="${SHIP_TO#*:}"
  log "shipping to $SHIP_TO"
  ssh -o BatchMode=yes -o ConnectTimeout=15 "$REMOTE_HOST" "mkdir -p '$(dirname "$REMOTE_PATH")'"
  # Single fixed filename, overwritten each run: restic supplies the history
  # (7d/4w/6m) and dedupes, so timestamped remote copies would just bloat the
  # repo. Same contract as the .snapshot siblings backup.sh already ships.
  rsync -a --timeout=120 "$TARGET" "$SHIP_TO"
  REMOTE_SIZE="$(ssh -o BatchMode=yes -o ConnectTimeout=15 "$REMOTE_HOST" "stat -c %s '$REMOTE_PATH'")"
  [ "$REMOTE_SIZE" = "$SIZE" ] || { log "FATAL: shipped size $REMOTE_SIZE != local $SIZE"; exit 5; }
  log "ship ok bytes=$REMOTE_SIZE -> the backup host's restic run will capture it"
else
  log "ship SKIPPED by explicit TOOLKIT_BACKUP_SHIP_TO=local — copies are on this disk ONLY"
fi

# ─── Prune, floor first ───────────────────────────────────────────────────────
mapfile -t COPIES < <(find "$BACKUP_DIR" -maxdepth 1 -name 'toolkit-*.db' -printf '%T@ %p\n' \
  | sort -rn | cut -d' ' -f2-)
TOTAL="${#COPIES[@]}"
PRUNED=0
if [ "$TOTAL" -gt "$KEEP_MIN" ]; then
  for f in "${COPIES[@]:$KEEP_MIN}"; do
    if [ -n "$(find "$f" -mtime +"$KEEP_DAYS" -print -quit)" ]; then
      rm -f "$f"
      PRUNED=$((PRUNED + 1))
    fi
  done
fi
log "prune ok kept=$((TOTAL - PRUNED)) pruned=$PRUNED floor=$KEEP_MIN keep_days=$KEEP_DAYS"
