-- MEM-140 down: drop the actors.email column (the partial index drops with it).

ALTER TABLE actors DROP COLUMN email;
