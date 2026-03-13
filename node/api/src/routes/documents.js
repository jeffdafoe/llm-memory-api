const { Router } = require('express');
const { saveNote, listNotes, readNote, deleteNote, restoreNote, editNote, grepNotes, moveNote } = require('../services/documents');
const { logError } = require('../services/logger');
const { requireAccess, getReadableNamespaces } = require('../services/namespace-permissions');

const router = Router();

// Helper to get actor identity for permission checks.
// Returns { actorId, actorName } or null if no actor context.
function getActor(req) {
    if (!req.actorId) return null;
    return {
        actorId: req.actorId,
        actorName: req.authenticatedAgent || (req.authenticatedUser && req.authenticatedUser.username) || 'unknown'
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
        const actor = getActor(req);
        if (actor) await requireAccess(actor.actorId, actor.actorName, namespace, 'write');
        const result = await saveNote(namespace, title, content, slug, created_by);
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        if (status >= 500) logError('documents', 'save', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(status).json({
            error: { code: status === 400 ? 'BAD_REQUEST' : status === 403 ? 'FORBIDDEN' : 'INTERNAL_ERROR', message: err.message }
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
        const actor = getActor(req);
        if (actor) await requireAccess(actor.actorId, actor.actorName, namespace, 'read');
        const result = await listNotes(namespace, limit, offset, prefix);
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        if (status >= 500) logError('documents', 'list', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(status).json({
            error: { code: status === 403 ? 'FORBIDDEN' : 'INTERNAL_ERROR', message: err.message }
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
        const actor = getActor(req);
        if (actor) await requireAccess(actor.actorId, actor.actorName, namespace, 'read');
        const result = await readNote(namespace, slug);
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        if (status >= 500) logError('documents', 'read', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(status).json({
            error: { code: status === 403 ? 'FORBIDDEN' : status === 404 ? 'NOT_FOUND' : 'INTERNAL_ERROR', message: err.message }
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
        const actor = getActor(req);
        if (actor) await requireAccess(actor.actorId, actor.actorName, namespace, 'delete');
        const result = await deleteNote(namespace, slug);
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        if (status >= 500) logError('documents', 'delete', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(status).json({
            error: { code: status === 403 ? 'FORBIDDEN' : status === 404 ? 'NOT_FOUND' : 'INTERNAL_ERROR', message: err.message }
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
        const actor = getActor(req);
        if (actor) await requireAccess(actor.actorId, actor.actorName, namespace, 'write');
        const result = await restoreNote(namespace, slug);
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        if (status >= 500) logError('documents', 'restore', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(status).json({
            error: { code: status === 403 ? 'FORBIDDEN' : status === 404 ? 'NOT_FOUND' : status === 409 ? 'CONFLICT' : 'INTERNAL_ERROR', message: err.message }
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
        const actor = getActor(req);
        if (actor) await requireAccess(actor.actorId, actor.actorName, namespace, 'write');
        const result = await editNote(namespace, slug, old_string, new_string, replace_all);
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        if (status >= 500) logError('documents', 'edit', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(status).json({
            error: { code: status === 400 ? 'BAD_REQUEST' : status === 403 ? 'FORBIDDEN' : status === 404 ? 'NOT_FOUND' : 'INTERNAL_ERROR', message: err.message }
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
        const actor = getActor(req);
        if (actor) {
            // Need write on source (to remove) and write on target (to create)
            await requireAccess(actor.actorId, actor.actorName, namespace, 'write');
            if (new_namespace && new_namespace !== namespace) {
                await requireAccess(actor.actorId, actor.actorName, new_namespace, 'write');
            }
        }
        const result = await moveNote(namespace, slug, new_slug, new_namespace);
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        if (status >= 500) logError('documents', 'move', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(status).json({
            error: { code: status === 403 ? 'FORBIDDEN' : status === 404 ? 'NOT_FOUND' : status === 409 ? 'CONFLICT' : 'INTERNAL_ERROR', message: err.message }
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
        if (actor) {
            // For wildcard searches, post-filter results to readable namespaces.
            // For specific namespace, check read access directly.
            if (namespace && namespace !== '*') {
                await requireAccess(actor.actorId, actor.actorName, namespace, 'read');
            }
        }
        let results = await grepNotes(pattern, namespace, limit);
        // Filter wildcard results to only namespaces the actor can read
        if (actor && (!namespace || namespace === '*')) {
            const readable = await getReadableNamespaces(actor.actorId);
            if (readable !== null) {
                results = results.filter(r => readable.includes(r.namespace));
            }
        }
        res.json({ results });
    } catch (err) {
        const status = err.statusCode || 500;
        if (status >= 500) logError('documents', 'grep', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(status).json({
            error: { code: status === 400 ? 'BAD_REQUEST' : status === 403 ? 'FORBIDDEN' : 'INTERNAL_ERROR', message: err.message }
        });
    }
});

module.exports = router;
