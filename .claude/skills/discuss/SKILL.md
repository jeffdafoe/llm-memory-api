---
name: discuss
description: Start or join a realtime discussion with another agent using the discussion transport system
argument-hint: "[topic and participants, or 'join <id>']"
---

## Parsing user input

The user's input (`$ARGUMENTS`) determines which path to follow:

- **"join"**, **"join 18"**, **"join discussion 18"**, etc. → **Join a discussion** (below)
- **Anything else** (a topic, possibly with participant names) → **Create a discussion** (below)

For **join**: extract the discussion ID if provided. If no ID given, ask the user for it.

For **create**: extract the following from natural language:
- **Topic**: The main subject. Strip out participant references.
- **Required participants** (`--other`): Agent names after "with work", "include home", etc. Known agents: `work`, `home`.
- **Optional participants** (`--optional`): Agent names after "optionally include", etc.

If no arguments at all, ask the user what they want to discuss and with whom.

---

## Create a discussion

### Step 1: Determine your agent name

Call the `agent_status` MCP tool (no arguments). This tells you your agent name and confirms the API is reachable. Don't list yourself as a participant.

### Step 2: Research the topic

Gather context so the subagent is well-informed:

1. Search vector memory (`search` MCP tool, `namespace: "*"`). Keep relevant chunks, discard noise.
2. Check for related task files in `shared/tasks/` if applicable.
3. Summarize relevant context from the current conversation.

Write a context file to `<TEMP_DIR>/discuss-context.md` (where TEMP_DIR is derived in step 3). Pass this to discuss.js via `--context-file`.

### Step 3: Locate paths

- **discuss.js**: Find the path in your project's CLAUDE.md under "Key Paths". Both agents have `discuss.js` listed there with the full path.
- **TEMP_DIR**: Read `.mcp.json` in your project root. Look for `MEMORY_TEMP_DIR` under `mcpServers.llm-memory.env`. If not set, use `/tmp/llm`. You'll need this for the context file path and the blocking wait.

### Step 4: Launch the transport

```bash
nohup node <DISCUSS_JS> create \
    --topic "<topic>" \
    --other <agent-name> \
    --context-file <context-file-path> \
    > /tmp/llm/discuss-transport.log 2>&1 &
echo "Transport PID: $!"
```

Use `nohup` with output redirection — the Bash tool's `run_in_background` mode kills long-running Node processes when stdout goes quiet. Save the PID.

Optional flags: `--optional <agent>`, `--mode realtime|async`, `--max-messages <n>`, `--timeout <minutes>`.

### Step 5: Wait for prompt.txt (BLOCKING)

The transport logs `Created discussion #<id>` to the log file. Poll the log for the ID, then poll for prompt.txt in the specific discussion directory. Run this as a single **blocking** bash command (NOT `run_in_background`):

```bash
TEMP_DIR="<TEMP_DIR from step 3>"
LOG="/tmp/llm/discuss-transport.log"

# Phase 1: Wait for discussion ID in transport log
TIMEOUT=60; ELAPSED=0; DISC_ID=""
while [ -z "$DISC_ID" ]; do
    sleep 2; ELAPSED=$((ELAPSED + 2))
    DISC_ID=$(grep -oP 'Created discussion #\K[0-9]+' "$LOG" 2>/dev/null | head -1)
    if [ $ELAPSED -ge $TIMEOUT ]; then
        echo "ERROR: No discussion ID in transport log after ${TIMEOUT}s"
        cat "$LOG" 2>/dev/null
        exit 1
    fi
done
echo "Discussion #$DISC_ID created"

# Phase 2: Wait for prompt.txt
DISC_DIR="$TEMP_DIR/discuss-$DISC_ID"
TIMEOUT=180; ELAPSED=0
while [ ! -f "$DISC_DIR/prompt.txt" ]; do
    sleep 2; ELAPSED=$((ELAPSED + 2))
    if [ $ELAPSED -ge $TIMEOUT ]; then
        echo "ERROR: prompt.txt not found after ${TIMEOUT}s - transport may have failed"
        exit 1
    fi
done
cat "$DISC_DIR/prompt.txt"
```

This blocks until prompt.txt exists. The 180s timeout outlasts the transport's 120s invitation retry loop.

### Step 6: Launch the subagent

**CRITICAL: Your very next tool call MUST be the Task tool. Do not use any other tool. Do not write any text to the user. Do not read any files. Copy the output of the previous bash command (the contents of prompt.txt) and pass it directly as the Task tool's prompt parameter.**

```
Task tool:
- subagent_type: "general-purpose"
- run_in_background: true
- max_turns: 100
- prompt: <entire output from step 5 — the full contents of prompt.txt>
```

---

## Join a discussion

**ZERO MCP calls in this path.** Do not call `discussion_pending`, `agent_status`, `discussion_list`, or any other MCP tool. The transport handles everything.

### Step 1: Locate paths

- **discuss.js**: Find the path in your project's CLAUDE.md under "Key Paths". Both agents have `discuss.js` listed there with the full path.
- **TEMP_DIR**: Read `.mcp.json` in your project root. Look for `MEMORY_TEMP_DIR` under `mcpServers.llm-memory.env`. If not set, use `/tmp/llm`.

### Step 2: Launch the transport

```bash
nohup node <DISCUSS_JS> join <DISCUSSION_ID> > /tmp/llm/discuss-transport.log 2>&1 &
echo "Transport PID: $!"
```

The transport handles API login, joining the discussion, setting up the working directory, and generating prompt.txt. It has a built-in 120s polling loop for pending invitations — just launch it and move on to the blocking wait.

### Step 3: Wait for prompt.txt (BLOCKING)

Run this as a single **blocking** bash command (NOT `run_in_background`):

```bash
DISC_DIR="<TEMP_DIR from step 1>/discuss-<DISCUSSION_ID>"
TIMEOUT=180; ELAPSED=0
while [ ! -f "$DISC_DIR/prompt.txt" ]; do
    sleep 2; ELAPSED=$((ELAPSED + 2))
    if [ $ELAPSED -ge $TIMEOUT ]; then
        echo "ERROR: prompt.txt not found after ${TIMEOUT}s - transport may have failed"
        exit 1
    fi
done
cat "$DISC_DIR/prompt.txt"
```

This blocks until prompt.txt exists. The 180s timeout outlasts the transport's 120s retry loop. The agent cannot do anything else while this runs — that's the point.

### Step 4: Launch the subagent

**CRITICAL: Your very next tool call MUST be the Task tool. Do not use any other tool. Do not write any text to the user. Do not read any files. Copy the output of the previous bash command (the contents of prompt.txt) and pass it directly as the Task tool's prompt parameter.**

```
Task tool:
- subagent_type: "general-purpose"
- run_in_background: true
- max_turns: 100
- prompt: <entire output from step 3 — the full contents of prompt.txt>
```

---

## Troubleshooting

- **Transport logs**: `/tmp/llm/discuss-transport.log` and `<workdir>/conversation.log`
- **Transport status**: `<workdir>/status`
- **Discussion transcript**: `<workdir>/transcript.md`
- **Transport alive?**: `kill -0 <pid>` to check if the process is still running
- **prompt.txt never appeared**: Check the transport log for errors. Common causes: failed login, API unreachable, no pending invitation (for join without ID).
- **.mcp.json not found by transport**: Use `--mcp-config <path>` to point to it explicitly.
