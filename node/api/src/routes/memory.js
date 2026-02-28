const { Router } = require('express');
const { ingestContent, searchMemory, deleteMemory, cleanupMemory, ingestStatus } = require('../services/memory');

const router = Router();

router.post('/memory/ingest', async (req, res) => {
    const { namespace, source_file, content } = req.body;

    if (!namespace || !source_file || !content) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, source_file, content' }
        });
    }

    try {
        const result = await ingestContent(namespace, source_file, content);
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        console.error('Memory ingest error:', err.message);
        res.status(status).json({
            error: { code: status === 400 ? 'BAD_REQUEST' : 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/memory/ingest/status', async (req, res) => {
    const { namespace } = req.body;

    try {
        const result = await ingestStatus(namespace);
        res.json(result);
    } catch (err) {
        console.error('Memory ingest/status error:', err.message);
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
        const result = await searchMemory(query, namespace, limit);
        res.json(result);
    } catch (err) {
        console.error('Memory search error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
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
        const result = await cleanupMemory(namespace, valid_source_files);
        res.json(result);
    } catch (err) {
        console.error('Memory cleanup error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
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
        const result = await deleteMemory(namespace, source_file);
        res.json(result);
    } catch (err) {
        console.error('Memory delete error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

module.exports = router;
