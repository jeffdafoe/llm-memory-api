// Service layer for document CRUD (save_note, list_notes, read_note, delete_note).
// Documents are stored in the documents table and auto-indexed into the vector DB.

const pool = require('../db');
const { ingestContent } = require('./memory');
const { resolveByName } = require('./actors');
const { handleError } = require('./error-handler');
const { broadcast } = require('./events');
const config = require('./config');
const { deleteRelationsForNote, updateRelationsForMove, autoExtractRelations } = require('./relations');

// Update namespace_usage counters. Fire-and-forget — usage tracking
// should never block or fail a document operation.
function updateUsage(namespace, countDelta, bytesDelta) {
    pool.query(`
        INSERT INTO namespace_usage (namespace, note_count, total_bytes, updated_at)
        VALUES ($1, $2, $3, NOW())
        ON CONFLICT (namespace) DO UPDATE SET
            note_count = GREATEST(0, namespace_usage.note_count + $2),
            total_bytes = GREATEST(0, namespace_usage.total_bytes + $3),
            updated_at = NOW()
    `, [namespace, countDelta, bytesDelta]).catch(() => {});
}

// Check if adding `additionalBytes` to this namespace would exceed the storage quota.
// Returns null if within quota, or an error message string if over.
async function checkQuota(namespace, additionalBytes) {
    // Get current usage
    const usageResult = await pool.query(
        'SELECT total_bytes FROM namespace_usage WHERE namespace = $1',
        [namespace]
    );
    const currentBytes = usageResult.rows.length > 0 ? parseInt(usageResult.rows[0].total_bytes) : 0;

    // Resolve quota: per-agent override > global default
    const agentResult = await pool.query(
        `SELECT agc.storage_quota
         FROM agent_configuration agc
         JOIN actors ac ON ac.id = agc.actor_id
         WHERE ac.name = $1`,
        [namespace]
    );

    let quota = null;
    if (agentResult.rows.length > 0 && agentResult.rows[0].storage_quota != null) {
        quota = parseInt(agentResult.rows[0].storage_quota);
    } else {
        try {
            quota = parseInt(config.get('default_storage_quota'));
        } catch (e) {
            // Config key doesn't exist — no quota enforcement
            return null;
        }
    }

    if (!quota || quota <= 0) return null; // No quota or unlimited

    const projected = currentBytes + additionalBytes;
    if (projected > quota) {
        const usedMB = (currentBytes / (1024 * 1024)).toFixed(1);
        const quotaMB = (quota / (1024 * 1024)).toFixed(1);
        return 'Storage quota exceeded (' + usedMB + ' MB / ' + quotaMB + ' MB limit)';
    }
    return null;
}

// Validate a slug to prevent malformed entries that break the tree UI.
// Throws 400 if invalid. Prefixes (for move-prefix) end with '/' and are validated
// with allowTrailingSlash=true.
function validateSlug(slug, { allowTrailingSlash = false } = {}) {
    if (!slug || typeof slug !== 'string') {
        throw Object.assign(new Error('Slug is required'), { statusCode: 400 });
    }
    if (slug.length > 500) {
        throw Object.assign(new Error('Slug too long (max 500 characters)'), { statusCode: 400 });
    }
    // No control characters, null bytes, or backslashes
    if (/[\x00-\x1f\x7f\\]/.test(slug)) {
        throw Object.assign(new Error('Slug contains invalid characters'), { statusCode: 400 });
    }
    // No leading slash
    if (slug.startsWith('/')) {
        throw Object.assign(new Error('Slug must not start with /'), { statusCode: 400 });
    }
    // No trailing slash (unless it's a prefix for folder operations)
    if (!allowTrailingSlash && slug.endsWith('/')) {
        throw Object.assign(new Error('Slug must not end with /'), { statusCode: 400 });
    }
    // No double slashes (creates empty segments in the tree)
    if (slug.includes('//')) {
        throw Object.assign(new Error('Slug must not contain //'), { statusCode: 400 });
    }
    // Every segment must be non-empty
    const segments = slug.replace(/\/$/, '').split('/');
    for (const seg of segments) {
        if (seg === '') {
            throw Object.assign(new Error('Slug must not contain empty path segments'), { statusCode: 400 });
        }
    }
}

