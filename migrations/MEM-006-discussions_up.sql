-- MEM-006: Discussion coordination API
-- Tracks multi-agent discussions, participants, and voting.
-- Discussions link to the chat system via channel but handle coordination separately.

CREATE TABLE discussions (
    id SERIAL PRIMARY KEY,
    topic TEXT NOT NULL,
    created_by VARCHAR(50) NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'active',
    channel VARCHAR(50),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    concluded_at TIMESTAMPTZ
);

CREATE TABLE discussion_participants (
    discussion_id INTEGER NOT NULL REFERENCES discussions(id),
    agent VARCHAR(50) NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'invited',
    invited_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    joined_at TIMESTAMPTZ,
    PRIMARY KEY (discussion_id, agent)
);

CREATE TABLE discussion_votes (
    id SERIAL PRIMARY KEY,
    discussion_id INTEGER NOT NULL REFERENCES discussions(id),
    proposed_by VARCHAR(50) NOT NULL,
    question TEXT NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'open',
    type VARCHAR(20) NOT NULL DEFAULT 'general',
    threshold VARCHAR(20) NOT NULL DEFAULT 'unanimous',
    closes_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    closed_at TIMESTAMPTZ
);

CREATE TABLE discussion_ballots (
    vote_id INTEGER NOT NULL REFERENCES discussion_votes(id),
    agent VARCHAR(50) NOT NULL,
    choice INTEGER NOT NULL,
    reason TEXT,
    cast_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (vote_id, agent)
);

CREATE INDEX idx_discussions_status ON discussions(status);
CREATE INDEX idx_discussion_participants_agent ON discussion_participants(agent, status);
CREATE INDEX idx_discussion_votes_discussion ON discussion_votes(discussion_id, status);

GRANT SELECT, INSERT, UPDATE, DELETE ON discussions TO memory_api;
GRANT SELECT, INSERT, UPDATE, DELETE ON discussion_participants TO memory_api;
GRANT SELECT, INSERT, UPDATE, DELETE ON discussion_votes TO memory_api;
GRANT SELECT, INSERT, UPDATE, DELETE ON discussion_ballots TO memory_api;
GRANT USAGE, SELECT ON SEQUENCE discussions_id_seq TO memory_api;
GRANT USAGE, SELECT ON SEQUENCE discussion_votes_id_seq TO memory_api;
