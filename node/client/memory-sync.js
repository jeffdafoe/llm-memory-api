#!/usr/bin/env node
'use strict';

// ---------------------------------------------------------------------------
// memory-sync.js — Bidirectional sync between local memory files and remote
// notes, plus conversation log sync, via the memory API.
//
// Reads .agent.json for auth credentials, scans a project directory for:
//   - {project-dir}/memory/*.md — note files (bidirectional sync)
//   - {project-dir}/*.jsonl — Claude Code session logs (one-way upload)
//
// Usage:
//   node memory-sync.js --project-dir <path>
//                        [--config <path-to-.agent.json>]
//                        [--user <username>]
//
// Example:
//   node memory-sync.js \
//     --project-dir C:/Users/jdafoe/.claude/projects/C--bill1st-dev \
//     --config C:/bill1st/dev/.agent.json \
//     --user jeff
//
// --local-dir is accepted as a deprecated alias for --project-dir.
// If --config is not provided, looks for .agent.json in the current directory.
// --user sets the label for user messages in conversation logs (default: "user").
// ---------------------------------------------------------------------------

const fs = require('fs');
const path = require('path');
const https = require('https');
const http = require('http');

// ---------------------------------------------------------------------------
// Argument parsing
// ---------------------------------------------------------------------------

const args = process.argv.slice(2);
let projectDir = null;
let configPath = null;
let userName = 'user';

for (let i = 0; i < args.length; i++) {
    if ((args[i] === '--project-dir' || args[i] === '--local-dir') && args[i + 1]) {
        projectDir = args[i + 1];
        i++;
    } else if (args[i] === '--prefix' && args[i + 1]) {
        // Deprecated — prefix is now server-controlled. Accept and ignore for backwards compat.
        i++;
    } else if (args[i] === '--config' && args[i + 1]) {
        configPath = args[i + 1];
        i++;
    } else if (args[i] === '--user' && args[i + 1]) {
        userName = args[i + 1];
        i++;
    }
}

if (!projectDir) {
    console.error('Usage: node memory-sync.js --project-dir <path> [--config <path>] [--user <name>]');
    process.exit(1);
}

// Derive memory dir from project dir
const memoryDir = path.join(projectDir, 'memory');

// ---------------------------------------------------------------------------
// Config — read .agent.json
// ---------------------------------------------------------------------------

if (!configPath) {
    configPath = path.join(process.cwd(), '.agent.json');
}

let config;
try {
    config = JSON.parse(fs.readFileSync(configPath, 'utf-8'));
} catch (e) {
    console.error('Failed to read config file ' + configPath + ': ' + e.message);
    process.exit(1);
}

if (!config.agent || !config.passphrase || !config.api_url) {
    console.error('.agent.json must contain: agent, passphrase, api_url');
    process.exit(1);
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

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
                    reject(new Error('HTTP ' + res.statusCode + ': ' + responseBody));
                }
            });
        });

        req.on('error', (e) => reject(e));
        req.setTimeout(30000, () => { req.destroy(); reject(new Error('Request timed out')); });
        req.write(data);
        req.end();
    });
}

// ---------------------------------------------------------------------------
// Validation helpers
// ---------------------------------------------------------------------------

// Defensive check — reject filenames that could escape the local directory.
// Must be a flat basename: no slashes, no traversal, no leading dots.
function isSafeFilename(name) {
    if (!name || typeof name !== 'string') return false;
    if (name.includes('/') || name.includes('\\')) return false;
    if (name === '.' || name === '..') return false;
    if (name.startsWith('.')) return false;
    return true;
}

// UUID format check for session IDs
const UUID_REGEX = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

// ---------------------------------------------------------------------------
// Conversation preprocessing
// ---------------------------------------------------------------------------

