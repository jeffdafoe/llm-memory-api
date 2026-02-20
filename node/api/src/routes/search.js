const { Router } = require('express');
const pool = require('../db');
const { embed } = require('../services/embeddings');
const pgvector = require('pgvector');

const router = Router();

router.post('/search', async (req, res) => {
    const { query, namespace, limit } = req.body;

    if (!query) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: query' }
        });
    }

    const maxResults = limit || 5;
    const embeddings = await embed(query);
    const queryVector = pgvector.toSql(embeddings[0]);

    let sql;
    let params;

    if (!namespace || namespace === '*') {
        sql = `
            SELECT source_file, heading, chunk_text, namespace,
                   1 - (embedding <=> $1) AS similarity
            FROM chunks
            ORDER BY embedding <=> $1
            LIMIT $2
        `;
        params = [queryVector, maxResults];
    } else {
        sql = `
            SELECT source_file, heading, chunk_text, namespace,
                   1 - (embedding <=> $1) AS similarity
            FROM chunks
            WHERE namespace = $2
            ORDER BY embedding <=> $1
            LIMIT $3
        `;
        params = [queryVector, namespace, maxResults];
    }

    const result = await pool.query(sql, params);

    res.json({
        results: result.rows
    });
});

module.exports = router;
