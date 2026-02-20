#!/bin/bash
set -e

echo -e "\033[1;36m==================================="
echo "  Memory API Installer"
echo -e "===================================\033[0m"
echo

# Check if running as root
if [ "$EUID" -ne 0 ]; then
    echo "Please run as root (sudo)"
    exit 1
fi

# Install dependencies
echo -e "\033[1m[1/4] Installing system dependencies...\033[0m"
apt update
apt install -y git ansible curl

# Clone repository
echo -e "\033[1m[2/4] Cloning repository...\033[0m"
if [ -d "/var/www/memory-api" ]; then
    echo "Directory /var/www/memory-api already exists. Pulling latest..."
    cd /var/www/memory-api
    git pull
else
    git clone https://github.com/jeffdafoe/llm-memory-api.git /var/www/memory-api
fi

# Run setup playbook
echo -e "\033[1m[3/4] Running setup...\033[0m"
cd /var/www/memory-api/infrastructure
export ANSIBLE_CONFIG=/var/www/memory-api/infrastructure/ansible.cfg
ansible-playbook -i inventory/production.yml playbooks/setup.yml

# Run deploy playbook
echo -e "\033[1m[4/4] Running deploy...\033[0m"
ansible-playbook -i inventory/production.yml playbooks/deploy.yml

echo ""
echo -e "\033[1;32m==================================="
echo "  Installation complete!"
echo -e "===================================\033[0m"
echo ""
