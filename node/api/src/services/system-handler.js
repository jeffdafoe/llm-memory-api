// System message handler — processes messages addressed to the 'system' agent.
// Messages use JSON payloads with a 'type' field for routing.
// Handlers are async and fire-and-forget (caller doesn't await).

const { log } = require('./logger');

const handlers = {};

// Register a handler for a message type.
function register(type, handler) {
    handlers[type] = handler;
}

// Process a message sent to system. Called fire-and-forget from chatSend.
async function handleMessage(messageId, fromAgent, message, channel) {
    let payload;
    try {
        payload = JSON.parse(message);
    } catch (err) {
        log('system-handler', 'parse-error', { messageId, fromAgent, error: err.message });
        return;
    }

    const { type } = payload;
    if (!type) {
        log('system-handler', 'missing-type', { messageId, fromAgent });
        return;
    }

    const handler = handlers[type];
    if (!handler) {
        log('system-handler', 'unknown-type', { messageId, fromAgent, type });
        return;
    }

    try {
        log('system-handler', 'dispatch', { messageId, fromAgent, type });
        await handler(payload, { messageId, fromAgent, channel });
    } catch (err) {
        log('system-handler', 'handler-error', { messageId, fromAgent, type, error: err.message });
    }
}

module.exports = { register, handleMessage };
