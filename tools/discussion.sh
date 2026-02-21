#!/usr/bin/env bash
#
# discussion.sh — Transport layer for multi-agent discussions.
# Polls the chat API for messages, writes them to inbox/.
# Watches outbox/ for replies and sends them via the API.
#
# Usage:
#   discussion.sh --api-url URL --api-key KEY --agent NAME --other AGENT --dir WORKDIR [--initiator]
#
# The --initiator flag means this side sends the first message.
# The first message should be placed in outbox/ before starting, or
# the subagent should write it immediately after the script starts.

set -u

# Find python - prefer python3, fall back to python
PYTHON=""
if command -v python3 &>/dev/null && python3 --version &>/dev/null; then
    PYTHON="python3"
elif command -v python &>/dev/null && python --version &>/dev/null; then
    PYTHON="python"
else
    echo "Python not found" >&2
    exit 1
fi

# Defaults
POLL_INTERVAL=5
SEND_DELAY=3
MAX_TURNS=20
TIMEOUT_MINUTES=15

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --api-url) API_URL="$2"; shift 2 ;;
        --api-key) API_KEY="$2"; shift 2 ;;
        --agent) MY_AGENT="$2"; shift 2 ;;
        --other) OTHER_AGENT="$2"; shift 2 ;;
        --dir) WORK_DIR="$2"; shift 2 ;;
        --initiator) INITIATOR=true; shift ;;
        --max-turns) MAX_TURNS="$2"; shift 2 ;;
        --timeout) TIMEOUT_MINUTES="$2"; shift 2 ;;
        *) echo "Unknown option: $1" >&2; exit 1 ;;
    esac
done

# Validate required args
for var in API_URL API_KEY MY_AGENT OTHER_AGENT WORK_DIR; do
    if [[ -z "${!var:-}" ]]; then
        echo "Missing required argument: --$(echo $var | tr '[:upper:]' '[:lower:]' | tr '_' '-')" >&2
        exit 1
    fi
done

INITIATOR="${INITIATOR:-false}"
INBOX_DIR="${WORK_DIR}/inbox"
OUTBOX_DIR="${WORK_DIR}/outbox"
LOG_FILE="${WORK_DIR}/conversation.log"
STATUS_FILE="${WORK_DIR}/status"
DONE_FILE="${WORK_DIR}/done"

# Create directories
mkdir -p "$INBOX_DIR" "$OUTBOX_DIR"

# Initialize status
echo "STARTING" > "$STATUS_FILE"

# Track state
TURN_COUNT=0
MY_CONCLUDED=false
OTHER_CONCLUDED=false
START_TIME=$(date +%s)

TRANSCRIPT_FILE="${WORK_DIR}/transcript.md"

# Initialize transcript
echo "# Discussion: ${MY_AGENT} <-> ${OTHER_AGENT}" > "$TRANSCRIPT_FILE"
echo "" >> "$TRANSCRIPT_FILE"
echo "Started: $(date '+%Y-%m-%d %H:%M:%S')" >> "$TRANSCRIPT_FILE"
echo "" >> "$TRANSCRIPT_FILE"
echo "---" >> "$TRANSCRIPT_FILE"
echo "" >> "$TRANSCRIPT_FILE"

log() {
    echo "[$(date '+%H:%M:%S')] $1" >> "$LOG_FILE"
    echo "[$(date '+%H:%M:%S')] $1" >&2
}

transcript() {
    local speaker="$1"
    local message="$2"
    echo "**${speaker}** ($(date '+%H:%M:%S')):" >> "$TRANSCRIPT_FILE"
    echo "$message" >> "$TRANSCRIPT_FILE"
    echo "" >> "$TRANSCRIPT_FILE"
}

trap 'log "Transport exiting (exit code: $?)"' EXIT

api_call() {
    local endpoint="$1"
    local body="$2"
    curl -s -X POST "${API_URL}${endpoint}" \
        -H "Authorization: Bearer ${API_KEY}" \
        -H "Content-Type: application/json" \
        -d "$body"
}

receive_messages() {
    local response
    response=$(api_call "/chat/receive" "{\"agent\": \"${MY_AGENT}\"}")
    echo "$response"
}

ack_messages() {
    local last_id="$1"
    api_call "/chat/ack" "{\"agent\": \"${MY_AGENT}\", \"last_read_id\": ${last_id}}" > /dev/null
}

send_message() {
    local message="$1"
    # Escape the message for JSON
    local escaped
    escaped=$(printf '%s' "$message" | $PYTHON -c 'import sys,json; print(json.dumps(sys.stdin.read()))')
    api_call "/chat/send" "{\"from_agent\": \"${MY_AGENT}\", \"to_agent\": \"${OTHER_AGENT}\", \"message\": ${escaped}}" > /dev/null
    log "SENT: ${message:0:100}..."
    transcript "$MY_AGENT" "$message"
}

