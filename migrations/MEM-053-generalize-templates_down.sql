-- MEM-053 rollback: Restore welcome_templates table structure.

-- Rename content -> body
ALTER TABLE templates RENAME COLUMN content TO body;

-- Extract subject from frontmatter and restore the column
ALTER TABLE templates ADD COLUMN subject VARCHAR(255);
UPDATE templates SET subject = regexp_replace(body, E'^---\\n.*?subject:\\s*(.+?)\\n.*?---\\n\\n?', E'\\1');
UPDATE templates SET body = regexp_replace(body, E'^---\\n.*?---\\n\\n?', '');
ALTER TABLE templates ALTER COLUMN subject SET NOT NULL;

-- Drop kind column
ALTER TABLE templates DROP CONSTRAINT templates_kind_check;
ALTER TABLE templates DROP COLUMN kind;

-- Rename table back
ALTER TABLE templates RENAME TO welcome_templates;
