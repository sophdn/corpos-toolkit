-- bench-diff-exact-equality-vs-live-data-defeats-regression-detection:
-- give measure.bench_run a meaningful pass/fail.
--
-- gate_metrics is a comma-separated allowlist of metric-name patterns
-- that constitute the deterministic regression gate. bench_run computes
-- gate_passed over ONLY the matched metrics (exact-zero delta), leaving
-- jittery / live-data-dependent metrics (latency, drifting envelope
-- sizes) as informational rows that don't fail the run. Empty (the
-- default) = report-only, preserving the pre-fix behavior; a harness
-- opts into gating by naming its deterministic metrics. A pattern
-- matches a metric when the name equals it OR ends with "."+pattern
-- (suffix gating for the "<shape>.<metric>" convention — "n" gates every
-- "<shape>.n").
--
-- updated_at: every other shared-db artifact table carries it, and
-- forge_edit's generic UPDATE path unconditionally SETs
-- updated_at = datetime('now'). bench_harnesses (migration 067) was the
-- lone exception, so forge_edit(bench, ...) failed with "no such column:
-- updated_at" — adding it here makes bench rows editable (the path used
-- to set gate_metrics on the already-registered parse-context row) and
-- restores table consistency. Backfilled to created_at for existing rows
-- so the column isn't a meaningless empty string on pre-migration rows.

ALTER TABLE bench_harnesses ADD COLUMN gate_metrics TEXT NOT NULL DEFAULT '';
ALTER TABLE bench_harnesses ADD COLUMN updated_at   TEXT NOT NULL DEFAULT '';

UPDATE bench_harnesses SET updated_at = created_at WHERE updated_at = '';
