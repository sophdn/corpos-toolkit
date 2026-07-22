-- canon: the deterministic artifact-identity / canonical-names map
-- (follow-on to chain 435 local-ecosystem-service-and-extraction-pattern;
-- suggestion extract-canonical-names-identity-map-into-deterministic-service).
--
-- Extracts the crystallized "old-name -> canonical" facts out of the soft
-- vault/memory corpus (e.g. memory/reference/corpos-rename-canonical-names) into
-- an owned deterministic lookup on the `ecosystem` surface, so a token like
-- `mcp-servers`, `sophdn-toolkit`, `~/dev/mcp-servers`, or `:3000` resolves to its
-- current canonical form instead of leaking as stale-canon.
--
-- Like `hosts` / the ecosystem tables, these are SHARED-INFRA identity facts and
-- deliberately OMIT project_id (an artifact's canonical name is not per-project).
-- DIRECT-WRITE, not event-sourced (configuration learned from the operator, not
-- lifecycle history) — matching ecosystem_* / chain_deps; excluded from the
-- events-rebuild byte-identical test (not a projection).

-- One canonical artifact: a repo, path, ledger project, DB, port, or service whose
-- identity has a single right answer and may carry retired aliases.
CREATE TABLE IF NOT EXISTS canon_entries (
    slug         TEXT    PRIMARY KEY,          -- stable canonical id, e.g. 'corpos-toolkit', 'toolkit-db', 'toolkit-port'
    kind         TEXT    NOT NULL CHECK (kind IN ('repo', 'path', 'project', 'db', 'port', 'service', 'other')),
    canonical    TEXT    NOT NULL,             -- the canonical value: 'sophdn/corpos-toolkit', '~/.local/share/toolkit/data/toolkit.db', '3001'
    status       TEXT    NOT NULL DEFAULT 'current' CHECK (status IN ('current', 'retired')),
    replacement  TEXT    NOT NULL DEFAULT '',  -- for retired entries: the current canonical form/slug that supersedes it
    gitea_owner  TEXT    NOT NULL DEFAULT '',  -- for repo/service: 'sophdn' | 'shared' (the org-owner facet)
    local_path   TEXT    NOT NULL DEFAULT '',  -- for repo: '~/dev/corpos-toolkit'
    port         INTEGER,                      -- for port/service: NULL when N/A
    notes        TEXT    NOT NULL DEFAULT '',
    soft_ref     TEXT    NOT NULL DEFAULT '',  -- vault pointer to the retirement/history prose
    created_at   TEXT    NOT NULL DEFAULT (datetime('now')),
    retired_at   TEXT
);

CREATE INDEX IF NOT EXISTS idx_canon_entries_kind   ON canon_entries (kind);
CREATE INDEX IF NOT EXISTS idx_canon_entries_status ON canon_entries (status);

-- Retired / alternate tokens that resolve to a canonical entry. This is the
-- catalog the refresolve detector matches against so a stale token in a message
-- surfaces the canonical form at parse_context orient-time.
CREATE TABLE IF NOT EXISTS canon_aliases (
    alias       TEXT    PRIMARY KEY,           -- 'mcp-servers', 'sophdn/toolkit', '~/dev/mcp-servers', '3000'
    entry_slug  TEXT    NOT NULL REFERENCES canon_entries(slug) ON DELETE CASCADE,
    dimension   TEXT    NOT NULL DEFAULT 'other' CHECK (dimension IN ('old-name', 'old-path', 'old-project', 'old-port', 'lan-form', 'other')),
    notes       TEXT    NOT NULL DEFAULT '',
    created_at  TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_canon_aliases_entry ON canon_aliases (entry_slug);
