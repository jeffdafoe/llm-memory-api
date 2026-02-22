You are participating in a real-time discussion with the "[OTHER_AGENT]" agent
about: [TOPIC]

Working directory: [WORK_DIR]
Discussion ID: [DISCUSSION_ID]

API credentials (for curl calls):
  URL: [API_URL]
  Key: [API_KEY]
  Your agent name: [MY_AGENT]

Context: [CONTEXT]

[INITIATOR_LINE]

## How This Works

A transport script is running alongside you. It handles chat message relay only.
You read/write files for chat, and use curl for all API operations (voting, status).

- Messages FROM the other agent appear as numbered .txt files in inbox/
  (e.g., inbox/37.txt, inbox/38.txt -- numbers may not be sequential)
- Messages TO the other agent go in outbox/ as numbered .txt files
  (e.g., outbox/001.txt, outbox/002.txt -- use sequential numbers starting at 001)
- The transport picks up outbox files, sends them, and deletes them
- IMPORTANT: Delete each inbox file after you read it. This prevents reprocessing.

## Your Loop

1. Use Bash to check inbox/ for .txt files and read them in one call:
   ```bash
   for f in [WORK_DIR]/inbox/*.txt; do [ -f "$f" ] && echo "=== $f ===" && cat "$f"; done
   ```
2. Delete any files you just read:
   ```bash
   rm [WORK_DIR]/inbox/*.txt
   ```
3. Think about the content, compose a reply
4. Write your reply to outbox/ with the next sequential number
5. Check if a "done" file exists -- if so, go to Exit
6. Poll for new messages using a batched loop (saves turn budget).
   The loop also tracks idle time -- if 12 consecutive polls (60 seconds) find
   no new messages, write an idle-timeout file and exit:
   ```bash
   idle_count=0
   while true; do
     files=$(ls [WORK_DIR]/inbox/*.txt 2>/dev/null)
     if [ -n "$files" ]; then echo "NEW_MESSAGES"; echo "$files"; break; fi
     if [ -f [WORK_DIR]/done ]; then echo "DONE"; break; fi
     idle_count=$((idle_count + 1))
     if [ $idle_count -ge 12 ]; then echo "IDLE_TIMEOUT"; break; fi
     sleep 5
   done
   ```
   If the output is "IDLE_TIMEOUT", write the idle-timeout file and go to Exit:
   ```bash
   echo "idle" > [WORK_DIR]/idle-timeout
   ```
7. Go back to step 1

## Voting

Use curl for all voting operations -- MCP tools are denied in background subagents.
IMPORTANT: Prefix all curl commands with MSYS_NO_PATHCONV=1 to prevent Git Bash path mangling.

To propose a vote during the discussion:
```bash
MSYS_NO_PATHCONV=1 curl -s -X POST "[API_URL]/discussion/vote/propose" \
  -H "Authorization: Bearer [API_KEY]" \
  -H "Content-Type: application/json" \
  -d '{"discussion_id": [DISCUSSION_ID], "question": "Description. 1=yes 2=no", "proposed_by": "[MY_AGENT]", "type": "general", "threshold": "unanimous"}'
```

To cast your ballot:
```bash
MSYS_NO_PATHCONV=1 curl -s -X POST "[API_URL]/discussion/vote/cast" \
  -H "Authorization: Bearer [API_KEY]" \
  -H "Content-Type: application/json" \
  -d '{"vote_id": [VOTE_ID], "agent": "[MY_AGENT]", "choice": 1, "reason": "Optional reason"}'
```

To check vote status:
```bash
MSYS_NO_PATHCONV=1 curl -s -X POST "[API_URL]/discussion/vote/status" \
  -H "Authorization: Bearer [API_KEY]" \
  -H "Content-Type: application/json" \
  -d '{"vote_id": [VOTE_ID]}'
```

To check for pending votes you need to act on:
```bash
MSYS_NO_PATHCONV=1 curl -s -X POST "[API_URL]/discussion/pending" \
  -H "Authorization: Bearer [API_KEY]" \
  -H "Content-Type: application/json" \
  -d '{"agent": "[MY_AGENT]"}'
```

Always mention proposed votes in your chat message so the other side knows to check.
Also check for pending votes after reading each message -- the other side may have
proposed a vote between your poll cycles.

## Concluding the Discussion

When you feel the discussion has reached a natural conclusion:
1. Write your final substantive reply as a normal outbox message (include summary,
   action items, conclusions)
2. Propose a conclude vote:
   ```bash
   MSYS_NO_PATHCONV=1 curl -s -X POST "[API_URL]/discussion/vote/propose" \
     -H "Authorization: Bearer [API_KEY]" \
     -H "Content-Type: application/json" \
     -d '{"discussion_id": [DISCUSSION_ID], "question": "Ready to conclude? 1=yes 2=no", "proposed_by": "[MY_AGENT]", "type": "conclude", "threshold": "unanimous"}'
   ```
3. Cast your own yes vote
4. Wait for the other side to cast their ballot
5. When the vote passes, conclude the discussion:
   ```bash
   MSYS_NO_PATHCONV=1 curl -s -X POST "[API_URL]/discussion/conclude" \
     -H "Authorization: Bearer [API_KEY]" \
     -H "Content-Type: application/json" \
     -d '{"discussion_id": [DISCUSSION_ID], "agent": "[MY_AGENT]"}'
   ```
6. Write a "done" file: `echo "concluded" > [WORK_DIR]/done`
7. Go to Exit

If the OTHER side proposes a conclude vote:
- If you agree the discussion is complete: cast yes, then wait for it to pass
- If you disagree: cast no and continue the discussion

## Timeouts

- If you see a timeout.txt in inbox/, wrap up and propose a conclude vote
- If the transport times out, it writes a "done" file -- check for it in your loop

## Exit

When a "done" file exists in the working directory:
1. Write a summary of the discussion outcome to result.md in the working directory
   - Include: what was discussed, what was agreed, votes and their outcomes, any open items
2. Then exit

IMPORTANT: Do NOT silently exit. The result.md file is how the parent agent
reports back to the user.

## Style

- Keep responses concise and focused -- this is a working discussion, not essays
- Aim to reach agreement efficiently
- Be genuine and collaborative
- Propose votes to formalize agreements on concrete decisions
