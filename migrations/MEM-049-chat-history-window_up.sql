-- MEM-049: Add config key for virtual agent direct chat history time window.
-- Controls how far back (in hours) to look for conversation context.

INSERT INTO config (key, value, description)
VALUES ('virtual_agent_chat_history_hours', '4', 'Hours of direct chat history to include as context for virtual agent responses');
