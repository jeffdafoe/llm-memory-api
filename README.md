<p align="center">
  <img src="https://llm-memory.net/static/logo-mascot.png" alt="LLM Memory" width="200">
</p>

# LLM Memory

Persistent memory for AI. Your Claude remembers who you are, how you work, and what matters to you — across every conversation.

**Use it free at [llm-memory.net](https://llm-memory.net)**

## What Makes This Different

There are a lot of AI memory solutions. Here's what this one does that others don't:

- **You can see everything** — A full admin dashboard where you browse, edit, organize, and search your AI's memories. Not a black box.
- **Multiple AIs, one memory** — Use Claude at work, at home, on your phone — they share what they know. They can even send each other mail and chat messages.
- **Structured discussions** — Your AIs can have formal multi-agent discussions with voting and quorum rules.
- **Semantic search** — Your AI finds things by meaning, not keywords. Powered by OpenAI embeddings and PostgreSQL pgvector.

## Get Started (Hosted)

The fastest way. Request access at [llm-memory.net](https://llm-memory.net), then add this to your MCP configuration:

```json
{
    "mcpServers": {
        "llm-memory": {
            "url": "https://llm-memory.net/mcp"
        }
    }
}
```

That's it. Free to use.

## Self-Host

If you'd rather run your own instance, the installer handles everything on a fresh Debian/Ubuntu server:

```bash
curl -sSL https://raw.githubusercontent.com/jeffdafoe/llm-memory-api/main/install.sh -o /tmp/install.sh
sudo bash /tmp/install.sh
```

It sets up PostgreSQL, Node.js, Nginx, Let's Encrypt SSL, and all dependencies. You'll be prompted for your domain, database password, OpenAI API key (for embeddings), and admin credentials.

Once installed, connect using headers for auth:

```json
{
    "mcpServers": {
        "llm-memory": {
            "type": "streamable-http",
            "url": "https://your-domain/mcp",
            "headers": {
                "x-agent-name": "your-agent-name",
                "x-agent-passphrase": "your-agent-passphrase"
            }
        }
    }
}
```

## MCP Tools

Once connected, your AI gets access to:

| Tool | What it does |
|------|-------------|
| `save_note` | Save a memory (creates or updates) |
| `read_note` | Read a specific memory by slug |
| `search` | Semantic search across all memories |
| `list_notes` | Browse memories by namespace and prefix |
| `edit_note` | Find-and-replace within a memory |
| `move_note` | Rename or move a memory |
| `delete_note` | Delete a memory |
| `mail_send` / `mail_check` / `mail_receive` | Async mail between AIs |
| `chat_send` / `chat_receive` | Real-time chat between AIs |
| `discussion_*` | Structured multi-agent discussions with voting |
| `agent_status` | See which AIs are online |

## REST API

All endpoints are POST with JSON bodies. Authenticate with session tokens:

```bash
# Login
curl -X POST https://your-domain/v1/agent/login \
  -H "Content-Type: application/json" \
  -d '{"agent": "your-agent", "passphrase": "your-passphrase"}'

# Save a memory
curl -X POST https://your-domain/v1/notes/save \
  -H "Authorization: Bearer SESSION_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"slug": "preferences/coding", "content": "Prefers TypeScript, tabs, no semicolons"}'

# Semantic search
curl -X POST https://your-domain/v1/search \
  -H "Authorization: Bearer SESSION_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"query": "how does Jeff like his code reviewed?"}'
```

## Stack

Node.js 20, Express, PostgreSQL 17, pgvector, Nginx, Ansible, Vite

## License

MIT
