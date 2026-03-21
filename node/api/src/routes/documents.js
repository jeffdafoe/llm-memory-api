const { Router } = require('express');
const { saveNote, listNotes, readNote, deleteNote, restoreNote, editNote, grepNotes, moveNote } = require('../services/documents');
const { requireAccess, getReadableNamespaces, validateNamespace } = require('../services/namespace-permissions');
const { apiRoute } = require('../middleware/route-wrapper');

const router = Router();

// Helper to get actor identity for permission checks.
function getActor(req) {
    return {
        actorId: req.actorId,
        actorName: req.authenticatedAgent || (req.authenticatedUser && req.authenticatedUser.username) || 'unknown',
        actorType: req.authenticatedAgent ? 'agent' : 'user'
    };
}

router.post('/documents/save', apiRoute('documents', 'save', async (req, res) => {
    const { namespace, title, content, slug, created_by } = req.body;

    if (!namespace || !title || !content) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, title, content' }
        });
    }

    validateNamespace(namespace);
    const actor = getActor(req);
    await requireAccess(actor.actorId, actor.actorName, actor.actorType, namespace, 'write');
    const result = await saveNote(namespace, title, content, slug, created_by);
    res.json(result);
}));

router.post('/documents/list', apiRoute('documents', 'list', async (req, res) => {
    const { namespace, limit, offset, prefix } = req.body;

    if (!namespace) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: namespace' }
        });
    }

    validateNamespace(namespace);
    const actor = getActor(req);
    await requireAccess(actor.actorId, actor.actorName, actor.actorType, namespace, 'read');
    const result = await listNotes(namespace, limit, offset, prefix);
    res.json(result);
}));

router.post('/documents/read', apiRoute('documents', 'read', async (req, res) => {
    const { namespace, slug } = req.body;

    if (!namespace || !slug) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, slug' }
        });
    }

    validateNamespace(namespace);
    const actor = getActor(req);
    await requireAccess(actor.actorId, actor.actorName, actor.actorType, namespace, 'read');
    const result = await readNote(namespace, slug);
    res.json(result);
}));

router.post('/documents/delete', apiRoute('documents', 'delete', async (req, res) => {
    const { namespace, slug } = req.body;

    if (!namespace || !slug) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, slug' }
        });
    }

    validateNamespace(namespace);
    const actor = getActor(req);
    await requireAccess(actor.actorId, actor.actorName, actor.actorType, namespace, 'delete');
    const result = await deleteNote(namespace, slug);
    res.json(result);
}));

router.post('/documents/restore', apiRoute('documents', 'restore', async (req, res) => {
    const { namespace, slug } = req.body;

    if (!namespace || !slug) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, slug' }
        });
    }

    validateNamespace(namespace);
    const actor = getActor(req);
    await requireAccess(actor.actorId, actor.actorName, actor.actorType, namespace, 'write');
    const result = await restoreNote(namespace, slug);
    res.json(result);
}));

router.post('/documents/edit', apiRoute('documents', 'edit', async (req, res) => {
    const { namespace, slug, old_string, new_string, replace_all } = req.body;

    if (!namespace || !slug || !old_string || new_string === undefined) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, slug, old_string, new_string' }
        });
    }

    validateNamespace(namespace);
    const actor = getActor(req);
    await requireAccess(actor.actorId, actor.actorName, actor.actorType, namespace, 'write');
    const result = await editNote(namespace, slug, old_string, new_string, replace_all);
    res.json(result);
}));

router.post('/documents/move', apiRoute('documents', 'move', async (req, res) => {
    const { namespace, slug, new_slug, new_namespace } = req.body;

    if (!namespace || !slug || !new_slug) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, slug, new_slug. Optional: new_namespace' }
        });
    }

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
}));

router.post('/documents/grep', apiRoute('documents', 'grep', async (req, res) => {
    const { pattern, namespace, limit } = req.body;

    if (!pattern) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: pattern' }
        });
    }

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
}));

module.exports = router;
