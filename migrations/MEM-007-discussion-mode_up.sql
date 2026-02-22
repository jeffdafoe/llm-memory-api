-- Add mode column to discussions table
-- Supports "realtime" (transport + subagent, live back-and-forth) and "async" (independent investigation + direct voting)
ALTER TABLE discussions ADD COLUMN mode VARCHAR(20) NOT NULL DEFAULT 'realtime';
