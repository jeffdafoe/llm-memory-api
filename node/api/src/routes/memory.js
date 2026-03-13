const { Router } = require('express');
const { ingestContent, searchMemory, deleteMemory, cleanupMemory, ingestStatus } = require('../services/memory');
const { logError } = require('../services/logger');
const { requireAccess, getReadableNamespaces, validateNamespace } = require('../services/namespace-permissions');

const router = Router();

// Helper to get actor identity for permission checks.
function getActor(req) {
    return {
        actorId: req.actorId,
        actorName: req.authenticatedAgent || (req.authenticatedUser && req.authenticatedUser.username) || 'unknown'
    };
}

router.post('/memory/ingest', async (req, res) => {
    const { namespace, source_file, content } = req.body;

    if (!namespace || !source_file || !content) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, source_file, content' }
        });
    }

    try {
        validateNamespace(namespace);
        const actor = getActor(req);
        await requireAccess(actor.actorId, actor.actorName, namespace, 'write');
        const result = await ingestContent(namespace, source_file, content);
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        if (status >= 500) logError('memory', 'ingest', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(status).json({
            error: { code: status === 400 ? 'BAD_REQUEST' : status === 403 ? 'FORBIDDEN' : 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/memory/ingest/status', async (req, res) => {
    const { namespace } = req.body;

    try {
        const result = await ingestStatus(namespace);
        res.json(result);
    } catch (err) {
        logError('memory', 'ingest-status', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/memory/search', async (req, res) => {
    const { query, namespace, limit } = req.body;

    if (!query) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: query' }
        });
    }

    try {
        const actor = getActor(req);
        if (namespace && namespace !== '*') {
            validateNamespace(namespace);
            await requireAccess(actor.actorId, actor.actorName, namespace, 'read');
        }
        // For wildcard searches, push namespace filtering into the query
        let readable = null;
        if (!namespace || namespace === '*') {
            readable = await getReadableNamespaces(actor.actorId, actor.actorName);
        }
        const result = await searchMemory(query, namespace, limit, readable);
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        if (status >= 500) logError('memory', 'search', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(status).json({
            error: { code: status === 403 ? 'FORBIDDEN' : 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/memory/cleanup', async (req, res) => {
    const { namespace, valid_source_files } = req.body;

    if (!namespace || !Array.isArray(valid_source_files)) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace (string), valid_source_files (array)' }
        });
    }

    try {
        validateNamespace(namespace);
        const actor = getActor(req);
        await requireAccess(actor.actorId, actor.actorName, namespace, 'delete');
        const result = await cleanupMemory(namespace, valid_source_files);
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        if (status >= 500) logError('memory', 'cleanup', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(status).json({
            error: { code: status === 403 ? 'FORBIDDEN' : 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/memory/delete', async (req, res) => {
    const { namespace, source_file } = req.body;

    if (!namespace || !source_file) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, source_file' }
        });
    }

    try {
        validateNamespace(namespace);
        const actor = getActor(req);
        await requireAccess(actor.actorId, actor.actorName, namespace, 'delete');
        const result = await deleteMemory(namespace, source_file);
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        if (status >= 500) logError('memory', 'delete', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(status).json({
            error: { code: status === 403 ? 'FORBIDDEN' : 'INTERNAL_ERROR', message: err.message }
        });
    }
});

module.exports = router;
