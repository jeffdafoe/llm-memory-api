-- MEM-099: Change visible_to_others default to false
-- New agents should be private by default

ALTER TABLE actors ALTER COLUMN visible_to_others SET DEFAULT false;
