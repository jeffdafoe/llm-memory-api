#!/usr/bin/env bash
#
# discuss.sh — Wrapper script for multi-agent discussions.
# Handles discussion creation/joining, working directory setup,
# transport launch, and subagent prompt generation.
#
# Usage:
#   discuss.sh --create --topic "..." --other AGENT [--context "..." | --context-file PATH] [--mode realtime|async] --api-url URL --api-key KEY --agent NAME
#   discuss.sh --join ID --api-url URL --api-key KEY --agent NAME
#
# Output: Creates working directory, starts transport, writes prompt.txt

set -u
export MSYS_NO_PATHCONV=1

if ! command -v node &>/dev/null; then
    echo "ERROR: Node.js not found — required for JSON parsing" >&2
    exit 1
fi


# Defaults
ACTION=""
TOPIC=""
OTHER_AGENT=""
CONTEXT=""
CONTEXT_FILE=""
MODE="realtime"
JOIN_ID=""
API_URL=""
API_KEY=""
MY_AGENT=""

# Detect temp directory (Windows vs Linux)
if [[ -d "/c/temp/llm" ]]; then
    TEMP_BASE="/c/temp/llm"
elif [[ -d "/tmp" ]]; then
    TEMP_BASE="/tmp"
else
    TEMP_BASE="."
fi

# Find the directory this script lives in (for locating sibling files)
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TRANSPORT_SCRIPT="${SCRIPT_DIR}/discussion.sh"
PROMPT_TEMPLATE="${SCRIPT_DIR}/discussion-prompt.tpl"

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --create) ACTION="create"; shift ;;
        --join) ACTION="join"; JOIN_ID="$2"; shift 2 ;;
        --topic) TOPIC="$2"; shift 2 ;;
        --other) OTHER_AGENT="$2"; shift 2 ;;
        --context) CONTEXT="$2"; shift 2 ;;
        --context-file) CONTEXT_FILE="$2"; shift 2 ;;
        --mode) MODE="$2"; shift 2 ;;
        --api-url) API_URL="$2"; shift 2 ;;
        --api-key) API_KEY="$2"; shift 2 ;;
        --agent) MY_AGENT="$2"; shift 2 ;;
        *) echo "Unknown option: $1" >&2; exit 1 ;;
    esac
done

# Validate common required args
for var in API_URL API_KEY MY_AGENT; do
    if [[ -z "${!var:-}" ]]; then
        echo "ERROR: Missing required argument: --$(echo $var | tr '[:upper:]' '[:lower:]' | tr '_' '-')" >&2
        exit 1
    fi
done

if [[ -z "$ACTION" ]]; then
    echo "ERROR: Must specify --create or --join ID" >&2
    exit 1
fi

api_call() {
    local endpoint="$1"
    local body="$2"
    node -e '
const https = require("https");
const http = require("http");
const url = new URL(process.argv[1] + process.argv[2]);
const mod = url.protocol === "https:" ? https : http;
const data = process.argv[3];
const req = mod.request(url, {
    method: "POST",
    headers: {
        "Authorization": "Bearer " + process.argv[4],
        "Content-Type": "application/json",
        "Content-Length": Buffer.byteLength(data)
    }
}, res => {
    let body = "";
    res.on("data", c => body += c);
    res.on("end", () => {
        if (res.statusCode >= 200 && res.statusCode < 300) {
            console.log(body);
        } else {
            console.error("HTTP " + res.statusCode + ": " + body);
            process.exit(1);
        }
    });
});
req.on("error", e => { console.error("ERR: " + e.message); process.exit(1); });
req.write(data);
req.end();
' "$API_URL" "$endpoint" "$body" "$API_KEY"
}

