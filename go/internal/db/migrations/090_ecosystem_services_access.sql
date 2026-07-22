-- ecosystem surface — the local-ecosystem service
-- (chain 435 local-ecosystem-service-and-extraction-pattern T3).
--
-- Completes the shared-infra host subsystem started by `hosts` (migration 003):
-- adds the multi-address, service, and access-method layers that back the
-- deterministic "do I have access to X" query. Like `hosts`, these tables are
-- SHARED INFRASTRUCTURE and deliberately OMIT project_id — an ecosystem belongs
-- to the agent-host (the tenant), not to any one project; a per-project split
-- would fragment the map when two projects target the same machine. This mirrors
-- the `hosts` decision documented in migration 003.
--
-- DIRECT-WRITE tables, NOT event-sourced projections — matching `hosts` /
-- `trained_models` and `chain_deps` (migration 087). Ecosystem facts are
-- configuration LEARNED from the operator, not lifecycle history that needs a
-- ledger replay; the ecosystem.*_learn handlers INSERT/UPDATE here directly
-- inside their write tx. Excluded from the events-rebuild byte-identical test for
-- the same reason `chain_deps` / `roadmap_meta` are.
--
-- CREDENTIAL INVARIANT (load-bearing): `ecosystem_access_methods.credential_pointer`
-- stores a POINTER to where a secret lives (a path or env name like
-- '~/.ssh/id_ed25519' or '~/.git-credentials'), NEVER a secret value. The
-- ecosystem.access_learn handler rejects pointer values that look like inline
-- secrets (long hex / base64 runs). The store is a map to secrets, not a secret
-- store; real secrets stay in the filesystem/agent, untouched.
--
-- POLYMORPHIC TARGET: `ecosystem_access_methods.target_kind` + `target_slug`
-- follow the polymorphic-ref convention (CONVENTIONS.md §Polymorphic-ref naming):
-- an access method attaches to EITHER a host OR a service, so the columns are
-- deliberately NOT named host_slug/service_slug and carry no FK — the handler
-- validates the referent. A service's unlocking method is found by querying
-- access_methods WHERE target_kind='service' AND target_slug=<svc> (one
-- direction, no back-pointer column on services).

-- A host is reachable by more than one address (tailnet MagicDNS, tailnet IP, LAN
-- IP, .local). `hosts.addr` holds the single canonical address; this side table
-- (side-table-over-column-widening convention) carries the alternates so the query
-- can report WHICH address to use and recommend a preferred one.
CREATE TABLE IF NOT EXISTS ecosystem_host_addresses (
    host_slug   TEXT    NOT NULL REFERENCES hosts(slug) ON DELETE CASCADE,
    kind        TEXT    NOT NULL CHECK (kind IN ('tailnet', 'lan', 'magicdns', 'hostname', 'other')),
    value       TEXT    NOT NULL,                 -- '203.0.113.10', 'example-host.tailnet.ts.net'
    preferred   INTEGER NOT NULL DEFAULT 0,       -- 1 = the address the query recommends first
    notes       TEXT    NOT NULL DEFAULT '',
    created_at  TEXT    NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (host_slug, value)
);

CREATE INDEX IF NOT EXISTS idx_eco_host_addr_host ON ecosystem_host_addresses (host_slug);

-- Something running on a host. `status` captures live-vs-retired (e.g. dm-manager
-- is 'retired'). `soft_ref` points at the vault how-to note for the procedural
-- prose the service deliberately does NOT hold (the determinism-vs-RAG boundary).
CREATE TABLE IF NOT EXISTS ecosystem_services (
    slug        TEXT    PRIMARY KEY,               -- 'gitea', 'jellyfin', 'campaign-settings'
    host_slug   TEXT    NOT NULL REFERENCES hosts(slug) ON DELETE CASCADE,
    kind        TEXT    NOT NULL DEFAULT '',        -- 'http' | 'db' | 'git' | 'media' | ... freeform
    endpoint    TEXT    NOT NULL DEFAULT '',        -- URL or 'host:port'
    port        INTEGER,                            -- NULL when N/A
    status      TEXT    NOT NULL DEFAULT 'live' CHECK (status IN ('live', 'retired')),
    soft_ref    TEXT    NOT NULL DEFAULT '',        -- vault how-to pointer, e.g. 'memory/reference/...'
    notes       TEXT    NOT NULL DEFAULT '',
    created_at  TEXT    NOT NULL DEFAULT (datetime('now')),
    retired_at  TEXT                                -- soft-delete
);

CREATE INDEX IF NOT EXISTS idx_eco_services_host    ON ecosystem_services (host_slug);
CREATE INDEX IF NOT EXISTS idx_eco_services_retired ON ecosystem_services (retired_at);

-- How you authenticate to a host or service. `credential_pointer` is a POINTER
-- only (see the credential invariant above). `enabled` lets a method be recorded
-- but marked unusable without deleting it.
CREATE TABLE IF NOT EXISTS ecosystem_access_methods (
    slug                TEXT    PRIMARY KEY,        -- 'example-host-ssh', 'gitea-api'
    target_kind         TEXT    NOT NULL CHECK (target_kind IN ('host', 'service')),
    target_slug         TEXT    NOT NULL,           -- hosts.slug or ecosystem_services.slug (handler-validated)
    method              TEXT    NOT NULL CHECK (method IN ('ssh', 'https-api', 'https-basic', 'token', 'none')),
    principal           TEXT    NOT NULL DEFAULT '', -- ssh user / api user, e.g. 'youruser', 'sophdn'
    credential_pointer  TEXT    NOT NULL DEFAULT '', -- '~/.ssh/id_ed25519' — POINTER, never a secret
    scope_note          TEXT    NOT NULL DEFAULT '', -- 'admin', 'repo-scoped not org', ...
    enabled             INTEGER NOT NULL DEFAULT 1,
    soft_ref            TEXT    NOT NULL DEFAULT '',
    notes               TEXT    NOT NULL DEFAULT '',
    created_at          TEXT    NOT NULL DEFAULT (datetime('now')),
    retired_at          TEXT
);

CREATE INDEX IF NOT EXISTS idx_eco_access_target  ON ecosystem_access_methods (target_kind, target_slug);
CREATE INDEX IF NOT EXISTS idx_eco_access_retired ON ecosystem_access_methods (retired_at);
