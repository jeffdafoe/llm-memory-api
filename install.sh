#!/bin/bash
set -e

# Usage:
#   First time:  curl -sSL https://raw.githubusercontent.com/jeffdafoe/llm-memory-api/main/install.sh -o /tmp/install.sh && sudo bash /tmp/install.sh [version]
#   Re-install:  sudo bash /opt/llm-memory-api/install.sh [version]
#   Deploy only: sudo bash /opt/llm-memory-api/deploy.sh
#
# [version] is a server release tag such as server-v1.0.0 (see the
# repository's Releases; the v* releases are the client tools).
# The install then uses that release's code and build, and later deploys stay
# on it (memory_api_release in infrastructure/local-secrets.yml). Without it, a
# re-install keeps the release already set there, and a first install follows
# main. To follow main again, set memory_api_release: "" and run deploy.sh.

SECRETS=/opt/llm-memory-api/infrastructure/local-secrets.yml
VERSION="${1:-}"
if [ -z "$VERSION" ] && [ -f "$SECRETS" ]; then
    VERSION=$(sed -n "s/^memory_api_release:[[:space:]]*[\"']\{0,1\}\([^\"']*\)[\"']\{0,1\}[[:space:]]*$/\1/p" "$SECRETS" | head -n 1)
fi
if [ -n "$VERSION" ] && ! [[ "$VERSION" =~ ^server-v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
    echo "Version '$VERSION' is not a server release tag such as server-v1.0.0"
    exit 1
fi

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

EXTRA_VARS=()
if [ -n "$VERSION" ]; then
    echo "Checking out release $VERSION"
    git -C /opt/llm-memory-api fetch --force origin "refs/tags/$VERSION:refs/tags/$VERSION"
    git -C /opt/llm-memory-api reset --hard "refs/tags/$VERSION^{commit}"
    EXTRA_VARS=(--extra-vars "memory_api_release=$VERSION")
fi

# Run setup playbook (will prompt for secrets on first run)
echo -e "\033[1m[3/4] Running setup...\033[0m"
cd /opt/llm-memory-api/infrastructure
export ANSIBLE_CONFIG=/opt/llm-memory-api/infrastructure/ansible.cfg
ansible-playbook -i inventory/production.yml playbooks/setup.yml "${EXTRA_VARS[@]}"

# Keep later deploys on this release. On a first install setup.yml wrote
# local-secrets.yml with the version already; on a re-install the file existed
# and setup.yml left it as it was, so the line is set here.
if [ -n "$VERSION" ]; then
    if grep -q '^memory_api_release:' "$SECRETS"; then
        sed -i "s/^memory_api_release:.*/memory_api_release: \"$VERSION\"/" "$SECRETS"
    else
        echo "memory_api_release: \"$VERSION\"" >> "$SECRETS"
    fi
fi

# Run deploy playbook
echo -e "\033[1m[4/4] Running deploy...\033[0m"
ansible-playbook -i inventory/production.yml playbooks/deploy.yml "${EXTRA_VARS[@]}"

echo ""
echo -e "\033[1;32m==================================="
echo "  Installation complete!"
echo -e "===================================\033[0m"
echo ""
echo "To deploy updates later, run:"
echo "  sudo bash /opt/llm-memory-api/deploy.sh"
echo ""
