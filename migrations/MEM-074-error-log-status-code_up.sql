-- Add status_code column to error_log so the dashboard can distinguish
-- 4xx (expected client errors) from 5xx (genuine server errors).
ALTER TABLE error_log ADD COLUMN status_code INTEGER;
