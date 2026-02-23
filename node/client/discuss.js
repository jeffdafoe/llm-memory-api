#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const http = require('http');
const https = require('https');

// ---------------------------------------------------------------------------
// Argument parsing
// ---------------------------------------------------------------------------

const args = process.argv.slice(2);
const command = args[0]; // create | join | transport

if (!command || !['create', 'join', 'transport'].includes(command)) {
    console.error('Usage: node discuss.js <create|join|transport> --config <json-file> [--discussion-id <id>]');
    process.exit(1);
}

let configPath = null;
let discussionId = null;

for (let i = 1; i < args.length; i++) {
    if (args[i] === '--config' && args[i + 1]) {
        configPath = args[i + 1];
        i++;
    }
    if (args[i] === '--discussion-id' && args[i + 1]) {
        discussionId = parseInt(args[i + 1], 10);
        i++;
    }
    // join <id> shorthand
    if (command === 'join' && !discussionId && /^\d+$/.test(args[i])) {
        discussionId = parseInt(args[i], 10);
    }
}

if (!configPath) {
    console.error('Missing --config <json-file>');
    process.exit(1);
}

if (command === 'join' && !discussionId) {
    console.error('join mode requires a discussion ID: node discuss.js join <id> --config <file>');
    process.exit(1);
}

const config = JSON.parse(fs.readFileSync(configPath, 'utf-8'));

// Merge defaults
const cfg = {
    apiUrl: config.apiUrl,
    passphrase: config.passphrase,
    agent: config.agent,
    other: config.other || null,
    workDir: config.workDir,
    channel: config.channel || 'discussion',
    mode: config.mode || 'realtime',
    topic: config.topic || '',
    context: config.context || '',
    contextFile: config.contextFile || null,
    initiator: config.initiator || false,
    maxTurns: config.maxTurns || 20,
    timeoutMinutes: config.timeoutMinutes || 120,
    templatePath: config.templatePath || path.join(__dirname, '..', '..', 'templates', 'discussion-prompt.tpl'),
    pollInterval: config.pollInterval || 5000,
    sendDelay: config.sendDelay || 3000,
    proxyPort: config.proxyPort || 0, // 0 = random available port
};

// Session token obtained at login, used for all authenticated API calls
let sessionToken = null;

// Validate required fields
for (const field of ['apiUrl', 'passphrase', 'agent', 'workDir']) {
    if (!cfg[field]) {
        console.error(`Missing required config field: ${field}`);
        process.exit(1);
    }
}

// ---------------------------------------------------------------------------
// Paths
// ---------------------------------------------------------------------------

const INBOX_DIR = path.join(cfg.workDir, 'inbox');
const OUTBOX_DIR = path.join(cfg.workDir, 'outbox');
const LOG_FILE = path.join(cfg.workDir, 'conversation.log');
const STATUS_FILE = path.join(cfg.workDir, 'status');
const DONE_FILE = path.join(cfg.workDir, 'done');
const HEARTBEAT_FILE = path.join(cfg.workDir, 'heartbeat');
const TRANSCRIPT_FILE = path.join(cfg.workDir, 'transcript.md');
const LAST_DELIVERED_FILE = path.join(cfg.workDir, 'last-delivered-id');
const IDLE_TIMEOUT_FILE = path.join(cfg.workDir, 'idle-timeout');
const PORT_FILE = path.join(cfg.workDir, 'port');
const PROMPT_FILE = path.join(cfg.workDir, 'prompt.txt');
const SEEN_IDS_FILE = path.join(cfg.workDir, '.seen_ids');
const READY_FILE = path.join(cfg.workDir, 'ready');

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

let turnCount = 0;
const startTime = Date.now();
const seenIds = new Set();

// ---------------------------------------------------------------------------
// Logging
// ---------------------------------------------------------------------------

function log(message) {
    const ts = new Date().toTimeString().slice(0, 8);
    const line = `[${ts}] ${message}`;
    fs.appendFileSync(LOG_FILE, line + '\n');
    process.stderr.write(line + '\n');
}

function writeStatus(status) {
    fs.writeFileSync(STATUS_FILE, status);
}

function appendTranscript(speaker, message) {
    const ts = new Date().toTimeString().slice(0, 8);
    fs.appendFileSync(TRANSCRIPT_FILE, `**${speaker}** (${ts}):\n${message}\n\n`);
}

// ---------------------------------------------------------------------------
// API client
// ---------------------------------------------------------------------------