generate_prompt() {
    local discussion_id="$1"
    local topic="$2"
    local other="$3"
    local context="$4"
    local work_dir="$5"
    local is_initiator="$6"

    if [[ ! -f "$PROMPT_TEMPLATE" ]]; then
        echo "ERROR: Prompt template not found at ${PROMPT_TEMPLATE}" >&2
        return 1
    fi

    local initiator_line
    if [[ "$is_initiator" == "true" ]]; then
        initiator_line="You are the INITIATOR. Write your opening message to outbox/001.txt immediately."
    else
        initiator_line="You are NOT the initiator. Wait for the first message to appear in inbox/."
    fi

    if [[ -z "$context" ]]; then
        context="No additional context provided."
    fi

    local prompt
    prompt=$(cat "$PROMPT_TEMPLATE")

    # Do variable substitution
    prompt="${prompt//\[DISCUSSION_ID\]/$discussion_id}"
    prompt="${prompt//\[TOPIC\]/$topic}"
    prompt="${prompt//\[OTHER_AGENT\]/$other}"
    prompt="${prompt//\[MY_AGENT\]/$MY_AGENT}"
    prompt="${prompt//\[API_URL\]/$API_URL}"
    prompt="${prompt//\[API_KEY\]/$API_KEY}"
    prompt="${prompt//\[WORK_DIR\]/$work_dir}"
    prompt="${prompt//\[INITIATOR_LINE\]/$initiator_line}"
    prompt="${prompt//\[CONTEXT\]/$context}"

    echo "$prompt"
}

# ─── CREATE MODE ───

