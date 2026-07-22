-- phase-4-legacy-field-deprecation F2 — drop the three retired prose
-- columns from the projection tables.
--
-- Disposition per docs/PHASE_4_LEGACY_FIELD_DEPRECATION.md §3:
--   • proj_current_bugs.resolution_note         → RETIRE
--   • proj_current_suggestions.resolution_note  → RETIRE
--   • proj_chain_status.design_decisions        → RETIRE
--
-- The underlying event payloads (BugResolved.resolution_note,
-- SuggestionResolved.resolution_note, ChainCreated.design_decisions,
-- ChainEdited.updated_values) STAY — the substrate's source of truth
-- never moves. Only the projection-side cache + the dashboard's prose
-- duplicate (deleted in F3) retire. The EventTimeline + structured
-- payload drawer continue to surface the values from the event ledger.
--
-- Fold modules (go/internal/projections/{bugs,suggestions,chains}.go)
-- are updated in the same commit to stop writing these columns; the
-- observe-http detail endpoints drop the corresponding JSON keys.
--
-- Other prose fields stay per F1's disposition matrix:
--   • chain.output / chain.completion_condition → KEEP (chain identity
--     + acceptance test; above-fold render)
--   • task.constraints / acceptance_criteria / handoff_output → KEEP
--     (no dashboard prose render; observe-http search content; the
--     handoff_output column-overwrite shape collision is filed as
--     Finding 1 / a separate bug, out of Phase 4 scope)
--   • bug.constraints / acceptance_criteria + suggestion.constraints /
--     acceptance_criteria → KEEP (pre-emission plan content)

ALTER TABLE proj_current_bugs        DROP COLUMN resolution_note;
ALTER TABLE proj_current_suggestions DROP COLUMN resolution_note;
ALTER TABLE proj_chain_status        DROP COLUMN design_decisions;
