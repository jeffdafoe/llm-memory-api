#!/usr/bin/env node
'use strict';

// ---------------------------------------------------------------------------
// discuss-launch.js — Launcher for discuss.js
//
// Spawns the discuss.js transport as a detached background process, polls the
// transport log for "Prompt written to <path>", then outputs the prompt
// contents to stdout. This collapses the nohup + wait-loop + cat sequence
// into a single blocking command.
//
// Usage:
//   node discuss-launch.js create --config .agent.json --topic "..." --other <agent> [--context-file <path>]
//   node discuss-launch.js join --config .agent.json [discussion-id]
//
// All flags are passed through to discuss.js. The --config flag is also read
// here to determine the work_dir (for the log file location).
// The config file is typically .agent.json which contains agent, passphrase,
// api_url, and work_dir.
// ---------------------------------------------------------------------------

const fs = require('fs');
const path = require('path');
const { spawn } = require('child_process');

const args = process.argv.slice(2);
const subCommand = args[0];

if (!subCommand || !['create', 'join'].includes(subCommand)) {
    console.error('Usage: node discuss-launch.js create [options...]');
    console.error('       node discuss-launch.js join [discussion-id] [options...]');
    console.error('');
    console.error('Spawns discuss.js as a background transport, waits for prompt.txt,');
    console.error('and outputs its contents to stdout.');
    process.exit(1);
}

// Parse --config and --work-dir from args to determine log file location
let configPath = null;
let workDir = null;
for (let i = 0; i < args.length; i++) {
    if (args[i] === '--config' && args[i + 1]) configPath = args[i + 1];
    if (args[i] === '--work-dir' && args[i + 1]) workDir = args[i + 1];
}

// Read config for work_dir default
if (configPath && !workDir) {
    try {
        const config = JSON.parse(fs.readFileSync(configPath, 'utf-8'));
        if (config.work_dir) workDir = config.work_dir;
    } catch (e) {
        console.error(`Failed to read config: ${e.message}`);
        process.exit(1);
    }
}

if (!workDir) workDir = '/tmp/llm';

const discussJs = path.join(__dirname, 'discuss.js');
const logFile = path.join(workDir, 'discuss-transport.log');

// Ensure work dir exists
fs.mkdirSync(workDir, { recursive: true });

// Clean up stale context temp files from previous launches
try {
    const files = fs.readdirSync(workDir);
    for (const f of files) {
        if (/^discuss-context-\d+\.md$/.test(f)) {
            try { fs.unlinkSync(path.join(workDir, f)); } catch (e) { /* ignore */ }
        }
    }
} catch (e) { /* ignore */ }

// If --context-file is provided, copy it to a unique temp file so multiple
// concurrent launches don't clobber each other's context.
const childArgs = [...args];
for (let i = 0; i < childArgs.length; i++) {
    if (childArgs[i] === '--context-file' && childArgs[i + 1]) {
        const origPath = childArgs[i + 1];
        try {
            const uniquePath = path.join(workDir, `discuss-context-${Date.now()}.md`);
            fs.copyFileSync(origPath, uniquePath);
            childArgs[i + 1] = uniquePath;
            console.error(`Context copied to ${uniquePath}`);
        } catch (e) {
            console.error(`WARNING: Could not copy context file: ${e.message}`);
            // Fall through with original path
        }
        break;
    }
}

// Clear old log so we don't match stale "Prompt written to" lines
try { fs.unlinkSync(logFile); } catch (e) { /* ignore */ }

// Spawn discuss.js as a detached background process with stdout/stderr to log
const logFd = fs.openSync(logFile, 'w');
const child = spawn(process.execPath, [discussJs, ...childArgs], {
    detached: true,
    stdio: ['ignore', logFd, logFd],
});
child.unref();
fs.closeSync(logFd);

console.error(`Transport PID: ${child.pid}`);

// Poll for "Prompt written to <path>" in the transport log
// Default 600s matches the discussion timeout (10 min). The transport's own
// join-timeout is shorter (300s), so this outlasts it comfortably.
const TIMEOUT_SEC = 600;
let elapsed = 0;

const poll = setInterval(() => {
    elapsed += 2;

    try {
        const logContent = fs.readFileSync(logFile, 'utf-8');
        const match = logContent.match(/Prompt written to (.+)/);
        if (match) {
            clearInterval(poll);
            const promptPath = match[1].trim();
            try {
                const prompt = fs.readFileSync(promptPath, 'utf-8');
                process.stdout.write(prompt);
                process.exit(0);
            } catch (readErr) {
                console.error(`Failed to read prompt at ${promptPath}: ${readErr.message}`);
                process.exit(1);
            }
        }
    } catch (e) {
        // Log file might not exist yet — keep polling
    }

    if (elapsed >= TIMEOUT_SEC) {
        clearInterval(poll);
        console.error(`ERROR: prompt.txt not found after ${TIMEOUT_SEC}s`);
        try {
            console.error('--- Transport log ---');
            console.error(fs.readFileSync(logFile, 'utf-8'));
        } catch (e) { /* ignore */ }
        process.exit(1);
    }
}, 2000);
