# The canonical event registry (`toolkit-event-registry`)

> Chain `emit-surface-forge-v2` T2. The CI-gated, append-only, fast-forward-only,
> git-backed **source of truth + disaster-recovery authority** that the forge-v2
> `record` surface mirrors to. Lives on the homelab Gitea at
> `sophdn/corpos-toolkit-event-registry`. Its local-side tooling is
> `go/cmd/event-registry` (package `go/internal/registry`).

## The local-hot / remote-authority boundary (enforced, not just documented)

Two layers of truth, deliberately split (EMIT_SURFACE_PHASE2 §3, §7):

| | Local `data/toolkit.db` (events table) | Registry (`toolkit-event-registry` on Gitea) |
|---|---|---|
| Role | **Hot draft** — fast, forgiving, the agent's working surface | **Remote authority** — published source of truth + DR |
| Mutability | freely rebuildable; disposable | **append-only-immutable + fast-forward-only** |
| Validation | thin-fast-local tier (T3) | thorough-CI validity-stamp tier (here) |
| Truth direction | a derived, rebuildable cache | the thing it is rebuildable *from* |

The boundary is **enforced**, not advisory:

- **Immutability + ff-only** is enforced at the git layer by the repo's branch
  protection: force-push and branch deletion are disabled, so a published event
  file can never be rewritten or removed. The only correction path for a
  published event is a **compensating event** (a new append), never an edit —
  the same discipline the local `events` table enforces with its
  `BEFORE UPDATE/DELETE` triggers.
- **Validity** is enforced by CI (`.gitea/workflows/validate-registry`), which
  re-runs the full enum-backed validation on every push (see below).
- **Rebuildability** (the local draft is always reconstructable from the
  registry — §7 invariant 2) is proven by `event-registry verify-dr`.

## On-disk format

One JSON file per event: `events/<event_id>.json`, where `<event_id>` is the
event's UUIDv7. Each file is the canonical 11-field event envelope
(`go/internal/events/schemas/_envelope.json`). **One-file-per-event is
load-bearing**: two machines appending different events never touch the same
file, so git merges are trivially a union of files and never conflict at the
line level — git does storage/sync/merge, CI does all semantic validation
(event-sourced-agent-os-notes.md §"Git as Substrate"). The UUIDv7 filename is
unique, filesystem-safe, and time-sortable, so a flat `events/` directory stays
in chronological order.

## CI: the validity-stamp tier

`go/cmd/event-registry validate` runs three sub-tiers (the §12 CI-rule taxonomy):

1. **schema** (structural + per-type payload) — every pushed event passes
   `events.ValidateRecordJSON`, the *single shared validator* the local `record`
   surface (T3) also calls. Strict on the **pushed delta** only; the immutable
   historical baseline is **grandfathered** (`ValidateOptions.StrictSchemaEventIDs`).
2. **causal** — every `caused_by_event_id` resolves to an event present on
   canonical (rejects an event claiming a non-canonical parent — the
   two-machine guard).
3. **projection-coherence** — the whole ledger folds cleanly into a from-empty
   projection rebuild (the expensive tier; an orphaned reference or
   contradiction surfaces here even when each event is individually valid).

### Why the baseline is grandfathered

The live ledger contains events that predate later schema tightening — e.g.
pre-`minLength` `ChainCreated`/`TaskCreated` rows with empty required fields,
and synthetic `started-`/`completed-` backfill event_ids that predate the
UUIDv7 envelope pattern. These were valid at emit time. An append-only registry
**cannot** retroactively fix them (no editing published events), and
re-validating them against today's schemas would make the registry permanently
red. So CI strict-checks only the **newly-pushed delta** and grandfathers the
baseline; the causal + projection-coherence tiers still run over the whole set,
which must remain coherent. (See bug
`per-type-event-schemas-tightened-in-place-orphan-historical-events` for the
underlying schema-evolution finding.)

## Disaster recovery (proven)

`event-registry verify-dr --db <toolkit.db> --dir <registry-clone>` reconstructs
the events ledger from a registry clone, rebuilds projections from empty, and
asserts they are **byte-identical** (per-table content hash) to a from-empty
rebuild of the source. Comparing two from-empty rebuilds isolates the registry
round-trip from any incremental-fold drift the live DB might carry.

