#!/usr/bin/env bash
# scripts/dedupe-grounding-events-backfill.sh — one-shot cleanup of
# duplicate grounding_events rows produced by the dual-writer steady
# state described in bug
# `grounding-events-online-emit-and-stop-hook-processor-create-duplicate-rows-per-search`.
#
# Background: until the fix landed (commit X), the online emit at
# handler-exit and the post-session processor each wrote a separate
# grounding_events row per search call — same action + source_refs +
# wall-clock instant, different (session_id, call_id) keys, so the
# ON CONFLICT did not collide. Steady state: ~2 rows per real search.
#
# The runtime fix (InsertGroundingEventTxBackstop) collapses NEW
# searches at write time. This script collapses the EXISTING rows.
#
# Algorithm:
#   For each (action, source_refs) group within a 60-second created_at
#   window, keep the row that carries the online emit's per-handler
#   telemetry (pass1_latency_ms / pass2_latency_ms / kiwix_hits_in
#   etc.) when present, otherwise the oldest row in the cluster.
#   Promote the processor's post-hoc fields (prompt_id, parent_span_id,
#   next_turn_has_output, used) onto the keeper row before deleting
#   the duplicate. Re-link any per-row foreign-key references
#   (query_interactions.grounding_event_id) onto the keeper.
#
# Usage:
#   bash scripts/dedupe-grounding-events-backfill.sh             # dry-run
#   bash scripts/dedupe-grounding-events-backfill.sh --execute   # apply
#
# Safe on a fresh DB: the SELECT predicates find no dup rows, the
# script reports "0 clusters" and exits.

set -euo pipefail

DB_PATH="${TOOLKIT_DB_PATH:-${HOME}/.local/share/toolkit/data/toolkit.db}"
MODE="dryrun"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --execute|--apply) MODE="execute"; shift ;;
    --db) DB_PATH="$2"; shift 2 ;;
    -h|--help)
      sed -n '1,30p' "$0"
      exit 0
      ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

if [[ ! -f "$DB_PATH" ]]; then
  echo "error: DB not found at $DB_PATH" >&2
  exit 2
fi

echo "[dedupe] DB: $DB_PATH"
echo "[dedupe] mode: $MODE"

# Detect clusters: rows sharing (action, source_refs) within 60s of
# each other. The CTE numbers rows per cluster by created_at; the
# keeper is the row with non-NULL pass1_latency_ms (online emit's
# signature for vault_search) when one exists, otherwise the earliest.
# For kiwix_search the online-emit signature is kiwix_hits_in NOT
# NULL; for knowledge_search there is no telemetry distinguisher, so
# the earliest stands in.
CLUSTER_COUNT=$(sqlite3 "$DB_PATH" <<'SQL'
WITH numbered AS (
  SELECT
    id,
    action,
    source_refs,
    created_at,
    pass1_latency_ms,
    kiwix_hits_in,
    LAG(created_at) OVER (PARTITION BY action, source_refs ORDER BY created_at) AS prev_ts
  FROM grounding_events
),
clusters AS (
  SELECT id, action, source_refs, created_at, pass1_latency_ms, kiwix_hits_in
  FROM numbered
  WHERE prev_ts IS NOT NULL
    AND (julianday(created_at) - julianday(prev_ts)) * 86400.0 <= 60
)
SELECT COUNT(*) FROM clusters;
SQL
)
echo "[dedupe] duplicate rows detected (excluding cluster heads): $CLUSTER_COUNT"

if [[ "$MODE" != "execute" ]]; then
  echo "[dedupe] dry-run only. Re-run with --execute to apply."
  exit 0
fi

# Execute the cleanup in a single transaction so a partial failure
# leaves the table consistent.
sqlite3 "$DB_PATH" <<'SQL'
BEGIN;

-- Stage 1: per (action, source_refs) cluster within 60s, pick the
-- keeper. Prefer rows with online-emit telemetry signatures
-- (pass1_latency_ms NOT NULL OR kiwix_hits_in NOT NULL); otherwise
-- pick the earliest. The picker uses a stable id ordering tiebreak
-- so re-runs are idempotent.
CREATE TEMP TABLE dedupe_groups AS
SELECT
  action,
  source_refs,
  CAST(julianday(created_at) * 1440 AS INT) AS bucket_min, -- ~1min bucketing
  MIN(
    CASE
      WHEN pass1_latency_ms IS NOT NULL OR kiwix_hits_in IS NOT NULL THEN id
      ELSE NULL
    END
  ) AS preferred_id,
  MIN(id) AS earliest_id
FROM grounding_events
GROUP BY action, source_refs, bucket_min
HAVING COUNT(*) > 1;

CREATE TEMP TABLE dedupe_targets AS
SELECT
  ge.id AS dup_id,
  COALESCE(dg.preferred_id, dg.earliest_id) AS keeper_id
FROM grounding_events ge
JOIN dedupe_groups dg
  ON ge.action = dg.action
  AND ge.source_refs = dg.source_refs
  AND CAST(julianday(ge.created_at) * 1440 AS INT) = dg.bucket_min
WHERE ge.id != COALESCE(dg.preferred_id, dg.earliest_id);

-- Stage 2: promote the processor's post-hoc fields onto the keeper
-- before deleting the dup. COALESCE preserves any keeper-side data
-- (online emit may have set query_text / user_message_id).
UPDATE grounding_events
SET
  next_turn_has_output = COALESCE(
    (SELECT MAX(g2.next_turn_has_output) FROM grounding_events g2
     JOIN dedupe_targets dt ON dt.dup_id = g2.id
     WHERE dt.keeper_id = grounding_events.id),
    grounding_events.next_turn_has_output
  ),
  used = COALESCE(
    grounding_events.used,
    (SELECT MAX(g2.used) FROM grounding_events g2
     JOIN dedupe_targets dt ON dt.dup_id = g2.id
     WHERE dt.keeper_id = grounding_events.id)
  ),
  prompt_id = COALESCE(
    grounding_events.prompt_id,
    (SELECT MAX(g2.prompt_id) FROM grounding_events g2
     JOIN dedupe_targets dt ON dt.dup_id = g2.id
     WHERE dt.keeper_id = grounding_events.id)
  ),
  parent_span_id = COALESCE(
    grounding_events.parent_span_id,
    (SELECT MAX(g2.parent_span_id) FROM grounding_events g2
     JOIN dedupe_targets dt ON dt.dup_id = g2.id
     WHERE dt.keeper_id = grounding_events.id)
  )
WHERE id IN (SELECT keeper_id FROM dedupe_targets);

-- Stage 3: re-link foreign-key references onto the keeper.
-- query_interactions is the only known fan-out; add more UPDATE
-- statements here if future tables join to grounding_events.id.
UPDATE query_interactions
SET grounding_event_id = (
  SELECT keeper_id FROM dedupe_targets
  WHERE dup_id = query_interactions.grounding_event_id
)
WHERE grounding_event_id IN (SELECT dup_id FROM dedupe_targets);

-- Stage 4: delete the duplicate rows.
DELETE FROM grounding_events
WHERE id IN (SELECT dup_id FROM dedupe_targets);

COMMIT;
SQL

echo "[dedupe] cleanup committed."
