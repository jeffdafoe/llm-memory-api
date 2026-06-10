-- MEM-137 down — remove the per-agent dream_source column.
-- Dropping the column also drops its CHECK constraint.

ALTER TABLE agent_configuration
    DROP COLUMN dream_source;
