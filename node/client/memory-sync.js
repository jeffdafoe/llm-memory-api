#!/usr/bin/env node
'use strict';

// ---------------------------------------------------------------------------
// memory-sync.js — Bidirectional sync between local memory files and remote
// notes via the memory API.
//
// Reads .agent.json for auth credentials, scans a local directory for .md
// files, sends them to POST /agent/memory/sync along with their modification
// times, and writes back any remotely-newer or remote-only files.
//
// Usage:
//   node memory-sync.js --local-dir <path> --prefix <remote-prefix>
//                        [--config <path-to-.agent.json>]
//
// Example:
//   node memory-sync.js \
//     --local-dir C:/Users/jdafoe/.claude/projects/C--bill1st-dev/memory \
//     --prefix instructions/memory/ \
//     --config C:/bill1st/dev/.agent.json
//
// If --config is not provided, looks for .agent.json in the current directory.
// ---------------------------------------------------------------------------

const fs = require('fs');
const path = require('path');
const https = require('https');
const http = require('http');

// ---------------------------------------------------------------------------
// Argument parsing
// ---------------------------------------------------------------------------

const args = process.argv.slice(2);
let localDir = null;
let prefix = '';
let configPath = null;

for (let i = 0; i < args.length; i++) {
    if (args[i] === '--local-dir' && args[i + 1]) {
        localDir = args[i + 1];
        i++;
    } else if (args[i] === '--prefix' && args[i + 1]) {
        prefix = args[i + 1];
        i++;
    } else if (args[i] === '--config' && args[i + 1]) {
        configPath = args[i + 1];
        i++;
    }
}

if (!localDir) {
    console.error('Usage: node memory-sync.js --local-dir <path> --prefix <remote-prefix> [--config <path>]');
    process.exit(1);
}

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
        // Step 2: Read local files
        // Ensure local directory exists
        fs.mkdirSync(localDir, { recursive: true });

        const localFiles = [];
        const entries = fs.readdirSync(localDir);
        for (const entry of entries) {
            if (!entry.endsWith('.md')) continue;
            const filePath = path.join(localDir, entry);
            const stat = fs.statSync(filePath);
            if (!stat.isFile()) continue;
            const content = fs.readFileSync(filePath, 'utf-8');
            localFiles.push({
                filename: entry,
                content: content,
                mtime: stat.mtime.toISOString()
            });
        }

        // Step 3: Call sync endpoint
        const result = await httpPost(config.api_url + '/agent/memory/sync', authHeaders, {
            prefix: prefix,
            files: localFiles
        });

        // Step 4: Process actions
        let pulled = 0;
        let pushed = 0;
        let unchanged = 0;
        let skipped = 0;

        for (const action of result.actions) {
            // Defensive: validate filenames returned by the server before writing
            if (!isSafeFilename(action.filename)) {
                console.error('  SKIP unsafe filename from server: ' + action.filename);
                skipped++;
                continue;
            }

            if (action.action === 'pull') {
                // Write remote content to local file
                const filePath = path.join(localDir, action.filename);
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
                    const filePath = path.join(localDir, action.filename);
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
