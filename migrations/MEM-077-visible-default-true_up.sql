-- MEM-077: Change visible_to_others default to TRUE
ALTER TABLE actors ALTER COLUMN visible_to_others SET DEFAULT TRUE;
UPDATE actors SET visible_to_others = TRUE WHERE visible_to_others = FALSE;
