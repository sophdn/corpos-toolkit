-- portal-write-api Layer A: chat sessions as first-class artifacts.
--
-- See seed-packet `process-docs/adhoc/portal-write-api/design_2026-05-08.md`
-- § (d) for the schema rationale. Two-table relational shape was chosen
-- over a single-row aggregated-JSON shape so (1) message-count queries
-- are cheap, (2) per-message inserts don't rewrite a growing column on
-- every turn, and (3) future content-search queries are trivial against
-- portal_chat_messages.

CREATE TABLE IF NOT EXISTS portal_chats (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id        TEXT NOT NULL REFERENCES projects(id),
    -- Unguessable user-facing identifier. Forge schema uses slug as the
    -- public chat ID; UUID-shaped values are the canonical input but the
    -- column accepts any non-empty string for testability.
    slug              TEXT NOT NULL,
    title             TEXT NOT NULL DEFAULT '',
    started_at        TEXT NOT NULL DEFAULT (datetime('now')),
    -- Null while active; set on close.
    ended_at          TEXT,
    -- One of: user_action | idle_timeout | subprocess_crash | server_shutdown.
    -- Empty while active.
    close_reason      TEXT NOT NULL DEFAULT '',
    -- Denormalized counters; updated on every message insert / tool_use_start.
    message_count     INTEGER NOT NULL DEFAULT 0,
    tool_calls_made   INTEGER NOT NULL DEFAULT 0,
    -- Last-known PID of the chat's `claude --headless` subprocess.
    -- Null when no subprocess is running for this chat.
    subprocess_pid    INTEGER,
    created_at        TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at        TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (project_id, slug)
);

CREATE INDEX IF NOT EXISTS idx_portal_chats_project ON portal_chats (project_id);
CREATE INDEX IF NOT EXISTS idx_portal_chats_active  ON portal_chats (ended_at) WHERE ended_at IS NULL;

CREATE TABLE IF NOT EXISTS portal_chat_messages (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    chat_id       INTEGER NOT NULL REFERENCES portal_chats(id) ON DELETE CASCADE,
    -- One of: user | assistant | tool_use | tool_result.
    role          TEXT NOT NULL,
    -- Plain text for user/assistant rows; JSON for tool_use/tool_result rows.
    content       TEXT NOT NULL,
    -- Per-turn token usage; set on assistant rows from message_stop.usage.
    -- Null on user/tool rows.
    tokens_input  INTEGER,
    tokens_output INTEGER,
    created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_portal_chat_messages_chat ON portal_chat_messages (chat_id);
