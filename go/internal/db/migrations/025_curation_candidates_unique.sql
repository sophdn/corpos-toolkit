-- Enforce uniqueness on (project_id, source_ref) in curation_candidates.
-- Without this, re-runs of knowledge_seeder passes 5/6 silently insert
-- duplicate rows; application-level candidate_exists() guards were added
-- as a workaround but the schema remained unprotected.
--
-- Deduplicate first (keep the earliest id per pair) to avoid the index
-- creation failing on any rows that slipped in before this migration.

DELETE FROM curation_candidates
WHERE id NOT IN (
    SELECT MIN(id)
    FROM curation_candidates
    GROUP BY project_id, source_ref
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_cc_project_source_ref
    ON curation_candidates (project_id, source_ref);
