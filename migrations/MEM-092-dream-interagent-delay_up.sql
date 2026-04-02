-- MEM-092: Add dream interagent delay config key.
-- Delay in milliseconds between processing each agent during dream runs.
-- Prevents hammering the provider with rapid sequential calls.

INSERT INTO config (key, value, description) VALUES
    ('dream_interagent_delay', '2000', 'Delay in milliseconds between processing each agent during dream runs. Prevents provider rate limiting on sequential calls. 0 to disable.');
