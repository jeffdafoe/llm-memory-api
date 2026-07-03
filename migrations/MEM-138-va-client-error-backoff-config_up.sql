-- MEM-138 — seed the virtual_agent_retry_backoff_client_error config key.
--
-- LLM-237 added a config.get('virtual_agent_retry_backoff_client_error') read in
-- services/virtual-agent.js (retryWithBackoff, which runs on every virtual-agent
-- call) but shipped no migration to register the key. config.get() throws on an
-- unknown key, so every VA turn threw before it could call the model — all
-- virtual-agent processing (NPC turns + utility agents like code_review) was down
-- from the LLM-237 deploy until this key was seeded. The value matches the parse
-- fallback the code already passes at the call site.
--
-- ON CONFLICT DO NOTHING: the key was hot-seeded directly in prod to end the
-- outage, so this must be a no-op there while still seeding fresh databases.
INSERT INTO config (key, value, description) VALUES
    ('virtual_agent_retry_backoff_client_error', '5,15,60', 'Comma-separated backoff delays in seconds for deterministic 4xx client-error retries (LLM-237)')
ON CONFLICT (key) DO NOTHING;
