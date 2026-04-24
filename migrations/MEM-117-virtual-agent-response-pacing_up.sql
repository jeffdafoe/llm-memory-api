-- MEM-117: Virtual agent discussion response pacing.
-- Two knobs to slow how quickly VAs respond in discussions so the conversation
-- feels unhurried and doesn't fire in bursts when multiple humans talk at once.

INSERT INTO config (key, value, description) VALUES
    ('virtual_agent_response_delay_seconds',    '5', 'Base delay before a virtual agent starts generating a discussion response (seconds)'),
    ('virtual_agent_response_stagger_seconds',  '5', 'Additional per-agent offset within a single response wave (seconds)');
