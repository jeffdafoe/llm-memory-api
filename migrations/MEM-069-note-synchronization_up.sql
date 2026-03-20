-- MEM-069: Create note_synchronization table for bidirectional note↔filesystem sync mappings.
-- Each row maps a note slug (or slug prefix) to a local filesystem path for a specific agent.

CREATE TABLE note_synchronization (
    id          SERIAL PRIMARY KEY,
    actor_id    INTEGER NOT NULL REFERENCES actors(id),
    namespace   VARCHAR(64) NOT NULL,
    slug        VARCHAR(255) NOT NULL,
    local_path  VARCHAR(500) NOT NULL,
    created_at  TIMESTAMPTZ DEFAULT NOW(),

    -- Each agent can only have one sync mapping per namespace+slug
    UNIQUE (actor_id, namespace, slug)
);

CREATE INDEX idx_note_sync_actor ON note_synchronization(actor_id);
