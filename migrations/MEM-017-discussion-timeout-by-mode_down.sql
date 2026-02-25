-- MEM-017 rollback

DELETE FROM config WHERE key IN ('discussion_wait_timeout_realtime', 'discussion_wait_timeout_async');

INSERT INTO config (key, value, description) VALUES
    ('discussion_wait_timeout', '5', 'Minutes to wait for all participants before starting a discussion');
