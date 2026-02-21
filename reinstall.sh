#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

echo -e "\033[1;33m==================================="
echo "       Memory API Reinstall"
echo -e "===================================\033[0m"
echo
echo -e "\033[1;33mPress Enter to keep existing values, or type a new value.\033[0m"
echo

cd "$SCRIPT_DIR/infrastructure"
export ANSIBLE_CONFIG="$SCRIPT_DIR/infrastructure/ansible.cfg"
ansible-playbook -i inventory/production.yml playbooks/setup.yml --extra-vars "reconfigure=true"
ansible-playbook -i inventory/production.yml playbooks/deploy.yml
