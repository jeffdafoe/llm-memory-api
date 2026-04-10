-- MEM-112: Virtual agent call logging
-- Full request/response persistence for diagnostics and fine-tuning.
-- Complements virtual_agent_usage (lightweight cost aggregation) with detailed call data.

CREATE TABLE virtual_agent_calls (
    id              BIGSERIAL PRIMARY KEY,
    actor_id        INTEGER NOT NULL REFERENCES actors(id),
    context         TEXT,                    -- mail, chat, discussion, dream, soul, learning, etc.
    context_id      TEXT,                    -- mail UUID, discussion ID, etc.
    provider        TEXT NOT NULL,
    model           TEXT NOT NULL,
    system_prompt   TEXT,                    -- static portion of system prompt
    user_message    TEXT,                    -- user/input message
    response        TEXT,                    -- full response text (or error message on failure)
    status          TEXT NOT NULL DEFAULT 'success',  -- success, error
    status_code     INTEGER,                -- HTTP status from provider (if available)
    error_message   TEXT,                    -- error details on failure
    input_tokens    INTEGER DEFAULT 0,
    output_tokens   INTEGER DEFAULT 0,
    cache_read_tokens   INTEGER DEFAULT 0,
    cache_write_tokens  INTEGER DEFAULT 0,
    cost            NUMERIC(10,6) DEFAULT 0,
    duration_ms     INTEGER,                -- wall-clock time for the provider call
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Index for querying by agent and time range (most common access pattern)
CREATE INDEX idx_va_calls_actor_created ON virtual_agent_calls (actor_id, created_at DESC);

-- Index for filtering by context type
CREATE INDEX idx_va_calls_context ON virtual_agent_calls (context);

-- Index for finding failures
CREATE INDEX idx_va_calls_status ON virtual_agent_calls (status) WHERE status != 'success';
