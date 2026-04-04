// Service layer for note graph relations.
// Manages structural connections between notes — dependencies, references,
// supersedes, etc. Supports both manual relations (agent-created) and
// auto-extracted references (slug mentions detected on save).

const pool = require('../db');
const { resolveByName } = require('./actors');
const { broadcast } = require('./events');

// Valid relation types. Extensible — add new types here.
const VALID_TYPES = [
    'depends-on',   // this note/task depends on the target
    'references',   // cites or refers to the target
    'supersedes',   // replaces the target (target is outdated)
    'led-to',       // this decision/discussion produced the target
    'related',      // general association
    'subtask-of',   // task hierarchy
];

// Create a relation between two notes.
// Upserts — if the exact relation already exists, it's a no-op.
// createdBy is an agent name (resolved to actor_id). metadata is optional JSONB.
async function createRelation(sourceNs, sourceSlug, targetNs, targetSlug, relationType, createdBy, metadata, autoExtracted) {
    if (!sourceNs || !sourceSlug || !targetNs || !targetSlug || !relationType) {
        throw Object.assign(new Error('Required: source_namespace, source_slug, target_namespace, target_slug, relation_type'), { statusCode: 400 });
    }
    if (!VALID_TYPES.includes(relationType)) {
        throw Object.assign(new Error('Invalid relation_type: ' + relationType + '. Valid types: ' + VALID_TYPES.join(', ')), { statusCode: 400 });
    }
    // Don't allow self-references
    if (sourceNs === targetNs && sourceSlug.toLowerCase() === targetSlug.toLowerCase()) {
        throw Object.assign(new Error('Cannot create a relation from a note to itself'), { statusCode: 400 });
    }

    let actorId = null;
    if (createdBy) {
        const actor = await resolveByName(createdBy);
        if (actor) actorId = actor.id;
    }

    const metadataJson = metadata ? JSON.stringify(metadata) : null;

    const result = await pool.query(`
        INSERT INTO note_relations (source_namespace, source_slug, target_namespace, target_slug, relation_type, created_by_actor_id, metadata, auto_extracted)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
        ON CONFLICT (source_namespace, source_slug, target_namespace, target_slug, relation_type) DO UPDATE SET
            metadata = COALESCE(EXCLUDED.metadata, note_relations.metadata),
            auto_extracted = EXCLUDED.auto_extracted
        RETURNING id, source_namespace, source_slug, target_namespace, target_slug, relation_type, auto_extracted, created_at
    `, [sourceNs, sourceSlug, targetNs, targetSlug, relationType, actorId, metadataJson, autoExtracted || false]);

    broadcast('relation_updated', { source: sourceNs + '/' + sourceSlug, target: targetNs + '/' + targetSlug, type: relationType });
    return result.rows[0];
}

// Delete a specific relation by its attributes.
async function deleteRelation(sourceNs, sourceSlug, targetNs, targetSlug, relationType) {
    const result = await pool.query(`
        DELETE FROM note_relations
        WHERE source_namespace = $1 AND LOWER(source_slug) = LOWER($2)
          AND target_namespace = $3 AND LOWER(target_slug) = LOWER($4)
          AND relation_type = $5
        RETURNING id
    `, [sourceNs, sourceSlug, targetNs, targetSlug, relationType]);

    if (result.rows.length === 0) {
        throw Object.assign(new Error('Relation not found'), { statusCode: 404 });
    }

    broadcast('relation_updated', { source: sourceNs + '/' + sourceSlug, target: targetNs + '/' + targetSlug, type: relationType, deleted: true });
    return { deleted: true };
}

