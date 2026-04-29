-- MEM-125: Add dream_backload_count config.
-- When an agent's soul note is empty (deleted or first run), the soul-update
-- pass replaces the per-chunk snapshot input with the N most recent dream
-- notes from the agent's namespace, concatenated in reverse-date order.
-- Lets us delete and regenerate a soul without losing accumulated personality
-- (which would otherwise come back from a single day's chunk and slowly fill
-- in over many cycles).

INSERT INTO config (key, value, description) VALUES
    ('dream_backload_count', '7', 'Number of recent dreams to feed into soul synthesis when the existing soul is empty. Lets a deleted soul rebuild from accumulated dream history rather than a single chunk. 0 to disable backload. Capped at 20 in code regardless of value.');
