#!/bin/bash
set -e

# Usage:
#   sudo bash /opt/llm-memory-api/deploy.sh
#
#   Pulls latest from GitHub (deploy key), runs ansible deploy playbook.

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

# Pull latest (fetch + reset to handle force pushes cleanly)
echo -e "\033[1m[1/3] Updating code...\033[0m"
cd /opt/llm-memory-api
git fetch origin
git reset --hard origin/main

# Run deploy playbook
echo -e "\033[1m[2/3] Running deploy...\033[0m"
cd /opt/llm-memory-api/infrastructure
export ANSIBLE_CONFIG=/opt/llm-memory-api/infrastructure/ansible.cfg
ansible-playbook -i inventory/production.yml playbooks/deploy.yml

# Restart service to pick up new code
echo -e "\033[1m[3/3] Restarting service...\033[0m"
systemctl restart memory-api.service
sleep 2
if systemctl is-active --quiet memory-api.service; then
    echo "memory-api.service is running"
else
    echo -e "\033[1;31mWARNING: memory-api.service failed to start!\033[0m"
    systemctl status memory-api.service --no-pager
    exit 1
fi

echo ""
echo -e "\033[1;32m==================================="
echo "  Deploy complete!"
echo -e "===================================\033[0m"
echo ""