// Raw HTTP call — used by both apiCall and apiCallNoAuth
function httpPost(url, headers, body) {
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

function apiCallNoAuth(endpoint, body) {
    return httpPost(cfg.apiUrl + '/' + endpoint, {}, body);
}

function apiCall(endpoint, body) {
    if (!sessionToken) {
        throw new Error('Not logged in — no session token available');
    }
    return httpPost(
        cfg.apiUrl + '/' + endpoint,
        { 'Authorization': 'Bearer ' + sessionToken },
        body
    );
}

async function transportLogin() {
    const data = await apiCallNoAuth('agent/login', {
        agent: cfg.agent,
        passphrase: cfg.passphrase,
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
    if (fs.existsSync(LAST_DELIVERED_FILE)) {
        const lastId = parseInt(fs.readFileSync(LAST_DELIVERED_FILE, 'utf-8').trim(), 10);
        if (!isNaN(lastId)) {
            body.after_id = lastId;
        }
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
            to_agent: cfg.other,
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

        fs.writeFileSync(path.join(INBOX_DIR, `${msg.id}.txt`), msg.message);
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
        const response = await apiCall('discussion/pending', { agent: cfg.agent });
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
    if (turnCount >= cfg.maxTurns) return true;
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
                    } else if (urlPath === '/vote/status') {
                        result = await apiCall('discussion/vote/status', {
                            vote_id: parsed.vote_id,
                        });
                    } else if (urlPath === '/pending') {
                        result = await apiCall('discussion/pending', {
                            agent: cfg.agent,
                        });
                    } else if (urlPath === '/conclude') {
                        result = await apiCall('discussion/conclude', {
                            discussion_id: discussionId,
                            agent: cfg.agent,
                        });
                    } else if (urlPath === '/status') {
                        // Return transport status
                        result = {
                            agent: cfg.agent,
                            other: cfg.other,
                            discussionId: discussionId,
                            turnCount: turnCount,
                            elapsedMinutes: Math.round((Date.now() - startTime) / 60000),
                            status: fs.existsSync(STATUS_FILE) ? fs.readFileSync(STATUS_FILE, 'utf-8').trim() : 'unknown',
                        };
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
            fs.writeFileSync(PORT_FILE, String(port));
            log(`Proxy server listening on 127.0.0.1:${port}`);
            resolve({ server, port });
        });
    });
}

// ---------------------------------------------------------------------------
// Setup: create mode
// ---------------------------------------------------------------------------

