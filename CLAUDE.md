# llm-memory-api

## Code Style

- **Comment the code** — Jeff doesn't know Node well, comments help him read it. Comment the *why* and non-obvious framework patterns (Express middleware, Node idioms, architectural decisions) — not the mechanics. He has 30 years of experience, so don't comment loops, conditionals, or anything self-evident.
- 4 spaces indentation
- No abbreviations (`infrastructure` not `infra`)
- No ternaries — use if/then
- All API routes are POST (no GET for anything that takes params)
- **No PG-specific SQL** — keep queries portable. No `ON CONFLICT`, no partial indexes, no PG-only functions.

## Architecture

- **API server**: `node/api/src/` — Express app, routes in `routes/`, middleware in `middleware/`
- **MCP endpoint**: `node/api/src/routes/mcp.js` — Streamable HTTP MCP endpoint, all agents connect here
- **Auth**: HMAC OAuth tokens (primary) and API keys. No JWTs. See `middleware/mcp-auth.js`.
- **Admin UI**: `node/api/public/admin/` — Vue 3 composables, no build step. Globals in `core.js`, features in separate modules.
- **Scripts**: `node/api/scripts/` — operational scripts deployed with the app (e.g., `db-cleanup.sh` for cron)
- **Migrations**: `migrations/` — raw SQL, sequential MEM-XXX numbering
- **Infrastructure**: `infrastructure/` — Ansible playbooks, roles, and templates for deployment
- **Discussion transport**: `node/client/discuss.js` — Node CLI for multi-agent discussions

## Deployment

- Push to GitHub, SSH to VPS, run `sudo bash /opt/llm-memory-api/deploy.sh`
- Deploy script pulls code to `/opt/llm-memory-api`, syncs app code to `/var/www/memory-api`
- Restart is handled by the deploy script (checks service health after restart)
- Migrations run automatically during deploy via Ansible

## VPS

- SSH: `ssh claude@165.245.142.212 'command'`
- Sudo: `echo ***REDACTED*** | sudo -S command 2>/dev/null`
- Database: PostgreSQL 17, database `memory_api`, user `memory_api`
- Service: `memory-api.service` (runs as `memory-api` system user)
- Cron: daily DB cleanup at 3:15 AM (`/var/www/memory-api/scripts/db-cleanup.sh`)
