-- MEM-045: Virtual agent self-learning via post-interaction extraction
-- Config entries for enabling extraction and setting minimum token threshold.

INSERT INTO config (key, value) VALUES ('virtual_agent_learning_enabled', 'true');
INSERT INTO config (key, value) VALUES ('virtual_agent_learning_min_tokens', '500');
