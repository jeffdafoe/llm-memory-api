#!/bin/bash
set -e

# Usage:
#   sudo bash /opt/llm-memory-api/deploy.sh
#
#   Pulls main's head, or the release tag in memory_api_release
#   (infrastructure/local-secrets.yml), runs ansible deploy playbook.

echo -e "\033[1;36m==================================="
echo "  Memory API Deploy"
echo -e "===================================\033[0m"
echo

# Check if running as root
if [ "$EUID" -ne 0 ]; then
    echo "Please run as root (sudo)"
    exit 1
fi

# Ensure PostgreSQL is running (fix missing conf.d, stale PID, etc.)
if ! pg_lsclusters -h | grep -q 'online'; then
    echo -e "\033[1;33mPostgreSQL is not running — attempting recovery...\033[0m"
    # Fix missing conf.d directory (required by postgresql.conf include_dir)
    PG_CONF_DIR=$(pg_lsclusters -h | awk '{print "/etc/postgresql/" $1 "/" $2}')
    if [ -n "$PG_CONF_DIR" ] && [ ! -d "$PG_CONF_DIR/conf.d" ]; then
        echo "  Creating missing $PG_CONF_DIR/conf.d"
        mkdir -p "$PG_CONF_DIR/conf.d"
        chown postgres:postgres "$PG_CONF_DIR/conf.d"
    fi
    # Remove stale PID file if present
    PG_DATA=$(pg_lsclusters -h | awk '{print $6}')
    if [ -n "$PG_DATA" ] && [ -f "$PG_DATA/postmaster.pid" ]; then
        if ! pgrep -F "$PG_DATA/postmaster.pid" >/dev/null 2>&1; then
            echo "  Removing stale PID file"
            rm -f "$PG_DATA/postmaster.pid"
        fi
    fi
    # Start PostgreSQL
    PG_VER=$(pg_lsclusters -h | awk '{print $1}')
    PG_NAME=$(pg_lsclusters -h | awk '{print $2}')
    pg_ctlcluster "$PG_VER" "$PG_NAME" start
    sleep 2
    if pg_lsclusters -h | grep -q 'online'; then
        echo -e "\033[1;32m  PostgreSQL recovered.\033[0m"
    else
        echo -e "\033[1;31m  PostgreSQL failed to start. Check logs.\033[0m"
        tail -20 /var/log/postgresql/postgresql-*-*.log
        exit 1
    fi
fi

# The build to deploy: memory_api_release in local-secrets.yml (gitignored, so
# the reset below keeps it). Empty or absent means main's head; a tag such as
# server-v1.0.0 means that server release (group_vars/all.yml,
# roles/memory-api-go).
SECRETS=/opt/llm-memory-api/infrastructure/local-secrets.yml
RELEASE=""
if [ -f "$SECRETS" ]; then
    RELEASE=$(sed -n "s/^memory_api_release:[[:space:]]*[\"']\{0,1\}\([^\"']*\)[\"']\{0,1\}[[:space:]]*$/\1/p" "$SECRETS" | head -n 1)
fi
if [ -n "$RELEASE" ] && ! [[ "$RELEASE" =~ ^server-v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
    echo -e "\033[1;31mmemory_api_release in $SECRETS is '$RELEASE'; it must be empty or a server release tag such as server-v1.0.0\033[0m"
    exit 1
fi

# Fetch + reset to handle force pushes cleanly.
echo -e "\033[1m[1/3] Updating code...\033[0m"
cd /opt/llm-memory-api
if [ -n "$RELEASE" ]; then
    echo "Deploying release $RELEASE"
    git fetch --force origin "refs/tags/$RELEASE:refs/tags/$RELEASE"
    git reset --hard "refs/tags/$RELEASE^{commit}"
else
    git fetch origin
    git reset --hard origin/main
fi

# Run deploy playbook
echo -e "\033[1m[2/3] Running deploy...\033[0m"
cd /opt/llm-memory-api/infrastructure
export ANSIBLE_CONFIG=/opt/llm-memory-api/infrastructure/ansible.cfg
ansible-playbook -i inventory/production.yml playbooks/deploy.yml

# Restart the server to pick up the new binary.
SERVICE=memory-api-go.service
echo -e "\033[1m[3/3] Restarting $SERVICE...\033[0m"
systemctl restart "$SERVICE"
sleep 2
if systemctl is-active --quiet "$SERVICE"; then
    echo "$SERVICE is running"
else
    echo -e "\033[1;31mWARNING: $SERVICE failed to start!\033[0m"
    systemctl status "$SERVICE" --no-pager
    exit 1
fi

echo ""
echo -e "\033[1;32m==================================="
echo "  Deploy complete!"
echo -e "===================================\033[0m"
echo ""
