#!/bin/bash
# db-cleanup.sh — Periodic database maintenance for llm-memory-api
#
# Cleans up:
#   1. request_log entries older than 7 days
#   2. Expired sessions (agent + web)
#   3. Stale MCP sessions older than 7 days
#   4. Old conversation notes (soft-delete at retention+1 days, hard-delete at 2x(retention+1) days)
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

-- 5. Soft-delete conversation notes older than retention+1 days.
--    Reads conversation_retention_days from the config table (default 30).
--    Sets deleted_at so they drop out of search results immediately.
UPDATE documents
SET deleted_at = NOW()
WHERE kind = 'conversation' AND deleted_at IS NULL
  AND created_at < NOW() - (
      (SELECT CAST(value AS INTEGER) FROM config WHERE key = 'conversation_retention_days') + 1
  ) * INTERVAL '1 day';
SQL

# 6. Hard-delete conversation notes that were soft-deleted more than retention+1 days ago.
#    This is a separate psql call wrapped in a transaction so chunks and documents
#    are removed atomically.
psql "$DATABASE_URL" --no-psqlrc -q <<'SQL'
BEGIN;

-- Remove vector chunks for conversations past the hard-delete window
DELETE FROM memory_chunks mc
USING documents d
WHERE d.kind = 'conversation' AND d.deleted_at IS NOT NULL
  AND d.deleted_at < NOW() - (
      (SELECT CAST(value AS INTEGER) FROM config WHERE key = 'conversation_retention_days') + 1
  ) * INTERVAL '1 day'
  AND mc.namespace = d.namespace AND mc.source_file = d.slug;

-- Remove the document rows themselves
DELETE FROM documents
WHERE kind = 'conversation' AND deleted_at IS NOT NULL
  AND deleted_at < NOW() - (
      (SELECT CAST(value AS INTEGER) FROM config WHERE key = 'conversation_retention_days') + 1
  ) * INTERVAL '1 day';

COMMIT;
SQL

echo "db-cleanup: completed at $(date -Iseconds)"
