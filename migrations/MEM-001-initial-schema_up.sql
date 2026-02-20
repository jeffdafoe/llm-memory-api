-- MEM-001: Initial schema — chunks, chat_messages, mail

CREATE TABLE IF NOT EXISTS chunks (
    id SERIAL PRIMARY KEY,
    namespace VARCHAR(50) NOT NULL,
    source_file VARCHAR(500) NOT NULL,
    heading VARCHAR(500),
    chunk_text TEXT NOT NULL,
    embedding vector(1536),
    ingested_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_chunks_namespace_source
    ON chunks (namespace, source_file);

CREATE INDEX IF NOT EXISTS idx_chunks_embedding
    ON chunks USING ivfflat (embedding vector_cosine_ops)
    WITH (lists = 100);

CREATE TABLE IF NOT EXISTS chat_messages (
    id SERIAL PRIMARY KEY,
    channel VARCHAR(100) NOT NULL,
    from_namespace VARCHAR(50) NOT NULL,
    message TEXT NOT NULL,
    sent_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_chat_messages_channel
    ON chat_messages (channel, id);

CREATE TABLE IF NOT EXISTS mail (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    from_namespace VARCHAR(50) NOT NULL,
    to_namespace VARCHAR(50) NOT NULL,
    subject VARCHAR(500) NOT NULL,
    body TEXT NOT NULL,
    sent_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    acked_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_mail_to_namespace_acked
    ON mail (to_namespace, acked_at);
