-- MEM-115: Rename decay_cleanup config keys to cleanup
UPDATE config SET key = 'cleanup_enabled' WHERE key = 'decay_cleanup_enabled';
UPDATE config SET key = 'cleanup_cron_schedule' WHERE key = 'decay_cleanup_cron_schedule';
UPDATE config SET key = 'cleanup_decay_threshold' WHERE key = 'search_decay_cleanup_threshold';
