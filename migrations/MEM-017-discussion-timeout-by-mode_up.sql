-- MEM-017: Mode-specific discussion wait timeouts
-- Realtime discussions timeout after 5 minutes, async after 24 hours.

DELETE FROM config WHERE key = 'discussion_wait_timeout';

INSERT INTO config (key, value, description) VALUES
    ('discussion_wait_timeout_realtime', '5', 'Minutes to wait for participants in realtime discussions'),
    ('discussion_wait_timeout_async', '1440', 'Minutes to wait for participants in async discussions');
