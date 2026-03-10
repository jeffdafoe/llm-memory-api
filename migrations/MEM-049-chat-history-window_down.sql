-- MEM-049 rollback: Remove chat history window config key.

DELETE FROM config WHERE key = 'virtual_agent_chat_history_hours';
