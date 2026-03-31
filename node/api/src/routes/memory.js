const { Router } = require('express');
const { ingestContent, searchMemory, deleteMemory, cleanupMemory, ingestStatus } = require('../services/memory');
const { requireAccess, getReadableNamespaces, validateNamespace } = require('../services/namespace-permissions');
const { apiRoute } = require('../middleware/route-wrapper');
const sanitize = require('../sanitize');

const router = Router();

// Helper to get actor identity for permission checks.
function getActor(req) {
    return {
        actorId: req.actorId,
        actorName: req.authenticatedAgent || (req.authenticatedUser && req.authenticatedUser.username) || 'unknown',
        actorType: req.authenticatedAgent ? 'agent' : 'user'
    };
}

router.post('/memory/ingest', apiRoute('memory', 'ingest', async (req, res) => {
    const { namespace } = req.body;
    const source_file = sanitize.identifier(req.body.source_file);
    const content = sanitize.content(req.body.content);

    if (!namespace || !source_file || !content) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, source_file, content' }
        });
    }

    validateNamespace(namespace);
    const actor = getActor(req);
    await requireAccess(actor.actorId, actor.actorName, actor.actorType, namespace, 'write');
    const result = await ingestContent(namespace, source_file, content);
    res.json(result);
}));

router.post('/memory/ingest/status', apiRoute('memory', 'ingest-status', async (req, res) => {
    const { namespace } = req.body;
    const actor = getActor(req);

    if (namespace) {
        // Specific namespace — require read access
        validateNamespace(namespace);
        await requireAccess(actor.actorId, actor.actorName, actor.actorType, namespace, 'read');
        const result = await ingestStatus(namespace);
        res.json(result);
    } else {
        // No namespace — filter to readable namespaces only
        const readable = await getReadableNamespaces(actor.actorId, actor.actorType);
        const allStatus = await ingestStatus(null);
        const filtered = {
            files: (allStatus.files || []).filter(f => readable.includes(f.namespace))
        };
        res.json(filtered);
    }
}));

router.post('/memory/search', apiRoute('memory', 'search', async (req, res) => {
    const query = sanitize.content(req.body.query);
    const { namespace, limit } = req.body;

    if (!query) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: query' }
        });
    }

    const actor = getActor(req);
    if (namespace && namespace !== '*') {
        validateNamespace(namespace);
        await requireAccess(actor.actorId, actor.actorName, actor.actorType, namespace, 'read');
    }
    // For wildcard searches, push namespace filtering into the query
    let readable = null;
    if (!namespace || namespace === '*') {
        readable = await getReadableNamespaces(actor.actorId, actor.actorName, actor.actorType);
    }
    const result = await searchMemory(query, namespace, limit, readable, actor.actorId);
    res.json(result);
}));

router.post('/memory/cleanup', apiRoute('memory', 'cleanup', async (req, res) => {
    const { namespace, valid_source_files } = req.body;

    if (!namespace || !Array.isArray(valid_source_files)) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace (string), valid_source_files (array)' }
        });
    }

    validateNamespace(namespace);
    const actor = getActor(req);
    await requireAccess(actor.actorId, actor.actorName, actor.actorType, namespace, 'delete');
    const result = await cleanupMemory(namespace, valid_source_files);
    res.json(result);
}));

router.post('/memory/delete', apiRoute('memory', 'delete', async (req, res) => {
    const { namespace } = req.body;
    const source_file = sanitize.identifier(req.body.source_file);

    if (!namespace || !source_file) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, source_file' }
        });
    }

    validateNamespace(namespace);
    const actor = getActor(req);
    await requireAccess(actor.actorId, actor.actorName, actor.actorType, namespace, 'delete');
    const result = await deleteMemory(namespace, source_file);
    res.json(result);
}));

module.exports = router;
