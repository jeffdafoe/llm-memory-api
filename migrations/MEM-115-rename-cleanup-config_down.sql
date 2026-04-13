-- MEM-115 rollback
UPDATE config SET key = 'decay_cleanup_enabled' WHERE key = 'cleanup_enabled';
UPDATE config SET key = 'decay_cleanup_cron_schedule' WHERE key = 'cleanup_cron_schedule';
UPDATE config SET key = 'search_decay_cleanup_threshold' WHERE key = 'cleanup_decay_threshold';
