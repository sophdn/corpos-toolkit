# Arc-close snapshot corpus

Chain `arc-close-snapshot-corpus-capture`. The training-corpus substrate for the two
arc-close ML chains — `trained-arc-close-filing-classifier-v1` (#275) and
`trained-smart-snapshot-filter-v1` (#276).

## Problem

The arc-close review pipeline emits an `ArcCloseFilingReviewed` event per fire carrying the
filing **decisions** (labels), `arc_summary`, `triggers`, and snapshot **metadata**
(`snapshot_message_count` / `_token_count` / `_truncated`) — but **not the snapshot
content**. The classifier's primary input feature is the *snapshot embedding*, and the
smart-filter operates per-turn on the snapshot content; neither has a stored source. So no
faithful corpus is accumulating (spike: `~/.claude/scratchpads/spikes/arcclose-snapshot-corpus-recovery.md`).

## Capture shape (T1 decision)

A dedicated table **`arcreview_snapshot_corpus`** (migration 074), keyed by the
`ArcCloseFilingReviewed` `event_id`, storing the exact kept-message set fed to Qwen
(`messages_json`, the `arcreview.Message` `[{role, content}]` shape) plus the caps in force,
`truncated`, `session_id`, `fire_ts`, and a `source ∈ {live, recovered}` flag.

**Why a table, not a `proj_*` projection.** The snapshot content is *primary* capture data
(the raw model input), not a derived/foldable view — a capture log, intentionally outside
`rebuild-projections`. Event-sourcing it (content on the event payload + a fold) was rejected:
it bloats the immutable ledger ~3 KB/fire for a field only the corpus reads, and the
rebuild-from-ledger property cannot hold for the **recovered** half anyway (historical events
lack the content), so a partly-event-derived projection would be a worse model than an honest
primary table.

**Durability + atomicity.** The live writer (T2) INSERTs into the corpus inside the *same*
`pool.WithWrite` tx as the `events.Emit` of the `ArcCloseFilingReviewed` event
(`handler.go::emitFilingReviewedEvent`). No fire lands without its snapshot; no orphan
snapshot lands. The cross-table guarantee is a writer guarantee locked by a write-side
regression test, not a single-table CHECK. The table's own CHECKs enforce row integrity
(non-empty `messages_json` / `fire_ts`, `message_count > 0`, `source` enum, `event_id` FK).

## `source`: live vs recovered

- **`live`** — captured at fire time by the T2 writer. Exact.
- **`recovered`** — reconstructed by the T4 cmd from the real session transcript at the fire's
  point-in-time (filter rows to `ts ≤ fire_ts`, apply the fire's original caps; an
  as-of-timestamp `ExtractSnapshot` variant). Real re-derivation, never synthetic. Sessions
  whose transcripts are gone (~4 of 33) stay an honest gap.

## ⚠️ Holdout MUST split by session, not by fire

The 265 live fires came from only **33 distinct sessions** (261 fires are in multi-fire
sessions). A fire-level train/holdout split **leaks**: same-session fires share a transcript
and overlap heavily. The ML exporters (#275 T1, #276 T1) MUST split by `session_id`.

## Consumers

- `#275` classifier: `(messages_json → snapshot embedding) + arc_summary + triggers →
  decisions`.
- `#276` smart-filter: per-turn relevance over `messages_json`, labeled by which turns
  informed a filed decision.

Both join `arcreview_snapshot_corpus.event_id` → the `ArcCloseFilingReviewed` event for
`arc_summary` / `triggers` / `decisions`.
