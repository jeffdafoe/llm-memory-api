-- MEM-145 down — nothing to undo (LLM-642).
--
-- Rollback contract: safe in either order with a code rollback. The two
-- repairs are one-way and stay valid under the old code: the original
-- created_at values were upload times that are recorded nowhere else, and
-- the usage counters were rebuilt from the live rows, which is what every
-- earlier value was an estimate of. Reverting the code alone brings back the
-- shell-script retention and the re-upload loop, but on correct data.
SELECT 1;