// Escape special characters for use in SQL LIKE patterns.
// Backslash is the LIKE escape char, so it must be escaped first,
// then % and _ which are LIKE wildcards.
function escapeLike(str) {
    return str.replace(/\\/g, '\\\\').replace(/%/g, '\\%').replace(/_/g, '\\_');
}

function slugToKind(slug) {
    if (slug.startsWith('instructions/')) return 'instruction';
    if (slug.startsWith('notes/codebase/')) return 'reference';
    if (slug.startsWith('conversations/')) return 'conversation';
    if (slug.startsWith('context/')) return 'context';
    if (slug.startsWith('dreams/')) return 'dream';
    if (slug.startsWith('tasks/done/')) return 'task';
    if (slug.startsWith('tasks/')) return 'task';
    if (slug.startsWith('learnings/')) return 'learning';
    if (slug.startsWith('notes/')) return 'note';
    return 'note';
}

function titleToSlug(title) {
    return title
        .toLowerCase()
        .replace(/[^a-z0-9\s-]/g, '')
        .replace(/\s+/g, '-')
        .replace(/-+/g, '-')
        .replace(/^-|-$/g, '')
        .substring(0, 200);
}

async function saveNote(namespace, title, content, slug, createdBy, metadata, extension) {
    if (!title || !content) {
        throw Object.assign(new Error('Required fields: title, content'), { statusCode: 400 });
    }

    // Enforce max note size (bytes). Default 500KB if not configured.
    const maxBytes = parseInt(config.get('note_maximum_size')) || 512000;
    const contentBytes = Buffer.byteLength(content, 'utf8');
    if (contentBytes > maxBytes) {
        throw Object.assign(
            new Error('Note content exceeds maximum size (' + Math.round(contentBytes / 1024) + 'KB / ' + Math.round(maxBytes / 1024) + 'KB limit)'),
            { statusCode: 500 }
        );
    }

    const resolvedSlug = slug || titleToSlug(title);

    if (!resolvedSlug) {
        throw Object.assign(new Error('Could not generate slug from title'), { statusCode: 400 });
    }
    validateSlug(resolvedSlug);

    // Detect redundant namespace prefix in slug (e.g. slug="shared/ideas/foo" in namespace="shared")
    if (resolvedSlug.startsWith(namespace + '/')) {
        throw Object.assign(new Error(
            'Slug "' + resolvedSlug + '" starts with its own namespace "' + namespace + '/". ' +
            'The namespace is already implicit — use "' + resolvedSlug.slice(namespace.length + 1) + '" instead.'
        ), { statusCode: 400 });
    }

    // Check if an active note already exists at this slug
    // Case-insensitive lookup — slugs are preserved as-is, only matching is lowered
    const existing = await pool.query(
        'SELECT id, LENGTH(content) AS content_length FROM documents WHERE namespace = $1 AND LOWER(slug) = LOWER($2) AND deleted_at IS NULL',
        [namespace, resolvedSlug]
    );

    // Check storage quota before writing
    const additionalBytes = existing.rows.length > 0
        ? content.length - (existing.rows[0].content_length || 0) // update: delta only
        : content.length; // insert: full size
    if (additionalBytes > 0) {
        const quotaError = await checkQuota(namespace, additionalBytes);
        if (quotaError) {
            throw Object.assign(new Error(quotaError), { statusCode: 413 });
        }
    }

    // Resolve createdBy name to actor_id (only needed for inserts)
    let createdByActorId = null;
    if (createdBy) {
        const actor = await resolveByName(createdBy);
        if (actor) createdByActorId = actor.id;
    }

    const kind = slugToKind(resolvedSlug);

    // Validate and normalize metadata: must be a plain object if provided, max 10KB serialized.
    // null/undefined → null (no metadata). Prevents arbitrary blobs or non-object types.
    let metadataJson = null;
    if (metadata) {
        if (typeof metadata !== 'object' || Array.isArray(metadata)) {
            throw Object.assign(new Error('metadata must be a plain object'), { statusCode: 400 });
        }
        const serialized = JSON.stringify(metadata);
        if (serialized.length > 10240) {
            throw Object.assign(new Error('metadata exceeds 10KB limit'), { statusCode: 400 });
        }
        metadataJson = serialized;
    }

    // Sanitize extension: dot + lowercase alphanumeric only, max 20 chars
    let cleanExtension = null;
    if (extension) {
        const extClean = extension.toLowerCase().replace(/^\.?/, '.').replace(/[^a-z0-9.]/g, '');
        if (extClean.length >= 2 && extClean.length <= 20 && /^\.[a-z0-9]+$/.test(extClean)) {
            cleanExtension = extClean;
        }
    }

    // Build optional SET clauses for fields that should only update when provided
    const optionalSets = [];
    const optionalParams = [];
    let paramIndex = 6; // $1-$5 are title, content, namespace, slug, kind
    if (metadataJson) {
        optionalSets.push('metadata = $' + paramIndex);
        optionalParams.push(metadataJson);
        paramIndex++;
    }
    if (cleanExtension) {
        optionalSets.push('extension = $' + paramIndex);
        optionalParams.push(cleanExtension);
        paramIndex++;
    }

    let result;
    if (existing.rows.length > 0) {
        // Update existing row — also clears deleted_at if it was soft-deleted.
        // Don't overwrite created_by_actor_id on updates — preserve original author.
        const extraSets = optionalSets.length ? ', ' + optionalSets.join(', ') : '';
        result = await pool.query(`
            UPDATE documents
            SET title = $1, content = $2, deleted_at = NULL, updated_at = NOW(), kind = $5${extraSets}
            WHERE namespace = $3 AND LOWER(slug) = LOWER($4)
            RETURNING id, namespace, slug, title, created_by_actor_id, created_at, updated_at, metadata, extension
        `, [title, content, namespace, resolvedSlug, kind, ...optionalParams]);
    } else {
        // For inserts, include extension and metadata directly
        result = await pool.query(`
            INSERT INTO documents (namespace, slug, title, content, created_by_actor_id, kind, metadata, extension)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
            RETURNING id, namespace, slug, title, created_by_actor_id, created_at, updated_at, metadata, extension
        `, [namespace, resolvedSlug, title, content, createdByActorId, kind, metadataJson, cleanExtension]);
    }

    const doc = result.rows[0];

    // Update namespace usage counters
    const newBytes = content.length;
    if (existing.rows.length > 0) {
        // Update — bytes delta only
        const oldBytes = existing.rows[0].content_length || 0;
        updateUsage(namespace, 0, newBytes - oldBytes);
    } else {
        // Insert — +1 note, +bytes
        updateUsage(namespace, 1, newBytes);
    }

    // Resolve created_by_actor_id to name for API response
    if (doc.created_by_actor_id) {
        const { resolveById } = require('./actors');
        const actor = await resolveById(doc.created_by_actor_id);
        doc.created_by = actor ? actor.name : null;
    } else {
        doc.created_by = null;
    }

    // Auto-index into vector DB (fire-and-forget — don't fail the save if indexing fails)
    ingestContent(namespace, resolvedSlug, content).catch(err => {
        handleError(null, 'documents', 'AUTO_INDEX_FAILED', {
            namespace, slug: resolvedSlug, error: err.message
        }).catch(() => {});
    });

    // Auto-extract slug references and create relation graph edges (fire-and-forget)
    autoExtractRelations(namespace, resolvedSlug, content).catch(() => {});

    // Notify admin dashboard clients that this note changed
    broadcast('note_updated', { namespace, slug: resolvedSlug, operation: 'saved' });

    // LLM-powered enrichment — generates keywords, tags, and suggested relations (fire-and-forget)
    const { enrichNote } = require('./enrichment');
    var parsedMeta = doc.metadata ? (typeof doc.metadata === 'object' ? doc.metadata : JSON.parse(doc.metadata)) : null;
    enrichNote(namespace, resolvedSlug, title, content, parsedMeta).catch(err => {
        handleError(null, 'documents', 'NOTE_ENRICHMENT_FAILED', {
            namespace, slug: resolvedSlug, error: err.message
        }).catch(() => {});
    });

    return doc;
}

