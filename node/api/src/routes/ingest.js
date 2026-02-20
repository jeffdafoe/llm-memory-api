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
});

module.exports = router;