// Extract user and assistant text messages from a Claude Code JSONL session file.
// Returns a markdown string with timestamped, labeled messages.
function preprocessSession(filePath, agentName) {
    const raw = fs.readFileSync(filePath, 'utf-8');
    const lines = raw.split('\n').filter(Boolean);
    const messages = [];

    for (const line of lines) {
        let entry;
        try {
            entry = JSON.parse(line);
        } catch (e) {
            continue; // skip malformed lines
        }

        if (entry.type === 'user') {
            // User messages: extract text content, skip tool results
            const msg = entry.message;
            if (!msg) continue;

            // msg can be a string or an object with role/content
            let content = null;
            if (typeof msg === 'string') {
                content = msg;
            } else if (msg.content) {
                // content can be a string or an array of content blocks
                if (typeof msg.content === 'string') {
                    content = msg.content;
                } else if (Array.isArray(msg.content)) {
                    // Filter to text blocks only (skip tool_result blocks)
                    const textParts = [];
                    for (const block of msg.content) {
                        if (block.type === 'text') {
                            textParts.push(block.text);
                        }
                        // Skip tool_result blocks entirely
                    }
                    if (textParts.length > 0) {
                        content = textParts.join('\n');
                    }
                }
            }

            if (content && content.trim()) {
                messages.push({
                    timestamp: entry.timestamp,
                    speaker: userName,
                    text: content.trim()
                });
            }
        } else if (entry.type === 'assistant') {
            // Assistant messages: extract text blocks only (skip tool_use blocks)
            const msg = entry.message;
            if (!msg || !msg.content) continue;

            const contentArr = Array.isArray(msg.content) ? msg.content : [];
            const textParts = [];
            for (const block of contentArr) {
                if (block.type === 'text' && block.text && block.text.trim()) {
                    textParts.push(block.text.trim());
                }
            }

            if (textParts.length > 0) {
                messages.push({
                    timestamp: entry.timestamp,
                    speaker: agentName,
                    text: textParts.join('\n')
                });
            }
        }
        // Skip progress, system, queue-operation entries entirely
    }

    return messages;
}

