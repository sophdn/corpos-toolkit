-- ml-capability-substrate T4: trained_model forge artifact.
--
-- One row per (project, task, version) ML model artifact. The row's
-- lifecycle drives the go/internal/ml/ registry (T3): only rows in
-- status='ab_testing' or 'promoted' are loadable for live traffic.
-- training / evaluating / retired are gated states (see
-- docs/ML_CAPABILITY_SUBSTRATE.md §4.2 for the state machine).
--
-- project_id mirrors the bugs / chains / tasks pattern — every row is
-- project-scoped, with cross-project queries falling out of the LIKE
-- and IN filters at read time.
--
-- Slug is unique within project, not globally — same pattern as bugs
-- and chains. Convention: '<task>-<version>' (e.g. 'source-router-v1').
--
-- Status CHECK constraint enforces the enum at the storage layer.
-- The forge schema and action handlers enforce it again at the call
-- seam — belt-and-suspenders per the existing forge-conventions guard
-- pattern.
--
-- eval_metrics is TEXT (JSON-shaped); the application layer parses
-- and validates. Keeping it as TEXT avoids tying the schema to a
-- particular metric vocabulary; new metrics land per-task without a
-- migration.

CREATE TABLE trained_models (
    id                          INTEGER PRIMARY KEY,
    project_id                  TEXT NOT NULL,
    slug                        TEXT NOT NULL,
    task                        TEXT NOT NULL,
    version                     TEXT NOT NULL,
    training_dataset_signature  TEXT NOT NULL,
    eval_metrics                TEXT NOT NULL DEFAULT '{}',
    status                      TEXT NOT NULL DEFAULT 'training'
        CHECK (status IN ('training','evaluating','ab_testing','promoted','retired')),
    artifact_path               TEXT NOT NULL,
    created_at                  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at                  TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (project_id, slug)
);

-- task + status are the two filters trained_model_list takes; index them.
CREATE INDEX idx_trained_models_task   ON trained_models (project_id, task);
CREATE INDEX idx_trained_models_status ON trained_models (project_id, status);

-- The promoted-model lookup (registry.LoadByPromoted in
-- go/internal/ml/) hits (project_id, task, status='promoted') — covered
-- by idx_trained_models_task above; SQLite's query planner will
-- intersect with the status filter.