async function listNotes(namespace, limit, offset, prefix, opts) {
    const maxResults = limit || 50;
    const skip = offset || 0;
    // When include_deleted is true, return soft-deleted notes alongside live ones.
    // Deleted notes have a non-null deleted_at field so callers can distinguish them.
    const includeDeleted = opts && opts.include_deleted;
    const deletedFilter = includeDeleted ? '' : ' AND d.deleted_at IS NULL';
    const deletedCol = includeDeleted ? ', d.deleted_at' : '';

    let sql, params;
    if (prefix) {
        // Filter by slug prefix — like listing a directory (e.g., "tasks/pending/")
        sql = `
            SELECT d.id, d.slug, d.title,
                   LEFT(d.content, 200) AS snippet,
                   MD5(d.content) AS content_hash,
                   ac.name AS created_by, d.created_at, d.updated_at${deletedCol}
            FROM documents d
            LEFT JOIN actors ac ON ac.id = d.created_by_actor_id
            WHERE d.namespace = $1 AND LOWER(d.slug) LIKE LOWER($4)${deletedFilter}
            ORDER BY d.updated_at DESC, d.slug ASC
            LIMIT $2 OFFSET $3
        `;
        params = [namespace, maxResults, skip, prefix + '%'];
    } else {
        sql = `
            SELECT d.id, d.slug, d.title,
                   LEFT(d.content, 200) AS snippet,
                   MD5(d.content) AS content_hash,
                   ac.name AS created_by, d.created_at, d.updated_at${deletedCol}
            FROM documents d
            LEFT JOIN actors ac ON ac.id = d.created_by_actor_id
            WHERE d.namespace = $1${deletedFilter}
            ORDER BY d.updated_at DESC
            LIMIT $2 OFFSET $3
        `;
        params = [namespace, maxResults, skip];
    }

    const result = await pool.query(sql, params);
    return { notes: result.rows };
}