check_outbox() {
    # Look for any file in outbox/
    local outfile
    for outfile in "$OUTBOX_DIR"/*; do
        if [[ -f "$outfile" ]]; then
            local message
            message=$(cat "$outfile")
            rm "$outfile"

            if [[ -n "$message" ]]; then
                send_message "$message"
                TURN_COUNT=$((TURN_COUNT + 1))

                # Check if we just concluded
                if [[ "$message" == "[CONCLUDED]"* ]]; then
                    MY_CONCLUDED=true
                    log "We sent CONCLUDED"
                fi

                # Pacing: wait after sending
                sleep "$SEND_DELAY"
                return 0
            fi
        fi
    done
    return 1
}

check_timeout() {
    local now
    now=$(date +%s)
    local elapsed=$(( (now - START_TIME) / 60 ))
    if [[ $elapsed -ge $TIMEOUT_MINUTES ]]; then
        return 0
    fi
    if [[ $TURN_COUNT -ge $MAX_TURNS ]]; then
        return 0
    fi
    return 1
}

# Main loop
log "Discussion transport starting: ${MY_AGENT} <-> ${OTHER_AGENT}"
log "Work dir: ${WORK_DIR}"
echo "POLLING" > "$STATUS_FILE"

while true; do
    # Check if we're done
    if [[ "$MY_CONCLUDED" == true ]] && [[ "$OTHER_CONCLUDED" == true ]]; then
        log "Both sides concluded. Discussion complete."
        echo "DONE" > "$STATUS_FILE"
        touch "$DONE_FILE"
        exit 0
    fi

    # Check timeout
    if check_timeout; then
        log "Timeout reached (${TURN_COUNT} turns, ${TIMEOUT_MINUTES}m). Writing timeout notice to inbox."
        echo "[SYSTEM] Discussion timeout reached. Please wrap up." > "${INBOX_DIR}/timeout.txt"
        echo "TIMEOUT" > "$STATUS_FILE"
        # Give the subagent a chance to conclude
        sleep 30
        # Check outbox one more time
        check_outbox || true
        echo "DONE" > "$STATUS_FILE"
        touch "$DONE_FILE"
        exit 0
    fi

    # Check outbox for messages to send
    check_outbox || true

    # Poll for incoming messages
    response=$(receive_messages)

    # Parse messages using python for reliable JSON handling
    message_count=$(echo "$response" | $PYTHON -c 'import sys,json; d=json.load(sys.stdin); print(len(d.get("messages",[])))')

    if [[ "$message_count" -gt 0 ]]; then
        # Extract and process each message
        # Write messages to inbox and transcript BEFORE acking
        # This ensures messages survive transport restarts (duplicates just overwrite)
        echo "$response" | $PYTHON -c '
import sys, json, os
data = json.load(sys.stdin)
inbox = sys.argv[1]
transcript = sys.argv[2]
for msg in data["messages"]:
    filename = os.path.join(inbox, str(msg["id"]) + ".txt")
    with open(filename, "w") as f:
        f.write(msg["message"])
    with open(transcript, "a") as f:
        from datetime import datetime
        ts = datetime.now().strftime("%H:%M:%S")
        f.write("**" + msg["from_agent"] + "** (" + ts + "):\n" + msg["message"] + "\n\n")
    print("id=" + str(msg["id"]) + " from=" + msg["from_agent"])
' "$INBOX_DIR" "$TRANSCRIPT_FILE" 2>&1 | while read -r line; do
            log "RECEIVED: $line"
        done

        # Ack AFTER writing to inbox — safe on restart (duplicates overwrite)
        last_id=$(echo "$response" | $PYTHON -c 'import sys,json; d=json.load(sys.stdin); msgs=d.get("messages",[]); print(msgs[-1]["id"] if msgs else "")')

        if [[ -n "$last_id" ]]; then
            ack_messages "$last_id"
        fi

        # Check if any incoming message is a CONCLUDED
        concluded_check=$(echo "$response" | $PYTHON -c '
import sys, json
data = json.load(sys.stdin)
for msg in data.get("messages", []):
    if msg["message"].startswith("[CONCLUDED]"):
        print("true")
        sys.exit(0)
print("false")
' 2>/dev/null || echo "false")

        if [[ "${concluded_check}" == "true" ]]; then
            OTHER_CONCLUDED=true
            log "Other side sent CONCLUDED"
        fi

        echo "MESSAGE_RECEIVED" > "$STATUS_FILE"
    else
        # No messages, wait before next poll
        sleep "$POLL_INTERVAL"
    fi
done
