// Service layer for memory operations (ingest, search, delete, cleanup, status).
// Extracted from routes/memory.js so both REST routes and MCP handler can share the same logic.

const pool = require('../db');
const { embed } = require('./embeddings');
const { chunkByHeading } = require('./chunker');
const pgvector = require('pgvector');

async function ingestContent(namespace, sourceFile, content) {
    const chunks = chunkByHeading(content);

    if (chunks.length === 0) {
        throw Object.assign(new Error('No chunks extracted from content'), { statusCode: 400 });
    }

    const texts = chunks.map(c => c.chunk_text);
    const embeddings = await embed(texts);

    const client = await pool.connect();
    try {
        await client.query('BEGIN');

        await client.query(
            'DELETE FROM memory_chunks WHERE namespace = $1 AND source_file = $2',
            [namespace, sourceFile]
        );

        for (let i = 0; i < chunks.length; i++) {
            await client.query(
                'INSERT INTO memory_chunks (namespace, source_file, heading, chunk_text, embedding) VALUES ($1, $2, $3, $4, $5)',
                [namespace, sourceFile, chunks[i].heading, chunks[i].chunk_text, pgvector.toSql(embeddings[i])]
            );
        }

        await client.query('COMMIT');
    } catch (err) {
        await client.query('ROLLBACK');
        throw err;
    } finally {
        client.release();
    }

    return { chunks_created: chunks.length, source_file: sourceFile, namespace };
}

async function searchMemory(query, namespace, limit) {
    const maxResults = limit || 5;
    const embeddings = await embed(query);
    const queryVector = pgvector.toSql(embeddings[0]);

    // Build ILIKE pattern from query words to boost chunks whose source_file
    // matches the search terms. This catches cases where the filename is the
    // most relevant signal but the chunk content uses different terminology.
    const queryWords = query.toLowerCase().split(/\s+/).filter(w => w.length > 2);

    let sql;
    let params;

    // Exclude chunks belonging to soft-deleted documents.
    // Chunks with no matching document row (from raw ingest) are kept.
    const softDeleteFilter = `
        AND NOT EXISTS (
            SELECT 1 FROM documents d
            WHERE d.namespace = mc.namespace
            AND d.slug = mc.source_file
            AND d.deleted_at IS NOT NULL
        )`;

    if (!namespace || namespace === '*') {
        const filenameClauses = queryWords.map((_, i) => `source_file ILIKE $${i + 3}`);
        const boostExpression = filenameClauses.length > 0
            ? `CASE WHEN ${filenameClauses.join(' OR ')} THEN 0.15 ELSE 0 END`
            : '0';
        sql = `
            SELECT source_file, heading, chunk_text, namespace,
                   (1 - (embedding <=> $1)) + ${boostExpression} AS similarity
            FROM memory_chunks mc
            WHERE 1=1 ${softDeleteFilter}
            ORDER BY similarity DESC
            LIMIT $2
        `;
        params = [queryVector, maxResults, ...queryWords.map(w => `%${w}%`)];
    } else {
        const filenameClauses = queryWords.map((_, i) => `source_file ILIKE $${i + 4}`);
        const boostExpression = filenameClauses.length > 0
            ? `CASE WHEN ${filenameClauses.join(' OR ')} THEN 0.15 ELSE 0 END`
            : '0';
        sql = `
            SELECT source_file, heading, chunk_text, namespace,
                   (1 - (embedding <=> $1)) + ${boostExpression} AS similarity
            FROM memory_chunks mc
            WHERE namespace = $2 ${softDeleteFilter}
            ORDER BY similarity DESC
            LIMIT $3
        `;
        params = [queryVector, namespace, maxResults, ...queryWords.map(w => `%${w}%`)];
    }

    const result = await pool.query(sql, params);
    return { results: result.rows };
}

async function deleteMemory(namespace, sourceFile) {
    const result = await pool.query(
        'DELETE FROM memory_chunks WHERE namespace = $1 AND source_file = $2',
        [namespace, sourceFile]
    );
    return { chunks_deleted: result.rowCount };
}

async function cleanupMemory(namespace, validSourceFiles) {
    const existing = await pool.query(
        'SELECT DISTINCT source_file FROM memory_chunks WHERE namespace = $1',
        [namespace]
    );

    const validSet = new Set(validSourceFiles);
    const orphans = existing.rows
        .map(r => r.source_file)
        .filter(sf => !validSet.has(sf));

    if (orphans.length === 0) {
        return { orphans_deleted: 0, orphan_files: [] };
    }

    const result = await pool.query(
        'DELETE FROM memory_chunks WHERE namespace = $1 AND source_file = ANY($2)',
        [namespace, orphans]
    );

    return { orphans_deleted: result.rowCount, orphan_files: orphans };
}

async function ingestStatus(namespace) {
    let query = `
        SELECT namespace, source_file, MAX(ingested_at) as ingested_at, COUNT(*) as chunk_count
        FROM memory_chunks
    `;
    const params = [];

    if (namespace) {
        query += ' WHERE namespace = $1';
        params.push(namespace);
    }

    query += ' GROUP BY namespace, source_file ORDER BY namespace, source_file';

    const result = await pool.query(query, params);
    return { files: result.rows };
}

module.exports = { ingestContent, searchMemory, deleteMemory, cleanupMemory, ingestStatus };
