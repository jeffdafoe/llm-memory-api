-- MEM-111 rollback
DELETE FROM config WHERE key IN ('search_decay_cleanup_threshold', 'decay_cleanup_cron_schedule', 'decay_cleanup_enabled');
UPDATE config SET value = '0' WHERE key = 'search_decay_halflife_conversation';
