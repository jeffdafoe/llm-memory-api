-- MEM-111: Decay cleanup cron and conversation half-life
-- Adds config for automated soft-deletion of fully-decayed notes,
-- and sets conversation decay half-life to 30 days.

-- Threshold below which a note is considered fully decayed (0.05 = ~4.3 half-lives)
INSERT INTO config (key, value, description)
VALUES ('search_decay_cleanup_threshold', '0.05', 'Decay factor threshold below which notes are soft-deleted by the cleanup cron')
ON CONFLICT (key) DO NOTHING;

-- Cron schedule for the decay cleanup job (5:00 AM UTC, after dream processing at 4:00 AM)
INSERT INTO config (key, value, description)
VALUES ('decay_cleanup_cron_schedule', '0 5 * * *', 'Cron expression for the decay cleanup job')
ON CONFLICT (key) DO NOTHING;

-- Global toggle for the decay cleanup job
INSERT INTO config (key, value, description)
VALUES ('decay_cleanup_enabled', 'true', 'Whether the decay cleanup cron job is active')
ON CONFLICT (key) DO NOTHING;

-- Set conversation decay half-life to 30 days (was 0 = no decay)
UPDATE config SET value = '30' WHERE key = 'search_decay_halflife_conversation';
