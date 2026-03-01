const { Router } = require('express');
const { saveNote, listNotes, readNote, deleteNote, editNote, grepNotes } = require('../services/documents');

const router = Router();

router.post('/documents/save', async (req, res) => {
    const { namespace, title, content, slug, created_by } = req.body;

    if (!namespace || !title || !content) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, title, content' }
        });
    }

    try {
        const result = await saveNote(namespace, title, content, slug, created_by);
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        console.error('Document save error:', err.message);
        res.status(status).json({
            error: { code: status === 400 ? 'BAD_REQUEST' : 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/documents/list', async (req, res) => {
    const { namespace, limit, offset } = req.body;

    if (!namespace) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: namespace' }
        });
    }

    try {
        const result = await listNotes(namespace, limit, offset);
        res.json(result);
    } catch (err) {
        console.error('Document list error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
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
        const result = await readNote(namespace, slug);
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        console.error('Document read error:', err.message);
        res.status(status).json({
            error: { code: status === 404 ? 'NOT_FOUND' : 'INTERNAL_ERROR', message: err.message }
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
        const result = await deleteNote(namespace, slug);
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        console.error('Document delete error:', err.message);
        res.status(status).json({
            error: { code: status === 404 ? 'NOT_FOUND' : 'INTERNAL_ERROR', message: err.message }
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
        const result = await editNote(namespace, slug, old_string, new_string, replace_all);
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        console.error('Document edit error:', err.message);
        res.status(status).json({
            error: { code: status === 400 ? 'BAD_REQUEST' : status === 404 ? 'NOT_FOUND' : 'INTERNAL_ERROR', message: err.message }
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
        const results = await grepNotes(pattern, namespace, limit);
        res.json({ results });
    } catch (err) {
        const status = err.statusCode || 500;
        console.error('Document grep error:', err.message);
        res.status(status).json({
            error: { code: status === 400 ? 'BAD_REQUEST' : 'INTERNAL_ERROR', message: err.message }
        });
    }
});

module.exports = router;
