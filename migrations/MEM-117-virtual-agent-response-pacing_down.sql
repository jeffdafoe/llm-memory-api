-- MEM-117 rollback: remove VA discussion response pacing config.

DELETE FROM config WHERE key IN (
    'virtual_agent_response_delay_seconds',
    'virtual_agent_response_stagger_seconds'
);
