-- work-batching-and-forge-templates T6 — bench_harnesses table for
-- forge(bench) registrations. Each row is one benchmark harness the
-- operator can re-run via measure.bench_run with a baseline diff.
--
-- UNIQUE on (project_id, slug); separate INTEGER PRIMARY KEY id so re-
-- forge with the same slug returns the existing row idempotently (the
-- forge handler's INSERT...ON CONFLICT DO NOTHING path leans on the
-- unique constraint).
--
-- parse_output_as is the closed enum the bench_run handler uses to
-- parse the harness's stdout. v1 ships `json` only; markdown_table +
-- key_value_pairs are flagged as stretch in the T6 spec and will land
-- as a follow-on if needed.
--
-- timeout_ms gates the subprocess wall-clock so a hung harness can't
-- pin the MCP handler. Default 60_000 (60s) matches the T6 constraints
-- block.
--
-- This migration was authored as a raw-edit fallback (not via
-- forge(migration)) because of bug `forge-migration-writes-relative-to-
-- toolkit-server-cwd-not-target-repo-root` — forge(migration) writes
-- relative to the toolkit-server process's CWD which is not the target
-- project's repo root for the stdio-launched MCP. T5's primary path
-- still works in-process for tests; this commit uses raw-edit until
-- the cwd bug fix lands.

CREATE TABLE bench_harnesses (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id          TEXT    NOT NULL,
    slug                TEXT    NOT NULL,
    binary_path         TEXT    NOT NULL,
    flag_set_json       TEXT    NOT NULL DEFAULT '[]',
    baseline_json_path  TEXT    NOT NULL,
    parse_output_as     TEXT    NOT NULL DEFAULT 'json',
    timeout_ms          INTEGER NOT NULL DEFAULT 60000,
    created_at          TEXT    NOT NULL,
    last_event_id       TEXT    NOT NULL DEFAULT '',
    last_event_ts       TEXT    NOT NULL DEFAULT '',
    UNIQUE (project_id, slug),
    CHECK (parse_output_as IN ('json'))
);
CREATE INDEX bench_harnesses_project_slug_idx ON bench_harnesses (project_id, slug);
