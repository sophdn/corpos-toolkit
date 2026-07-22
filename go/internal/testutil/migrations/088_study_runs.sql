-- study-run-persistence chain T1 — study-run projection tables.
--
-- corpos-lab runs behavioral-assay "study runs" and POSTs a JSON run record
-- to this toolkit (measure surface, action study_run_record) to persist it.
-- Each run is a PARENT provenance record (assay + study/materials digests +
-- model identity + a responses-dir POINTER) plus N CHILD score-grid rows
-- (one per condition×run cell, each carrying a flattened verdict). The
-- dashboard reads these back over the observe HTTP API and updates live on
-- SSE.
--
-- WHY DEDICATED TABLES + A TYPED EVENT + A PROJECTION, NOT A FORGE SCHEMA:
-- a study run is MACHINE-EMITTED (not agent-forged) and is a parent + child-
-- rows shape with provenance — the forge/construct single-flat-row storage
-- can't express the score grid. So this mirrors the benchmark feature
-- (035_benchmark_provenance.sql + 058_proj_benchmark_results.sql): the
-- StudyRunRecorded event is the source of truth, and these two projection
-- tables are a denormalised read cache the fold in
-- go/internal/projections/study_runs.go rebuilds from the ledger. This is an
-- EVENT-SOURCED projection, NOT a direct-write table: the only writer is the
-- fold (via events.Emit inside the study_run_record handler's WithWrite tx),
-- and a full RebuildFromEmpty replays StudyRunRecorded events to a byte-
-- identical state. Contrast 087_chain_deps.sql, which IS a direct-write
-- config table (INSERT/DELETE from its handler, excluded from the rebuild
-- parity test) — study runs are lifecycle history, so they go through the
-- ledger.
--
-- BLOBS STAY ON DISK: responses_dir is a filesystem path pointer and
-- materials_hash_json holds a small map of SHA-256 hex digests — never the
-- raw response or material bytes.

CREATE TABLE proj_study_runs (
    -- id == run_id (the StudyRunRecorded entity_slug). One row per run.
    id                   TEXT    NOT NULL,
    project_id           TEXT    NOT NULL REFERENCES projects(id),
    name                 TEXT    NOT NULL,
    assay                TEXT    NOT NULL,
    item_id              TEXT    NOT NULL DEFAULT '',
    image_ref            TEXT    NOT NULL DEFAULT '',
    image_digest         TEXT    NOT NULL DEFAULT '',
    study_digest         TEXT    NOT NULL DEFAULT '',
    -- materials_hash_json: JSON object of {filename: sha256hex}. Hashes, not
    -- blobs — allowed inline.
    materials_hash_json  TEXT    NOT NULL DEFAULT '{}',
    model_id             TEXT    NOT NULL DEFAULT '',
    model_version        TEXT    NOT NULL DEFAULT '',
    status               TEXT    NOT NULL,
    error                TEXT    NOT NULL DEFAULT '',
    -- responses_dir: on-disk pointer to the raw responses directory. Path
    -- only; the blobs never enter the ledger.
    responses_dir        TEXT    NOT NULL DEFAULT '',
    -- run_at: RFC 3339 (UTC) timestamp stored verbatim. Observe orders by
    -- run_at DESC (lexical == chronological for Z-suffixed RFC 3339).
    run_at               TEXT    NOT NULL,
    last_event_id        TEXT    NOT NULL DEFAULT '',
    last_event_ts        TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (id)
);

CREATE INDEX proj_study_runs_project_idx  ON proj_study_runs (project_id);
CREATE INDEX proj_study_runs_assay_idx    ON proj_study_runs (assay);
CREATE INDEX proj_study_runs_model_idx    ON proj_study_runs (model_id);
CREATE INDEX proj_study_runs_status_idx   ON proj_study_runs (status);
CREATE INDEX proj_study_runs_run_at_idx   ON proj_study_runs (run_at DESC);

CREATE TABLE proj_study_run_scores (
    -- run_id FKs the parent. ON DELETE CASCADE keeps the child clean if the
    -- parent is ever truncated (foreign_keys is off in the shared DSN, so
    -- the projection's RebuildFromEmpty also deletes this table explicitly).
    run_id          TEXT    NOT NULL REFERENCES proj_study_runs(id) ON DELETE CASCADE,
    project_id      TEXT    NOT NULL REFERENCES projects(id),
    condition       TEXT    NOT NULL DEFAULT '',
    run_idx         INTEGER NOT NULL DEFAULT 0,
    verdict_kind    TEXT    NOT NULL DEFAULT '',
    verdict_reason  TEXT    NOT NULL DEFAULT '',
    item            TEXT    NOT NULL DEFAULT '',
    rationale       TEXT    NOT NULL DEFAULT '',
    last_event_id   TEXT    NOT NULL DEFAULT '',
    last_event_ts   TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX proj_study_run_scores_project_run_idx ON proj_study_run_scores (project_id, run_id);
CREATE INDEX proj_study_run_scores_run_idx         ON proj_study_run_scores (run_id);
