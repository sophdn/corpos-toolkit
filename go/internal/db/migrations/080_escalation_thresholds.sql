-- orchestrator-tier-escalation-contract T2 — escalation_thresholds: the
-- per-trigger threshold config table for the orchestrator-tier escalation
-- contract. See docs/ORCHESTRATOR_ESCALATION.md §6.
--
-- One row per (project_id, trigger_kind). project_id = '' (empty) is the
-- GLOBAL DEFAULT row, applied when no project-specific row exists; a
-- project-specific row overrides the global for that (project, trigger).
-- This matches the contract's "global defaults seeded; projects override"
-- model and lets escalation_threshold_list merge global + per-project.
--
-- trigger_kind is CHECK-constrained to the 5 closed §2 trigger kinds, so an
-- unknown kind is rejected at write time rather than silently stored — the
-- enum is enforced at the DB level (extending it is a chain-level decision +
-- a follow-on migration, mirroring the closed-event-type discipline).
--
-- threshold_value is a single REAL whose semantics are per-trigger (a count
-- for retry_exhaustion/repeated_tool_error/parse_failure/explicit_handoff; a
-- confidence floor in [0,1] for low_confidence). The detector for each
-- trigger interprets its own value; the config layer stays uniform.
--
-- de_escalation_turns is the hysteresis K (docs §5): the router stays
-- escalated until K consecutive clean turns elapse. Stored per-row and kept
-- uniform across a project's rows by the escalation_threshold_set action.
--
-- last_event_id / last_event_ts follow the substrate projection-column
-- convention (default ''); reserved for a future fold if escalation-config
-- mutation becomes event-sourced.

CREATE TABLE escalation_thresholds (
    project_id          TEXT    NOT NULL DEFAULT '',
    trigger_kind        TEXT    NOT NULL,
    threshold_value     REAL    NOT NULL,
    enabled             INTEGER NOT NULL DEFAULT 1,
    de_escalation_turns INTEGER NOT NULL DEFAULT 2,
    updated_at          TEXT    NOT NULL DEFAULT '',
    last_event_id       TEXT    NOT NULL DEFAULT '',
    last_event_ts       TEXT    NOT NULL DEFAULT '',
    UNIQUE (project_id, trigger_kind),
    CHECK (trigger_kind IN (
        'retry_exhaustion',
        'low_confidence',
        'repeated_tool_error',
        'parse_failure',
        'explicit_handoff'
    )),
    CHECK (enabled IN (0, 1))
);

CREATE INDEX escalation_thresholds_project_idx
    ON escalation_thresholds (project_id, trigger_kind);

-- Seed the five GLOBAL-DEFAULT rows (project_id = '') so the contract works
-- out of the box with the defaults from docs/ORCHESTRATOR_ESCALATION.md §2.
INSERT INTO escalation_thresholds
    (project_id, trigger_kind, threshold_value, enabled, de_escalation_turns, updated_at)
VALUES
    ('', 'retry_exhaustion',    2,    1, 2, '2026-05-26T00:00:00Z'),
    ('', 'low_confidence',      0.35, 1, 2, '2026-05-26T00:00:00Z'),
    ('', 'repeated_tool_error', 3,    1, 2, '2026-05-26T00:00:00Z'),
    ('', 'parse_failure',       2,    1, 2, '2026-05-26T00:00:00Z'),
    ('', 'explicit_handoff',    1,    1, 2, '2026-05-26T00:00:00Z');
