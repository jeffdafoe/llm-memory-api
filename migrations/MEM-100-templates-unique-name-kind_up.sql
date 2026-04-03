-- MEM-100: Change templates unique constraint from (name) to (name, kind)
-- Allows the same name (e.g. "default") across different template kinds.

ALTER TABLE templates DROP CONSTRAINT welcome_templates_name_key;
ALTER TABLE templates ADD CONSTRAINT templates_name_kind_key UNIQUE (name, kind);

-- Fix the seed template that had to use a different name due to the old constraint
UPDATE templates SET name = 'default' WHERE name = 'default-getting-started' AND kind = 'welcome-note';
