#!/bin/bash
# db-cleanup.sh — Periodic database maintenance for llm-memory-api
#
# Cleans up:
#   1. request_log entries older than 7 days
#   2. Expired sessions (agent + web)
#   3. Stale MCP sessions older than 7 days
#
# Designed to run as a daily cron job. Reads DATABASE_URL from the app's
# env file so it uses the same credentials as the running service.

set -e

ENV_FILE="/etc/memory-api/env"

if [ ! -f "$ENV_FILE" ]; then
    echo "db-cleanup: env file not found at $ENV_FILE" >&2
    exit 1
fi

# Source DATABASE_URL from the app env file
DATABASE_URL=$(grep '^DATABASE_URL=' "$ENV_FILE" | cut -d= -f2-)

if [ -z "$DATABASE_URL" ]; then
    echo "db-cleanup: DATABASE_URL not found in $ENV_FILE" >&2
    exit 1
fi

export PGCONNECT_TIMEOUT=10

# Run all cleanup queries in a single psql invocation
psql "$DATABASE_URL" --no-psqlrc -q <<'SQL'
-- 1. Purge request_log entries older than 7 days
DELETE FROM request_log WHERE timestamp < NOW() - INTERVAL '7 days';

-- 2. Delete expired sessions (unified table covers both agent and web sessions)
DELETE FROM sessions WHERE expires_at < NOW();

-- 4. Delete stale MCP sessions older than 7 days
--    (MCP sessions have no expiry — they auto-rehydrate on demand,
--     so old rows are just orphaned debris from past connections)
DELETE FROM mcp_sessions WHERE created_at < NOW() - INTERVAL '7 days';
SQL

echo "db-cleanup: completed at $(date -Iseconds)"
