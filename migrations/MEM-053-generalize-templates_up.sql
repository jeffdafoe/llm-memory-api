-- MEM-053: Generalize welcome_templates into a general-purpose templates table.
-- Renames table, adds kind column, renames body -> content, folds subject into content as frontmatter.

-- Rename table
ALTER TABLE welcome_templates RENAME TO templates;

-- Add kind column with check constraint
ALTER TABLE templates ADD COLUMN kind VARCHAR(50) NOT NULL DEFAULT 'welcome';
ALTER TABLE templates ADD CONSTRAINT templates_kind_check CHECK (kind IN ('welcome'));

-- Fold subject into content as YAML frontmatter, then drop subject
UPDATE templates SET body = '---' || E'\n' || 'subject: ' || subject || E'\n' || '---' || E'\n\n' || body;
ALTER TABLE templates DROP COLUMN subject;

-- Rename body -> content
ALTER TABLE templates RENAME COLUMN body TO content;
