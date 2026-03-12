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

// ── Error logging (writes to error_log table + stdout + file) ────────────────
// Fire-and-forget DB insert — never throws, never blocks the caller.
// Parameters:
//   subsystem: 'virtual-agent', 'mail', 'discussion', etc.
//   action: 'direct-mail-error', 'api-call-failed', etc.
//   opts: { agent?, context?, contextId?, message, detail? }

function logError(subsystem, action, opts) {
    const { agent, context, contextId, message, detail } = opts;

    // Always log to stdout + file via the regular logger
    log(subsystem, action, { agent, context, contextId, error: message });

    // Fire-and-forget insert into error_log table
    const pool = require('../db');
    pool.query(
        `INSERT INTO error_log (subsystem, action, agent, context, context_id, error_message, error_detail)
         VALUES ($1, $2, $3, $4, $5, $6, $7)`,
        [subsystem, action, agent || null, context || null, contextId || null, message, detail || null]
    ).catch(err => {
        console.error('Failed to write to error_log table:', err.message);
    });
}

// Query error_log entries for the admin UI.
// Supports since_id for incremental polling and limit for initial load.
async function getErrorLogEntries(sinceId, limit) {
    const pool = require('../db');
    const cols = 'id, created_at, subsystem, action, agent, context, context_id, error_message, error_detail';
    if (sinceId) {
        const result = await pool.query(
            `SELECT ${cols} FROM error_log WHERE id > $1 ORDER BY id ASC LIMIT 500`,
            [sinceId]
        );
        return result.rows;
    }
    const result = await pool.query(
        `SELECT ${cols} FROM error_log ORDER BY id DESC LIMIT $1`,
        [limit || 100]
    );
    return result.rows.reverse();
}

// Classify an error into a safe, caller-facing description.
// Full details go to error_log; callers get only the category.
function safeErrorMessage(err) {
    const msg = (err && err.message) || '';
    if (msg.includes('API error 4')) return 'Provider API rejected the request (client error).';
    if (msg.includes('API error 5')) return 'Provider API is temporarily unavailable (server error).';
    if (msg.includes('API error')) return 'Provider API returned an error.';
    if (msg.includes('Stale configuration')) return 'Agent configuration is outdated — re-save settings in the admin dashboard.';
    if (msg.includes('No pricing data')) return 'Missing pricing data for this model — check provider configuration.';
    if (msg.includes('ECONNREFUSED') || msg.includes('ETIMEDOUT') || msg.includes('fetch failed')) return 'Could not reach the provider API (network error).';
    if (msg.includes('API key')) return 'API key error — check the agent\'s API key configuration.';
    if (msg.includes('Unsupported provider')) return 'Unsupported provider — check agent profile settings.';
    return 'An internal error occurred while processing the request.';
}

module.exports = { log, logError, getErrorLogEntries, safeErrorMessage };
