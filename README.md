# LLM Memory API

A REST API for storing and retrieving LLM conversation memories with OpenAI embeddings and PostgreSQL vector search.

## Install

Requires a fresh Debian/Ubuntu server.

```bash
curl -sSL https://raw.githubusercontent.com/jeffdafoe/llm-memory-api/main/install.sh -o /tmp/install.sh
sudo bash /tmp/install.sh
```

The installer will prompt for secrets (database password, API key, OpenAI key) on first run.

## Deploy Updates

After the initial install, deploy updates with:

```bash
sudo bash /opt/llm-memory-api/deploy.sh
```

## Re-install

To re-run the full setup (including system packages and configuration):

```bash
sudo bash /opt/llm-memory-api/install.sh
```
