-- Corrective migration for bug task-lifecycle-event-folds-fan-out-across-
-- duplicate-task-slugs. Eight chains each own a task slug='retrospective'.
-- Because task lifecycle events carried no chain disambiguation, a single
-- legit TaskCompleted(598865b) (arc-close-filing-review's own T7 commit)
-- fanned out and closed-stamped all 8 retrospectives, and a later reopen
-- fanned out and flipped all 8 to active. The fold is fixed at 5fc2caa5
-- (chain_slug now scopes each lifecycle fold); this migration corrects the
-- 8 already-corrupted projection rows AND appends chain-scoped compensating
-- events so a future `rebuild-projections` reproduces the corrected state
-- (the old chain-less fanout events still replay-then-fan-out, then these
-- compensating events — which carry chain_slug — target each task precisely).
--
-- True per-chain state (verified: the ONLY legitimate retrospective
-- completion in the whole ledger is arc-close-filing-review's 598865b):
--   arc-close-filing-review                -> closed + 598865b (legit)
--   train-skill-auto-loader-v1             -> pending (never completed)
--   orchestrator-tier-escalation-contract  -> pending
--   bridge-harness-mcp-client              -> pending
--   cross-harness-reflex-port              -> pending
--   harness-swap-validation                -> pending
--   per-tool-per-model-observability       -> pending
--   retire-native-task-tools-via-env-var   -> pending (closed/abandoned
--       chain; its retrospective was never legitimately done — honest
--       pre-fanout state is pending)
--
-- Gated on EXISTS (the target chain+retrospective is present) so this is a
-- no-op in fresh/hermetic test DBs that don't carry these chains — same
-- safety shape as migration 062. Idempotent: NOT EXISTS on actor_id guards
-- the event inserts; the projection UPDATEs set fixed states.

-- ============================================================================
-- PART 1: compensating events (durable on rebuild; chain-scoped)
-- ============================================================================

-- 1a. The 7 never-completed retrospectives -> pending.
INSERT INTO events (
    event_id, ts, actor_kind, actor_id, type,
    entity_kind, entity_slug, entity_project_id,
    payload, rationale, caused_by_event_id, related_entities,
    span_id, schema_version
)
SELECT
    lower(printf('%08x-%04x-7%03x-%s%03x-%s',
        (unixepoch('now') * 1000) / 65536, (unixepoch('now') * 1000) % 65536,
        (abs(random()) % 4096), substr('89ab', 1 + (abs(random()) % 4), 1),
        abs(random()) % 4096, lower(hex(randomblob(6))))),
    strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
    'system', 'retrospective-fanout-correction-069', 'TaskTransitioned',
    'task', 'retrospective', 'mcp-servers',
    json_object('chain_slug', v.chain_slug, 'from_status', 'active', 'to_status', 'pending'),
    'correct task-event fold-fanout (bug task-lifecycle-event-folds-fan-out-across-duplicate-task-slugs): restore never-completed retrospective to its true pending state',
    NULL, '[]',
    lower(printf('%s-%s-4%s-%s%s-%s',
        lower(hex(randomblob(4))), lower(hex(randomblob(2))),
        substr(lower(hex(randomblob(2))), 2), substr('89ab', 1 + (abs(random()) % 4), 1),
        substr(lower(hex(randomblob(2))), 2), lower(hex(randomblob(6))))),
    1
FROM (
    SELECT 'train-skill-auto-loader-v1' AS chain_slug
    UNION ALL SELECT 'orchestrator-tier-escalation-contract'
    UNION ALL SELECT 'bridge-harness-mcp-client'
    UNION ALL SELECT 'cross-harness-reflex-port'
    UNION ALL SELECT 'harness-swap-validation'
    UNION ALL SELECT 'per-tool-per-model-observability'
    UNION ALL SELECT 'retire-native-task-tools-via-env-var'
) v
WHERE EXISTS (
    SELECT 1 FROM proj_chain_status c JOIN proj_current_tasks t ON t.chain_id = c.id
    WHERE c.slug = v.chain_slug AND c.project_id = 'mcp-servers' AND t.slug = 'retrospective'
)
AND NOT EXISTS (
    SELECT 1 FROM events WHERE actor_id = 'retrospective-fanout-correction-069' AND type = 'TaskTransitioned'
);

-- 1b. arc-close-filing-review's retrospective -> closed + 598865b (legit).
INSERT INTO events (
    event_id, ts, actor_kind, actor_id, type,
    entity_kind, entity_slug, entity_project_id,
    payload, rationale, caused_by_event_id, related_entities,
    span_id, schema_version
)
SELECT
    lower(printf('%08x-%04x-7%03x-%s%03x-%s',
        (unixepoch('now') * 1000) / 65536, (unixepoch('now') * 1000) % 65536,
        (abs(random()) % 4096), substr('89ab', 1 + (abs(random()) % 4), 1),
        abs(random()) % 4096, lower(hex(randomblob(6))))),
    strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
    'system', 'retrospective-fanout-correction-069', 'TaskCompleted',
    'task', 'retrospective', 'mcp-servers',
    json_object('chain_slug', 'arc-close-filing-review', 'commit_sha', '598865b'),
    'restore arc-close-filing-review retrospective to its legitimate closed state (598865b is its own T7 retrospective commit) after fold-fanout + reopen knocked it active',
    NULL, '[]',
    lower(printf('%s-%s-4%s-%s%s-%s',
        lower(hex(randomblob(4))), lower(hex(randomblob(2))),
        substr(lower(hex(randomblob(2))), 2), substr('89ab', 1 + (abs(random()) % 4), 1),
        substr(lower(hex(randomblob(2))), 2), lower(hex(randomblob(6))))),
    1
WHERE EXISTS (
    SELECT 1 FROM proj_chain_status c JOIN proj_current_tasks t ON t.chain_id = c.id
    WHERE c.slug = 'arc-close-filing-review' AND c.project_id = 'mcp-servers' AND t.slug = 'retrospective'
)
AND NOT EXISTS (
    SELECT 1 FROM events WHERE actor_id = 'retrospective-fanout-correction-069' AND type = 'TaskCompleted'
);

-- ============================================================================
-- PART 2: immediate projection correction (the fold hook does not fire on
-- raw event INSERTs, so the live projection is corrected directly here;
-- the PART 1 events make a future rebuild reproduce this)
-- ============================================================================

UPDATE proj_current_tasks
SET status = 'pending', commit_sha = NULL, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE slug = 'retrospective'
  AND chain_id IN (
      SELECT id FROM proj_chain_status WHERE project_id = 'mcp-servers' AND slug IN (
          'train-skill-auto-loader-v1', 'orchestrator-tier-escalation-contract',
          'bridge-harness-mcp-client', 'cross-harness-reflex-port',
          'harness-swap-validation', 'per-tool-per-model-observability',
          'retire-native-task-tools-via-env-var'
      )
  );

UPDATE proj_current_tasks
SET status = 'closed', commit_sha = '598865b', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE slug = 'retrospective'
  AND chain_id = (SELECT id FROM proj_chain_status WHERE project_id = 'mcp-servers' AND slug = 'arc-close-filing-review');

-- ============================================================================
-- PART 3: refresh the affected chains' task-status counters (the live
-- counters still read active=1, which is why the chains showed "in progress")
-- ============================================================================

UPDATE proj_chain_status
SET total_tasks = (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = proj_chain_status.id),
    pending     = (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = proj_chain_status.id AND status = 'pending'),
    active      = (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = proj_chain_status.id AND status = 'active'),
    blocked     = (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = proj_chain_status.id AND status = 'blocked'),
    closed      = (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = proj_chain_status.id AND status = 'closed'),
    cancelled   = (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = proj_chain_status.id AND status = 'cancelled')
WHERE project_id = 'mcp-servers' AND slug IN (
    'train-skill-auto-loader-v1', 'orchestrator-tier-escalation-contract',
    'bridge-harness-mcp-client', 'cross-harness-reflex-port',
    'harness-swap-validation', 'per-tool-per-model-observability',
    'retire-native-task-tools-via-env-var', 'arc-close-filing-review'
);
