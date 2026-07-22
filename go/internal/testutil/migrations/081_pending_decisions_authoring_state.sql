-- arc-close-decision-authoring-split T5 — pending_decisions.authoring_state:
-- the staging state machine for body-heavy decisions that are staged for
-- in-session agent authoring rather than auto-forged with Qwen's draft body.
-- See docs/ARC_CLOSE_DECISION_AUTHORING_SPLIT.md §Staging state machine.
--
-- Values (NULL = not a staged row; the steady state for rows carrying only
-- auto_execute / surface_for_confirm decisions):
--   'staged'          — at least one decision in the row is StagedForAuthoring;
--                       awaiting the agent's authored forge.
--   'authored'        — the agent authored a matching artifact this session;
--                       no fallback needed (terminal, success).
--   'fallback_forged' — the agent never authored by the trigger (session end /
--                       explicit skip), so Qwen's retained draft was forged
--                       flagged `unreviewed` (terminal, capture-not-lost).
--
-- NULL default keeps every existing row and every non-staged future row
-- untouched — only the staging path writes a non-NULL value.
ALTER TABLE pending_decisions ADD COLUMN authoring_state TEXT;

-- The fallback sweep queries for rows still 'staged' for a session, oldest
-- first. A partial index keeps that scan cheap and ignores the NULL majority.
CREATE INDEX pending_decisions_staged_idx
    ON pending_decisions (target_session_id, created_at)
    WHERE authoring_state = 'staged';