async function setupCreate() {
    // Read context from file if specified
    let context = cfg.context || '';
    if (cfg.contextFile) {
        context = fs.readFileSync(cfg.contextFile, 'utf-8');
    }

    // Create discussion via API
    const result = await apiCall('discussion/create', {
        topic: cfg.topic,
        participants: [cfg.agent, cfg.other],
        created_by: cfg.agent,
        mode: cfg.mode,
        context: context || undefined,
    });

    discussionId = result.discussion.id;
    cfg.channel = `discuss-${discussionId}`;

    log(`Created discussion #${discussionId}: ${cfg.topic}`);
    log(`Channel: ${cfg.channel}`);

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
    cfg.mode = discussion.mode || 'realtime';

    // Find the other participant
    const participants = result.participants || [];
    const otherParticipant = participants.find(p => p.agent !== cfg.agent);
    if (otherParticipant) {
        cfg.other = otherParticipant.agent;
    }

    // Join the discussion
    await apiCall('discussion/join', {
        discussion_id: discussionId,
        agent: cfg.agent,
    });

    log(`Joined discussion #${discussionId}: ${cfg.topic}`);
    log(`Channel: ${cfg.channel}, Other: ${cfg.other}`);

    return discussionId;
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

    const replacements = {
        '[OTHER_AGENT]': cfg.other,
        '[TOPIC]': cfg.topic,
        '[WORK_DIR]': cfg.workDir,
        '[DISCUSSION_ID]': String(discussionId),
        '[API_URL]': `http://127.0.0.1:${proxyPort}`,
        '[API_KEY]': '', // local proxy handles auth — subagent doesn't need credentials
        '[MY_AGENT]': cfg.agent,
        '[CONTEXT]': cfg.context || '(none)',
        '[INITIATOR_LINE]': cfg.initiator
            ? 'You are the INITIATOR. Send the first message to start the discussion.'
            : 'You are JOINING. Wait for the first message from the other side.',
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
    log(`Discussion transport starting: ${cfg.agent} <-> ${cfg.other}`);
    log(`Work dir: ${cfg.workDir}`);
    writeStatus('POLLING');

    while (true) {
        // 1. Check done
        if (checkDone()) {
            log('Done file detected. Discussion concluded.');
            writeStatus('DONE');
            return;
        }

        // 2. Check idle timeout
        if (checkIdleTimeout()) {
            log('Idle timeout detected. Shutting down.');
            writeStatus('IDLE_TIMEOUT');
            return;
        }

        // 3. Check timeout
        if (checkTimeout()) {
            log(`Timeout reached (${turnCount} turns, ${cfg.timeoutMinutes}m). Writing timeout notice.`);
            fs.writeFileSync(path.join(INBOX_DIR, 'timeout.txt'), '[SYSTEM] Discussion timeout reached. Please wrap up.');
            writeStatus('TIMEOUT');
            await sleep(30000);
            const outMsg = checkOutbox();
            if (outMsg) {
                await sendMessage(outMsg);
            }
            writeStatus('DONE');
            fs.writeFileSync(DONE_FILE, 'timeout');
            return;
        }

        // 4. Check outbox
        const outMsg = checkOutbox();
        if (outMsg) {
            await sendMessage(outMsg);
            await sleep(cfg.sendDelay);
        }

        // 5. Poll for incoming messages
        const messages = await receiveMessages();

        // 6-7. Process: dedup, write inbox, transcript
        const { allIds, newIds } = processReceivedMessages(messages);

        // 8. Poll pending votes
        await checkPendingVotes();

        if (allIds.length > 0) {
            // 9. Delayed ack
            if (newIds.length > 0) {
                await waitForSubagentReads(newIds);
            }

            // 10. Ack all
            await ackMessages(allIds);

            // 11. Record last-delivered-id
            const maxId = Math.max(...allIds);
            if (isFinite(maxId)) {
                fs.writeFileSync(LAST_DELIVERED_FILE, String(maxId));
            }

            writeStatus('MESSAGE_RECEIVED');

            // Signal other side is active
            if (!fs.existsSync(READY_FILE)) {
                fs.writeFileSync(READY_FILE, '');
                log('Other side is active — ready file written');
            }
        } else {
            await sleep(cfg.pollInterval);
        }

        // Heartbeat
        fs.writeFileSync(HEARTBEAT_FILE, String(Date.now()));
    }
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

function initTranscript() {
    const header = [
        `# Discussion: ${cfg.agent} <-> ${cfg.other}`,
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
            if (!cfg.other) {
                console.error('create mode requires "other" in config');
                process.exit(1);
            }
            if (!cfg.topic) {
                console.error('create mode requires "topic" in config');
                process.exit(1);
            }
            ensureDirectories();
            await setupCreate();
        } else if (command === 'join') {
            ensureDirectories();
            await setupJoin();
        } else {
            // transport mode — everything should already be set up
            if (!cfg.other) {
                console.error('transport mode requires "other" in config');
                process.exit(1);
            }
        }

        ensureDirectories();
        initTranscript();
        writeStatus('STARTING');

        // Load seen IDs from file if resuming
        if (fs.existsSync(SEEN_IDS_FILE)) {
            const lines = fs.readFileSync(SEEN_IDS_FILE, 'utf-8').trim().split('\n').filter(Boolean);
            for (const line of lines) {
                seenIds.add(parseInt(line, 10));
            }
        }

        // Start local proxy server
        const { server, port } = await startProxyServer();

        // Generate prompt
        generatePrompt(port);

        // Log summary
        log(`Discussion #${discussionId} ready`);
        log(`Mode: ${command}, Agent: ${cfg.agent}, Other: ${cfg.other}`);
        log(`Proxy: 127.0.0.1:${port}`);
        log(`Prompt: ${PROMPT_FILE}`);

        // Handle clean shutdown
        const shutdown = async () => {
            log('Shutting down...');
            await transportLogout();
            server.close();
            writeStatus('SHUTDOWN');
            process.exit(0);
        };
        process.on('SIGINT', shutdown);
        process.on('SIGTERM', shutdown);

        // Run transport
        await runTransport();

        // Clean exit
        await transportLogout();
        server.close();
    } catch (err) {
        console.error(`Fatal error: ${err.message}`);
        log(`FATAL: ${err.message}`);
        writeStatus('ERROR');
        process.exit(1);
    }
}

// Persist seen IDs on exit
process.on('exit', () => {
    try {
        const ids = Array.from(seenIds).join('\n');
        fs.writeFileSync(SEEN_IDS_FILE, ids + '\n');
    } catch (e) {
        // Best effort
    }
});

main();