async function readNote(namespace, slug) {
    const result = await pool.query(`
        SELECT d.id, d.namespace, d.slug, d.title, d.content,
               ac.name AS created_by, d.created_at, d.updated_at, d.metadata, d.extension
        FROM documents d
        LEFT JOIN actors ac ON ac.id = d.created_by_actor_id
        WHERE d.namespace = $1 AND LOWER(d.slug) = LOWER($2) AND d.deleted_at IS NULL
    `, [namespace, slug]);

    if (result.rows.length === 0) {
        throw Object.assign(new Error(`Note not found: ${slug}`), { statusCode: 404 });
    }

    // Touch last_accessed for search decay/boost (fire-and-forget)
    pool.query(
        'UPDATE documents SET last_accessed = NOW() WHERE namespace = $1 AND LOWER(slug) = LOWER($2)',
        [namespace, slug]
    ).catch(err => {
        handleError(null, 'documents', 'LAST_ACCESSED_UPDATE_FAILED', {
            namespace, slug, error: err.message
        }).catch(() => {});
    });

    return result.rows[0];
}

async function deleteNote(namespace, slug) {
    // Soft delete the document and hard-delete its vector chunks.
    // The document row is kept (with deleted_at set) so restoreNote can
    // re-ingest from the preserved content. Chunks are cheap to regenerate
    // but expensive to leave behind — stale chunks leak into RAG context.
    const result = await pool.query(`
        UPDATE documents
        SET deleted_at = NOW()
        WHERE namespace = $1 AND LOWER(slug) = LOWER($2) AND deleted_at IS NULL
        RETURNING id, LENGTH(content) AS content_length
    `, [namespace, slug]);

    if (result.rows.length === 0) {
        throw Object.assign(new Error(`Note not found: ${slug}`), { statusCode: 404 });
    }

    // Hard-delete vector chunks so they can't appear in search results
    pool.query(
        'DELETE FROM memory_chunks WHERE namespace = $1 AND LOWER(source_file) = LOWER($2)',
        [namespace, slug]
    ).catch(() => {});

    updateUsage(namespace, -1, -(result.rows[0].content_length || 0));
    deleteRelationsForNote(namespace, slug).catch(() => {});
    broadcast('note_updated', { namespace, slug, operation: 'deleted' });

    return { deleted: true, slug };
}