// Get relations for a note.
// direction: 'outgoing' (this note is source), 'incoming' (this note is target), 'both' (default).
// type: optional filter by relation_type.
async function getRelations(namespace, slug, direction, type) {
    const conditions = [];
    const params = [];
    let idx = 1;

    if (direction === 'outgoing') {
        conditions.push(`nr.source_namespace = $${idx} AND LOWER(nr.source_slug) = LOWER($${idx + 1})`);
        params.push(namespace, slug);
        idx += 2;
    } else if (direction === 'incoming') {
        conditions.push(`nr.target_namespace = $${idx} AND LOWER(nr.target_slug) = LOWER($${idx + 1})`);
        params.push(namespace, slug);
        idx += 2;
    } else {
        // both
        conditions.push(`(
            (nr.source_namespace = $${idx} AND LOWER(nr.source_slug) = LOWER($${idx + 1}))
            OR (nr.target_namespace = $${idx} AND LOWER(nr.target_slug) = LOWER($${idx + 1}))
        )`);
        params.push(namespace, slug);
        idx += 2;
    }

    if (type) {
        conditions.push(`nr.relation_type = $${idx}`);
        params.push(type);
        idx++;
    }

    const result = await pool.query(`
        SELECT nr.id, nr.source_namespace, nr.source_slug, nr.target_namespace, nr.target_slug,
               nr.relation_type, nr.auto_extracted, nr.created_at, nr.metadata,
               ac.name AS created_by
        FROM note_relations nr
        LEFT JOIN actors ac ON ac.id = nr.created_by_actor_id
        WHERE ${conditions.join(' AND ')}
        ORDER BY nr.created_at DESC
    `, params);

    return result.rows;
}

// Graph traversal — recursive CTE to find all notes connected within N hops.
// Returns { nodes: [...], edges: [...] } for visualization.
async function getGraph(namespace, slug, depth) {
    const maxDepth = Math.min(Math.max(1, depth || 2), 5);

    const result = await pool.query(`
        WITH RECURSIVE graph AS (
            -- Seed: direct relations from/to the starting note
            SELECT nr.id, nr.source_namespace, nr.source_slug, nr.target_namespace, nr.target_slug,
                   nr.relation_type, nr.auto_extracted, 1 AS depth
            FROM note_relations nr
            WHERE (nr.source_namespace = $1 AND LOWER(nr.source_slug) = LOWER($2))
               OR (nr.target_namespace = $1 AND LOWER(nr.target_slug) = LOWER($2))

            UNION

            -- Expand: follow relations from discovered nodes
            SELECT nr.id, nr.source_namespace, nr.source_slug, nr.target_namespace, nr.target_slug,
                   nr.relation_type, nr.auto_extracted, g.depth + 1
            FROM note_relations nr
            JOIN graph g ON (
                (nr.source_namespace = g.target_namespace AND LOWER(nr.source_slug) = LOWER(g.target_slug))
                OR (nr.target_namespace = g.source_namespace AND LOWER(nr.target_slug) = LOWER(g.source_slug))
                OR (nr.source_namespace = g.source_namespace AND LOWER(nr.source_slug) = LOWER(g.source_slug))
                OR (nr.target_namespace = g.target_namespace AND LOWER(nr.target_slug) = LOWER(g.target_slug))
            )
            WHERE g.depth < $3
              AND nr.id NOT IN (SELECT id FROM graph)
        )
        SELECT DISTINCT id, source_namespace, source_slug, target_namespace, target_slug,
               relation_type, auto_extracted, depth
        FROM graph
        ORDER BY depth, id
    `, [namespace, slug, maxDepth]);

    // Build nodes and edges from the result
    const nodeSet = new Set();
    const nodes = [];
    const edges = [];

    // Always include the starting node
    const startKey = namespace + '/' + slug;
    nodeSet.add(startKey);
    nodes.push({ namespace, slug, root: true });

    for (const row of result.rows) {
        const sourceKey = row.source_namespace + '/' + row.source_slug;
        const targetKey = row.target_namespace + '/' + row.target_slug;

        if (!nodeSet.has(sourceKey)) {
            nodeSet.add(sourceKey);
            nodes.push({ namespace: row.source_namespace, slug: row.source_slug });
        }
        if (!nodeSet.has(targetKey)) {
            nodeSet.add(targetKey);
            nodes.push({ namespace: row.target_namespace, slug: row.target_slug });
        }

        edges.push({
            id: row.id,
            source: sourceKey,
            target: targetKey,
            type: row.relation_type,
            auto_extracted: row.auto_extracted,
            depth: row.depth
        });
    }

    return { nodes, edges };
}

// Delete all relations involving a note (called on note delete).
async function deleteRelationsForNote(namespace, slug) {
    await pool.query(`
        DELETE FROM note_relations
        WHERE (source_namespace = $1 AND LOWER(source_slug) = LOWER($2))
           OR (target_namespace = $1 AND LOWER(target_slug) = LOWER($2))
    `, [namespace, slug]);
}

