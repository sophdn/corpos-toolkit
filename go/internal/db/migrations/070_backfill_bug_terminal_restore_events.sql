-- Bug `rebuild-projections-regresses-terminal-bugs-from-incomplete-events-ledger`
-- (886) — backfill the missing terminal BugResolved events so a
-- replay-from-empty reconstructs the same terminal bug state the live
-- projection holds.
--
-- ROOT CAUSE (diagnosed 2026-05-23). 41 bugs regressed open on
-- `rebuild-projections` (pre=3 post=44, Δ=+41). They are NOT missing a
-- BugResolved event — each has the history
-- {BugReported, BugResolved, BugResolved, BugReopened}, i.e. their LAST
-- ledger event is BugReopened, so a fold-from-empty correctly leaves
-- them `open`. But the live projection holds them as terminal
-- (32 fixed / 5 routed / 3 dup / 1 wontfix).
--
-- The divergence traces to the 2026-05-22 recovery sequence: these bugs
-- were genuinely resolved (≤2026-05-20), restored via a synthetic
-- BugResolved by `migration-recovery-2026-05-22`, then swept to wontfix
-- and immediately REVERTED by the "bug 164 corpus" pass (the BugReopened
-- events, rationale "Reverting per bug 164 corpus"). The reopen reverted
-- the erroneous wontfix but overshot to `open`; the correct prior
-- terminal state was then restored DIRECTLY in the projection (the
-- recovery path) WITHOUT emitting the corresponding BugResolved. So the
-- ledger ends at BugReopened while the projection is terminal.
--
-- This is a one-time historical-recovery residue, NOT a live writer bug:
-- the current bug_resolve path emits BugResolved correctly (proven by
-- the 4 sibling bugs that were reopened-then-properly-re-resolved and
-- replay clean to fixed). The live projection is ground truth — these
-- bugs carry real recorded resolution_kind + resolved_commit_sha +
-- routing from genuine resolutions. Same synthetic-event-backfill
-- pattern as migration 061; closes the regression class migration 066's
-- header documented (707 bugs) but never backfilled.
--
-- The synthetic BugResolved is stamped at last_event_ts + 1 second so it
-- folds strictly AFTER the BugReopened and the replayed terminal state
-- wins. Payload fields are sourced verbatim from proj_current_bugs
-- (resolution_note was retired from the projection in migration 065, so
-- the note carries a recovery marker rather than the original prose).
--
-- Data-driven + idempotent: the WHERE clause matches nothing on a fresh
-- DB (the testutil mirror) or any DB without reopened-then-restored
-- bugs, and migrations run once via _migrations.
-- ───────────────────────────────────────────────────────────────────
INSERT INTO events (
    event_id, ts, actor_kind, actor_id, type,
    entity_kind, entity_slug, entity_project_id,
    payload, rationale, caused_by_event_id, related_entities,
    span_id, schema_version
)
SELECT
    lower(printf(
        '%08x-%04x-7%03x-%s%03x-%s',
        (unixepoch(b.last_event_ts, '+1 second') * 1000) / 65536,
        (unixepoch(b.last_event_ts, '+1 second') * 1000) % 65536,
        (abs(random()) % 4096),
        substr('89ab', 1 + (abs(random()) % 4), 1),
        abs(random()) % 4096,
        lower(hex(randomblob(6)))
    )) AS event_id,
    strftime('%Y-%m-%dT%H:%M:%fZ', b.last_event_ts, '+1 second') AS ts,
    'system' AS actor_kind,
    'migration-070-bug886-terminal-restore' AS actor_id,
    'BugResolved' AS type,
    'bug' AS entity_kind,
    b.slug AS entity_slug,
    b.project_id AS entity_project_id,
    json_object(
        'kind', b.resolution_kind,
        'commit_sha', nullif(b.resolved_commit_sha, ''),
        'routed_chain_slug', nullif(b.routed_chain_slug, ''),
        'routed_task_slug', nullif(b.routed_task_slug, ''),
        'routed_suggestion_slug', nullif(b.routed_suggestion_slug, ''),
        'resolution_note', 'Restored by migration 070 (bug 886): terminal state re-derived from proj_current_bugs after the 2026-05-22 reopen left the ledger ending at BugReopened. See migration header.'
    ) AS payload,
    'migration 070: backfill missing terminal BugResolved so rebuild-projections reconstructs terminal state (bug 886)' AS rationale,
    NULL AS caused_by_event_id,
    '[]' AS related_entities,
    lower(printf(
        '%s-%s-4%s-%s%s-%s',
        lower(hex(randomblob(4))),
        lower(hex(randomblob(2))),
        substr(lower(hex(randomblob(2))), 2),
        substr('89ab', 1 + (abs(random()) % 4), 1),
        substr(lower(hex(randomblob(2))), 2),
        lower(hex(randomblob(6)))
    )) AS span_id,
    1 AS schema_version
FROM proj_current_bugs b
WHERE b.status != 'open'
  AND (
    SELECT e.type FROM events e
    WHERE e.entity_kind = 'bug'
      AND e.entity_slug = b.slug
      AND e.entity_project_id = b.project_id
    ORDER BY e.ts DESC, e.event_id DESC
    LIMIT 1
  ) = 'BugReopened';