async function restoreNote(namespace, slug) {
    // Check the note exists and get its size before restoring
    const check = await pool.query(
        'SELECT id, LENGTH(content) AS content_length FROM documents WHERE namespace = $1 AND LOWER(slug) = LOWER($2) AND deleted_at IS NOT NULL',
        [namespace, slug]
    );
    if (check.rows.length === 0) {
        throw Object.assign(new Error(`No deleted note found: ${slug}`), { statusCode: 404 });
    }

    // Check storage quota before restoring
    const restoreBytes = check.rows[0].content_length || 0;
    const quotaError = await checkQuota(namespace, restoreBytes);
    if (quotaError) {
        throw Object.assign(new Error(quotaError), { statusCode: 413 });
    }

    // Clear the deleted_at flag. Vector chunks were hard-deleted on
    // soft-delete — re-save the note after restoring to re-ingest them.
    const result = await pool.query(`
        UPDATE documents d
        SET deleted_at = NULL
        WHERE d.id = $1
        RETURNING d.id, d.namespace, d.slug, d.title
    `, [check.rows[0].id]);

    updateUsage(namespace, 1, restoreBytes);
    broadcast('note_updated', { namespace, slug, operation: 'restored' });

    return result.rows[0];
}

// Search-and-replace edit on a note's content — like the Edit tool in Claude Code.
// Finds old_string in the document and replaces it with new_string.
// By default, old_string must appear exactly once (prevents ambiguous edits).
// Set replace_all to true to replace every occurrence.
async function editNote(namespace, slug, oldString, newString, replaceAll) {
    if (!oldString || newString === undefined || newString === null) {
        throw Object.assign(new Error('Required fields: old_string, new_string'), { statusCode: 400 });
    }

    // Fetch the current document
    const doc = await readNote(namespace, slug);
    const content = doc.content;

    // Count occurrences to validate uniqueness
    let count = 0;
    let pos = 0;
    while ((pos = content.indexOf(oldString, pos)) !== -1) {
        count++;
        pos += oldString.length;
    }

    if (count === 0) {
        throw Object.assign(
            new Error('old_string not found in document'),
            { statusCode: 400 }
        );
    }

    if (count > 1 && !replaceAll) {
        throw Object.assign(
            new Error(`old_string appears ${count} times — use replace_all to replace all occurrences, or provide more context to make it unique`),
            { statusCode: 400 }
        );
    }

    // Perform the replacement (split/join is literal — unlike String.replace,
    // it won't interpret $ sequences in newString as special patterns)
    const updatedContent = content.split(oldString).join(newString);

    // Enforce max note size after edit
    const maxBytes = parseInt(config.get('note_maximum_size')) || 512000;
    const updatedBytes = Buffer.byteLength(updatedContent, 'utf8');
    if (updatedBytes > maxBytes) {
        throw Object.assign(
            new Error('Edit would exceed maximum note size (' + Math.round(updatedBytes / 1024) + 'KB / ' + Math.round(maxBytes / 1024) + 'KB limit)'),
            { statusCode: 500 }
        );
    }

    // Check storage quota if edit increases size
    const editDelta = updatedContent.length - content.length;
    if (editDelta > 0) {
        const quotaError = await checkQuota(namespace, editDelta);
        if (quotaError) {
            throw Object.assign(new Error(quotaError), { statusCode: 413 });
        }
    }

    // Save the updated content
    const result = await pool.query(`
        UPDATE documents d
        SET content = $1, updated_at = NOW()
        WHERE d.namespace = $2 AND LOWER(d.slug) = LOWER($3)
        RETURNING d.id, d.namespace, d.slug, d.title, d.created_by_actor_id, d.created_at, d.updated_at
    `, [updatedContent, namespace, slug]);

    // Update usage — bytes delta only (note count unchanged)
    updateUsage(namespace, 0, updatedContent.length - content.length);

    // Resolve created_by_actor_id to name
    if (result.rows[0] && result.rows[0].created_by_actor_id) {
        const { resolveById } = require('./actors');
        const actor = await resolveById(result.rows[0].created_by_actor_id);
        result.rows[0].created_by = actor ? actor.name : null;
    } else if (result.rows[0]) {
        result.rows[0].created_by = null;
    }

    // Re-index into vector DB (fire-and-forget)
    ingestContent(namespace, slug, updatedContent).catch(err => {
        handleError(null, 'documents', 'REINDEX_FAILED', {
            namespace, slug, error: err.message
        }).catch(() => {});
    });

    // Re-extract slug references after edit (fire-and-forget)
    autoExtractRelations(namespace, slug, updatedContent).catch(() => {});

    // Notify admin dashboard clients that this note changed
    broadcast('note_updated', { namespace, slug, operation: 'edited' });

    return {
        ...result.rows[0],
        replacements: replaceAll ? count : 1
    };
}

