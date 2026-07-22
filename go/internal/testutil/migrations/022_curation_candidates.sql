-- curation_candidates: pointers awaiting quality scoring and promotion review.
-- Auto-promote when quality_score >= 0.85; others queue for human review.
-- Unreviewed candidates expire after 30 days (expires_at).

CREATE TABLE curation_candidates (
    id                     INTEGER PRIMARY KEY,
    project_id             TEXT NOT NULL,
    source_type            TEXT NOT NULL,
    source_ref             TEXT NOT NULL,
    question               TEXT NOT NULL,
    invoke_when            TEXT NOT NULL,
    description            TEXT NOT NULL,
    tags                   TEXT NOT NULL DEFAULT '[]',
    quality_score          REAL,
    origin                 TEXT NOT NULL,
    origin_ref             TEXT,
    promoted_automatically INTEGER NOT NULL DEFAULT 0,
    promoted_at            TEXT,
    expires_at             TEXT,
    status                 TEXT NOT NULL DEFAULT 'pending',
    created_at             TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_cc_project_status ON curation_candidates (project_id, status);
CREATE INDEX idx_cc_expires ON curation_candidates (expires_at) WHERE status = 'pending';
