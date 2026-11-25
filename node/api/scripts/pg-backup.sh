#!/bin/bash
# pg-backup.sh — Daily PostgreSQL backup for all databases
#
# Dumps each database individually (compressed) to /var/backups/postgresql/.
# Retains backup_retention_days (memory-api config, default 7) days of
# backups, for every database on this server; older files are pruned.
#
# Must run as a user that can sudo to postgres (or as root).
# Cron entry (deployed by Ansible):
#   0 2 * * * /var/www/memory-api/scripts/pg-backup.sh

set -e

BACKUP_DIR="/var/backups/postgresql"

# The retention period is the backup_retention_days row of the memory-api
# config table, read with the app's own DATABASE_URL. A missing env file or
# a failed read falls back to 7 and never stops the backup.
ENV_FILE="/etc/memory-api/env"
DATABASE_URL=""
if [ -f "$ENV_FILE" ]; then
    DATABASE_URL=$(grep '^DATABASE_URL=' "$ENV_FILE" | cut -d= -f2-) || DATABASE_URL=""
fi
export PGCONNECT_TIMEOUT=10

# read_days KEY: the KEY row of the config table if it is a whole number of
# days from 1 to 999999; else 7. The read is optional: it is bounded by a
# 15-second wall clock (a stalled connection or a lock must not hold up the
# job), never prompts for a password, and any failure gives 7. The privacy
# page (LLM-1021) states the same period by the same rule.
read_days() {
    local value
    if value=$(timeout 15 psql "$DATABASE_URL" --no-psqlrc -tAq -w -c "SELECT value FROM config WHERE key = '$1'" 2>/dev/null); then
        # Trim leading and trailing whitespace only; inner spaces stay and
        # make the value invalid.
        value="${value#"${value%%[![:space:]]*}"}"
        value="${value%"${value##*[![:space:]]}"}"
    else
        value=""
    fi
    # 10# reads the digits as decimal, so "08" is 8 and not a bad octal.
    if [[ "$value" =~ ^[0-9]{1,6}$ ]] && [ "$((10#$value))" -ge 1 ]; then
        echo "$((10#$value))"
    else
        echo 7
    fi
}

RETENTION_DAYS=7
if [ -n "$DATABASE_URL" ]; then
    RETENTION_DAYS=$(read_days backup_retention_days)
fi
DATE=$(date +%Y%m%d)

# Ensure backup directory exists
mkdir -p "$BACKUP_DIR"

# Get list of non-template databases
DATABASES=$(sudo -u postgres psql -t -A -c "SELECT datname FROM pg_database WHERE datistemplate = false AND datname != 'postgres';")

for DB in $DATABASES; do
    OUTFILE="${BACKUP_DIR}/${DB}_${DATE}.sql.gz"
    sudo -u postgres pg_dump "$DB" | gzip > "$OUTFILE"
    chmod 600 "$OUTFILE"
done

# Prune backups older than retention period
find "$BACKUP_DIR" -name "*.sql.gz" -mtime +${RETENTION_DAYS} -delete

echo "pg-backup: completed at $(date -Iseconds) — $(echo "$DATABASES" | wc -w) databases backed up"
