const { logError } = require('../services/logger');

const STATUS_CODES = {
    400: 'BAD_REQUEST',
    401: 'UNAUTHORIZED',
    403: 'FORBIDDEN',
    404: 'NOT_FOUND',
    409: 'CONFLICT',
    429: 'RATE_LIMITED'
};

function defaultCode(statusCode) {
    return STATUS_CODES[statusCode] || 'ERROR';
}

// Wrapper for route handlers that provides automatic error handling.
// Handlers throw on error instead of try/catch — the wrapper catches, logs to
// both console and the error_log table, and sends the appropriate response.
// All errors (4xx and 5xx) are logged with their status code so the dashboard
// can distinguish expected client errors from genuine server failures.
function apiRoute(category, label, fn) {
    return async (req, res) => {
        try {
            await fn(req, res);
        } catch (err) {
            const status = err.statusCode || 500;
            const code = err.code || defaultCode(status);
            logError(category, label, {
                message: err.message,
                detail: err.stack,
                statusCode: status
            });
            if (status < 500) {
                return res.status(status).json({
                    error: { code, message: err.message }
                });
            }
            console.error(`${category} ${label} error:`, err.message);
            res.status(500).json({
                error: { code: 'INTERNAL', message: `Failed: ${label}` }
            });
        }
    };
}

module.exports = { apiRoute };
