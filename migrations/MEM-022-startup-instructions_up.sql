-- MEM-022: Add startup_instructions column to agents table.
-- Stores per-agent instructions that MCP tools can read/write.
-- Used by non-technical agents (e.g., Wendy on claude.ai) to persist
-- their own bootstrap instructions without needing the notes system.

ALTER TABLE agents ADD COLUMN startup_instructions TEXT;
