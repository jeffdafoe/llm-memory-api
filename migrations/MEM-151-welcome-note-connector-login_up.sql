-- MEM-151: the getting-started note's claude.ai steps no longer ask for a
-- Client ID and Client Secret (LLM-733).
--
-- The claude.ai connector can now be added with only the URL: claude.ai
-- identifies itself (CIMD) or registers itself (DCR), and the user logs in on
-- /authorize with agent name + password. The welcome-note template (the
-- instructions/getting-started note every signup gets) still told users to
-- paste the agent name as Client ID and the API key as Client Secret. This
-- patches the two passages in place.
--
-- The live template has been edited by hand in the admin UI, so only the exact
-- passages are replaced, and the migration fails if either is not there word
-- for word (the MEM-149 pattern). Notes already saved into existing accounts
-- are left alone: the old Client ID / Secret steps still work for them.

BEGIN;

DO $MIGRATION$
DECLARE
    old_steps CONSTANT text := E'2. Name it **LLM Memory** and enter the URL `https://llm-memory.net/mcp`\n3. Under **Advanced settings**, enter `{agent}` for **Client ID** and `{api_key}` for **Client Secret**';
    new_steps CONSTANT text := E'2. Name it **LLM Memory**, enter the URL `https://llm-memory.net/mcp`, and leave **Advanced settings** empty\n3. Click **Connect** and log in with your agent name (`{agent}`) and the password you chose at signup';
    old_key_line CONSTANT text := 'The **API key** is used by web-based tools (claude.ai) for MCP integration.';
    new_key_line CONSTANT text := 'The **API key** goes in Claude Code''s `.mcp.json` and other tools that take an API key. claude.ai only needs your agent name and password.';
    updated integer;
BEGIN
    UPDATE templates
       SET content = replace(replace(content, old_steps, new_steps), old_key_line, new_key_line),
           updated_at = NOW()
     WHERE kind = 'welcome-note'
       AND name = 'default'
       AND position(old_steps IN content) > 0
       AND position(old_key_line IN content) > 0;
    GET DIAGNOSTICS updated = ROW_COUNT;
    IF updated <> 1 THEN
        RAISE EXCEPTION 'MEM-151: expected 1 default welcome-note template with both claude.ai passages, found %', updated;
    END IF;
END
$MIGRATION$;

COMMIT;
