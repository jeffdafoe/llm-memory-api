#!/usr/bin/env bash
#
# discussion.sh — Transport layer for multi-agent discussions.
# Polls the chat API for messages, writes them to inbox/.
# Watches outbox/ for replies and sends them via the API.
#
# Dependencies: node (for JSON parsing), curl
#
# Usage:
#   discussion.sh --api-url URL --api-key KEY --agent NAME --other AGENT --dir WORKDIR [--initiator] [--conclude-timeout SECONDS]
#
# The --initiator flag means this side sends the first message.
# The first message should be placed in outbox/ before starting, or
# the subagent should write it immediately after the script starts.

set -u

# Verify node is available (required for JSON parsing)
if ! command -v node &>/dev/null; then
    echo "Node.js not found — required for JSON parsing" >&2
    exit 1
fi

# Defaults
POLL_INTERVAL=5
SEND_DELAY=3
MAX_TURNS=20
TIMEOUT_MINUTES=15
CONCLUDE_TIMEOUT=120

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
        --channel) CHANNEL="$2"; shift 2 ;;
        --conclude-timeout) CONCLUDE_TIMEOUT="$2"; shift 2 ;;
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
CHANNEL="${CHANNEL:-discussion}"
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
CONCLUDE_SEEN_AT=0
START_TIME=$(date +%s)
SEEN_IDS_FILE="${WORK_DIR}/.seen_ids"
READY_FILE="${WORK_DIR}/ready"
> "$SEEN_IDS_FILE"

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
    response=$(api_call "/chat/receive" "{\"agent\": \"${MY_AGENT}\", \"channel\": \"${CHANNEL}\"}")
    echo "$response"
}

ack_messages() {
    local ids_json="$1"
    api_call "/chat/ack" "{\"agent\": \"${MY_AGENT}\", \"message_ids\": ${ids_json}}" > /dev/null
}

send_message() {
    local message="$1"
    local escaped
    escaped=$(printf '%s' "$message" | node -e 'let d="";process.stdin.on("data",c=>d+=c);process.stdin.on("end",()=>console.log(JSON.stringify(d)))')
    api_call "/chat/send" "{\"from_agent\": \"${MY_AGENT}\", \"to_agent\": \"${OTHER_AGENT}\", \"message\": ${escaped}, \"channel\": \"${CHANNEL}\"}" > /dev/null
    log "SENT: ${message:0:100}..."
    transcript "$MY_AGENT" "$message"
}

check_outbox() {
    local outfile
    for outfile in "$OUTBOX_DIR"/*; do
        if [[ -f "$outfile" ]]; then
            local message
            message=$(cat "$outfile")
            rm "$outfile"

            if [[ -n "$message" ]]; then
                send_message "$message"
                TURN_COUNT=$((TURN_COUNT + 1))

                if [[ "$message" == *"[CONCLUDED]"* ]]; then
                    MY_CONCLUDED=true
                    if [[ $CONCLUDE_SEEN_AT -eq 0 ]]; then
                        CONCLUDE_SEEN_AT=$(date +%s)
                    fi
                    log "We sent CONCLUDED"
                fi

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

# Process received messages in a single node call.
# Writes messages to inbox/ and appends to transcript BEFORE returning.
# Outputs structured lines to stdout: count=N, ids_json=[1,2,3], concluded=true/false, log=...
process_received() {
    local response="$1"
    echo "$response" | node -e '
const fs = require("fs");
const path = require("path");
let d = "";
process.stdin.on("data", c => d += c);
process.stdin.on("end", () => {
    let data;
    try { data = JSON.parse(d); } catch (e) { console.log("count=0"); return; }
    const msgs = data.messages || [];
    const inbox = process.argv[1];
    const transcript = process.argv[2];
    const seenFile = process.argv[3];

    // Load seen IDs for dedup
    let seen = new Set();
    try {
        const lines = fs.readFileSync(seenFile, "utf-8").trim().split("\n").filter(Boolean);
        for (const line of lines) seen.add(line);
    } catch (e) {}

    if (msgs.length === 0) {
        console.log("count=0");
        return;
    }

    let concluded = false;
    const ids = [];
    const newIds = [];
    for (const msg of msgs) {
        ids.push(msg.id);
        if (seen.has(String(msg.id))) continue;
        newIds.push(msg.id);
        fs.writeFileSync(path.join(inbox, msg.id + ".txt"), msg.message);
        const ts = new Date().toTimeString().slice(0, 8);
        fs.appendFileSync(transcript, "**" + msg.from_agent + "** (" + ts + "):\n" + msg.message + "\n\n");
        console.log("log=id=" + msg.id + " from=" + msg.from_agent);
        fs.appendFileSync(seenFile, msg.id + "\n");
        if (msg.message.includes("[CONCLUDED]")) concluded = true;
    }

    console.log("count=" + newIds.length);
    console.log("ids_json=" + JSON.stringify(ids));
    console.log("concluded=" + concluded);
});' "$INBOX_DIR" "$TRANSCRIPT_FILE" "$SEEN_IDS_FILE"
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

    # Check conclude timeout — one side concluded but the other hasn't responded
    if [[ $CONCLUDE_SEEN_AT -gt 0 ]]; then
        local_now=$(date +%s)
        conclude_elapsed=$((local_now - CONCLUDE_SEEN_AT))
        if [[ $conclude_elapsed -ge $CONCLUDE_TIMEOUT ]]; then
            log "Conclude timeout reached (${CONCLUDE_TIMEOUT}s). Other side did not respond."
            echo "CONCLUDE_TIMEOUT" > "$STATUS_FILE"
            echo "TIMEOUT" > "$DONE_FILE"
            exit 0
        fi
    fi

    # Check timeout
    if check_timeout; then
        log "Timeout reached (${TURN_COUNT} turns, ${TIMEOUT_MINUTES}m). Writing timeout notice to inbox."
        echo "[SYSTEM] Discussion timeout reached. Please wrap up." > "${INBOX_DIR}/timeout.txt"
        echo "TIMEOUT" > "$STATUS_FILE"
        sleep 30
        check_outbox || true
        echo "DONE" > "$STATUS_FILE"
        touch "$DONE_FILE"
        exit 0
    fi

    # Check outbox for messages to send
    check_outbox || true

    # Poll for incoming messages
    response=$(receive_messages)

    # Single node call: write to inbox, append transcript, extract metadata
    result=$(process_received "$response")

    # Log received messages
    echo "$result" | grep "^log=" | while read -r line; do
        log "RECEIVED: ${line#log=}"
    done

    message_count=$(echo "$result" | grep "^count=" | cut -d= -f2)

    if [[ "$message_count" -gt 0 ]]; then
        ids_json=$(echo "$result" | grep "^ids_json=" | cut -d= -f2-)
        concluded=$(echo "$result" | grep "^concluded=" | cut -d= -f2)

        # Ack AFTER writing to inbox — safe on restart (duplicates overwrite)
        if [[ -n "$ids_json" ]]; then
            ack_messages "$ids_json"
        fi

        if [[ "$concluded" == "true" ]]; then
            OTHER_CONCLUDED=true
            if [[ $CONCLUDE_SEEN_AT -eq 0 ]]; then
                CONCLUDE_SEEN_AT=$(date +%s)
            fi
            log "Other side sent CONCLUDED"
        fi

        echo "MESSAGE_RECEIVED" > "$STATUS_FILE"

        # Signal that the other side is active (first inbound message)
        if [[ ! -f "$READY_FILE" ]]; then
            touch "$READY_FILE"
            log "Other side is active — ready file written"
        fi
    else
        sleep "$POLL_INTERVAL"
    fi
done
