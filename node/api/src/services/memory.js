// Service layer for memory operations (ingest, search, delete, cleanup, status).
// Extracted from routes/memory.js so both REST routes and MCP handler can share the same logic.

const pool = require('../db');
const { embed } = require('./embeddings');
const { chunkByHeading, chunkConversation } = require('./chunker');
const pgvector = require('pgvector');
const config = require('./config');

function parseNonNegativeFinite(value, fallback = 0) {
    const n = Number(value);
    if (Number.isFinite(n) && n >= 0) return n;
    return fallback;
}

// Query preprocessing: strip filler words/phrases before embedding.
// "can you tell me about how the auth middleware works" → "auth middleware works"
const FILLER_PHRASES = [
    /^(can you |could you |please |tell me |show me |explain |describe |help me |i want to know |i need to know |what is |what are |what's |how do i |how does |how do |how to |where is |where are |where's |who is |who are |find me |search for |look for |give me |let me know )/i,
    /^(about |regarding |related to |concerning )/i,
];
const FILLER_WORDS = new Set([
    'a', 'an', 'the', 'is', 'are', 'was', 'were', 'be', 'been', 'being',
    'do', 'does', 'did', 'have', 'has', 'had', 'will', 'would', 'could',
    'should', 'can', 'may', 'might', 'shall', 'must',
    'i', 'me', 'my', 'we', 'our', 'you', 'your',
    'it', 'its', 'this', 'that', 'these', 'those',
    'of', 'in', 'to', 'for', 'with', 'on', 'at', 'by', 'from',
    'and', 'or', 'but', 'not', 'so', 'if', 'then',
    'just', 'also', 'very', 'really', 'actually', 'basically',
    'please', 'thanks', 'thank',
]);

function preprocessQuery(query) {
    let cleaned = query.trim();
    // Strip leading filler phrases (apply repeatedly for stacked phrases)
    let prev;
    do {
        prev = cleaned;
        for (const re of FILLER_PHRASES) {
            cleaned = cleaned.replace(re, '');
        }
        cleaned = cleaned.trim();
    } while (cleaned !== prev && cleaned.length > 0);

    // If stripping phrases left us with nothing, fall back to original
    if (cleaned.length < 3) return query.trim();

    // Strip individual filler words
    const words = cleaned.split(/\s+/).filter(w => !FILLER_WORDS.has(w.toLowerCase()));

    // If we stripped too aggressively, fall back
    if (words.length === 0) return query.trim();

    return words.join(' ');
}

