const { Router } = require('express');
const pool = require('../db');
const { embed } = require('../services/embeddings');
const { chunkByHeading } = require('../services/chunker');
const pgvector = require('pgvector');

const router = Router();

router.post('/ingest', async (req, res) => {
    const { namespace, source_file, content } = req.body;

    if (!namespace || !source_file || !content) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, source_file, content' }
        });
    }

    try {
        const chunks = chunkByHeading(content);

        if (chunks.length === 0) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'No chunks extracted from content' }
            });
        }

        const texts = chunks.map(c => c.chunk_text);
        const embeddings = await embed(texts);

        const client = await pool.connect();
        try {
            await client.query('BEGIN');

            await client.query(
                'DELETE FROM chunks WHERE namespace = $1 AND source_file = $2',
                [namespace, source_file]
            );

            for (let i = 0; i < chunks.length; i++) {
                await client.query(
                    'INSERT INTO chunks (namespace, source_file, heading, chunk_text, embedding) VALUES ($1, $2, $3, $4, $5)',
                    [namespace, source_file, chunks[i].heading, chunks[i].chunk_text, pgvector.toSql(embeddings[i])]
                );
            }

            await client.query('COMMIT');
        } catch (err) {
            await client.query('ROLLBACK');
            throw err;
        } finally {
            client.release();
        }

        res.json({
            chunks_created: chunks.length,
            source_file,
            namespace
        });
    } catch (err) {
        console.error('Ingest error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

// Returns all indexed source files with their latest ingestion timestamp and chunk count.
// Optional namespace filter in request body. Used by the MCP ingest_notes tool to determine
// which files need re-ingestion by comparing against local filesystem mtimes.
router.post('/ingest/status', async (req, res) => {
    const { namespace } = req.body;

    try {
        let query = `
            SELECT namespace, source_file, MAX(ingested_at) as ingested_at, COUNT(*) as chunk_count
            FROM chunks
        `;
        const params = [];

        if (namespace) {
            query += ' WHERE namespace = $1';
            params.push(namespace);
        }

        query += ' GROUP BY namespace, source_file ORDER BY namespace, source_file';

        const result = await pool.query(query, params);

        res.json({
            files: result.rows
        });
    } catch (err) {
        console.error('Ingest status error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

module.exports = router;
