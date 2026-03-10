-- MEM-047: Backfill descriptions for config entries.
-- The description column already exists (MEM-013) but most rows were inserted without one.

UPDATE config SET description = 'Minutes to wait for all participants before starting a discussion' WHERE key = 'discussion_wait_timeout' AND description IS NULL;
UPDATE config SET description = 'Minutes before an async discussion times out' WHERE key = 'discussion_async_timeout' AND description IS NULL;
UPDATE config SET description = 'Minutes before a realtime discussion times out' WHERE key = 'discussion_realtime_timeout' AND description IS NULL;
UPDATE config SET description = 'Minutes a deferred invitation stays valid before timing out' WHERE key = 'discussion_defer_timeout' AND description IS NULL;
UPDATE config SET description = 'Maximum number of times a participant can defer an invitation' WHERE key = 'max_defer_count' AND description IS NULL;
UPDATE config SET description = 'AES-256-GCM key for encrypting virtual agent API keys (hex)' WHERE key = 'virtual_agent_encryption_key' AND description IS NULL;
UPDATE config SET description = 'Max API calls per agent within the rate window' WHERE key = 'virtual_agent_rate_limit' AND description IS NULL;
UPDATE config SET description = 'Rate limit sliding window duration (seconds)' WHERE key = 'virtual_agent_rate_window_seconds' AND description IS NULL;
UPDATE config SET description = 'Cooldown period after hitting rate limit (seconds)' WHERE key = 'virtual_agent_cooldown_seconds' AND description IS NULL;
UPDATE config SET description = 'Default token budget per agent per reset cycle' WHERE key = 'virtual_agent_default_token_budget' AND description IS NULL;
UPDATE config SET description = 'Days between automatic token budget resets' WHERE key = 'virtual_agent_budget_reset_days' AND description IS NULL;
UPDATE config SET description = 'Global toggle for post-interaction learning extraction' WHERE key = 'virtual_agent_learning_enabled' AND description IS NULL;
UPDATE config SET description = 'Minimum response tokens before learning extraction triggers' WHERE key = 'virtual_agent_learning_min_tokens' AND description IS NULL;