// Format preprocessed messages into a markdown note
function formatConversation(sessionId, messages) {
    if (messages.length === 0) return null;

    // Derive date from first message timestamp
    const firstTimestamp = messages[0].timestamp;
    const sessionDate = firstTimestamp
        ? new Date(firstTimestamp).toISOString().slice(0, 10)
        : 'unknown';

    // Build header
    const headerLines = [
        'Session: ' + sessionDate + ' (' + sessionId + ')',
        'Project: ' + projectDir,
        '',
        '---',
        ''
    ];

    // Build message lines
    const msgLines = [];
    for (const msg of messages) {
        let timeStr = '';
        if (msg.timestamp) {
            const d = new Date(msg.timestamp);
            const hh = String(d.getHours()).padStart(2, '0');
            const mm = String(d.getMinutes()).padStart(2, '0');
            timeStr = hh + ':' + mm;
        }
        msgLines.push('[' + timeStr + ' ' + msg.speaker + '] ' + msg.text);
    }

    return headerLines.join('\n') + msgLines.join('\n');
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

async function main() {
    // Step 1: Log in
    let sessionToken;
    try {
        const loginResult = await httpPost(config.api_url + '/agent/login', {}, {
            agent: config.agent,
            passphrase: config.passphrase,
            subsystem: 'memory-sync'
        });
        sessionToken = loginResult.session_token;
    } catch (err) {
        console.error('Login failed: ' + err.message);
        process.exit(1);
    }

    const authHeaders = { 'Authorization': 'Bearer ' + sessionToken };

    try {
        // ---------------------------------------------------------------
        // Phase 1: Note sync (existing behavior)
        // ---------------------------------------------------------------

        // Ensure memory directory exists
        fs.mkdirSync(memoryDir, { recursive: true });

        const localFiles = [];
        const entries = fs.readdirSync(memoryDir);
        for (const entry of entries) {
            if (!entry.endsWith('.md')) continue;
            const filePath = path.join(memoryDir, entry);
            const stat = fs.statSync(filePath);
            if (!stat.isFile()) continue;
            const content = fs.readFileSync(filePath, 'utf-8');
            localFiles.push({
                filename: entry,
                content: content,
                mtime: stat.mtime.toISOString()
            });
        }

        // Call sync endpoint — memory files + conversation support signal
        const result = await httpPost(config.api_url + '/agent/memory/sync', authHeaders, {
            memory: { files: localFiles },
            conversations: {}
        });

        // Process memory sync actions
        let pulled = 0;
        let pushed = 0;
        let unchanged = 0;
        let skipped = 0;

        const memoryActions = (result.memory && result.memory.actions) || [];
        for (const action of memoryActions) {
            // Defensive: validate filenames returned by the server before writing
            if (!isSafeFilename(action.filename)) {
                console.error('  SKIP unsafe filename from server: ' + action.filename);
                skipped++;
                continue;
            }

            if (action.action === 'pull') {
                // Write remote content to local file
                const filePath = path.join(memoryDir, action.filename);
                fs.writeFileSync(filePath, action.content, 'utf-8');
                // Set mtime to match remote so future syncs see them as equal
                if (action.remote_updated_at) {
                    const mtime = new Date(action.remote_updated_at);
                    fs.utimesSync(filePath, mtime, mtime);
                }
                console.log('  PULL ' + action.filename);
                pulled++;
            } else if (action.action === 'push') {
                // Update local mtime to match remote after push
                if (action.remote_updated_at) {
                    const filePath = path.join(memoryDir, action.filename);
                    if (fs.existsSync(filePath)) {
                        const mtime = new Date(action.remote_updated_at);
                        fs.utimesSync(filePath, mtime, mtime);
                    }
                }
                console.log('  PUSH ' + action.filename);
                pushed++;
            } else {
                unchanged++;
            }
        }

        let summary = 'Sync complete: ' + pulled + ' pulled, ' + pushed + ' pushed, ' + unchanged + ' unchanged';
        if (skipped > 0) {
            summary += ', ' + skipped + ' skipped (unsafe filenames)';
        }
        console.log(summary);

        // ---------------------------------------------------------------
        // Phase 2: Conversation sync
        // ---------------------------------------------------------------

        // Only proceed if server returned conversation config
        if (result.conversations && result.conversations.retention_days) {
            const retentionDays = result.conversations.retention_days;
            const cutoffMs = Date.now() - (retentionDays * 24 * 60 * 60 * 1000);
            const activeThresholdMs = 5 * 60 * 1000; // 5 minutes — skip active sessions

            // Scan for JSONL session files in the project directory
            const allEntries = fs.readdirSync(projectDir);
            const candidateIds = [];

            for (const entry of allEntries) {
                if (!entry.endsWith('.jsonl')) continue;
                // Skip subagent sessions
                if (entry.startsWith('agent-')) continue;

                // Extract session ID (filename without .jsonl)
                const sessionId = entry.replace('.jsonl', '');
                if (!UUID_REGEX.test(sessionId)) continue;

                const filePath = path.join(projectDir, entry);
                const stat = fs.statSync(filePath);
                if (!stat.isFile()) continue;

                // Skip files outside retention window
                if (stat.mtime.getTime() < cutoffMs) continue;

                // Skip files modified in the last 5 minutes (likely active sessions)
                if (Date.now() - stat.mtime.getTime() < activeThresholdMs) continue;

                candidateIds.push(sessionId.toLowerCase());
            }

            if (candidateIds.length > 0) {
                // Ask server which sessions are missing
                const diffResult = await httpPost(config.api_url + '/agent/memory/sync', authHeaders, {
                    conversations: { session_ids: candidateIds }
                });

                const missing = (diffResult.conversations && diffResult.conversations.missing) || [];

                if (missing.length > 0) {
                    // Preprocess all missing sessions into file entries for the sync endpoint
                    const conversationFiles = [];
                    let conversationErrors = 0;

                    for (const sessionId of missing) {
                        try {
                            // Find the local file (case-insensitive match)
                            const jsonlName = sessionId + '.jsonl';
                            let actualPath = path.join(projectDir, jsonlName);

                            if (!fs.existsSync(actualPath)) {
                                const match = allEntries.find(e =>
                                    e.toLowerCase() === jsonlName.toLowerCase()
                                );
                                if (!match) {
                                    console.error('  CONV SKIP missing file: ' + jsonlName);
                                    conversationErrors++;
                                    continue;
                                }
                                actualPath = path.join(projectDir, match);
                            }

                            // Preprocess the session
                            const messages = preprocessSession(actualPath, config.agent);
                            const content = formatConversation(sessionId, messages);

                            if (!content) {
                                // Empty session (no text messages) — skip silently
                                continue;
                            }

                            // Derive date from first message
                            const firstTs = messages[0].timestamp;
                            const dateStr = firstTs
                                ? new Date(firstTs).toISOString().slice(0, 10)
                                : 'unknown';

                            conversationFiles.push({
                                session_id: sessionId,
                                date: dateStr,
                                content: content,
                                metadata: {
                                    session_id: sessionId,
                                    session_date: dateStr,
                                    project: projectDir,
                                    agent: config.agent,
                                    user: userName,
                                    message_count: messages.length,
                                    source: 'claude-code-jsonl'
                                }
                            });
                        } catch (err) {
                            console.error('  CONV ERROR ' + sessionId + ': ' + err.message);
                            conversationErrors++;
                        }
                    }

                    // Upload sessions one at a time to avoid exceeding nginx body size limits.
                    // Each session can be several MB after preprocessing — batching them would
                    // compound the size and hit 413 errors.
                    if (conversationFiles.length > 0) {
                        let uploaded = 0;
                        let serverErrors = 0;

                        for (const file of conversationFiles) {
                            try {
                                const uploadResult = await httpPost(config.api_url + '/agent/memory/sync', authHeaders, {
                                    conversations: { uploads: [file] }
                                });

                                const count = (uploadResult.conversations && uploadResult.conversations.uploaded) || 0;
                                uploaded += count;

                                const uploadErrors = uploadResult.conversations && uploadResult.conversations.upload_errors;
                                if (uploadErrors && uploadErrors.length > 0) {
                                    for (const ue of uploadErrors) {
                                        console.error('  CONV SERVER ERROR ' + ue.session_id + ': ' + ue.error);
                                    }
                                    serverErrors += uploadErrors.length;
                                } else {
                                    console.log('  CONV conversations/' + file.date + '-' + file.session_id);
                                }
                            } catch (uploadErr) {
                                console.error('  CONV UPLOAD ERROR ' + file.session_id + ': ' + uploadErr.message);
                                conversationErrors++;
                            }
                        }

                        let conversationSummary = 'Conversations: ' + uploaded + ' uploaded';
                        if (conversationErrors > 0) {
                            conversationSummary += ', ' + conversationErrors + ' errors';
                        }
                        if (serverErrors > 0) {
                            conversationSummary += ', ' + serverErrors + ' server errors';
                        }
                        console.log(conversationSummary);
                    } else if (conversationErrors > 0) {
                        console.log('Conversations: 0 uploaded, ' + conversationErrors + ' errors');
                    }
                } else {
                    console.log('Conversations: all up to date');
                }
            } else {
                console.log('Conversations: no new sessions');
            }
        }
    } finally {
        // Always attempt logout so we don't leak sessions
        try {
            await httpPost(config.api_url + '/agent/logout', authHeaders, {
                agent: config.agent
            });
        } catch (err) {
            // Non-fatal — session will expire on its own
        }
    }
}

main().catch(err => {
    console.error('Unexpected error: ' + err.message);
    process.exit(1);
});
