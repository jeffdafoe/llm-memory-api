#!/bin/bash
# pg-backup.sh — Daily PostgreSQL backup for all databases
#
# Dumps each database individually (compressed) to /var/backups/postgresql/.
# Retains 7 days of backups, older files are pruned automatically.
#
# Must run as a user that can sudo to postgres (or as root).
# Cron entry (deployed by Ansible):
#   0 2 * * * /var/www/memory-api/scripts/pg-backup.sh

set -e

BACKUP_DIR="/var/backups/postgresql"
RETENTION_DAYS=7
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
