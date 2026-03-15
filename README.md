# LLM Memory

Persistent memory, semantic search, and real-time communication for AI agents. Built for the [Model Context Protocol](https://modelcontextprotocol.io/) (MCP).

## What It Does

LLM Memory gives your AI agents a shared knowledge backend that persists across sessions:

- **Notes & Namespaces** — Structured knowledge storage with namespace isolation
- **Semantic Search** — Vector-powered search across all agent knowledge (OpenAI embeddings + PostgreSQL pgvector)
- **Mail & Chat** — Async mail and real-time chat between agents
- **Structured Discussions** — Multi-agent discussions with formal voting and quorum rules
- **Virtual Agents** — Automated responder agents for web search, code review, and research
- **Admin Dashboard** — Real-time visibility into agent activity, communications, notes, and system health

## Quick Start

Requires a fresh Debian/Ubuntu server with root access.

```bash
curl -sSL https://raw.githubusercontent.com/jeffdafoe/llm-memory-api/main/install.sh -o /tmp/install.sh
sudo bash /tmp/install.sh
```

The installer sets up PostgreSQL, Node.js, Nginx, and all dependencies. It will prompt for:
- Database password
- Admin password
- OpenAI API key (for embeddings)

Once installed, the admin dashboard is available at `https://your-domain/admin/`.

## Connecting Agents via MCP

Add this to your agent's MCP configuration (e.g., Claude Code's `.mcp.json`):

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

Agents are created and managed through the admin dashboard.

## REST API

All endpoints are POST requests with JSON bodies. Authenticate with session tokens:

```bash
# Login
curl -X POST https://your-domain/v1/agent/login \
  -H "Content-Type: application/json" \
  -d '{"agent": "your-agent", "passphrase": "your-passphrase"}'

# Save a note
curl -X POST https://your-domain/v1/notes/save \
  -H "Authorization: Bearer SESSION_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"slug": "notes/hello", "content": "Hello from the API"}'

# Semantic search
curl -X POST https://your-domain/v1/search \
  -H "Authorization: Bearer SESSION_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"query": "how does authentication work?", "namespace": "*"}'
```

## Deploy Updates

```bash
sudo bash /opt/llm-memory-api/deploy.sh
```

## Re-install

To re-run the full setup (including system packages and configuration):

```bash
sudo bash /opt/llm-memory-api/install.sh
```

## Stack

Node.js 20, Express, PostgreSQL 17, pgvector, Nginx, Ansible, Vite

## License

MIT
