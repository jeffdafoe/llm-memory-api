# llm-memory-api

## Agent Identity

You are the **work** agent. Your namespace is `work`.

## At Session Start

1. Read your bootstrap document: use `read_note` MCP tool with `namespace="work"`, `slug="instructions/bootstrap"`
2. Follow the bootstrap instructions — it tells you what to read next and in what order

## After Compaction

When context is compressed mid-session, your bootstrap instructions are lost. Re-read your bootstrap immediately: `read_note` with `namespace="work"`, `slug="instructions/bootstrap"`. This prevents behavioral drift on critical rules.

## After Context Continuation

When a session starts from a continuation summary, treat operational details in the summary as potentially stale or wrong. Re-read your bootstrap immediately — same as after compaction. The summary preserves narrative but may carry forward incorrect credentials, tool prefixes, connection details, or failed approaches as if they were correct.

## Local Memory (MEMORY.md)

**Do not manually edit MEMORY.md.** It is generated from the remote note `instructions/memory` (namespace: work) at session start by the bootstrap process. To update persistent memory, edit the remote note instead.

## Code Style

- **Comment the code** — Jeff doesn't know Go or Node well, comments help him read it. Comment the *why* and non-obvious language and framework patterns (Go idioms such as context cancellation, goroutines, channels, defer and interfaces; the admin UI's Vue and Vite patterns) and architectural decisions — not the mechanics. He has 30 years of experience, so don't comment loops, conditionals, or anything self-evident.
- 4 spaces indentation
- No abbreviations (`infrastructure` not `infra`)
- No ternaries — use if/then
- All API routes are POST (no GET for anything that takes params)
- **PostgreSQL is the target database** — PG-specific syntax (`ON CONFLICT`, partial indexes, PG functions, `USING` in deletes, etc.) is fine.

## Architecture

- **API server**: `go/server/` — Go. Entry point `cmd/memory-api` (also the `staleness-check` and `create-user` subcommands); the HTTP handler chain is in `internal/server`, and each area has its own package under `internal/` (notes, mail, chat, discussion, admin, ...)
- **MCP endpoint**: `go/server/internal/mcp` — Streamable HTTP MCP endpoint, all agents connect here
- **Auth**: HMAC OAuth tokens (primary), session tokens and API keys. No JWTs. See `go/server/internal/auth`.
- **Admin UI**: `node/api/public/admin/` — Vue 3 composables; Vite compiles the templates at build time (`node/api/vite.config.js`), and CI publishes the build. nginx serves it. Globals in `core.js`, features in separate modules.
- **Scripts**: `node/api/scripts/` — the cron scripts the deploy copies (`db-cleanup.sh`, `pg-backup.sh`)
- **Migrations**: `migrations/` — raw SQL, sequential MEM-XXX numbering
- **Infrastructure**: `infrastructure/` — Ansible playbooks, roles, and templates for deployment
- **Discussion transport**: `go/client/cmd/discuss/` — Go binary for multi-agent discussions (replaces legacy `node/client/discuss.js`)

## Deployment

- Push to GitHub, SSH to VPS, run `sudo bash /opt/llm-memory-api/deploy.sh`
- Deploy script pulls code to `/opt/llm-memory-api`, installs the Go server binary and the admin UI build that CI published for that commit, and syncs the public files and cron scripts to `/var/www/memory-api`
- Restart is handled by the deploy script (checks service health after restart)
- Migrations run automatically during deploy via Ansible

## VPS

- Credentials stored in remote note: `read_note(namespace="work", slug="instructions/credentials/vps")`
- Service: `memory-api-go.service`, binary `/usr/local/bin/memory-api` (runs as `memory-api` system user)
- Cron: daily DB cleanup at 3:15 AM (`/var/www/memory-api/scripts/db-cleanup.sh`) — request_log, expired sessions, stale MCP sessions. Conversation retention (soft-delete at `conversation_retention_days` + 1, tombstone one window later) runs inside the server's own cleanup job (`go/server/internal/cleanup`, `cleanup_cron_schedule`, 05:00 UTC).
