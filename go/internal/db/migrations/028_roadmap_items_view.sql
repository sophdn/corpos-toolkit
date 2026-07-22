-- Polymorphic-ref naming explainer + read-only convenience view.
--
-- `roadmap_items` uses `ref_kind` ('chain' | 'task') + `ref_slug` instead
-- of the chain_slug / task_slug naming the sibling tables use
-- (chains.slug, tasks.chain_slug, bugs.routed_chain_slug). The
-- polymorphism is deliberate: a roadmap entry can point at either a
-- chain or a task, and a single ref column keeps the schema flat
-- without two nullable foreign keys. The naming divergence is the
-- intentional consequence — agents pattern-matching from chains/tasks
-- to write roadmap SQL MUST use ref_kind + ref_slug, not chain_slug
-- (which is a separate nullable column carrying the parent chain for
-- task-kind rows so the dashboard doesn't have to re-join).
--
-- `roadmap_items` also intentionally has no `status` column. The
-- lifecycle of a roadmap entry is "is the row still present?" — when a
-- chain/task closes, the row is DELETEd by the work-surface handlers
-- (see cleanupBlockersAfterClose / transitionTask's roadmap cleanup
-- branch). The actual status of the referenced work lives on
-- chains.status / tasks.status; joining to surface a `status` column
-- for natural-shape queries is the role of the
-- `roadmap_items_v_with_status` view below.
--
-- See CONVENTIONS.md §polymorphic-ref-naming for the convention this
-- table follows and how new polymorphic-reference tables should be
-- shaped (same form: ref_kind + ref_slug + project_id; status NOT
-- mirrored; lifecycle = row presence).

CREATE VIEW IF NOT EXISTS roadmap_items_v_with_status AS
SELECT
    r.id,
    r.project_id,
    r.position,
    r.ref_kind,
    r.ref_slug,
    r.chain_slug,
    r.note,
    r.created_at,
    r.updated_at,
    CASE WHEN r.ref_kind = 'chain' THEN c.status
         WHEN r.ref_kind = 'task'  THEN t.status END AS status,
    CASE WHEN r.ref_kind = 'chain' THEN c.updated_at
         WHEN r.ref_kind = 'task'  THEN t.updated_at END AS target_updated_at
FROM roadmap_items r
LEFT JOIN chains c ON r.ref_kind = 'chain' AND c.slug = r.ref_slug
LEFT JOIN tasks  t ON r.ref_kind = 'task'  AND t.slug = r.ref_slug;
