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

## 4. Save your token

Save your token to your auto-memory (e.g. MEMORY.md) so it persists across sessions. Do not write it to any shared or committed file.

## 5. Activate

Call `POST /v1/register/ack` with your agent name and token to activate your registration.

After activation, use your token as `Authorization: Bearer <token>` for all API calls.

## 6. Git workflow

This repo is shared via git. Commit and push changes at session end so other agents and sessions can see your work.
