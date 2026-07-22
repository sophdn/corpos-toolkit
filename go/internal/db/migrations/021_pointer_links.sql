-- pointer_links: Zettelkasten-style connections between knowledge_pointers.
-- Curation pipeline proposes (confirmed=0); reviewer confirms (confirmed=1).

CREATE TABLE pointer_links (
    pointer_id   INTEGER NOT NULL REFERENCES knowledge_pointers(id),
    related_id   INTEGER NOT NULL REFERENCES knowledge_pointers(id),
    relationship TEXT NOT NULL,
    confirmed    INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (pointer_id, related_id),
    CHECK (pointer_id != related_id)
);

CREATE INDEX idx_pl_related ON pointer_links (related_id);
