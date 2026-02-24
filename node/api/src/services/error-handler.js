const pool = require('../db');
const { sendSystemMessage } = require('./system-notify');

// Handler map: "source:error_code" → async function(agent, context) → handler_action string
// Each handler receives the reporting agent and the context payload.
// Returns a description of what it did (stored in handler_action column).
const handlers = {
    'discuss-transport:SUBAGENT_DEAD': async (agent, context) => {
        const discussionId = context && context.discussion_id;
        let message = `Your subagent appears to have died`;
        if (discussionId) {
            message += ` in discussion #${discussionId}`;
        }
        message += `. The transport is still running. Relaunch the subagent to continue.`;

        await sendSystemMessage(agent, message);
        return `Sent system message to ${agent}: "${message}"`;
    },
};

// Process an error report: insert into system_errors, run handler if one exists.
// Looks up handler by "source:error_code" key for precise matching.
// Returns the inserted row with status and handler_action.
async function handleError(agent, source, errorCode, context) {
    const handlerKey = `${source}:${errorCode}`;
    const handler = handlers[handlerKey];
    let status;
    let handlerAction = null;
    let resolvedAt = null;

    if (handler) {
        try {
            handlerAction = await handler(agent, context);
            status = 'auto_resolved';
            resolvedAt = new Date();
        } catch (err) {
            // Handler failed — record the failure but still store the error
            handlerAction = `Handler failed: ${err.message}`;
            status = 'unhandled';
        }
    } else {
        status = 'unhandled';
    }

    const result = await pool.query(
        `INSERT INTO system_errors (agent, source, error_code, context, status, handler_action, resolved_at)
         VALUES ($1, $2, $3, $4, $5, $6, $7)
         RETURNING id, status, handler_action, reported_at`,
        [agent, source, errorCode, context ? JSON.stringify(context) : null, status, handlerAction, resolvedAt]
    );

    return result.rows[0];
}

module.exports = { handleError };
