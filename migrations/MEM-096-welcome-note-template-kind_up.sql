-- MEM-096: Add 'welcome-note' to templates kind check constraint
-- Allows templates that produce a getting-started note saved to the new agent's namespace

ALTER TABLE templates DROP CONSTRAINT templates_kind_check;
ALTER TABLE templates ADD CONSTRAINT templates_kind_check
    CHECK (kind IN ('welcome', 'welcome-note'));
