// Service layer for memory operations (ingest, search, delete, cleanup, status).
// Extracted from routes/memory.js so both REST routes and MCP handler can share the same logic.

const pool = require('../db');
const { embed } = require('./embeddings');
const { chunkByHeading, chunkConversation } = require('./chunker');
const pgvector = require('pgvector');
const config = require('./config');
const { handleError } = require('./error-handler');

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

// OpenAI's text-embedding-3-small caps each input at 8192 tokens. Search callers
// pass raw inbound content — a virtual agent's mail subject+body, chat turns, a
// discussion topic — that routinely exceeds that (a review diff mailed to
// code_review is 25KB+). Without a clamp the embeddings API 400s and the caller
// (loadRAGContext) swallows it, so the VA answers with zero memory recall exactly
// when context matters most, leaving only a rag-error journal line. Clamp by chars:
// the codebase carries no tokenizer, and ~3 chars/token for dense diffs keeps this
// near 6000 tokens, well under the cap. Truncating a search query is cheap —
// semantic search over the head of a long message is nearly as good as the whole,
// and far better than the empty-recall fallback.
const MAX_EMBED_QUERY_CHARS = 18000;

function preprocessQuery(query) {
    // Defensive guard — the MCP dispatcher validates required fields, but
    // the /v1/search HTTP route and internal callers may reach here with a
    // non-string. Return empty to keep embedding calls from crashing; the
    // caller will get "no results" rather than a 500.
    if (typeof query !== 'string') return '';
    // Clamp before any processing so every return path below — including the two
    // raw-query fallbacks — stays within the embedding budget. slice() cuts UTF-16
    // code units, so it can land inside a surrogate pair; drop a dangling high
    // surrogate so the truncated query holds no malformed trailing character.
    if (query.length > MAX_EMBED_QUERY_CHARS) {
        query = query.slice(0, MAX_EMBED_QUERY_CHARS);
        if (/[\uD800-\uDBFF]$/.test(query)) {
            query = query.slice(0, -1);
        }
    }
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

// Escape the three LIKE metacharacters so a caller-supplied prefix is matched
// literally. Backslash is the ESCAPE character in the LIKE clauses below, so it
// must be escaped first (the alternation handles that — each match is replaced
// with a backslash + itself). Without this, an underscore in a slug prefix
// (sanitize.identifier permits '_') would act as a single-char wildcard, and a
// '%' would match anything — a scoping filter that silently over-matches.
function escapeLikePattern(value) {
    return value.replace(/[\\%_]/g, '\\$&');
}

async function searchMemory(query, namespace, limit, readableNamespaces, actorId, slugPrefix) {
    // Coerce limit to a positive integer — Postgres LIMIT requires an integer,
    // and callers may pass floats (e.g. 0.15) which cause a SQL type error.
    const maxResults = Math.max(1, Math.floor(limit) || 5);
    const cleanedQuery = preprocessQuery(query);
    const embeddings = await embed(cleanedQuery);
    const queryVector = pgvector.toSql(embeddings[0]);

    // Candidate pool: fetch more results than requested from pgvector, then
    // apply all boosts (BM25, filename, decay, access) and trim down.
    // This improves recall — results that rank low on pure vector similarity
    // can bubble up after boosts are applied.
    const poolMultiplier = Math.min(parseNonNegativeFinite(config.get('search_pool_multiplier'), 3), 10);
    const poolSize = Math.min(Math.max(maxResults, Math.round(maxResults * poolMultiplier)), 500);

    // Build ILIKE pattern from query words to boost chunks whose source_file
    // matches the search terms. This catches cases where the filename is the
    // most relevant signal but the chunk content uses different terminology.
    const queryWords = cleanedQuery.toLowerCase().split(/\s+/).filter(w => w.length > 2);

    // Load boost magnitudes from config (tunable without redeploy)
    const filenameBoostValue = parseNonNegativeFinite(config.get('search_filename_boost'), 0.15);
    const bm25BoostScale = parseNonNegativeFinite(config.get('search_bm25_boost'), 0.1);

    // Load decay/boost config (validated to finite non-negative numbers)
    // Kind-based half-lives (fallback when no cognitive type is set)
    const halfLives = {
        task: parseNonNegativeFinite(config.get('search_decay_halflife_task')),
        learning: parseNonNegativeFinite(config.get('search_decay_halflife_learning')),
        note: parseNonNegativeFinite(config.get('search_decay_halflife_note')),
        reference: parseNonNegativeFinite(config.get('search_decay_halflife_reference')),
        instruction: parseNonNegativeFinite(config.get('search_decay_halflife_instruction')),
        conversation: parseNonNegativeFinite(config.get('search_decay_halflife_conversation')),
        dream: parseNonNegativeFinite(config.get('search_decay_halflife_dream')),
    };
    // Cognitive type half-lives (override kind-based when metadata.cognitive_type is set)
    const cognitiveHalfLives = {
        semantic: parseNonNegativeFinite(config.get('search_decay_halflife_semantic')),
        episodic: parseNonNegativeFinite(config.get('search_decay_halflife_episodic'), 90),
        procedural: parseNonNegativeFinite(config.get('search_decay_halflife_procedural')),
        reflective: parseNonNegativeFinite(config.get('search_decay_halflife_reflective'), 180),
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
        ? `CASE WHEN ${filenameClauses.join(' OR ')} THEN $${fnBoostIdx}::numeric ELSE 0.0 END`
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
            THEN $${bm25ScaleIdx}::numeric * ts_rank(mc.tsv, plainto_tsquery('english', $${tsqIdx}))
            ELSE 0.0 END`;
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

    // Slug-prefix scope (LLM-355): narrow a search to source_files under a
    // prefix, e.g. "anne-walker/memory/" so one shared-VA namespace holds many
    // NPCs' private memory without any of them recalling another's. Undefined =
    // no filter, so existing callers are unaffected. The prefix is escaped and
    // matched case-insensitively (consistent with the LOWER()-based join), then
    // '%' is appended as the one live wildcard. Interpolated into BOTH inner
    // selects below — a filter applied to only one path is a silent leak.
    let slugPrefixFilter = '';
    if (typeof slugPrefix === 'string' && slugPrefix.length > 0) {
        const slugIdx = paramIdx;
        params.push(escapeLikePattern(slugPrefix.toLowerCase()) + '%');
        paramIdx++;
        slugPrefixFilter = `AND LOWER(mc.source_file) LIKE $${slugIdx} ESCAPE '\\'`;
    }

    // Time-decay: two-tier system. Cognitive type takes priority when available
    // (stored in metadata->>'cognitive_type' by enrichment), falls back to kind-based.
    // decay = 0.5 ^ (age_days / half_life). If half_life is 0, no decay (1.0).
    // Age is based on the most recent of created_at, updated_at, or last_accessed —
    // so a note that keeps getting pulled into search results stays fresh.

    // Cognitive type decay cases (checked first via metadata)
    // Use LOWER(TRIM(...)) to handle any legacy data with inconsistent casing/whitespace
    const cogDecayCases = Object.entries(cognitiveHalfLives).map(([ctype, hl]) => {
        if (!hl) return `WHEN LOWER(TRIM(d.metadata->>'cognitive_type')) = '${ctype}' THEN 1.0`;
        const hlIdx = paramIdx;
        params.push(hl);
        paramIdx++;
        return `WHEN LOWER(TRIM(d.metadata->>'cognitive_type')) = '${ctype}' THEN POWER(0.5, EXTRACT(EPOCH FROM (NOW() - GREATEST(d.created_at, COALESCE(d.updated_at, d.created_at), COALESCE(d.last_accessed, d.created_at)))) / 86400.0 / $${hlIdx}::numeric)`;
    });

    // Kind-based decay cases (fallback for notes without cognitive type)
    const kindDecayCases = Object.entries(halfLives).map(([kind, hl]) => {
        if (!hl) return `WHEN d.kind = '${kind}' THEN 1.0`;
        const hlIdx = paramIdx;
        params.push(hl);
        paramIdx++;
        return `WHEN d.kind = '${kind}' THEN POWER(0.5, EXTRACT(EPOCH FROM (NOW() - GREATEST(d.created_at, COALESCE(d.updated_at, d.created_at), COALESCE(d.last_accessed, d.created_at)))) / 86400.0 / $${hlIdx}::numeric)`;
    });

    // Cognitive type cases go first — if metadata has a cognitive_type, use that decay.
    // Otherwise fall through to kind-based decay.
    const decayExpression = `CASE ${cogDecayCases.join(' ')} ${kindDecayCases.join(' ')} ELSE 1.0 END`;

    // Kind-level weight multipliers
    const convWeightIdx = paramIdx;
    params.push(conversationWeight);
    paramIdx++;
    const dreamWeightIdx = paramIdx;
    params.push(dreamWeight);
    paramIdx++;
    const kindWeightExpression = `CASE WHEN d.kind = 'conversation' THEN $${convWeightIdx}::numeric WHEN d.kind = 'dream' THEN $${dreamWeightIdx}::numeric ELSE 1.0 END`;

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
            THEN $${boostMaxIdx}::numeric * (1.0 - EXTRACT(EPOCH FROM (NOW() - d.last_accessed)) / $${windowIdx}::numeric)
            ELSE 0.0 END`;
    }

    // Soft-delete filter: exclude chunks belonging to deleted documents.
    // The LEFT JOIN matches ALL document rows (including deleted ones). We filter
    // in WHERE so that: d.id IS NULL = no document row (raw ingest, kept),
    // d.deleted_at IS NULL = active document (kept), otherwise excluded.
    // IMPORTANT: Do not add deleted_at filtering to the ON clause — that would
    // make deleted documents look like raw ingests and incorrectly include them.
    const softDeleteFilter = 'AND (d.id IS NULL OR d.deleted_at IS NULL)';

    // Inner SELECT scores chunks through the full pipeline (vector + BM25 +
    // filename + access + decay + kind weight). Outer SELECT collapses to one
    // row per note: keep the highest-scoring chunk as the snippet, expose
    // chunk_count so callers can see "this note matched in N places".
    let innerSql;

    // created_at rides along from the documents join (LLM-390) so callers can
    // show how old a memory is — salem's recall tool renders it as "From two
    // days ago — <topic>". Deliberately created_at alone, NOT the
    // GREATEST(created/updated/last_accessed) the decay uses: last_accessed
    // re-stamps on every search hit, so an age based on it would make every
    // recalled memory look freshly written. NULL for raw ingests (no documents
    // row).
    if (!namespace || namespace === '*') {
        innerSql = `
            SELECT mc.source_file, mc.heading, mc.chunk_text, mc.namespace, d.created_at,
                   ((1 - (mc.embedding <=> $1)) + ${filenameBoostExpr} + ${bm25BoostExpr} + ${accessBoostExpression})
                   * ${decayExpression} * ${kindWeightExpression} AS similarity
            FROM memory_chunks mc
            LEFT JOIN documents d ON d.namespace = mc.namespace AND LOWER(d.slug) = LOWER(mc.source_file)
            WHERE 1=1 ${nsFilter} ${slugPrefixFilter} ${softDeleteFilter}
            ORDER BY similarity DESC
            LIMIT $2
        `;
    } else {
        innerSql = `
            SELECT mc.source_file, mc.heading, mc.chunk_text, mc.namespace, d.created_at,
                   ((1 - (mc.embedding <=> $1)) + ${filenameBoostExpr} + ${bm25BoostExpr} + ${accessBoostExpression})
                   * ${decayExpression} * ${kindWeightExpression} AS similarity
            FROM memory_chunks mc
            LEFT JOIN documents d ON d.namespace = mc.namespace AND LOWER(d.slug) = LOWER(mc.source_file)
            WHERE mc.namespace = $2 ${slugPrefixFilter} ${softDeleteFilter}
            ORDER BY similarity DESC
            LIMIT $3
        `;
    }

    const finalLimitIdx = paramIdx;
    params.push(maxResults);
    paramIdx++;

    const sql = `
        SELECT source_file, heading, chunk_text, namespace, created_at, similarity, chunk_count
        FROM (
            SELECT source_file, heading, chunk_text, namespace, created_at, similarity,
                   ROW_NUMBER() OVER (PARTITION BY namespace, source_file ORDER BY similarity DESC) AS rn,
                   COUNT(*) OVER (PARTITION BY namespace, source_file) AS chunk_count
            FROM (${innerSql}) candidates
        ) ranked
        WHERE rn = 1
        ORDER BY similarity DESC
        LIMIT $${finalLimitIdx}
    `;

    const result = await pool.query(sql, params);

    // Retrieval reinforces a memory (LLM-355). Stamp last_accessed on the
    // documents a search surfaced — the same column readNote touches on a direct
    // read. It feeds the access boost AND resets decay age (age is GREATEST of
    // created_at, updated_at, last_accessed), so a note that keeps being found
    // stays fresh and a note never recalled fades on its half-life. Keyed on
    // (namespace, slug) because a wildcard search spans namespaces. Raw ingests
    // (chunks with no documents row) simply match nothing. Fire-and-forget:
    // stamping must never add latency to or fail the search — but a failure is
    // recorded (not silently swallowed) via the same handleError path readNote's
    // last_accessed stamp uses, so a regression after a migration/perm change is
    // visible in system_errors.
    if (result.rows.length > 0) {
        stampLastAccessed(result.rows).catch(err => {
            handleError(null, 'memory', 'LAST_ACCESSED_STAMP_FAILED', {
                error: err.message
            }).catch(() => {});
        });
    }

    return { results: result.rows };
}

// Reset last_accessed to now for the (namespace, source_file) pairs a search
// returned. unnest pairs the two arrays positionally into rows to update.
async function stampLastAccessed(rows) {
    const namespaces = rows.map(r => r.namespace);
    const sourceFiles = rows.map(r => r.source_file);
    await pool.query(
        `UPDATE documents d
         SET last_accessed = NOW()
         FROM (SELECT unnest($1::text[]) AS ns, unnest($2::text[]) AS sf) t
         WHERE d.namespace = t.ns AND LOWER(d.slug) = LOWER(t.sf) AND d.deleted_at IS NULL`,
        [namespaces, sourceFiles]
    );
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

module.exports = { ingestContent, searchMemory, deleteMemory, cleanupMemory, ingestStatus, escapeLikePattern, preprocessQuery, MAX_EMBED_QUERY_CHARS };
