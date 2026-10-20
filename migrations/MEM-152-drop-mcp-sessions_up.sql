-- MEM-152: drop mcp_sessions (LLM-731).
--
-- /mcp moved to the v2 MCP SDK, which serves both protocol eras statelessly:
-- no Mcp-Session-Id is issued, so there is nothing to persist or rehydrate
-- after a restart. MEM-023 created this table for that rehydration.

DROP TABLE IF EXISTS mcp_sessions;
