#!/bin/bash
set -e

# Usage:
#   First time:  curl -sSL https://raw.githubusercontent.com/jeffdafoe/llm-memory-api/main/install.sh -o /tmp/install.sh && sudo bash /tmp/install.sh
#   Re-install:  sudo bash /opt/llm-memory-api/install.sh
#   Deploy only: sudo bash /opt/llm-memory-api/deploy.sh

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
if [ -d "/opt/llm-memory-api/.git" ]; then
    echo "Git repo exists. Pulling latest..."
    cd /opt/llm-memory-api
    git pull
elif [ -d "/opt/llm-memory-api" ]; then
    echo "Directory exists (no git). Skipping clone."
else
    git clone https://github.com/jeffdafoe/llm-memory-api.git /opt/llm-memory-api
fi

# Run setup playbook (will prompt for secrets on first run)
echo -e "\033[1m[3/4] Running setup...\033[0m"
cd /opt/llm-memory-api/infrastructure
export ANSIBLE_CONFIG=/opt/llm-memory-api/infrastructure/ansible.cfg
ansible-playbook -i inventory/production.yml playbooks/setup.yml

# Run deploy playbook
echo -e "\033[1m[4/4] Running deploy...\033[0m"
ansible-playbook -i inventory/production.yml playbooks/deploy.yml

echo ""
echo -e "\033[1;32m==================================="
echo "  Installation complete!"
echo -e "===================================\033[0m"
echo ""
echo "To deploy updates later, run:"
echo "  sudo bash /opt/llm-memory-api/deploy.sh"
echo ""
