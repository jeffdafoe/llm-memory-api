-- MEM-015 rollback: Remove system agent
-- Also removes any messages sent by system agent

DELETE FROM chat_messages WHERE from_agent = 'system';
DELETE FROM mail WHERE from_agent = 'system';
DELETE FROM agents WHERE agent = 'system';