async function ingestContent(namespace, sourceFile, content) {
    // Context documents (e.g. context/soul) are injected directly into sessions,
    // not indexed for search — skip chunking entirely.
    if (sourceFile.startsWith('context/')) {
        return { chunks_created: 0 };
    }

    // Use conversation-aware chunking for conversation logs, heading-based for everything else.
    // Conversation chunk params are configurable — smaller windows produce more focused embeddings.
    const isConversation = sourceFile.startsWith('conversations/');
    let chunks;
    if (isConversation) {
        const config = require('./config');
        const windowSize = parseInt(config.get('conversation_chunk_window')) || 5;
        const overlap = parseInt(config.get('conversation_chunk_overlap')) || 2;
        const maxChars = parseInt(config.get('conversation_chunk_max_chars')) || 0;
        chunks = chunkConversation(content, windowSize, overlap, maxChars);
    } else {
        chunks = chunkByHeading(content);
    }

    if (chunks.length === 0) {
        throw Object.assign(new Error('No chunks extracted from content'), { statusCode: 400 });
    }

    const texts = chunks.map(c => c.chunk_text);
    const embeddings = await embed(texts);

    const client = await pool.connect();
    try {
        await client.query('BEGIN');

        await client.query(
            'DELETE FROM memory_chunks WHERE namespace = $1 AND LOWER(source_file) = LOWER($2)',
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

async function searchMemory(query, namespace, limit, readableNamespaces, actorId) {
    const maxResults = limit || 5;
    const cleanedQuery = preprocessQuery(query);
    const embeddings = await embed(cleanedQuery);
    const queryVector = pgvector.toSql(embeddings[0]);

    // Candidate pool: fetch more results than requested from pgvector, then
    // apply all boosts (BM25, filename, graph, decay, access) and trim down.
    // This improves recall — results that rank low on pure vector similarity
    // can bubble up after boosts are applied.
    const poolMultiplier = parseNonNegativeFinite(config.get('search_pool_multiplier'), 3);
    const poolSize = Math.max(maxResults, Math.round(maxResults * poolMultiplier));

    // Build ILIKE pattern from query words to boost chunks whose source_file
    // matches the search terms. This catches cases where the filename is the
    // most relevant signal but the chunk content uses different terminology.
    const queryWords = cleanedQuery.toLowerCase().split(/\s+/).filter(w => w.length > 2);

    // Load boost magnitudes from config (tunable without redeploy)
    const filenameBoostValue = parseNonNegativeFinite(config.get('search_filename_boost'), 0.15);
    const bm25BoostScale = parseNonNegativeFinite(config.get('search_bm25_boost'), 0.1);

    // Load decay/boost config (validated to finite non-negative numbers)
    const halfLives = {
        task: parseNonNegativeFinite(config.get('search_decay_halflife_task')),
        learning: parseNonNegativeFinite(config.get('search_decay_halflife_learning')),
        note: parseNonNegativeFinite(config.get('search_decay_halflife_note')),
        reference: parseNonNegativeFinite(config.get('search_decay_halflife_reference')),
        instruction: parseNonNegativeFinite(config.get('search_decay_halflife_instruction')),
        conversation: parseNonNegativeFinite(config.get('search_decay_halflife_conversation')),
        dream: parseNonNegativeFinite(config.get('search_decay_halflife_dream')),
    };
    const accessBoostMax = parseNonNegativeFinite(config.get('search_access_boost_max'));
    const accessBoostWindowDays = parseNonNegativeFinite(config.get('search_access_boost_window_days'));
    const accessBoostWindowSeconds = accessBoostWindowDays * 86400;

    // Kind weight multipliers: applied to results so noisy kinds don't outrank
    // same-relevance curated notes. 1.0 = no penalty, 0.0 = invisible.
    const conversationWeight = parseNonNegativeFinite(config.get('search_conversation_weight'), 0.7);
    const dreamWeight = parseNonNegativeFinite(config.get('search_dream_weight'), 1.0);

    // Build params array incrementally. All config values are bound, not interpolated.
    // paramIdx tracks the next available $N placeholder.
    let params;
    let paramIdx;

    if (!namespace || namespace === '*') {
        params = [queryVector, poolSize];
        paramIdx = 3;
    } else {
        params = [queryVector, namespace, poolSize];
        paramIdx = 4;
    }

    // ILIKE filename boost params
    const filenameStartIdx = paramIdx;
    for (const w of queryWords) {
        params.push(`%${w}%`);
        paramIdx++;
    }
    const filenameClauses = queryWords.map((_, i) => `mc.source_file ILIKE $${filenameStartIdx + i}`);
    // Filename boost magnitude is config-driven (search_filename_boost, default 0.15)
    const fnBoostIdx = paramIdx;
    params.push(filenameBoostValue);
    paramIdx++;
    const filenameBoostExpr = filenameClauses.length > 0
        ? `CASE WHEN ${filenameClauses.join(' OR ')} THEN $${fnBoostIdx} ELSE 0 END`
        : '0';

    // BM25 full-text search boost (gated by vector similarity threshold).
    // Only boosts chunks that already have decent vector similarity, preventing
    // keyword-heavy but semantically irrelevant chunks from polluting results.
    // Convert query words to a tsquery using & (AND) for precision.
    let bm25BoostExpr = '0';
    if (queryWords.length > 0) {
        const tsqIdx = paramIdx;
        // plainto_tsquery is safe with arbitrary input — no special syntax needed
        params.push(cleanedQuery);
        paramIdx++;
        // ts_rank returns 0-1ish; scale by config value so it's a tunable boost.
        // Gate: only apply when vector similarity > 0.3 (cosine distance < 0.7)
        const bm25ScaleIdx = paramIdx;
        params.push(bm25BoostScale);
        paramIdx++;
        bm25BoostExpr = `CASE WHEN (1 - (mc.embedding <=> $1)) > 0.3 AND mc.tsv IS NOT NULL
            THEN $${bm25ScaleIdx} * ts_rank(mc.tsv, plainto_tsquery('english', $${tsqIdx}))
            ELSE 0 END`;
    }

    // Namespace filter param (global search only)
    let nsFilter = '';
    if ((!namespace || namespace === '*') && readableNamespaces) {
        // Include chunks from: (1) namespaces the actor has full read access to,
        // or (2) specific notes/folders shared via note_permissions.
        const nsArrayIdx = paramIdx;
        params.push(readableNamespaces);
        paramIdx++;

        if (actorId) {
            const actorIdx = paramIdx;
            params.push(actorId);
            paramIdx++;
            nsFilter = `AND (mc.namespace = ANY($${nsArrayIdx}) OR EXISTS (
                SELECT 1 FROM note_permissions np
                WHERE np.owner_namespace = mc.namespace
                  AND (np.slug_pattern = mc.source_file OR mc.source_file LIKE np.slug_pattern || '%')
                  AND (np.grantee_actor_id = $${actorIdx} OR np.grantee_actor_id IS NULL)
                  AND np.revoked_at IS NULL AND np.can_read = true
            ))`;
        } else {
            nsFilter = `AND mc.namespace = ANY($${nsArrayIdx})`;
        }
    }

    // Time-decay CASE expression per kind (half-life values as bound params).
    // decay = 0.5 ^ (age_days / half_life). If half_life is 0, no decay (1.0).
    // Kind names are hardcoded strings, not user input.
    const decayCases = Object.entries(halfLives).map(([kind, hl]) => {
        if (!hl) return `WHEN d.kind = '${kind}' THEN 1.0`;
        const hlIdx = paramIdx;
        params.push(hl);
        paramIdx++;
        return `WHEN d.kind = '${kind}' THEN POWER(0.5, EXTRACT(EPOCH FROM (NOW() - COALESCE(d.updated_at, d.created_at))) / 86400.0 / $${hlIdx})`;
    });
    const decayExpression = `CASE ${decayCases.join(' ')} ELSE 1.0 END`;

    // Kind-level weight multipliers
    const convWeightIdx = paramIdx;
    params.push(conversationWeight);
    paramIdx++;
    const dreamWeightIdx = paramIdx;
    params.push(dreamWeight);
    paramIdx++;
    const kindWeightExpression = `CASE WHEN d.kind = 'conversation' THEN $${convWeightIdx} WHEN d.kind = 'dream' THEN $${dreamWeightIdx} ELSE 1.0 END`;

    // Access boost: linear ramp-down from max to 0 over the window.
    // Uses epoch math with bound params.
    let accessBoostExpression = '0';
    if (accessBoostMax > 0 && accessBoostWindowSeconds > 0) {
        const boostMaxIdx = paramIdx;
        params.push(accessBoostMax);
        paramIdx++;
        const windowIdx = paramIdx;
        params.push(accessBoostWindowSeconds);
        paramIdx++;
        accessBoostExpression = `
            CASE WHEN d.last_accessed IS NOT NULL
                AND EXTRACT(EPOCH FROM (NOW() - d.last_accessed)) < $${windowIdx}
            THEN $${boostMaxIdx} * (1.0 - EXTRACT(EPOCH FROM (NOW() - d.last_accessed)) / $${windowIdx})
            ELSE 0 END`;
    }

    // Soft-delete filter: exclude chunks belonging to deleted documents.
    // The LEFT JOIN matches ALL document rows (including deleted ones). We filter
    // in WHERE so that: d.id IS NULL = no document row (raw ingest, kept),
    // d.deleted_at IS NULL = active document (kept), otherwise excluded.
    // IMPORTANT: Do not add deleted_at filtering to the ON clause — that would
    // make deleted documents look like raw ingests and incorrectly include them.
    const softDeleteFilter = 'AND (d.id IS NULL OR d.deleted_at IS NULL)';

    let sql;

    if (!namespace || namespace === '*') {
        sql = `
            SELECT mc.source_file, mc.heading, mc.chunk_text, mc.namespace,
                   ((1 - (mc.embedding <=> $1)) + ${filenameBoostExpr} + ${bm25BoostExpr} + ${accessBoostExpression})
                   * ${decayExpression} * ${kindWeightExpression} AS similarity
            FROM memory_chunks mc
            LEFT JOIN documents d ON d.namespace = mc.namespace AND LOWER(d.slug) = LOWER(mc.source_file)
            WHERE 1=1 ${nsFilter} ${softDeleteFilter}
            ORDER BY similarity DESC
            LIMIT $2
        `;
    } else {
        sql = `
            SELECT mc.source_file, mc.heading, mc.chunk_text, mc.namespace,
                   ((1 - (mc.embedding <=> $1)) + ${filenameBoostExpr} + ${bm25BoostExpr} + ${accessBoostExpression})
                   * ${decayExpression} * ${kindWeightExpression} AS similarity
            FROM memory_chunks mc
            LEFT JOIN documents d ON d.namespace = mc.namespace AND LOWER(d.slug) = LOWER(mc.source_file)
            WHERE mc.namespace = $2 ${softDeleteFilter}
            ORDER BY similarity DESC
            LIMIT $3
        `;
    }

    const result = await pool.query(sql, params);
    let rows = result.rows;

    // Graph boost: results connected to top results via note relations get a score bump.
    // Post-processing step — queries the relation graph for the top N results and boosts
    // any other results that share a relation edge with them.
    const graphBoost = parseNonNegativeFinite(config.get('search_graph_boost'), 0);
    if (graphBoost > 0 && rows.length > 1) {
        try {
            // Take the top 5 results as "anchor" nodes
            const topN = Math.min(5, rows.length);
            const anchors = rows.slice(0, topN);
            const anchorKeys = new Set(anchors.map(r => (r.namespace || namespace) + '/' + r.source_file));

            // Query relations for all anchor notes in one batch
            const anchorPairs = anchors.map(r => [r.namespace || namespace, r.source_file]);
            const relResult = await pool.query(`
                SELECT source_namespace, source_slug, target_namespace, target_slug
                FROM note_relations
                WHERE ${anchorPairs.map((_, i) =>
                    `(source_namespace = $${i * 2 + 1} AND LOWER(source_slug) = LOWER($${i * 2 + 2}))
                     OR (target_namespace = $${i * 2 + 1} AND LOWER(target_slug) = LOWER($${i * 2 + 2}))`
                ).join(' OR ')}
            `, anchorPairs.flat());

            // Build set of notes connected to anchors
            const connectedKeys = new Set();
            for (const rel of relResult.rows) {
                const sKey = rel.source_namespace + '/' + rel.source_slug;
                const tKey = rel.target_namespace + '/' + rel.target_slug;
                // If one side is an anchor, the other side gets boosted
                if (anchorKeys.has(sKey)) connectedKeys.add(tKey.toLowerCase());
                if (anchorKeys.has(tKey)) connectedKeys.add(sKey.toLowerCase());
            }
            // Don't boost the anchors themselves
            for (const k of anchorKeys) connectedKeys.delete(k.toLowerCase());

            // Apply boost to connected results
            if (connectedKeys.size > 0) {
                for (const row of rows) {
                    const key = ((row.namespace || namespace) + '/' + row.source_file).toLowerCase();
                    if (connectedKeys.has(key)) {
                        row.similarity = parseFloat(row.similarity) + graphBoost;
                    }
                }
                // Re-sort by boosted similarity
                rows.sort((a, b) => parseFloat(b.similarity) - parseFloat(a.similarity));
            }
        } catch (e) {
            // Graph boost failure shouldn't break search
        }
    }

    // Trim the candidate pool down to the requested result count.
    // All boosts (BM25, filename, graph, decay, access, kind weight) have been
    // applied, so the final ordering reflects the full scoring pipeline.
    if (rows.length > maxResults) {
        rows = rows.slice(0, maxResults);
    }

    return { results: rows };
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
