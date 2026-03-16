const { Router } = require('express');
const { saveNote, listNotes, readNote, deleteNote, restoreNote, editNote, grepNotes, moveNote } = require('../services/documents');
const { logError } = require('../services/logger');
const { requireAccess, getReadableNamespaces, validateNamespace } = require('../services/namespace-permissions');

const router = Router();

// Helper to get actor identity for permission checks.
function getActor(req) {
    return {
        actorId: req.actorId,
        actorName: req.authenticatedAgent || (req.authenticatedUser && req.authenticatedUser.username) || 'unknown',
        actorType: req.authenticatedAgent ? 'agent' : 'user'
    };
}

router.post('/documents/save', async (req, res) => {
    const { namespace, title, content, slug, created_by } = req.body;

    if (!namespace || !title || !content) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, title, content' }
        });
    }

    try {
        validateNamespace(namespace);
        const actor = getActor(req);
        await requireAccess(actor.actorId, actor.actorName, actor.actorType, namespace, 'write');
        const result = await saveNote(namespace, title, content, slug, created_by);
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        if (status >= 500) logError('documents', 'save', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(status).json({
            error: { code: status === 400 ? 'BAD_REQUEST' : status === 403 ? 'FORBIDDEN' : 'INTERNAL_ERROR', message: status >= 500 ? 'An internal error occurred' : err.message }
        });
    }
});

router.post('/documents/list', async (req, res) => {
    const { namespace, limit, offset, prefix } = req.body;

    if (!namespace) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: namespace' }
        });
    }

    try {
        validateNamespace(namespace);
        const actor = getActor(req);
        await requireAccess(actor.actorId, actor.actorName, actor.actorType, namespace, 'read');
        const result = await listNotes(namespace, limit, offset, prefix);
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        if (status >= 500) logError('documents', 'list', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(status).json({
            error: { code: status === 403 ? 'FORBIDDEN' : 'INTERNAL_ERROR', message: status >= 500 ? 'An internal error occurred' : err.message }
        });
    }
});

router.post('/documents/read', async (req, res) => {
    const { namespace, slug } = req.body;

    if (!namespace || !slug) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, slug' }
        });
    }

    try {
        validateNamespace(namespace);
        const actor = getActor(req);
        await requireAccess(actor.actorId, actor.actorName, actor.actorType, namespace, 'read');
        const result = await readNote(namespace, slug);
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        if (status >= 500) logError('documents', 'read', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(status).json({
            error: { code: status === 403 ? 'FORBIDDEN' : status === 404 ? 'NOT_FOUND' : 'INTERNAL_ERROR', message: status >= 500 ? 'An internal error occurred' : err.message }
        });
    }
});

router.post('/documents/delete', async (req, res) => {
    const { namespace, slug } = req.body;

    if (!namespace || !slug) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, slug' }
        });
    }

    try {
        validateNamespace(namespace);
        const actor = getActor(req);
        await requireAccess(actor.actorId, actor.actorName, actor.actorType, namespace, 'delete');
        const result = await deleteNote(namespace, slug);
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        if (status >= 500) logError('documents', 'delete', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(status).json({
            error: { code: status === 403 ? 'FORBIDDEN' : status === 404 ? 'NOT_FOUND' : 'INTERNAL_ERROR', message: status >= 500 ? 'An internal error occurred' : err.message }
        });
    }
});

router.post('/documents/restore', async (req, res) => {
    const { namespace, slug } = req.body;

    if (!namespace || !slug) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, slug' }
        });
    }

    try {
        validateNamespace(namespace);
        const actor = getActor(req);
        await requireAccess(actor.actorId, actor.actorName, actor.actorType, namespace, 'write');
        const result = await restoreNote(namespace, slug);
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        if (status >= 500) logError('documents', 'restore', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(status).json({
            error: { code: status === 403 ? 'FORBIDDEN' : status === 404 ? 'NOT_FOUND' : status === 409 ? 'CONFLICT' : 'INTERNAL_ERROR', message: status >= 500 ? 'An internal error occurred' : err.message }
        });
    }
});

router.post('/documents/edit', async (req, res) => {
    const { namespace, slug, old_string, new_string, replace_all } = req.body;

    if (!namespace || !slug || !old_string || new_string === undefined) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, slug, old_string, new_string' }
        });
    }

    try {
        validateNamespace(namespace);
        const actor = getActor(req);
        await requireAccess(actor.actorId, actor.actorName, actor.actorType, namespace, 'write');
        const result = await editNote(namespace, slug, old_string, new_string, replace_all);
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        if (status >= 500) logError('documents', 'edit', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(status).json({
            error: { code: status === 400 ? 'BAD_REQUEST' : status === 403 ? 'FORBIDDEN' : status === 404 ? 'NOT_FOUND' : 'INTERNAL_ERROR', message: status >= 500 ? 'An internal error occurred' : err.message }
        });
    }
});

router.post('/documents/move', async (req, res) => {
    const { namespace, slug, new_slug, new_namespace } = req.body;

    if (!namespace || !slug || !new_slug) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, slug, new_slug. Optional: new_namespace' }
        });
    }

    try {
        validateNamespace(namespace);
        if (new_namespace) validateNamespace(new_namespace);
        const actor = getActor(req);
        // Need write on source (to remove) and write on target (to create)
        await requireAccess(actor.actorId, actor.actorName, actor.actorType, namespace, 'write');
        if (new_namespace && new_namespace !== namespace) {
            await requireAccess(actor.actorId, actor.actorName, actor.actorType, new_namespace, 'write');
        }
        const result = await moveNote(namespace, slug, new_slug, new_namespace);
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        if (status >= 500) logError('documents', 'move', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(status).json({
            error: { code: status === 403 ? 'FORBIDDEN' : status === 404 ? 'NOT_FOUND' : status === 409 ? 'CONFLICT' : 'INTERNAL_ERROR', message: status >= 500 ? 'An internal error occurred' : err.message }
        });
    }
});

router.post('/documents/grep', async (req, res) => {
    const { pattern, namespace, limit } = req.body;

    if (!pattern) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: pattern' }
        });
    }

    try {
        const actor = getActor(req);
        // For specific namespace, validate and check read access directly.
        // For wildcard searches, push namespace filtering into the query.
        if (namespace && namespace !== '*') {
            validateNamespace(namespace);
            await requireAccess(actor.actorId, actor.actorName, actor.actorType, namespace, 'read');
        }
        // For wildcard searches, push namespace filtering into the query
        let readable = null;
        if (!namespace || namespace === '*') {
            readable = await getReadableNamespaces(actor.actorId, actor.actorName, actor.actorType);
        }
        let results = await grepNotes(pattern, namespace, limit, readable);
        res.json({ results });
    } catch (err) {
        const status = err.statusCode || 500;
        if (status >= 500) logError('documents', 'grep', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(status).json({
            error: { code: status === 400 ? 'BAD_REQUEST' : status === 403 ? 'FORBIDDEN' : 'INTERNAL_ERROR', message: status >= 500 ? 'An internal error occurred' : err.message }
        });
    }
});

module.exports = router;
