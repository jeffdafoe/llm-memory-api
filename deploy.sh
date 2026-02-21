#!/bin/bash
set -e

# Usage:
#   On VPS:    sudo bash /opt/llm-memory-api/deploy.sh
#   From local: sudo bash /opt/llm-memory-api/deploy.sh --push   (syncs code to VPS first)
#
#   For private repos without git on VPS, push code first:
#     scp -r /c/dev/llm-memory-api/* user@host:/opt/llm-memory-api/

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
echo -e "\033[1m[1/2] Updating code...\033[0m"
if [ -d "/opt/llm-memory-api/.git" ]; then
    cd /opt/llm-memory-api
    git pull
else
    echo "No git repo (private repo deployment). Assuming code is already synced."
fi

# Run deploy playbook
echo -e "\033[1m[2/2] Running deploy...\033[0m"
cd /opt/llm-memory-api/infrastructure
export ANSIBLE_CONFIG=/opt/llm-memory-api/infrastructure/ansible.cfg
ansible-playbook -i inventory/production.yml playbooks/deploy.yml

echo ""
echo -e "\033[1;32m==================================="
echo "  Deploy complete!"
echo -e "===================================\033[0m"
echo ""
