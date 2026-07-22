-- finish-sophdn-repo-split T10 — ledger project rename: mcp-servers -> corpos-toolkit
-- (MIRROR scheme, Sophi-locked 2026-06-07: ledger projects mirror repos; the
-- substrate repo sophdn/toolkit was renamed to sophdn/corpos-toolkit at T9, so its
-- ledger project follows). The separate 'corpos' project (agent-OS work) is NOT
-- touched — this is a rename, NOT an umbrella merge.
--
-- WHY a migration (not a host-side UPDATE): post-cutover the canonical DB is
-- SINGLE-WRITER (the toolkit-server container). A raw host-side UPDATE would be a
-- second writer = the cross-mount-namespace WAL/dual-writer corruption hazard the
-- flip exists to prevent. So the re-scope is baked into the image and applied by the
-- container on startup, as the sole writer, preceded by a DB-file backup and followed
-- by a projection rebuild + count verification.
--
-- EVENT IMMUTABILITY: this rewrites events.entity_project_id, i.e. it relabels the
-- (otherwise append-only) ledger. That is the accepted exception for a project
-- rename — the event identity/causality is unchanged; only the project label moves,
-- exactly mirroring the repo rename. No payload or causal field is altered.
--
-- IDEMPOTENT / TEST-SAFE: every statement is guarded WHERE ...='mcp-servers'. A
-- hermetic test DB has no mcp-servers rows, so this is a no-op there; re-running on
-- production matches nothing the second time.

-- 1) FK target: the corpos-toolkit project row must exist before re-scoping the
--    project_id columns that REFERENCE projects(id). Path = the RENAMED repo dir
--    (~/dev/corpos-toolkit); name/created_at carried from the old row. No-op if the
--    old row is gone (fresh/test DB) or corpos-toolkit already exists.
INSERT INTO projects (id, name, path, created_at)
  SELECT 'corpos-toolkit', 'Corpos Toolkit', '/home/user/dev/corpos-toolkit', created_at
    FROM projects WHERE id = 'mcp-servers'
  ON CONFLICT(id) DO NOTHING;

-- 2) entity_project_id (events = source of truth; query_resolutions). Both tables
--    carry a BEFORE UPDATE append-only guard (events_no_update / query_resolutions_
--    no_update) that RAISEs on any UPDATE. A project RENAME is the sanctioned
--    schema-layer relabel, NOT a content edit, so this migration drops each guard,
--    performs the privileged label rewrite, and restores the guard verbatim — all
--    inside the one migration transaction. The _no_delete guards stay (we never
--    DELETE here); event identity/causality is untouched, only the project label moves.
DROP TRIGGER IF EXISTS events_no_update;
UPDATE events SET entity_project_id = 'corpos-toolkit' WHERE entity_project_id = 'mcp-servers';
CREATE TRIGGER events_no_update
BEFORE UPDATE ON events
BEGIN
    SELECT RAISE(ABORT, 'events table is append-only; use a compensating event with caused_by_event_id');
END;

DROP TRIGGER IF EXISTS query_resolutions_no_update;
UPDATE query_resolutions SET entity_project_id = 'corpos-toolkit' WHERE entity_project_id = 'mcp-servers';
CREATE TRIGGER query_resolutions_no_update
BEFORE UPDATE ON query_resolutions
BEGIN
    SELECT RAISE(ABORT, 'query_resolutions is append-only; reopen+resolve cycles get new rows');
END;

-- 3) project_id across the 23 projection / state tables (enumerated live 2026-06-09).
-- benchmark_results_quarantine is a LEGACY table created at runtime (not by any
-- migration) and referenced by no current code — so it is ABSENT on hermetic test
-- DBs. Adopt it with CREATE IF NOT EXISTS (a no-op on production, where it already
-- exists with these columns) so the relabel below is test-safe. No FK to projects,
-- so the projects-row DELETE in step 4 is unaffected.
CREATE TABLE IF NOT EXISTS benchmark_results_quarantine (
  id TEXT, project_id TEXT, scenario_id TEXT, tool_name TEXT, model_name TEXT,
  run_id TEXT, run_at INT, wall_clock_ms INT, input_tokens INT, output_tokens INT,
  invoked_contextually INT, invocation_ok INT, args_match INT, extracted_args TEXT,
  interpretation_ok INT, detected_tool TEXT, notes TEXT, layer TEXT, task_shape TEXT,
  accuracy_score REAL, honesty_score REAL, ranking_quality_score REAL, within_budget_score REAL
);
UPDATE bench_harnesses                  SET project_id = 'corpos-toolkit' WHERE project_id = 'mcp-servers';
UPDATE benchmark_results_quarantine     SET project_id = 'corpos-toolkit' WHERE project_id = 'mcp-servers';
UPDATE curation_candidates              SET project_id = 'corpos-toolkit' WHERE project_id = 'mcp-servers';
UPDATE emotive_results                  SET project_id = 'corpos-toolkit' WHERE project_id = 'mcp-servers';
UPDATE escalation_thresholds            SET project_id = 'corpos-toolkit' WHERE project_id = 'mcp-servers';
UPDATE ghosts                           SET project_id = 'corpos-toolkit' WHERE project_id = 'mcp-servers';
UPDATE grounding_events                 SET project_id = 'corpos-toolkit' WHERE project_id = 'mcp-servers';
UPDATE kiwix_references                 SET project_id = 'corpos-toolkit' WHERE project_id = 'mcp-servers';
UPDATE knowledge_pointers               SET project_id = 'corpos-toolkit' WHERE project_id = 'mcp-servers';
UPDATE library_entries                  SET project_id = 'corpos-toolkit' WHERE project_id = 'mcp-servers';
UPDATE pending_decisions                SET project_id = 'corpos-toolkit' WHERE project_id = 'mcp-servers';
UPDATE proj_benchmark_results           SET project_id = 'corpos-toolkit' WHERE project_id = 'mcp-servers';
UPDATE proj_chain_status                SET project_id = 'corpos-toolkit' WHERE project_id = 'mcp-servers';
UPDATE proj_current_bugs                SET project_id = 'corpos-toolkit' WHERE project_id = 'mcp-servers';
UPDATE proj_current_suggestions         SET project_id = 'corpos-toolkit' WHERE project_id = 'mcp-servers';
UPDATE proj_memories                    SET project_id = 'corpos-toolkit' WHERE project_id = 'mcp-servers';
UPDATE proj_query_volume_by_source      SET project_id = 'corpos-toolkit' WHERE project_id = 'mcp-servers';
UPDATE proj_retrieval_success_per_query SET project_id = 'corpos-toolkit' WHERE project_id = 'mcp-servers';
UPDATE proj_roadmap_view                SET project_id = 'corpos-toolkit' WHERE project_id = 'mcp-servers';
UPDATE remote_ops                       SET project_id = 'corpos-toolkit' WHERE project_id = 'mcp-servers';
UPDATE session_registry                 SET project_id = 'corpos-toolkit' WHERE project_id = 'mcp-servers';
UPDATE setup_recipes                    SET project_id = 'corpos-toolkit' WHERE project_id = 'mcp-servers';
UPDATE trained_models                   SET project_id = 'corpos-toolkit' WHERE project_id = 'mcp-servers';

-- 4) Drop the now-unreferenced old project row (every referrer moved in steps 2-3).
DELETE FROM projects WHERE id = 'mcp-servers';
