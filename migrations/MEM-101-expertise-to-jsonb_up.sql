-- MEM-101: Convert actors.expertise from text to jsonb
-- Prevents invalid JSON from being stored. The column was text with a
-- default of '[]' and all API routes already JSON.stringify before writing,
-- but a direct insert or UI edge case produced invalid JSON ([memory-enrichment]
-- without quotes) which broke the dream cron's jsonb cast.

-- Convert the column type. Existing valid JSON text values cast cleanly.
ALTER TABLE actors ALTER COLUMN expertise TYPE jsonb USING expertise::jsonb;

-- Update the default to be a jsonb literal instead of a text literal.
ALTER TABLE actors ALTER COLUMN expertise SET DEFAULT '[]'::jsonb;
