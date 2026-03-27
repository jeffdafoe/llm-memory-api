-- MEM-076 down: Remove note sharing permissions + actor visibility

DROP TABLE IF EXISTS note_permissions;
ALTER TABLE actors DROP COLUMN IF EXISTS visible_to_others;
