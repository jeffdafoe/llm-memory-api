#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const http = require('http');
const https = require('https');
const os = require('os');

// ---------------------------------------------------------------------------
// Argument parsing
// ---------------------------------------------------------------------------

const args = process.argv.slice(2);
const command = args[0]; // create | join

if (!command || !['create', 'join'].includes(command)) {
    console.error('Usage: node discuss.js create --topic "..." --other <agent> [--optional <agent2>]');
    console.error('       node discuss.js join [discussion-id]');
    process.exit(1);
}

let discussionId = null;
let topic = null;
const others = [];
const optionalParticipants = [];
let cliContext = null;
let contextFile = null;
let mode = 'realtime';
let mcpConfigPath = null;
let workDirBase = null;
let maxMessages = 200;
let timeoutMinutes = 120;
let joinTimeout = 300;
let cliAgent = null;
let cliPassphrase = null;
let cliApiUrl = null;
let configFilePath = null;

for (let i = 1; i < args.length; i++) {
    if (args[i] === '--topic' && args[i + 1]) {
        topic = args[i + 1];
        i++;
    } else if (args[i] === '--other' && args[i + 1]) {
        others.push(args[i + 1]);
        i++;
    } else if (args[i] === '--optional' && args[i + 1]) {
        optionalParticipants.push(args[i + 1]);
        i++;
    } else if (args[i] === '--context' && args[i + 1]) {
        cliContext = args[i + 1];
        i++;
    } else if (args[i] === '--context-file' && args[i + 1]) {
        contextFile = args[i + 1];
        i++;
    } else if (args[i] === '--mode' && args[i + 1]) {
        mode = args[i + 1];
        i++;
    } else if (args[i] === '--mcp-config' && args[i + 1]) {
        mcpConfigPath = args[i + 1];
        i++;
    } else if (args[i] === '--max-messages' && args[i + 1]) {
        maxMessages = parseInt(args[i + 1], 10);
        i++;
    } else if (args[i] === '--timeout' && args[i + 1]) {
        timeoutMinutes = parseInt(args[i + 1], 10);
        i++;
    } else if (args[i] === '--work-dir' && args[i + 1]) {
        workDirBase = args[i + 1];
        i++;
    } else if (args[i] === '--join-timeout' && args[i + 1]) {
        joinTimeout = parseInt(args[i + 1], 10);
        i++;
    } else if (args[i] === '--agent' && args[i + 1]) {
        cliAgent = args[i + 1];
        i++;
    } else if (args[i] === '--passphrase' && args[i + 1]) {
        cliPassphrase = args[i + 1];
        i++;
    } else if (args[i] === '--api-url' && args[i + 1]) {
        cliApiUrl = args[i + 1];
        i++;
    } else if (args[i] === '--config' && args[i + 1]) {
        configFilePath = args[i + 1];
        i++;
    } else if (command === 'join' && !discussionId && /^\d+$/.test(args[i])) {
        discussionId = parseInt(args[i], 10);
    }
}

// ---------------------------------------------------------------------------
// Config file — provides defaults for agent, passphrase, api_url, work_dir.
// CLI flags take precedence over config file values.
// ---------------------------------------------------------------------------

if (configFilePath) {
    try {
        const fileConfig = JSON.parse(fs.readFileSync(configFilePath, 'utf-8'));
        if (!cliAgent && fileConfig.agent) cliAgent = fileConfig.agent;
        if (!cliPassphrase && fileConfig.passphrase) cliPassphrase = fileConfig.passphrase;
        if (!cliApiUrl && fileConfig.api_url) cliApiUrl = fileConfig.api_url;
        if (!workDirBase && fileConfig.work_dir) workDirBase = fileConfig.work_dir;
    } catch (e) {
        console.error(`Failed to read config file ${configFilePath}: ${e.message}`);
        process.exit(1);
    }
}

// ---------------------------------------------------------------------------
// .mcp.json discovery
// ---------------------------------------------------------------------------

function findMcpConfig() {
    if (mcpConfigPath) {
        return mcpConfigPath;
    }
    let dir = process.cwd();
    while (true) {
        const candidate = path.join(dir, '.mcp.json');
        if (fs.existsSync(candidate)) {
            return candidate;
        }
        const parent = path.dirname(dir);
        if (parent === dir) break;
        dir = parent;
    }
    return null;
}

