-- MEM-032: Add response_length column to request_log
-- Tracks the size of HTTP response bodies alongside the existing request_length.

ALTER TABLE request_log ADD COLUMN response_length INTEGER;
