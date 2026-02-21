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

# Pull latest if git repo exists
echo -e "\033[1m[1/3] Updating code...\033[0m"
cd /opt/llm-memory-api
git pull

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
