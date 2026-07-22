# deploy/systemd — canonical-ledger backup units

User units for the box that holds the canonical ledger. Unlike `deploy/quadlet/`
(which defines the toolkit-server *container*), these run on the host as the
owning user: the ship step uses that user's ssh key, and the ledger is that
user's file.

## Why this exists

Until 2026-07-15 the canonical ledger had **no backup**. Verified on the dev
box: no restic, borg, timeshift or duplicity installed, no backup timer, no
cron. The only copies were `~/db-snapshots/toolkit.db.pre-t7-flip-*` — hand-taken
one-offs from 2026-06-02, made as a pre-migration safety copy and never
re-taken.

That one file holds every chain, task, bug and suggestion, the roadmap, and the
append-only event history. None of it is re-derivable.

**Event sourcing is not a backup.** The replay gate proves the projections can
be rebuilt *from the event log*. It does not give you a second copy of the event
log. Once the file is gone there is nothing to replay.

## What runs

| Unit | Cadence | Does |
|---|---|---|
| `corpos-ledger-backup.timer` | nightly 02:30 | online-backup the ledger → local timestamped copy → rsync newest to the backup host |
| `corpos-ledger-restore-test.timer` | Sun 04:00 | open the newest copy, verify it, cross-check counts against live |

02:30 is chosen to land **before the backup host's own nightly backup run**, so
the copy that run captures is tonight's rather than last night's. **If either
time moves, keep the order.**

`Persistent=true` on both — this box is a workstation and is often off at 02:30.
Missed windows run at next boot rather than silently not happening.

## Where the data goes

```
sqlite online-backup API   (WAL-correct; a live writer is fine, no downtime)
  -> ~/.local/share/toolkit/backups/toolkit-<UTC>.db   (14d, HARD FLOOR of 3)
  -> rsync newest -> <backup-host>:<path its restic already covers>
  -> that host's restic -> separate physical NVMe
  -> 7 daily / 4 weekly / 6 monthly, with a performed restore drill
```

The design goal is to **inherit a proven regime rather than invent a second
one**. This script knows nothing about restic; it just puts a consistent file
where an existing, restore-drilled backup will find it.

Local copies are a fast-rollback cache (a bad migration, a fat-finger). The
shipped copy is the one that survives this disk dying — the scenario that
matters, since this box has no backup regime of its own.

Remote is a single fixed filename, overwritten each run: the target's restic
supplies the history and dedupes, so timestamped remote copies would only bloat
the repo.

**Threat model.** Survives this disk dying, a bad migration, a fat-finger
delete. Does **not** survive a house fire — that's inherited from the target's
threat model, and off-box replication is a deferred follow-up there too.

## Configuration

Required. The real file lives in `~/.config/corpos-toolkit/ledger-backup.env`,
**not** in this repo — a backup host is deployment config, not source. Keeping
it out means this repo carries no infra identifiers (the commit-time `pii-scan`
enforces exactly that, and rejected an earlier draft of these files) and the
public mirror has nothing to scrub.

```bash
mkdir -p ~/.config/corpos-toolkit
cp deploy/systemd/ledger-backup.env.example ~/.config/corpos-toolkit/ledger-backup.env
$EDITOR ~/.config/corpos-toolkit/ledger-backup.env   # set TOOLKIT_BACKUP_SHIP_TO
```

`TOOLKIT_BACKUP_SHIP_TO` has **no default and unset is a hard failure**, in both
the script and the unit (`EnvironmentFile=` with no leading `-`). Local-only
copies share the disk whose death is the whole scenario, so quietly falling back
to them would be a comforting lie. `TOOLKIT_BACKUP_SHIP_TO=local` says
local-only out loud. Every other var is optional — see the `.example`.

## Two traps this encodes

**Never `cp` the ledger.** It is WAL mode and routinely has a multi-MB `-wal`
beside it (20 MB when this was written). A copy of `toolkit.db` alone misses
committed-but-not-yet-checkpointed pages. It will often *look* fine — the file
is only torn if you copy during an uncheckpointed window — which is exactly what
makes it dangerous: it works until the one night it doesn't. The online-backup
API removes the race.

**FTS5 integrity complaints are not corruption here, and must not veto a
backup.** python's bundled SQLite (3.45.1) reports `malformed inverted index for
FTS5 table knowledge_pointers_fts` on this ledger. sqlite 3.50.6 reports `ok`,
the live modernc/`sqlite_fts5` engine serves the FTS surface fine, and 3.45.1
*itself* MATCH-queries the table successfully — it contradicts its own checker.
It is a cross-version checker disagreement. And even if it were real, an FTS5
index is derived data, rebuildable with
`INSERT INTO <fts>(<fts>) VALUES('rebuild')` — not data loss. The scripts log it
and continue; any **non**-FTS5 integrity complaint is still fatal. A backup
script that refuses to back up because of an index-checker quirk produces no
backups, which is the failure it exists to end.

## Install

```bash
mkdir -p ~/.config/systemd/user
cp deploy/systemd/corpos-ledger-*.{service,timer} ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now corpos-ledger-backup.timer
systemctl --user enable --now corpos-ledger-restore-test.timer
loginctl enable-linger "$USER"   # required so timers run when logged out
```

Prove it without waiting for the timers:

```bash
scripts/backup-canonical-ledger.sh        # expect "backup ok … integrity=ok" then "ship ok"
scripts/restore-test-canonical-ledger.sh  # expect "restore test PASSED"
systemctl --user list-timers | grep corpos-ledger
```

## Failure signalling — the known gap

A failure is a failed user unit plus stderr in the journal. That is the
"logfile nobody reads" shape, tracked as bug
`backup-and-disk-events-do-not-reach-a-human-silent-failure`. Nothing here is
wrapped in `|| true`, so every failure path can warn once there is somewhere to
warn to.

## If the toolkit migrates to the backup host

This is deliberately useful either way. Post-migration the ledger would sit on
that host inside its restic scope directly, and its `restic/scripts/backup.sh`
already quiesces a `toolkit.db` via `sqlite3 .backup` — that entry would simply
point at the real ledger, and these units retire. Until then, this is the only
thing standing between the ledger and a single disk. It is also the copy you
want *taken* before any such migration starts.
