-- MEM-118: Add dream interchunk delay config key.
-- Per-day chunking inside runDream means an agent that's fallen behind
-- triggers multiple sequential dream calls (one per missed UTC day) in
-- one cron run. This delay sits between those chunks for the same agent
-- to avoid hammering the provider when catching up.

INSERT INTO config (key, value, description) VALUES
    ('dream_interchunk_delay', '1000', 'Delay in milliseconds between per-day chunks within a single agent''s dream run. Lighter than dream_interagent_delay since it''s the same agent + provider. 0 to disable.');
