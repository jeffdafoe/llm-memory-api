-- Per-agent storage quota. NULL = use global default from config table.
ALTER TABLE agent_configuration ADD COLUMN storage_quota BIGINT DEFAULT NULL;

-- Global default quota (50MB). Agents without a per-agent override use this.
INSERT INTO config (key, value, description) VALUES
    ('default_storage_quota', '52428800', 'Default storage quota per namespace in bytes (50MB). Agents can override via storage_quota column.')
ON CONFLICT (key) DO NOTHING;