// If all credentials are provided via CLI/env, .mcp.json is optional.
// Otherwise, discover and parse .mcp.json for connection info.
let mcpEnv;
if (cliApiUrl && cliAgent && cliPassphrase) {
    // All credentials supplied directly — no .mcp.json needed
    mcpEnv = {
        MEMORY_API_URL: cliApiUrl,
        MEMORY_DEFAULT_AGENT: cliAgent,
        MEMORY_AGENT_PASSPHRASE: cliPassphrase,
    };
} else {
    const mcpPath = findMcpConfig();
    if (!mcpPath) {
        if (cliAgent && cliPassphrase) {
            console.error('Could not find .mcp.json and no --api-url provided.');
            console.error('Use --api-url https://memory.jeffdafoe.com/v1 or run from a project directory.');
        } else {
            console.error('Could not find .mcp.json — run from a project directory or use --mcp-config <path>');
        }
        process.exit(1);
    }

    const mcpConfig = JSON.parse(fs.readFileSync(mcpPath, 'utf-8'));
    const memoryServer = mcpConfig.mcpServers && mcpConfig.mcpServers['llm-memory'];
    if (!memoryServer) {
        console.error('.mcp.json does not contain an llm-memory server configuration');
        process.exit(1);
    }

    // Support both formats:
    //   Old (local server): { env: { MEMORY_API_URL, MEMORY_DEFAULT_AGENT, MEMORY_AGENT_PASSPHRASE, ... } }
    //   New (HTTP):         { type: "http", url: "https://host/mcp", headers: { Authorization: "Bearer ..." } }
    // For HTTP format, derive API URL from MCP URL and require --agent/--passphrase CLI args.
    if (memoryServer.env) {
        mcpEnv = memoryServer.env;
    } else if (memoryServer.type === 'http' && memoryServer.url) {
        // Derive REST API base URL: strip /mcp path, add /v1
        const mcpUrl = new URL(memoryServer.url);
        const apiUrl = `${mcpUrl.protocol}//${mcpUrl.host}/v1`;
        mcpEnv = {
            MEMORY_API_URL: apiUrl,
            MEMORY_DEFAULT_AGENT: cliAgent || process.env.MEMORY_DEFAULT_AGENT,
            MEMORY_AGENT_PASSPHRASE: cliPassphrase || process.env.MEMORY_AGENT_PASSPHRASE,
        };
        if (!mcpEnv.MEMORY_DEFAULT_AGENT || !mcpEnv.MEMORY_AGENT_PASSPHRASE) {
            console.error('HTTP-format .mcp.json detected — agent and passphrase required.');
            console.error('Use: --agent <name> --passphrase <phrase>');
            console.error('  or: MEMORY_DEFAULT_AGENT and MEMORY_AGENT_PASSPHRASE env vars');
            process.exit(1);
        }
    } else {
        console.error('.mcp.json llm-memory server: expected env block or type:"http" with url');
        process.exit(1);
    }
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

const cfg = {
    apiUrl: mcpEnv.MEMORY_API_URL,
    passphrase: cliPassphrase || mcpEnv.MEMORY_AGENT_PASSPHRASE,
    agent: cliAgent || mcpEnv.MEMORY_DEFAULT_AGENT,
    repoPath: mcpEnv.MEMORY_REPO_PATH || null,
    others: others,
    optionalParticipants: optionalParticipants,
    otherAgents: [...others, ...optionalParticipants],
    workDir: null, // set after discussion ID is known
    channel: 'discussion',
    mode: mode,
    topic: topic || '',
    context: cliContext || '',
    contextFile: contextFile || null,
    initiator: command === 'create',
    maxMessages: maxMessages,
    timeoutMinutes: timeoutMinutes,
    templatePath: path.join(__dirname, '..', '..', 'templates', 'discussion-prompt.tpl'),
    pollInterval: 5000,
    sendDelay: 3000,
    proxyPort: 0,
};

// Validate required fields
for (const field of ['apiUrl', 'passphrase', 'agent']) {
    if (!cfg[field]) {
        console.error(`Missing required config: ${field} (check .mcp.json)`);
        process.exit(1);
    }
}

// Session token obtained at login, used for all authenticated API calls
let sessionToken = null;

// ---------------------------------------------------------------------------
// Paths (initialized after workDir is determined)
// ---------------------------------------------------------------------------

let INBOX_DIR, OUTBOX_DIR, LOG_FILE, STATE_FILE, DONE_FILE, TRANSCRIPT_FILE;
let IDLE_TIMEOUT_FILE, PROMPT_FILE, POLL_SCRIPT;

function initPaths() {
    INBOX_DIR = path.join(cfg.workDir, 'inbox');
    OUTBOX_DIR = path.join(cfg.workDir, 'outbox');
    LOG_FILE = path.join(cfg.workDir, 'conversation.log');
    STATE_FILE = path.join(cfg.workDir, 'state.json');
    DONE_FILE = path.join(cfg.workDir, 'done');
    TRANSCRIPT_FILE = path.join(cfg.workDir, 'transcript.md');
    IDLE_TIMEOUT_FILE = path.join(cfg.workDir, 'idle-timeout');
    PROMPT_FILE = path.join(cfg.workDir, 'prompt.txt');
    POLL_SCRIPT = path.join(cfg.workDir, 'poll.sh');
}

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

let turnCount = 0;
const startTime = Date.now();
const seenIds = new Set();
let currentStatus = 'INIT';
let proxyPort = 0;
let lastDeliveredId = 0;
let readySignaled = false;

// Subagent death detection
let lastOutboxTime = Date.now();
let lastInboxDeliveryTime = 0;
let subagentDeathReported = false;
const SUBAGENT_DEAD_THRESHOLD_MS = 3 * 60 * 1000; // 3 minutes

// Stall detection
let stallWarningLogged = false;
const STALL_WARNING_MS = 90000;

// Server-side status polling (check every N poll cycles)
let statusCheckCounter = 0;
const STATUS_CHECK_INTERVAL = 10;

// ---------------------------------------------------------------------------
// Logging
// ---------------------------------------------------------------------------

function log(message) {
    const ts = new Date().toTimeString().slice(0, 8);
    const line = `[${ts}] ${message}`;
    if (LOG_FILE) {
        fs.appendFileSync(LOG_FILE, line + '\n');
    }
    process.stderr.write(line + '\n');
}

function writeState(overrides) {
    if (!STATE_FILE) return;
    const state = {
        status: currentStatus,
        pid: process.pid,
        port: proxyPort,
        heartbeat: new Date().toISOString(),
        turnCount,
        lastDeliveredId,
        seenIds: [...seenIds],
        ready: readySignaled,
        startedAt: new Date(startTime).toISOString(),
    };
    if (overrides) {
        Object.assign(state, overrides);
    }
    const tmp = STATE_FILE + '.tmp';
    fs.writeFileSync(tmp, JSON.stringify(state, null, 2));
    fs.renameSync(tmp, STATE_FILE);
}

function readState() {
    try {
        return JSON.parse(fs.readFileSync(STATE_FILE, 'utf-8'));
    } catch (e) {
        return null;
    }
}

function setStatus(status) {
    currentStatus = status;
    writeState();
}

function appendTranscript(speaker, message) {
    const ts = new Date().toTimeString().slice(0, 8);
    fs.appendFileSync(TRANSCRIPT_FILE, `**${speaker}** (${ts}):\n${message}\n\n`);
}

async function saveTranscript() {
    if (!cfg.repoPath || !TRANSCRIPT_FILE) return;
    if (!fs.existsSync(TRANSCRIPT_FILE)) return;

    const transcriptsDir = path.join(cfg.repoPath, cfg.agent, 'notes', 'discussions');
    if (!fs.existsSync(transcriptsDir)) {
        fs.mkdirSync(transcriptsDir, { recursive: true });
    }

    const dest = path.join(transcriptsDir, `discussion-${discussionId}.md`);
    fs.copyFileSync(TRANSCRIPT_FILE, dest);
    log(`Transcript saved to ${dest}`);

    try {
        const content = fs.readFileSync(dest, 'utf-8');
        const sourceFile = `${cfg.agent}/notes/discussions/discussion-${discussionId}.md`;
        await apiCall('memory/ingest', {
            namespace: cfg.agent,
            source_file: sourceFile,
            content: content,
        });
        log(`Transcript ingested into vector memory (${sourceFile})`);
    } catch (err) {
        log(`Transcript ingest failed (non-fatal): ${err.message}`);
    }
}

// Save the subagent's result.md as a remote note so it's searchable and
// accessible to both agents. Saved to the agent's own namespace under
// notes/discussions/discussion-<id>-result.md.
async function saveResult() {
    const resultFile = path.join(cfg.workDir, 'result.md');
    if (!fs.existsSync(resultFile)) {
        log('No result.md found — skipping result save');
        return;
    }

    const content = fs.readFileSync(resultFile, 'utf-8');
    if (!content.trim()) {
        log('result.md is empty — skipping result save');
        return;
    }

    const slug = `notes/discussions/discussion-${discussionId}-result`;
    const title = `Discussion #${discussionId} Result — ${cfg.topic || 'untitled'}`;

    try {
        await apiCall('documents/save', {
            namespace: cfg.agent,
            slug: slug,
            title: title,
            content: content,
        });
        log(`Result saved as remote note: ${cfg.agent}/${slug}`);
    } catch (err) {
        log(`Result save failed (non-fatal): ${err.message}`);
    }
}

// ---------------------------------------------------------------------------
// API client
// ---------------------------------------------------------------------------

// Single HTTP request (no retry)
function httpPostOnce(url, headers, body) {
    return new Promise((resolve, reject) => {
        const urlObj = new URL(url);
        const mod = urlObj.protocol === 'https:' ? https : http;
        const data = JSON.stringify(body);

        const allHeaders = {
            'Content-Type': 'application/json',
            'Content-Length': Buffer.byteLength(data),
            ...headers,
        };

        const req = mod.request(urlObj, { method: 'POST', headers: allHeaders }, (res) => {
            let responseBody = '';
            res.on('data', (chunk) => responseBody += chunk);
            res.on('end', () => {
                if (res.statusCode >= 200 && res.statusCode < 300) {
                    try {
                        resolve(JSON.parse(responseBody));
                    } catch (e) {
                        resolve(responseBody);
                    }
                } else {
                    reject(new Error(`HTTP ${res.statusCode}: ${responseBody}`));
                }
            });
        });

        req.on('error', (e) => reject(e));
        req.write(data);
        req.end();
    });
}

const RETRYABLE_CODES = new Set(['ECONNRESET', 'ECONNREFUSED', 'ETIMEDOUT', 'ENOTFOUND', 'EAI_AGAIN', 'EPIPE']);
const RETRYABLE_STATUS = new Set([502, 503, 504]);
const MAX_RETRIES = 3;
const BASE_DELAY_MS = 1000;

function isRetryable(err) {
    if (RETRYABLE_CODES.has(err.code)) return true;
    const match = err.message && err.message.match(/^HTTP (\d+):/);
    if (match && RETRYABLE_STATUS.has(parseInt(match[1]))) return true;
    return false;
}

async function httpPost(url, headers, body) {
    let lastErr;
    for (let attempt = 0; attempt <= MAX_RETRIES; attempt++) {
        try {
            return await httpPostOnce(url, headers, body);
        } catch (err) {
            lastErr = err;
            if (attempt < MAX_RETRIES && isRetryable(err)) {
                const delay = BASE_DELAY_MS * Math.pow(2, attempt);
                log(`Retryable error (attempt ${attempt + 1}/${MAX_RETRIES + 1}): ${err.message} — retrying in ${delay}ms`);
                await new Promise(r => setTimeout(r, delay));
            }
        }
    }
    throw lastErr;
}

function apiCallNoAuth(endpoint, body) {
    return httpPost(cfg.apiUrl + '/' + endpoint, {}, body);
}

async function apiCall(endpoint, body) {
    if (!sessionToken) {
        throw new Error('Not logged in — no session token available');
    }
    try {
        return await httpPost(
            cfg.apiUrl + '/' + endpoint,
            { 'Authorization': 'Bearer ' + sessionToken },
            body
        );
    } catch (err) {
        const match = err.message && err.message.match(/^HTTP (\d+):/);
        if (match && (match[1] === '401' || match[1] === '403')) {
            log('Session expired or invalid — re-logging in');
            await transportLogin();
            return httpPost(
                cfg.apiUrl + '/' + endpoint,
                { 'Authorization': 'Bearer ' + sessionToken },
                body
            );
        }
        throw err;
    }
}

async function transportLogin() {
    const data = await apiCallNoAuth('agent/login', {
        agent: cfg.agent,
        passphrase: cfg.passphrase,
        subsystem: 'discussion',
    });
    sessionToken = data.session_token;
    log(`Logged in as ${cfg.agent} (session expires ${data.expires_at})`);
}

async function transportLogout() {
    if (!sessionToken) return;
    try {
        await apiCall('agent/logout', { agent: cfg.agent });
        log('Logged out');
    } catch (err) {
        log(`Logout error (non-fatal): ${err.message}`);
    }
    sessionToken = null;
}

// ---------------------------------------------------------------------------
// Transport: receive messages
// ---------------------------------------------------------------------------

async function receiveMessages() {
    const body = { agent: cfg.agent, channel: cfg.channel };

    // Use after_id for crash recovery
    if (lastDeliveredId > 0) {
        body.after_id = lastDeliveredId;
    }

    try {
        const response = await apiCall('chat/receive', body);
        return response.messages || [];
    } catch (err) {
        log(`ERROR receiving: ${err.message}`);
        return [];
    }
}

async function ackMessages(ids) {
    if (ids.length === 0) return;
    try {
        await apiCall('chat/ack', { agent: cfg.agent, message_ids: ids });
    } catch (err) {
        log(`ERROR acking: ${err.message}`);
    }
}

async function sendMessage(message) {
    try {
        await apiCall('chat/send', {
            from_agent: cfg.agent,
            discussion_id: discussionId,
            message: message,
            channel: cfg.channel,
        });
        log(`SENT: ${message.slice(0, 100)}...`);
        appendTranscript(cfg.agent, message);
        turnCount++;
    } catch (err) {
        log(`ERROR sending: ${err.message}`);
    }
}

// ---------------------------------------------------------------------------
// Transport: inbox/outbox file operations
// ---------------------------------------------------------------------------

function processReceivedMessages(messages) {
    const allIds = [];
    const newIds = [];

    for (const msg of messages) {
        allIds.push(msg.id);
        if (seenIds.has(msg.id)) continue;

        seenIds.add(msg.id);
        newIds.push(msg.id);

        const inboxContent = `From: ${msg.from_agent}\nSent: ${msg.sent_at}\n\n${msg.message}`;
        fs.writeFileSync(path.join(INBOX_DIR, `${msg.id}.txt`), inboxContent);
        appendTranscript(msg.from_agent, msg.message);
        log(`RECEIVED: id=${msg.id} from=${msg.from_agent}`);
    }

    return { allIds, newIds };
}

function checkOutbox() {
    let files;
    try {
        files = fs.readdirSync(OUTBOX_DIR).filter(f => f.endsWith('.txt')).sort();
    } catch (e) {
        return false;
    }

    for (const file of files) {
        const filePath = path.join(OUTBOX_DIR, file);
        const message = fs.readFileSync(filePath, 'utf-8');
        fs.unlinkSync(filePath);

        if (message.trim()) {
            // sendMessage is async but we return a promise indicator
            return message;
        }
    }

    return false;
}

// ---------------------------------------------------------------------------
// Transport: delayed ack — wait for subagent to consume inbox files
// ---------------------------------------------------------------------------

function waitForSubagentReads(newIds, timeoutMs = 120000) {
    return new Promise((resolve) => {
        if (newIds.length === 0) {
            resolve();
            return;
        }

        const start = Date.now();
        const check = () => {
            const allDeleted = newIds.every(id => !fs.existsSync(path.join(INBOX_DIR, `${id}.txt`)));

            if (allDeleted) {
                log('All inbox files consumed by subagent');
                resolve();
                return;
            }

            if (Date.now() - start >= timeoutMs) {
                log(`WARNING: Delayed ack timeout (${timeoutMs / 1000}s) — acking unconsumed messages`);
                resolve();
                return;
            }

            setTimeout(check, 2000);
        };

        check();
    });
}

// ---------------------------------------------------------------------------
// Transport: vote polling
// ---------------------------------------------------------------------------

const seenVoteIds = new Set();

async function checkPendingVotes() {
    try {
        const response = await apiCall('discussion/pending', { agent: cfg.agent, discussion_id: discussionId });
        const votes = response.open_votes || [];

        for (const vote of votes) {
            if (seenVoteIds.has(vote.id)) continue;
            seenVoteIds.add(vote.id);

            const notice = `[VOTE PENDING] Vote #${vote.id} (${vote.type}, ${vote.threshold}): ${vote.question}`;
            fs.writeFileSync(path.join(INBOX_DIR, `vote-pending-${vote.id}.txt`), notice);
            log(`VOTE NOTIFICATION: vote #${vote.id}`);
        }
    } catch (err) {
        log(`ERROR checking votes: ${err.message}`);
    }
}

// ---------------------------------------------------------------------------
// Transport: timeout checks
// ---------------------------------------------------------------------------

function checkDone() {
    return fs.existsSync(DONE_FILE);
}

function checkIdleTimeout() {
    return fs.existsSync(IDLE_TIMEOUT_FILE);
}

function checkTimeout() {
    const elapsedMinutes = (Date.now() - startTime) / 60000;
    if (elapsedMinutes >= cfg.timeoutMinutes) return true;
    if (turnCount >= cfg.maxMessages) return true;
    return false;
}

// ---------------------------------------------------------------------------
// Local proxy server
// ---------------------------------------------------------------------------

function startProxyServer() {
    return new Promise((resolve) => {
        const server = http.createServer(async (req, res) => {
            if (req.method !== 'POST') {
                res.writeHead(405);
                res.end(JSON.stringify({ error: 'Method not allowed' }));
                return;
            }

            let body = '';
            req.on('data', (chunk) => body += chunk);
            req.on('end', async () => {
                let parsed = {};
                try {
                    if (body) parsed = JSON.parse(body);
                } catch (e) {
                    res.writeHead(400);
                    res.end(JSON.stringify({ error: 'Invalid JSON' }));
                    return;
                }

                try {
                    let result;
                    const urlPath = req.url;

                    if (urlPath === '/vote/propose') {
                        // If proposing a conclude vote, check for an existing one first
                        if ((parsed.type || 'general') === 'conclude') {
                            const pending = await apiCall('discussion/pending', { agent: cfg.agent, discussion_id: discussionId });
                            const existingConclude = (pending.open_votes || []).find(v => v.type === 'conclude');
                            if (existingConclude) {
                                log(`Found existing conclude vote #${existingConclude.id}, casting yes instead of proposing`);
                                result = await apiCall('discussion/vote/cast', {
                                    vote_id: existingConclude.id,
                                    agent: cfg.agent,
                                    choice: 1,
                                    reason: 'Auto-agreed to existing conclude vote',
                                });
                                // Mark as seen so checkPendingVotes doesn't re-notify
                                seenVoteIds.add(existingConclude.id);
                                res.writeHead(200, { 'Content-Type': 'application/json' });
                                res.end(JSON.stringify(result));
                                return;
                            }
                        }
                        result = await apiCall('discussion/vote/propose', {
                            discussion_id: discussionId,
                            proposed_by: cfg.agent,
                            question: parsed.question,
                            type: parsed.type || 'general',
                            threshold: parsed.threshold || 'unanimous',
                        });
                    } else if (urlPath === '/vote/cast') {
                        result = await apiCall('discussion/vote/cast', {
                            vote_id: parsed.vote_id,
                            agent: cfg.agent,
                            choice: parsed.choice,
                            reason: parsed.reason || undefined,
                        });
                        try {
                            const reasonText = parsed.reason ? ` (${parsed.reason})` : '';
                            appendTranscript('system', `${cfg.agent} voted ${parsed.choice} on vote #${parsed.vote_id}${reasonText}`);
                        } catch (err) {
                            log(`Transcript append failed after vote/cast (non-fatal): ${err.message}`);
                        }
                    } else if (urlPath === '/vote/status') {
                        result = await apiCall('discussion/vote/status', {
                            vote_id: parsed.vote_id,
                        });
                    } else if (urlPath === '/pending') {
                        result = await apiCall('discussion/pending', {
                            agent: cfg.agent,
                        });
                    } else if (urlPath === '/conclude') {
                        try {
                            appendTranscript('system', `${cfg.agent} concluded the discussion`);
                        } catch (err) {
                            log(`Transcript append failed before conclude (non-fatal): ${err.message}`);
                        }
                        result = await apiCall('discussion/conclude', {
                            discussion_id: discussionId,
                            agent: cfg.agent,
                        });
                    } else if (urlPath === '/status') {
                        // Return transport status from state.json
                        const state = readState() || {};
                        result = Object.assign({
                            agent: cfg.agent,
                            otherAgents: cfg.otherAgents,
                            discussionId: discussionId,
                            elapsedMinutes: Math.round((Date.now() - startTime) / 60000),
                        }, state);
                    } else {
                        res.writeHead(404);
                        res.end(JSON.stringify({ error: `Unknown endpoint: ${urlPath}` }));
                        return;
                    }

                    res.writeHead(200, { 'Content-Type': 'application/json' });
                    res.end(JSON.stringify(result));
                } catch (err) {
                    log(`Proxy error ${req.url}: ${err.message}`);
                    res.writeHead(502);
                    res.end(JSON.stringify({ error: err.message }));
                }
            });
        });

        server.listen(cfg.proxyPort, '127.0.0.1', () => {
            const port = server.address().port;
            proxyPort = port;
            writeState();
            log(`Proxy server listening on 127.0.0.1:${port}`);
            resolve({ server, port });
        });
    });
}

// ---------------------------------------------------------------------------
// Setup: create mode
// ---------------------------------------------------------------------------

async function setupCreate() {
    if (cfg.contextFile) {
        cfg.context = fs.readFileSync(cfg.contextFile, 'utf-8');
    }
    let createContext = cfg.context || '';

    const participants = [cfg.agent, ...cfg.others];

    // Create discussion via API — if the server returns 409 (agent already
    // in an active discussion), extract the existing ID and join that instead
    let result;
    try {
        const createBody = {
            topic: cfg.topic,
            participants: participants,
            created_by: cfg.agent,
            mode: cfg.mode,
            context: createContext || undefined,
        };
        if (cfg.optionalParticipants.length > 0) {
            createBody.optional_participants = cfg.optionalParticipants;
        }
        result = await apiCall('discussion/create', createBody);
    } catch (err) {
        if (err.message && err.message.includes('DISCUSSION_CONFLICT')) {
            const idMatch = err.message.match(/discussion #(\d+)/);
            if (idMatch) {
                const staleId = parseInt(idMatch[1], 10);
                log(`Create rejected — already in discussion #${staleId}`);

                // Check the stale discussion's status before deciding what to do
                let staleStatus = null;
                try {
                    const statusResult = await apiCall('discussion/status', { discussion_id: staleId });
                    staleStatus = statusResult.discussion.status;
                    log(`Discussion #${staleId} status: ${staleStatus}`);
                } catch (statusErr) {
                    log(`Could not check status of #${staleId}: ${statusErr.message}`);
                }

                // If the stale discussion is already dead, leave it and retry create
                if (staleStatus && !['waiting', 'active'].includes(staleStatus)) {
                    log(`Discussion #${staleId} is ${staleStatus} — leaving and retrying create`);
                    try {
                        await apiCall('discussion/leave', { discussion_id: staleId, agent: cfg.agent });
                        log(`Left stale discussion #${staleId}`);
                    } catch (leaveErr) {
                        log(`Leave #${staleId} failed: ${leaveErr.message}`);
                    }
                } else {
                    // Discussion is still active/waiting — try to join it
                    discussionId = staleId;
                    log(`Discussion #${staleId} is still ${staleStatus || 'unknown'} — trying to join`);
                    try {
                        return await setupJoin();
                    } catch (joinErr) {
                        log(`Join #${staleId} failed (${joinErr.message}), leaving and retrying create`);
                        try {
                            await apiCall('discussion/leave', { discussion_id: staleId, agent: cfg.agent });
                            log(`Left stale discussion #${staleId}`);
                        } catch (leaveErr) {
                            log(`Leave #${staleId} failed: ${leaveErr.message}`);
                        }
                    }
                }

                // Retry create after clearing the stale discussion
                discussionId = null;
                const retryBody = {
                    topic: cfg.topic,
                    participants: participants,
                    created_by: cfg.agent,
                    mode: cfg.mode,
                    context: createContext || undefined,
                };
                if (cfg.optionalParticipants.length > 0) {
                    retryBody.optional_participants = cfg.optionalParticipants;
                }
                result = await apiCall('discussion/create', retryBody);
                log(`Created discussion #${result.discussion.id} (after clearing stale #${staleId})`);
            }
        }
        if (!result) {
            throw err;
        }
    }

    discussionId = result.discussion.id;
    cfg.channel = `discuss-${discussionId}`;
    cfg.otherAgents = [...cfg.others, ...cfg.optionalParticipants];

    log(`Created discussion #${discussionId}: ${cfg.topic} (status: ${result.discussion.status})`);
    log(`Channel: ${cfg.channel}, timeout_at: ${result.discussion.timeout_at}`);

    return discussionId;
}

// ---------------------------------------------------------------------------
// Setup: join mode
// ---------------------------------------------------------------------------

async function setupJoin() {
    // Fetch discussion details
    const result = await apiCall('discussion/status', {
        discussion_id: discussionId,
    });

    const discussion = result.discussion;
    cfg.topic = discussion.topic;
    cfg.channel = discussion.channel || `discuss-${discussionId}`;
    cfg.context = discussion.context || '';
    if (cfg.contextFile) {
        cfg.context = fs.readFileSync(cfg.contextFile, 'utf-8');
    }
    cfg.mode = discussion.mode || 'realtime';

    // Find other participants
    const participants = result.participants || [];
    cfg.otherAgents = participants.filter(p => p.agent !== cfg.agent).map(p => p.agent);

    // Join the discussion
    const joinResult = await apiCall('discussion/join', {
        discussion_id: discussionId,
        agent: cfg.agent,
    });

    log(`Joined discussion #${discussionId}: ${cfg.topic} (status: ${joinResult.discussion_status})`);
    log(`Channel: ${cfg.channel}, Others: ${cfg.otherAgents.join(', ')}`);

    return discussionId;
}

// ---------------------------------------------------------------------------
// Readiness: wait for discussion to transition from waiting to active
// ---------------------------------------------------------------------------

async function waitForReady() {
    // Check current status first
    const initial = await apiCall('discussion/status', { discussion_id: discussionId });
    if (initial.discussion.status === 'active') {
        log('Discussion is already active');
        return;
    }

    if (initial.discussion.status !== 'waiting') {
        throw new Error('Discussion is ' + initial.discussion.status + ' — cannot start transport');
    }

    log('Discussion is waiting for participants...');
    setStatus('WAITING');

    while (true) {
        await sleep(cfg.pollInterval);

        const result = await apiCall('discussion/status', { discussion_id: discussionId });
        const status = result.discussion.status;

        if (status === 'active') {
            log('Discussion is now active — all participants ready');
            return;
        }

        if (status === 'timed_out') {
            throw new Error('Discussion timed out waiting for required participants');
        }

        if (status === 'concluded' || status === 'cancelled') {
            throw new Error('Discussion was ' + status + ' before it started');
        }

        // Still waiting
    }
}

// ---------------------------------------------------------------------------
// Prompt generation
// ---------------------------------------------------------------------------

function generatePrompt(proxyPort) {
    let template;
    try {
        template = fs.readFileSync(cfg.templatePath, 'utf-8');
    } catch (err) {
        log(`WARNING: Could not read template at ${cfg.templatePath}: ${err.message}`);
        return null;
    }

    // Build human-readable participant list for the prompt
    let otherAgentsStr;
    if (cfg.otherAgents && cfg.otherAgents.length > 0) {
        if (cfg.otherAgents.length === 1) {
            otherAgentsStr = `the "${cfg.otherAgents[0]}" agent`;
        } else {
            const quoted = cfg.otherAgents.map(a => `"${a}"`);
            otherAgentsStr = `agents ${quoted.join(', ')}`;
        }
    } else {
        otherAgentsStr = 'other agents';
    }

    // Extract communication standards from GUIDELINES.md
    let guidelines = '';
    if (cfg.repoPath) {
        try {
            const guidelinesPath = path.join(cfg.repoPath, 'shared', 'GUIDELINES.md');
            const content = fs.readFileSync(guidelinesPath, 'utf-8');
            const match = content.match(/# Cross-Agent Communication Standards[\s\S]*?(?=\n## Relaying|$)/);
            if (!match) {
                const altMatch = content.match(/# Mailbox Communication Guidelines[\s\S]*?(?=\n## Relaying|$)/);
                if (altMatch) {
                    guidelines = altMatch[0].trim();
                }
            } else {
                guidelines = match[0].trim();
            }
        } catch (err) {
            log(`WARNING: Could not read GUIDELINES.md: ${err.message}`);
        }
    }

    const replacements = {
        '[OTHER_AGENTS]': otherAgentsStr,
        '[TOPIC]': cfg.topic,
        '[WORK_DIR]': cfg.workDir,
        '[DISCUSSION_ID]': String(discussionId),
        '[API_URL]': `http://127.0.0.1:${proxyPort}`,
        '[API_KEY]': '', // local proxy handles auth — subagent doesn't need credentials
        '[MY_AGENT]': cfg.agent,
        '[CONTEXT]': cfg.context || '(none)',
        '[GUIDELINES]': guidelines,
        '[FIRST_CONTACT]': cfg.initiator
            ? `## First Contact

You are the CREATOR of this discussion. Send your opening message immediately after completing
your setup steps (defining helper functions). Do not wait for the other side to message first.
Your opening message should introduce the topic and your initial position.

If no reply arrives within 90 seconds of your opening message, write a diagnostic to outbox:
"Sent opening message but no reply yet. Is the other participant's transport running?"

If no contact from the other side after 5 minutes total, write "No contact after 5 minutes.
Exiting." to outbox and exit.`
            : `## First Contact

You are JOINING this discussion. After completing your setup steps (defining helper functions),
begin your polling loop. Your inbox may be empty initially — this is normal, the creator may
still be launching.

If no message arrives within 90 seconds of your first poll, write a diagnostic to outbox:
"Joined and waiting — is the creator side running?"

If no contact after 5 minutes total, write "No contact after 5 minutes. Exiting." to outbox
and exit.`,
    };

    let prompt = template;
    for (const [placeholder, value] of Object.entries(replacements)) {
        prompt = prompt.split(placeholder).join(value);
    }

    fs.writeFileSync(PROMPT_FILE, prompt);
    log(`Prompt written to ${PROMPT_FILE}`);

    return prompt;
}

// ---------------------------------------------------------------------------
// Main transport loop
// ---------------------------------------------------------------------------

async function runTransport() {
    log(`Discussion transport starting: ${cfg.agent} <-> ${cfg.otherAgents.join(', ')}`);
    log(`Work dir: ${cfg.workDir}`);
    setStatus('POLLING');

    while (true) {
        touchLock();

        // 1. Check done
        if (checkDone()) {
            log('Done file detected. Discussion concluded.');
            setStatus('DONE');
            return;
        }

        // 2. Check idle timeout
        if (checkIdleTimeout()) {
            log('Idle timeout detected. Shutting down.');
            setStatus('IDLE_TIMEOUT');
            return;
        }

        // 3. Check timeout
        if (checkTimeout()) {
            log(`Timeout reached (${turnCount} turns, ${cfg.timeoutMinutes}m). Writing timeout notice.`);
            fs.writeFileSync(path.join(INBOX_DIR, 'timeout.txt'), '[SYSTEM] Discussion timeout reached. Please wrap up.');
            setStatus('TIMEOUT');
            await sleep(30000);
            const outMsg = checkOutbox();
            if (outMsg) {
                await sendMessage(outMsg);
            }
            setStatus('DONE');
            fs.writeFileSync(DONE_FILE, 'timeout');
            return;
        }

        // 4. Check outbox
        const outMsg = checkOutbox();
        if (outMsg) {
            const sizeKB = (Buffer.byteLength(outMsg, 'utf-8') / 1024).toFixed(1);
            const responseTime = (lastInboxDeliveryTime > 0)
                ? Math.round((Date.now() - lastInboxDeliveryTime) / 1000)
                : null;
            if (responseTime !== null) {
                log(`Outbox: ${sizeKB}KB, response time: ${responseTime}s`);
            } else {
                log(`Outbox: ${sizeKB}KB`);
            }
            lastOutboxTime = Date.now();
            stallWarningLogged = false;
            subagentDeathReported = false;
            await sendMessage(outMsg);
            await sleep(cfg.sendDelay);
        }

        // 5. Poll for incoming messages
        const messages = await receiveMessages();

        // 6-7. Process: dedup, write inbox, transcript
        const { allIds, newIds } = processReceivedMessages(messages);

        // 8. Poll pending votes
        await checkPendingVotes();

        // 9. Periodic server-side status check
        statusCheckCounter++;
        if (statusCheckCounter >= STATUS_CHECK_INTERVAL) {
            statusCheckCounter = 0;
            try {
                const statusResult = await apiCall('discussion/status', { discussion_id: discussionId });
                const serverStatus = statusResult.discussion.status;
                if (serverStatus === 'cancelled' || serverStatus === 'concluded') {
                    log(`Discussion ${serverStatus} server-side. Notifying subagent and shutting down.`);
                    const notice = `[SYSTEM] This discussion has been ${serverStatus}. Please wrap up and write your result file.`;
                    fs.writeFileSync(path.join(INBOX_DIR, `${serverStatus}.txt`), notice);
                    appendTranscript('system', `Discussion ${serverStatus} server-side`);
                    await sleep(15000);
                    const outMsg = checkOutbox();
                    if (outMsg) {
                        await sendMessage(outMsg);
                    }
                    fs.writeFileSync(DONE_FILE, serverStatus);
                    setStatus('DONE');
                    return;
                }
            } catch (err) {
                log(`Status check failed: ${err.message}`);
            }
        }

        if (allIds.length > 0) {
            // 10. Delayed ack
            if (newIds.length > 0) {
                await waitForSubagentReads(newIds);
            }

            // 11. Ack all
            await ackMessages(allIds);

            // 12. Record last-delivered-id
            const maxId = Math.max(...allIds);
            if (isFinite(maxId)) {
                lastDeliveredId = maxId;
            }

            lastInboxDeliveryTime = Date.now();

            if (!readySignaled) {
                readySignaled = true;
                log('Received first message — ready signaled');
            }

            setStatus('MESSAGE_RECEIVED');
        } else {
            await sleep(cfg.pollInterval);
        }

        // Stall warning: no outbox response for a while after inbox delivery
        if (!stallWarningLogged
            && lastInboxDeliveryTime > 0
            && lastInboxDeliveryTime > lastOutboxTime
            && (Date.now() - lastInboxDeliveryTime) > STALL_WARNING_MS) {
            stallWarningLogged = true;
            log(`WARNING: No outbox response ${Math.round((Date.now() - lastInboxDeliveryTime) / 1000)}s after inbox delivery`);
        }

        // Check for dead subagent: inbox has unprocessed messages but no
        // outbox activity for SUBAGENT_DEAD_THRESHOLD_MS. Report once.
        if (!subagentDeathReported
            && lastInboxDeliveryTime > 0
            && lastInboxDeliveryTime > lastOutboxTime
            && (Date.now() - lastInboxDeliveryTime) > SUBAGENT_DEAD_THRESHOLD_MS) {
            subagentDeathReported = true;
            log('Subagent appears dead — no outbox activity after inbox delivery');
            reportSubagentDead().catch(err => {
                log(`Failed to report subagent death: ${err.message}`);
            });
        }

    }
}

// Report subagent death to the error reporting endpoint
async function reportSubagentDead() {
    const elapsedMinutes = Math.round((Date.now() - startTime) / 60000);
    const silentMinutes = Math.round((Date.now() - lastOutboxTime) / 60000);
    const context = {
        discussion_id: discussionId,
        agent: cfg.agent,
        messages_sent: turnCount,
        elapsed_minutes: elapsedMinutes,
        silent_minutes: silentMinutes,
        last_outbox_activity: new Date(lastOutboxTime).toISOString(),
        last_inbox_delivery: new Date(lastInboxDeliveryTime).toISOString(),
        max_turns_budget: 100,
    };
    log(`Subagent death detected: ${turnCount} messages sent, ${elapsedMinutes}m elapsed, ${silentMinutes}m silent`);
    const result = await apiCall('system/error/report', {
        source: 'discuss-transport',
        error_code: 'SUBAGENT_DEAD',
        context: context,
    });
    log(`Subagent death reported (id: ${result.id}, status: ${result.status})`);
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function sleep(ms) {
    return new Promise(resolve => setTimeout(resolve, ms));
}

function ensureDirectories() {
    fs.mkdirSync(INBOX_DIR, { recursive: true });
    fs.mkdirSync(OUTBOX_DIR, { recursive: true });
}

// Prevent two transport instances from running for the same discussion.
// Uses a PID lockfile — if an existing lock points to a live process, refuse to start.
let LOCK_FILE = null;

const LOCK_STALE_MS = 30000;

function acquireLock() {
    LOCK_FILE = path.join(cfg.workDir, '.lock');
    const lockInfoFile = path.join(LOCK_FILE, 'info.json');
    try {
        fs.mkdirSync(LOCK_FILE);
    } catch (e) {
        if (e.code !== 'EEXIST') throw e;
        // Lock dir exists — check if stale via mtime
        try {
            const stat = fs.statSync(lockInfoFile);
            const age = Date.now() - stat.mtimeMs;
            if (age < LOCK_STALE_MS) {
                const info = JSON.parse(fs.readFileSync(lockInfoFile, 'utf-8'));
                console.error(`Another transport (PID ${info.pid}) is already running for discussion #${discussionId}`);
                process.exit(1);
            }
            log(`Stale lock (${Math.round(age / 1000)}s old) — cleaning up`);
        } catch (statErr) {
            log('Lock dir exists but info unreadable — cleaning up');
        }
        // Remove stale lock and recreate
        try { fs.unlinkSync(lockInfoFile); } catch (e2) { /* ignore */ }
        try { fs.rmdirSync(LOCK_FILE); } catch (e2) { /* ignore */ }
        fs.mkdirSync(LOCK_FILE);
    }
    fs.writeFileSync(lockInfoFile, JSON.stringify({ pid: process.pid, started: Date.now() }));
    log(`Acquired lock (PID ${process.pid})`);
}

function touchLock() {
    try {
        const lockInfoFile = path.join(LOCK_FILE, 'info.json');
        const now = new Date();
        fs.utimesSync(lockInfoFile, now, now);
    } catch (e) {
        // Best effort
    }
}

function releaseLock() {
    try {
        if (LOCK_FILE && fs.existsSync(LOCK_FILE)) {
            const lockInfoFile = path.join(LOCK_FILE, 'info.json');
            try { fs.unlinkSync(lockInfoFile); } catch (e) { /* ignore */ }
            fs.rmdirSync(LOCK_FILE);
        }
    } catch (e) {
        // Best effort
    }
}

function writePollScript() {
    const workDir = cfg.workDir.replace(/\\/g, '/');
    const script = `#!/bin/bash
# Poll for new inbox messages, done file, or idle timeout.
# Outputs: NEW_MESSAGES, DONE, or IDLE_TIMEOUT
idle_count=0
while true; do
  files=$(ls ${workDir}/inbox/*.txt 2>/dev/null)
  if [ -n "$files" ]; then echo "NEW_MESSAGES"; echo "$files"; break; fi
  if [ -f ${workDir}/done ]; then echo "DONE"; break; fi
  idle_count=$((idle_count + 1))
  if [ $idle_count -ge 60 ]; then echo "IDLE_TIMEOUT"; break; fi
  sleep 5
done
`;
    fs.writeFileSync(POLL_SCRIPT, script);
}

function initTranscript() {
    const header = [
        `# Discussion: ${cfg.agent} <-> ${cfg.otherAgents.join(', ')}`,
        '',
        `Started: ${new Date().toISOString().replace('T', ' ').slice(0, 19)}`,
        '',
        '---',
        '',
    ].join('\n');
    fs.writeFileSync(TRANSCRIPT_FILE, header);
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

async function main() {
    try {
        // Login to get session token before any API calls
        await transportLogin();

        // Setup phase based on command
        if (command === 'create') {
            if (cfg.others.length === 0) {
                console.error('create requires at least one --other participant');
                process.exit(1);
            }
            if (!cfg.topic) {
                console.error('create requires --topic');
                process.exit(1);
            }
            await setupCreate();
        } else if (command === 'join') {
            // Auto-discover discussion ID from pending invitations
            if (!discussionId) {
                // Retry loop — the other agent may not have created the discussion yet
                const pollIntervalMs = 5000;
                const maxAttempts = Math.ceil((joinTimeout * 1000) / pollIntervalMs);
                let invitations = [];

                for (let attempt = 1; attempt <= maxAttempts; attempt++) {
                    const pending = await apiCall('discussion/pending', { agent: cfg.agent });
                    invitations = pending.invited_discussions || [];
                    if (invitations.length > 0) break;

                    if (attempt === maxAttempts) {
                        console.error(`No pending discussion invitations found after ${joinTimeout}s`);
                        process.exit(1);
                    }
                    console.error(`[${new Date().toTimeString().slice(0, 8)}] No invitations yet, retrying (${attempt}/${maxAttempts})...`);
                    await new Promise(resolve => setTimeout(resolve, pollIntervalMs));
                }

                if (invitations.length > 1) {
                    console.error('Multiple pending invitations:');
                    for (const inv of invitations) {
                        console.error(`  #${inv.id}: ${inv.topic} (${inv.status})`);
                    }
                    console.error('Specify one: node discuss.js join <id>');
                    process.exit(1);
                }
                discussionId = invitations[0].id;
                log(`Auto-discovered pending invitation: discussion #${discussionId}`);
            }
            await setupJoin();
        }

        // Set workDir now that we have the discussion ID.
        // Priority: --work-dir CLI flag > MEMORY_TEMP_DIR from .mcp.json > os.tmpdir()/llm
        if (workDirBase) {
            cfg.workDir = path.join(workDirBase, `discuss-${discussionId}`);
        } else if (mcpEnv.MEMORY_TEMP_DIR) {
            cfg.workDir = path.join(mcpEnv.MEMORY_TEMP_DIR, `discuss-${discussionId}`);
        } else {
            const tmpBase = path.join(os.tmpdir(), 'llm');
            cfg.workDir = path.join(tmpBase, `discuss-${discussionId}`);
        }
        initPaths();
        ensureDirectories();
        acquireLock();
        writePollScript();
        setStatus('STARTING');

        // Wait for all participants to join before proceeding
        await waitForReady();

        initTranscript();

        // Load state from previous run if resuming
        const prevState = readState();
        if (prevState) {
            if (Array.isArray(prevState.seenIds)) {
                for (const id of prevState.seenIds) {
                    seenIds.add(id);
                }
            }
            if (prevState.lastDeliveredId > 0) {
                lastDeliveredId = prevState.lastDeliveredId;
            }
        }

        // Start local proxy server
        const { server, port } = await startProxyServer();

        // Generate prompt
        generatePrompt(port);

        // Log summary
        log(`Discussion #${discussionId} ready`);
        log(`Mode: ${command}, Agent: ${cfg.agent}, Others: ${cfg.otherAgents.join(', ')}`);
        log(`Proxy: 127.0.0.1:${port}`);
        log(`Prompt: ${PROMPT_FILE}`);

        // Handle clean shutdown
        const shutdown = async () => {
            log('Shutting down...');
            releaseLock();
            await transportLogout();
            server.close();
            setStatus('SHUTDOWN');
            process.exit(0);
        };
        process.on('SIGINT', shutdown);
        process.on('SIGTERM', shutdown);

        // Run transport
        await runTransport();

        // Save transcript to persistent location and ingest into vector memory
        await saveTranscript();

        // Save result.md as a remote note for cross-agent access
        await saveResult();

        // Clean exit
        await transportLogout();
        server.close();
    } catch (err) {
        console.error(`Fatal error: ${err.message}`);
        log(`FATAL: ${err.message}`);
        setStatus('ERROR');
        process.exit(1);
    }
}

// Persist state on exit
process.on('exit', () => {
    releaseLock();
    try {
        writeState();
    } catch (e) {
        // Best effort
    }
});

main();
