-- MEM-054 rollback: Remove _configVersion from all agent configurations.

UPDATE agents
SET configuration = (configuration::jsonb - '_configVersion')::text
WHERE configuration IS NOT NULL
  AND configuration::jsonb ? '_configVersion';
