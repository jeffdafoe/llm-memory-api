-- MEM-077 down: Revert visible_to_others default to FALSE
ALTER TABLE actors ALTER COLUMN visible_to_others SET DEFAULT FALSE;
