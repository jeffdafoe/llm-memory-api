-- MEM-076: Note-level sharing permissions + actor visibility
-- Allows actors to share individual notes or folders with specific actors or all actors.

CREATE TABLE note_permissions (
    id SERIAL PRIMARY KEY,
    owner_namespace TEXT NOT NULL,
    slug_pattern TEXT NOT NULL,
    grantee_actor_id INTEGER REFERENCES actors(id) ON DELETE CASCADE,  -- NULL = shared with all
    can_read BOOLEAN NOT NULL DEFAULT FALSE,
    can_write BOOLEAN NOT NULL DEFAULT FALSE,
    can_delete BOOLEAN NOT NULL DEFAULT FALSE,
    granted_by INTEGER NOT NULL REFERENCES actors(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at TIMESTAMPTZ
);

-- Index for looking up shares by grantee (what's shared with me?)
CREATE INDEX idx_note_permissions_grantee ON note_permissions (grantee_actor_id) WHERE revoked_at IS NULL;

-- Index for looking up shares by owner namespace (what have I shared?)
CREATE INDEX idx_note_permissions_owner ON note_permissions (owner_namespace) WHERE revoked_at IS NULL;

-- Index for search integration: find active shares that apply to a given slug
CREATE INDEX idx_note_permissions_slug ON note_permissions (owner_namespace, slug_pattern) WHERE revoked_at IS NULL;

-- Actor visibility: controls whether other actors can find you in share search
ALTER TABLE actors ADD COLUMN visible_to_others BOOLEAN NOT NULL DEFAULT FALSE;
