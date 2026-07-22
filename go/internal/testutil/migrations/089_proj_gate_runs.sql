-- chain 434 (corpos-gate) T6 — gate-run projection tables.
--
-- The measure surface's gate_run action runs corpos-gate (internal/gate.Run
-- over a repo's gate.yml) and POSTs the aggregated verdict to this toolkit to
-- persist it as trend data. Each run is a PARENT verdict record (project +
-- commit + tier + overall_ok + parsed coverage/mutation + duration) plus N
-- CHILD check rows (one per executed check, each carrying its ok/skipped/
-- duration/note). Read back via the gate_trend action so coverage / mutation /
-- verdict become a time series per project.
--
-- WHY DEDICATED TABLES + A TYPED EVENT + A PROJECTION, NOT A FORGE SCHEMA:
-- a gate run is MACHINE-EMITTED (not agent-forged) and is a parent + child-
-- rows shape — the forge/construct single-flat-row storage can't express the
-- per-check grid. So this mirrors the benchmark feature
-- (058_proj_benchmark_results.sql) and the study-run feature
-- (088_study_runs.sql): the GateRunCompleted event is the source of truth, and
-- these two projection tables are a denormalised read cache the fold in
-- go/internal/projections/gate_runs.go rebuilds from the ledger. This is an
-- EVENT-SOURCED projection, NOT a direct-write table: the only writer is the
-- fold (via events.Emit inside the gate_run handler's WithWrite tx), and a full
-- RebuildFromEmpty replays GateRunCompleted events to a byte-identical state.
--
-- NO BACKFILL: this is a brand-new event type with no historical rows to
-- synthesise (contrast 058/088). The tables start empty and populate as
-- gate_run actions fire.

CREATE TABLE proj_gate_runs (
    -- id == the GateRunCompleted event_id (UUIDv7). One row per run. The event
    -- id is the natural primary key: each gate run is exactly one event, so
    -- re-folding is idempotent via ON CONFLICT(id).
    id              TEXT    NOT NULL,
    project_id      TEXT    NOT NULL REFERENCES projects(id),
    commit_sha      TEXT    NOT NULL DEFAULT '',
    tier            TEXT    NOT NULL,
    overall_ok      INTEGER NOT NULL,
    -- coverage_pct / branch_pct / mutation_score: -1.0 signals N/A (check did
    -- not run / was skipped / no parseable metric). branch_pct is always -1
    -- today (Go cover reports statement coverage only).
    coverage_pct    REAL    NOT NULL DEFAULT -1,
    branch_pct      REAL    NOT NULL DEFAULT -1,
    mutation_score  REAL    NOT NULL DEFAULT -1,
    duration_ms     INTEGER NOT NULL DEFAULT 0,
    -- ran_at: the event envelope ts (RFC 3339 UTC) stored verbatim. gate_trend
    -- orders by ran_at DESC (lexical == chronological for Z-suffixed RFC 3339).
    ran_at          TEXT    NOT NULL,
    last_event_id   TEXT    NOT NULL DEFAULT '',
    last_event_ts   TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (id)
);

CREATE INDEX proj_gate_runs_project_idx ON proj_gate_runs (project_id);
CREATE INDEX proj_gate_runs_tier_idx    ON proj_gate_runs (tier);
CREATE INDEX proj_gate_runs_ran_at_idx  ON proj_gate_runs (ran_at DESC);

CREATE TABLE proj_gate_check_results (
    -- run_id FKs the parent. ON DELETE CASCADE keeps the child clean if the
    -- parent is ever truncated (foreign_keys is off in the shared DSN, so the
    -- projection's RebuildFromEmpty also deletes this table explicitly).
    run_id          TEXT    NOT NULL REFERENCES proj_gate_runs(id) ON DELETE CASCADE,
    project_id      TEXT    NOT NULL REFERENCES projects(id),
    -- run_seq: 0-based index of the check within the run, preserving execution
    -- order (child rows have no other natural key).
    run_seq         INTEGER NOT NULL DEFAULT 0,
    name            TEXT    NOT NULL,
    tier            TEXT    NOT NULL DEFAULT '',
    ok              INTEGER NOT NULL DEFAULT 0,
    skipped         INTEGER NOT NULL DEFAULT 0,
    duration_ms     INTEGER NOT NULL DEFAULT 0,
    note            TEXT    NOT NULL DEFAULT '',
    last_event_id   TEXT    NOT NULL DEFAULT '',
    last_event_ts   TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX proj_gate_check_results_project_run_idx ON proj_gate_check_results (project_id, run_id);
CREATE INDEX proj_gate_check_results_run_idx         ON proj_gate_check_results (run_id);