// Text search across notes — like grep but for the notes database.
// Returns matching documents with line numbers and surrounding context.
async function grepNotes(pattern, namespace, limit, readableNamespaces) {
    if (!pattern) {
        throw Object.assign(new Error('Required field: pattern'), { statusCode: 400 });
    }

    const maxResults = limit || 20;
    const ilikePattern = `%${pattern}%`;

    let sql, params;
    if (namespace && namespace !== '*') {
        sql = `
            SELECT id, namespace, slug, title, content, updated_at
            FROM documents
            WHERE namespace = $1 AND deleted_at IS NULL AND (content ILIKE $2 OR title ILIKE $2)
            ORDER BY updated_at DESC
            LIMIT $3
        `;
        params = [namespace, ilikePattern, maxResults];
    } else if (readableNamespaces) {
        // Filter at query level to ensure LIMIT returns correct result count
        sql = `
            SELECT id, namespace, slug, title, content, updated_at
            FROM documents
            WHERE namespace = ANY($1) AND deleted_at IS NULL AND (content ILIKE $2 OR title ILIKE $2)
            ORDER BY updated_at DESC
            LIMIT $3
        `;
        params = [readableNamespaces, ilikePattern, maxResults];
    } else {
        sql = `
            SELECT id, namespace, slug, title, content, updated_at
            FROM documents
            WHERE deleted_at IS NULL AND (content ILIKE $1 OR title ILIKE $1)
            ORDER BY updated_at DESC
            LIMIT $2
        `;
        params = [ilikePattern, maxResults];
    }

    const result = await pool.query(sql, params);

    // Extract matching lines with context (±2 lines, like grep -C 2)
    const lowerPattern = pattern.toLowerCase();
    return result.rows.map(doc => {
        const lines = doc.content.split('\n');
        const matchLineNumbers = new Set();

        // Find lines that contain the pattern
        for (let i = 0; i < lines.length; i++) {
            if (lines[i].toLowerCase().includes(lowerPattern)) {
                matchLineNumbers.add(i);
            }
        }

        // Expand to include context lines (±2)
        const contextLineNumbers = new Set();
        for (const lineNum of matchLineNumbers) {
            for (let offset = -2; offset <= 2; offset++) {
                const idx = lineNum + offset;
                if (idx >= 0 && idx < lines.length) {
                    contextLineNumbers.add(idx);
                }
            }
        }

        // Build match groups (consecutive context lines grouped together)
        const sortedLines = Array.from(contextLineNumbers).sort((a, b) => a - b);
        const matches = sortedLines.map(idx => ({
            lineNumber: idx + 1,
            line: lines[idx],
            isMatch: matchLineNumbers.has(idx)
        }));

        return {
            namespace: doc.namespace,
            slug: doc.slug,
            title: doc.title,
            matchCount: matchLineNumbers.size,
            matches
        };
    });
}

