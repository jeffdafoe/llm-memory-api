const { Router } = require('express');
const { saveNote, listNotes, readNote, deleteNote, restoreNote, editNote, grepNotes, moveNote, paginateContent } = require('../services/documents');
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
    const { namespace, title, content, slug, created_by, upsert } = req.body;

    if (!namespace || !title || !content) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, title, content' }
        });
    }

    validateNamespace(namespace);
    const actor = getActor(req);
    await requireAccess(actor.actorId, actor.actorName, actor.actorType, namespace, 'write');
    // upsert:true allows overwriting an existing note at the same slug. Used by
    // sync clients (e.g. Go memory-sync) where "the local file is the source of
    // truth and may differ from the server" is the expected semantics. Default
    // false so callers without this intent get the safer 409 DUPLICATE_SLUG path.
    const opts = upsert === true ? { upsert: true } : undefined;
    const result = await saveNote(namespace, title, content, slug, created_by, null, null, opts);
    res.json(result);
}));

router.post('/documents/list', apiRoute('documents', 'list', async (req, res) => {
    const { namespace, limit, offset, prefix, include_deleted } = req.body;

    if (!namespace) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: namespace' }
        });
    }

    validateNamespace(namespace);
    const actor = getActor(req);
    await requireAccess(actor.actorId, actor.actorName, actor.actorType, namespace, 'read');
    const result = await listNotes(namespace, limit, offset, prefix, { include_deleted: !!include_deleted });
    res.json(result);
}));

router.post('/documents/read', apiRoute('documents', 'read', async (req, res) => {
    const { namespace, slug, offset, limit } = req.body;

    if (!namespace || !slug) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, slug' }
        });
    }

    validateNamespace(namespace);
    const actor = getActor(req);
    await requireAccess(actor.actorId, actor.actorName, actor.actorType, namespace, 'read');
    const result = await readNote(namespace, slug);

    // Apply pagination if offset or limit was supplied. When neither is
    // given, the response is byte-for-byte identical to the pre-pagination
    // shape — callers that expect the full content (admin UI, memory-sync)
    // are unaffected.
    const paginated = paginateContent(result.content, offset, limit);
    if (paginated.paginated) {
        result.content = paginated.text;
        result.total_lines = paginated.totalLines;
    }
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
    const { pattern, namespace, limit, context_before, context_after, context, regex } = req.body;

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
    const options = {
        contextBefore: context_before,
        contextAfter: context_after,
        context,
        regex
    };
    const results = await grepNotes(pattern, namespace, limit, readable, options);
    res.json({ results });
}));

module.exports = router;
