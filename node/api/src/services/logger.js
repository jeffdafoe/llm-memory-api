const fs = require('fs');
const path = require('path');

const LOG_DIR = process.env.LOG_DIR || '/var/log/llm-memory-api';
const LOG_FILE = path.join(LOG_DIR, 'activity.log');

// Track whether we can write to the log file.
// Falls back to stdout-only if the directory doesn't exist (e.g. local dev).
let fileLoggingEnabled = false;
try {
    fs.accessSync(LOG_DIR, fs.constants.W_OK);
    fileLoggingEnabled = true;
} catch (err) {
    // Log directory not writable — file logging disabled
}

function formatLine(subsystem, action, details) {
    const timestamp = new Date().toISOString();
    const detailString = JSON.stringify(details);
    return `${timestamp} [${subsystem}] ${action}: ${detailString}`;
}

function log(subsystem, action, details) {
    const line = formatLine(subsystem, action, details);
    console.log(`[${subsystem}] ${new Date().toISOString()} ${action}:`, JSON.stringify(details));

    if (fileLoggingEnabled) {
        fs.appendFile(LOG_FILE, line + '\n', (err) => {
            if (err) {
                console.error(`Failed to write to ${LOG_FILE}:`, err.message);
            }
        });
    }
}

module.exports = { log };
