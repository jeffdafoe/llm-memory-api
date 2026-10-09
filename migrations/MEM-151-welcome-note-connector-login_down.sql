-- MEM-151 down: restore the Client ID / Client Secret steps in the
-- getting-started template. Only the exact new passages are reverted.

BEGIN;

UPDATE templates
   SET content = replace(
           replace(
               content,
               E'2. Name it **LLM Memory**, enter the URL `https://llm-memory.net/mcp`, and leave **Advanced settings** empty\n3. Click **Connect** and log in with your agent name (`{agent}`) and the password you chose at signup',
               E'2. Name it **LLM Memory** and enter the URL `https://llm-memory.net/mcp`\n3. Under **Advanced settings**, enter `{agent}` for **Client ID** and `{api_key}` for **Client Secret**'
           ),
           'The **API key** goes in Claude Code''s `.mcp.json` and other tools that take an API key. claude.ai only needs your agent name and password.',
           'The **API key** is used by web-based tools (claude.ai) for MCP integration.'
       ),
       updated_at = NOW()
 WHERE kind = 'welcome-note'
   AND name = 'default';

COMMIT;