// Update relations when a note is moved/renamed.
async function updateRelationsForMove(oldNs, oldSlug, newNs, newSlug) {
    // Update source side
    await pool.query(`
        UPDATE note_relations
        SET source_namespace = $3, source_slug = $4
        WHERE source_namespace = $1 AND LOWER(source_slug) = LOWER($2)
    `, [oldNs, oldSlug, newNs, newSlug]);

    // Update target side
    await pool.query(`
        UPDATE note_relations
        SET target_namespace = $3, target_slug = $4
        WHERE target_namespace = $1 AND LOWER(target_slug) = LOWER($2)
    `, [oldNs, oldSlug, newNs, newSlug]);
}

// Auto-extract slug references from note content.
// Looks for patterns like namespace/slug that match known namespaces.
// Returns an array of { namespace, slug } objects.
async function extractSlugReferences(content, sourceNamespace) {
    // Get known namespaces to anchor the regex
    const nsResult = await pool.query('SELECT DISTINCT namespace FROM documents WHERE deleted_at IS NULL');
    const knownNamespaces = nsResult.rows.map(r => r.namespace);
    if (knownNamespaces.length === 0) return [];

    // Build regex: namespace/path where namespace is one of the known ones
    // Matches: work/tasks/dev-2329, shared/GUIDELINES, home/notes/active-work, etc.
    // Word boundary at start, stops at whitespace, quotes, parens, brackets, backtick, comma
    const nsPattern = knownNamespaces.map(ns => ns.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|');
    const regex = new RegExp('(?:^|[\\s`(\\["\'])(' + nsPattern + ')/([a-zA-Z0-9][a-zA-Z0-9/_-]*[a-zA-Z0-9])', 'gm');

    const refs = [];
    const seen = new Set();
    let match;
    while ((match = regex.exec(content)) !== null) {
        const ns = match[1];
        const slug = match[2];
        const key = ns + '/' + slug;
        if (!seen.has(key.toLowerCase())) {
            seen.add(key.toLowerCase());
            refs.push({ namespace: ns, slug });
        }
    }

    // Also match bare slug references (no namespace prefix) — assume sourceNamespace
    // Matches patterns like: tasks/dev-2329, notes/active-work, instructions/bootstrap
    const bareRegex = /(?:^|[\s`(\["'])((?:tasks|notes|instructions|conversations|dreams|learnings|context|ideas)\/[a-zA-Z0-9][a-zA-Z0-9/_-]*[a-zA-Z0-9])/gm;
    while ((match = bareRegex.exec(content)) !== null) {
        const slug = match[1];
        const key = sourceNamespace + '/' + slug;
        if (!seen.has(key.toLowerCase())) {
            seen.add(key.toLowerCase());
            refs.push({ namespace: sourceNamespace, slug });
        }
    }

    return refs;
}

// Run auto-extraction and upsert references relations.
// Called from saveNote as fire-and-forget.
async function autoExtractRelations(sourceNamespace, sourceSlug, content) {
    // Remove existing auto-extracted references for this source
    // (they'll be re-created from current content)
    await pool.query(`
        DELETE FROM note_relations
        WHERE source_namespace = $1 AND LOWER(source_slug) = LOWER($2)
          AND auto_extracted = TRUE AND relation_type = 'references'
    `, [sourceNamespace, sourceSlug]);

    const refs = await extractSlugReferences(content, sourceNamespace);

    for (const ref of refs) {
        // Skip self-references
        if (ref.namespace === sourceNamespace && ref.slug.toLowerCase() === sourceSlug.toLowerCase()) {
            continue;
        }
        // Upsert — don't overwrite manually created relations of the same type
        await pool.query(`
            INSERT INTO note_relations (source_namespace, source_slug, target_namespace, target_slug, relation_type, auto_extracted)
            VALUES ($1, $2, $3, $4, 'references', TRUE)
            ON CONFLICT (source_namespace, source_slug, target_namespace, target_slug, relation_type)
            DO NOTHING
        `, [sourceNamespace, sourceSlug, ref.namespace, ref.slug]);
    }
}

module.exports = {
    VALID_TYPES,
    createRelation,
    deleteRelation,
    getRelations,
    getGraph,
    deleteRelationsForNote,
    updateRelationsForMove,
    autoExtractRelations,
};
