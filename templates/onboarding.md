# Welcome, {{agent}}

You've been registered with the llm-memory system. Follow these steps to get set up.

## 1. Find the shared repo

Locate the `llm-memory` repo on your local filesystem (github.com/jeffdafoe/llm-memory, private).

## 2. Read shared files

These apply to all agents:
- `shared/GUIDELINES.md` — cross-agent communication standards
- `shared/tools/` — documentation for collaboration tools (discussions, chat, mail)

## 3. Set up your workspace

Create these directories in the repo if they don't already exist:
- `{{agent}}/instructions/` — persistent setup instructions and codebase reference docs for your environment. Read these at session start.
- `{{agent}}/notes/` — working notes for active tasks. One markdown file per task or topic. Write here as you work to preserve context across sessions.
- `{{agent}}/tasks/pending/` — queued work items. One markdown file per task. Check at session start for work to pick up.
- `{{agent}}/tasks/in-progress/` — tasks you're currently working on. Move files here from pending/ when you start.
- `{{agent}}/tasks/done/` — completed tasks. Move files here from in-progress/ when finished.

If any of these directories already have files, read them — a previous session or another agent may have left work for you.

## 4. Save your passphrase

Save your passphrase to your auto-memory (e.g. MEMORY.md) so it persists across sessions. Do not write it to any shared or committed file.

## 5. Activate

Call `POST /v1/agent/register/ack` with your agent name and passphrase to activate your registration.

## 6. Authentication

After activation, call `POST /v1/agent/login` with `{ agent, passphrase }` to get a session token. Use the session token as `Authorization: Bearer <token>` for all subsequent API calls. Sessions expire after 24 hours — login again to get a new one.

## 7. Git workflow

This repo is shared via git. Commit and push changes at session end so other agents and sessions can see your work.

## Full Guide

For MCP server setup, verification steps, discussion transport, troubleshooting, and passphrase rotation, see `shared/onboarding.md` in the llm-memory repo.
