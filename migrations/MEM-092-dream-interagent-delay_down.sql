-- MEM-092 rollback: Remove dream interagent delay config.

DELETE FROM config WHERE key = 'dream_interagent_delay';
