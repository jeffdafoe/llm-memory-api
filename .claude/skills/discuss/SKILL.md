---
name: discuss
description: Start or join a realtime discussion with another agent using the discussion transport system
argument-hint: "[topic and participants, or 'join']"
---

## Parsing user input

The user's input (`$ARGUMENTS`) is natural language. Extract the following:

- **Action**: "join" if the user says "join" or similar. Otherwise "create".
- **Topic**: The main subject described by the user. Strip out participant references.
- **Required participants** (`--other`): Agent names after phrases like "with work", "include home", "and work". Known agent names: `work`, `home`.
- **Optional participants** (`--optional`): Agent names after phrases like "optionally include", "maybe include", "as optional".

If no arguments are provided, ask the user what they want to discuss and with whom.

Examples:
- `/discuss the problem with user signups and include work as required` → create, topic="the problem with user signups", --other work
- `/discuss join` → join (auto-discover pending invitation)
- `/discuss API refactoring with work and home` → create, topic="API refactoring", --other work --other home
- `/discuss onboarding docs with work, optionally include home` → create, topic="onboarding docs", --other work --optional home

## Step 1: Determine your agent name

Call the `discussion_pending` MCP tool (no args needed — it defaults to your configured agent). The response context tells you your agent name. You'll need this so you don't list yourself as a participant.

## Step 2: Research the topic

Before launching the discussion, gather relevant context so the subagent is well-informed:

1. Search vector memory for the topic (`search` MCP tool with `namespace: "*"`). Review the results — keep chunks that are relevant, discard noise.
2. Check for related task files in `shared/tasks/` and `work/tasks/` if applicable.
3. If the current conversation has relevant context (decisions made, files discussed, etc.), summarize the key points.

Write a context file to `C:/temp/llm/discuss-context.md` combining the curated results. Include source file paths so the subagent can read full files if needed. Pass this to discuss.js via `--context-file C:/temp/llm/discuss-context.md`.

## Step 3: Find discuss.js

The transport script is at `node/client/discuss.js` relative to the llm-memory-api repo root. Find the repo by locating `.mcp.json` in your project root — it contains the path to the MCP server script under `mcpServers.llm-memory.args[0]`. The repo root is two directories up from that script path (`node/mcp/server.js` -> repo root).

## Step 4: Run discuss.js

discuss.js reads credentials (API URL, agent name, passphrase) from `.mcp.json` automatically. It searches up from the current working directory to find it. Run it from your project root.

**To create a discussion:**
```bash
node <path-to-discuss.js> create --topic "Your topic here" --other <agent-name> &
```

**To join a discussion (auto-discovers pending invitation):**
```bash
node <path-to-discuss.js> join &
```

**To join a specific discussion by ID:**
```bash
node <path-to-discuss.js> join <discussion-id> &
```

Optional flags:
- `--other <agent>` (repeatable, for additional required participants)
- `--optional <agent>` (repeatable, for optional participants who may join later)
- `--context "background info"` or `--context-file <path>`
- `--mode realtime|async` (default: realtime)
- `--max-messages <n>` (default: 200)
- `--timeout <minutes>` (default: 120)
- `--mcp-config <path>` (override .mcp.json auto-discovery)

**IMPORTANT:** Run the command in the background (with `&`) so it doesn't block your session.

## Step 5: Wait for transport readiness

After starting discuss.js, wait for the `prompt.txt` file to appear in the work directory. The work directory is auto-generated at `<os-temp>/llm/discuss-<id>/`. Check stderr output from the backgrounded process — it will log the exact paths:

```
[HH:MM:SS] Prompt: <workdir>/prompt.txt
```

Poll until the prompt file exists (it may take a few seconds while the transport logs in, creates/joins the discussion, and waits for participants).

## Step 6: Launch the subagent

Read the generated `prompt.txt` and launch a background Task agent with its contents:

```
Use the Task tool with:
- subagent_type: "general-purpose"
- run_in_background: true
- max_turns: 100
- prompt: <contents of prompt.txt>
```

The subagent handles the actual discussion — reading messages from `inbox/`, writing replies to `outbox/`, voting on proposals, and concluding the discussion. The transport handles all API communication.

## If joining and no invitation is found

If `discuss.js join` reports "No pending discussion invitations found", the other agent hasn't created the discussion yet. Wait and try again, or ask the user.

## Troubleshooting

- **Transport logs**: `<workdir>/conversation.log`
- **Transport status**: `<workdir>/status`
- **Discussion transcript**: `<workdir>/transcript.md`
- If the transport can't find `.mcp.json`, use `--mcp-config <path>` to point to it explicitly.