DR runbook (workstation loss):

```sh
git clone https://example-host.local/git/sophdn/corpos-toolkit-event-registry.git
# rebuild a fresh toolkit.db from the registry:
#   (reconstructDB + RebuildAll is what verify-dr exercises; a `restore`
#    subcommand that writes the reconstructed DB to a path is the natural
#    next addition — for now verify-dr proves the path is lossless.)
event-registry verify-dr --db /path/to/any/toolkit.db --dir ./toolkit-event-registry
```

Validated end-to-end on the live 15,735-event ledger: export 0.7s, verify-dr 8.7s,
byte-identical.

## The async tail: mirror + completion-ping (T5)

The `record(events[])` call returns immediately after the local validate →
append → fold (§2) — it NEVER blocks on the network or CI. Durability + the CI
validity stamp ride a separate **async tail**, driven by `event-registry mirror`:

```
event-registry mirror --db data/toolkit.db --registry <clone> --insecure \
  [--await-ci --api-base https://example-host.local/git/api/v1 \
   --owner sophdn --repo toolkit-event-registry --token <read-token>]
```

- **Mirror** exports the local ledger into the registry checkout and, if there
  are new events, commits + pushes them. It is incremental for free: the
  export is deterministic, so only genuinely-new events show up as adds (a
  mirror with nothing new is a no-op). The push triggers the CI validity-stamp
  gate, which strict-validates exactly the pushed delta and grandfathers the
  baseline.
- **The completion ping** (`--await-ci`, or a separate `PollVerdict` call):
  after the push, CI re-validates + stamps the commit; polling the commit
  status yields the verdict (`success` = blessed / `failure` / still
  `pending`). This is fetched OUT OF BAND — a slow or red CI never stalls the
  mirror, and the agent's record() return already happened long before.

### Save boundary (where mirror runs)

Mirror is invoked on a **save boundary**, not on every `record()` — a
commit / Stop / flush hook (the same seam `commit-landed-emit` and the
Stop-hook exporters use). The verdict re-enters the agent's context through
the existing hook → `additionalContext` path (the same mechanism the
SessionStart memory materialization and the `pending_decisions` → Stop hook
use), so "the ping re-enters context" reuses proven harness plumbing rather
than a bespoke channel. The vault is outside the repo, so the boundary is the
Stop/flush trigger rather than a git commit on the toolkit repo itself.

## Provisioning (reproducible)

```sh
# 1. create the repo (admin token from ~/.git-credentials)
curl -sk -X POST -H "Authorization: token $TOK" \
  https://example-host.local/git/api/v1/user/repos \
  -d '{"name":"toolkit-event-registry","private":true,"auto_init":false,"default_branch":"main"}'

# 2. export the baseline + push
event-registry export --db data/toolkit.db --dest /tmp/reg
cp docs/event-registry/workflow-template/ci.yaml /tmp/reg/.gitea/workflows/
( cd /tmp/reg && git init && git add -A && git commit -m "baseline" \
  && git remote add origin https://example-host.local/git/sophdn/corpos-toolkit-event-registry.git \
  && git push -u origin main )

# 3. fast-forward-only branch protection (disable force-push + deletion)
curl -sk -X POST -H "Authorization: token $TOK" \
  https://example-host.local/git/api/v1/repos/sophdn/corpos-toolkit-event-registry/branch_protections \
  -d '{"branch_name":"main","enable_push":true,"enable_force_push":false,"require_signed_commits":false}'

# 4. CI secret so the registry CI can clone mcp-servers for the validator
curl -sk -X PUT -H "Authorization: token $TOK" \
  https://example-host.local/git/api/v1/repos/sophdn/corpos-toolkit-event-registry/actions/secrets/MCP_SERVERS_TOKEN \
  -d '{"data":"<token>"}'
```

The CI workflow template is version-controlled at
`docs/event-registry/workflow-template/ci.yaml`; the registry repo carries a
copy at `.gitea/workflows/`.
