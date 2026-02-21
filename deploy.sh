#!/bin/bash
set -e

echo -e "\033[1;36m==================================="
echo "  Memory API Deploy"
echo -e "===================================\033[0m"
echo

# Check if running as root
if [ "$EUID" -ne 0 ]; then
    echo "Please run as root (sudo)"
    exit 1
fi

# Pull latest
echo -e "\033[1m[1/2] Pulling latest code...\033[0m"
cd /opt/llm-memory-api
git pull

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