if [[ "$ACTION" == "create" ]]; then
    if [[ -z "$TOPIC" ]]; then
        echo "ERROR: --topic is required for --create" >&2
        exit 1
    fi
    if [[ -z "$OTHER_AGENT" ]]; then
        echo "ERROR: --other is required for --create" >&2
        exit 1
    fi

    # Load context from file if specified
    if [[ -n "$CONTEXT_FILE" ]]; then
        if [[ ! -f "$CONTEXT_FILE" ]]; then
            echo "ERROR: Context file not found: ${CONTEXT_FILE}" >&2
            exit 1
        fi
        CONTEXT=$(cat "$CONTEXT_FILE")
    fi

    # Create discussion via API
    CREATE_BODY=$(node -e '
const body = {
    topic: process.argv[1],
    created_by: process.argv[2],
    participants: [process.argv[2], process.argv[3]],
    channel: "discuss-" + Date.now(),
    mode: process.argv[4]
};
const ctx = process.argv[5];
if (ctx && ctx.trim()) body.context = ctx;
console.log(JSON.stringify(body));
' "$TOPIC" "$MY_AGENT" "$OTHER_AGENT" "$MODE" "$CONTEXT")

    RESPONSE=$(curl -s -X POST "${API_URL}/discussion/create" \
        -H "Authorization: Bearer ${API_KEY}" \
        -H "Content-Type: application/json" \
        -d "$CREATE_BODY")

    # Extract fields from response
    DISCUSSION_ID=$(echo "$RESPONSE" | node -e 'let d="";process.stdin.on("data",c=>d+=c);process.stdin.on("end",()=>{try{console.log(JSON.parse(d).id)}catch(e){console.error("Failed to parse response: "+d);process.exit(1)}})')
    CHANNEL=$(echo "$RESPONSE" | node -e 'let d="";process.stdin.on("data",c=>d+=c);process.stdin.on("end",()=>{try{console.log(JSON.parse(d).channel)}catch(e){process.exit(1)}})')

    if [[ -z "$DISCUSSION_ID" ]] || [[ "$DISCUSSION_ID" == "undefined" ]]; then
        echo "ERROR: Failed to create discussion. API response:" >&2
        echo "$RESPONSE" >&2
        exit 1
    fi

    # Set up working directory
    WORK_DIR="${TEMP_BASE}/discuss-${DISCUSSION_ID}"
    mkdir -p "${WORK_DIR}/inbox" "${WORK_DIR}/outbox"

    # Generate prompt
    PROMPT=$(generate_prompt "$DISCUSSION_ID" "$TOPIC" "$OTHER_AGENT" "$CONTEXT" "$WORK_DIR" "true")
    echo "$PROMPT" > "${WORK_DIR}/prompt.txt"

    # Output summary
    echo "=== Discussion Created ==="
    echo "Discussion ID: ${DISCUSSION_ID}"
    echo "Channel: ${CHANNEL}"
    echo "Mode: ${MODE}"
    echo "Working directory: ${WORK_DIR}"
    echo "Transport script: ${TRANSPORT_SCRIPT}"
    echo "Prompt written to: ${WORK_DIR}/prompt.txt"
    echo ""
    echo "Next steps:"
    echo "  1. Start the transport in a background shell:"
    echo "     bash ${TRANSPORT_SCRIPT} --api-url ${API_URL} --api-key ${API_KEY} --agent ${MY_AGENT} --other ${OTHER_AGENT} --dir ${WORK_DIR} --channel ${CHANNEL} --initiator"
    echo "  2. Launch a background Task subagent with the contents of prompt.txt (use max_turns: 50)"
    exit 0
fi

# ─── JOIN MODE ───

if [[ "$ACTION" == "join" ]]; then
    if [[ -z "$JOIN_ID" ]]; then
        echo "ERROR: --join requires a discussion ID" >&2
        exit 1
    fi

    # Join the discussion
    JOIN_RESPONSE=$(api_call "/discussion/join" "{\"discussion_id\": ${JOIN_ID}, \"agent\": \"${MY_AGENT}\"}")

    # Get discussion status
    STATUS_RESPONSE=$(api_call "/discussion/status" "{\"discussion_id\": ${JOIN_ID}}")

    # Extract fields as JSON, then parse each with a helper
    EXTRACTED=$(echo "$STATUS_RESPONSE" | node -e '
let d = "";
process.stdin.on("data", c => d += c);
process.stdin.on("end", () => {
    try {
        const data = JSON.parse(d);
        const disc = data.discussion;
        const myAgent = process.argv[1];
        const other = data.participants.find(p => p.agent !== myAgent);
        console.log(JSON.stringify({
            topic: disc.topic,
            channel: disc.channel || "discuss-" + disc.id,
            mode: disc.mode || "realtime",
            context: disc.context || "",
            other: other ? other.agent : "unknown"
        }));
    } catch (e) {
        console.error("Failed to parse status: " + d);
        process.exit(1);
    }
});' "$MY_AGENT")

    read_field() {
        echo "$EXTRACTED" | node -p "JSON.parse(require('fs').readFileSync(0,'utf8')).$1"
    }
    TOPIC=$(read_field topic)
    CHANNEL=$(read_field channel)
    MODE=$(read_field mode)
    CONTEXT=$(read_field context)
    OTHER_AGENT=$(read_field other)

    if [[ -z "$TOPIC" ]]; then
        echo "ERROR: Failed to get discussion details. API response:" >&2
        echo "$STATUS_RESPONSE" >&2
        exit 1
    fi

    # Set up working directory
    WORK_DIR="${TEMP_BASE}/discuss-${JOIN_ID}"
    mkdir -p "${WORK_DIR}/inbox" "${WORK_DIR}/outbox"

    # Generate prompt
    PROMPT=$(generate_prompt "$JOIN_ID" "$TOPIC" "$OTHER_AGENT" "$CONTEXT" "$WORK_DIR" "false")
    echo "$PROMPT" > "${WORK_DIR}/prompt.txt"

    # Output summary
    echo "=== Discussion Joined ==="
    echo "Discussion ID: ${JOIN_ID}"
    echo "Topic: ${TOPIC}"
    echo "Channel: ${CHANNEL}"
    echo "Mode: ${MODE}"
    echo "Other agent: ${OTHER_AGENT}"
    echo "Working directory: ${WORK_DIR}"
    echo "Transport script: ${TRANSPORT_SCRIPT}"
    echo "Prompt written to: ${WORK_DIR}/prompt.txt"
    echo ""
    echo "Next steps:"
    echo "  1. Start the transport in a background shell:"
    echo "     bash ${TRANSPORT_SCRIPT} --api-url ${API_URL} --api-key ${API_KEY} --agent ${MY_AGENT} --other ${OTHER_AGENT} --dir ${WORK_DIR} --channel ${CHANNEL}"
    echo "  2. Launch a background Task subagent with the contents of prompt.txt (use max_turns: 50)"
    exit 0
fi
