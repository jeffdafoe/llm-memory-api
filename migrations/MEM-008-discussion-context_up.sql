-- Add context column to discussions table
-- Allows the creator to attach background/context when creating a discussion.
-- Readable via discussion/status so the joining agent can bootstrap.
ALTER TABLE discussions ADD COLUMN context TEXT;
