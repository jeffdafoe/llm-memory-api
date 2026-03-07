ALTER TABLE discussions DROP CONSTRAINT IF EXISTS chk_discussions_outcome;
ALTER TABLE discussions DROP COLUMN IF EXISTS outcome;
