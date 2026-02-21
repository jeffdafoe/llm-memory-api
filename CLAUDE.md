# llm-memory-api

## Code Style

- **Comment the code** — Jeff doesn't know Node well, comments help him read it. Comment the *why* and non-obvious framework patterns (Express middleware, Node idioms, architectural decisions) — not the mechanics. He has 30 years of experience, so don't comment loops, conditionals, or anything self-evident.
- 4 spaces indentation
- No abbreviations (`infrastructure` not `infra`)
- No ternaries — use if/then
- All API routes are POST (no GET for anything that takes params)

## Architecture

- **API server**: `node/api/src/` — Express app, routes in `routes/`, middleware in `middleware/`
- **MCP server**: `node/mcp/server.js` — stdio MCP server, talks to the API
- **Migrations**: `migrations/` — raw SQL, sequential MEM-XXX numbering
- **Infrastructure**: `infrastructure/` — Ansible playbooks for deployment

## Deployment

- Push to GitHub, SSH to VPS, run `sudo bash /opt/llm-memory-api/deploy.sh`
- Deploy script pulls code to `/opt/llm-memory-api`, syncs app code to `/var/www/memory-api`
- Restart service after deploy: `sudo systemctl restart memory-api`
- Migrations are manual: run SQL against `llm_memory` database on VPS

## VPS

- SSH: `ssh claude@165.245.142.212 'command'`
- Sudo: `echo ***REDACTED*** | sudo -S command 2>/dev/null`
- Database: PostgreSQL 17, database `llm_memory`
- Service: `memory-api.service` (runs as jeff)
