const { Router } = require('express');
const { handleError } = require('../services/error-handler');
const { log } = require('../services/logger');
const { apiRoute } = require('../middleware/route-wrapper');

const router = Router();

// POST /system/error/report — report an error from any agent or process.
// The server looks up the error_code in the handler map and takes action
// if a handler exists. All errors are recorded in the system_errors table.
router.post('/system/error/report', apiRoute('system', 'error-report', async (req, res) => {
    const agent = req.authenticatedAgent;
    const { source, error_code, context } = req.body;

    if (!source || !error_code) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: source, error_code' }
        });
    }

    if (typeof source !== 'string' || typeof error_code !== 'string') {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'source and error_code must be strings' }
        });
    }

    if (context !== undefined && (typeof context !== 'object' || context === null || Array.isArray(context))) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'context must be a JSON object' }
        });
    }

    const result = await handleError(agent, source, error_code, context || null);

    log('system', 'error-report', {
        agent,
        source,
        error_code,
        status: result.status,
        id: result.id,
    });

    res.json({
        id: result.id,
        status: result.status,
        handler_action: result.handler_action,
        reported_at: result.reported_at,
    });
}));

module.exports = router;
