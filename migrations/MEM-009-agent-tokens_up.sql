-- MEM-009: Agent registration tokens
-- Adds token_hash, token_salt, status columns to agents table for per-agent auth

ALTER TABLE agents ADD COLUMN token_hash VARCHAR(128);
ALTER TABLE agents ADD COLUMN token_salt VARCHAR(64);
ALTER TABLE agents ADD COLUMN status VARCHAR(20) NOT NULL DEFAULT 'active';