// Move/rename a note by changing its slug (and optionally namespace).
// Updates both the document row and any associated vector chunks.
async function moveNote(namespace, slug, newSlug, newNamespace) {
    validateSlug(newSlug);
    const targetNamespace = newNamespace || namespace;

    // Verify source exists and isn't deleted
    const source = await pool.query(
        'SELECT id, title, LENGTH(content) AS content_length FROM documents WHERE namespace = $1 AND LOWER(slug) = LOWER($2) AND deleted_at IS NULL',
        [namespace, slug]
    );
    if (source.rows.length === 0) {
        throw Object.assign(new Error(`Note not found: ${slug}`), { statusCode: 404 });
    }

    // Check target slug isn't already taken by an active note
    const conflict = await pool.query(
        'SELECT id FROM documents WHERE namespace = $1 AND LOWER(slug) = LOWER($2) AND deleted_at IS NULL',
        [targetNamespace, newSlug]
    );
    if (conflict.rows.length > 0) {
        throw Object.assign(
            new Error(`Target slug already exists: ${targetNamespace}/${newSlug}`),
            { statusCode: 409 }
        );
    }

    // Update the document (recalculate kind from new slug)
    const newKind = slugToKind(newSlug);
    const result = await pool.query(`
        UPDATE documents
        SET slug = $1, namespace = $2, updated_at = NOW(), kind = $5
        WHERE namespace = $3 AND LOWER(slug) = LOWER($4)
        RETURNING id, namespace, slug, title, created_by_actor_id, created_at, updated_at
    `, [newSlug, targetNamespace, namespace, slug, newKind]);

    // Update vector chunks to match the new slug/namespace
    await pool.query(
        'UPDATE memory_chunks SET source_file = $1, namespace = $2 WHERE namespace = $3 AND LOWER(source_file) = LOWER($4)',
        [newSlug, targetNamespace, namespace, slug]
    );

    // Update relation graph edges to follow the move
    updateRelationsForMove(namespace, slug, targetNamespace, newSlug).catch(() => {});

    const doc = result.rows[0];

    // Update usage if namespace changed — move bytes from old to new
    if (targetNamespace !== namespace) {
        const bytes = source.rows[0].content_length || 0;
        updateUsage(namespace, -1, -bytes);
        updateUsage(targetNamespace, 1, bytes);
    }

    // Resolve created_by_actor_id to name for API response
    if (doc.created_by_actor_id) {
        const { resolveById } = require('./actors');
        const actor = await resolveById(doc.created_by_actor_id);
        doc.created_by = actor ? actor.name : null;
    } else {
        doc.created_by = null;
    }

    // Notify for both old location (stale view) and new location
    broadcast('note_updated', { namespace, slug, operation: 'moved' });
    broadcast('note_updated', { namespace: targetNamespace, slug: newSlug, operation: 'moved' });

    return doc;
}

module.exports = { saveNote, listNotes, readNote, deleteNote, restoreNote, editNote, grepNotes, moveNote, titleToSlug, validateSlug, escapeLike };
