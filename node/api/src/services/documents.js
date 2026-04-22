// Service layer for document CRUD (save_note, list_notes, read_note, delete_note).
// Documents are stored in the documents table and auto-indexed into the vector DB.

const pool = require('../db');
const safeRegex = require('safe-regex');
const { ingestContent } = require('./memory');
const { resolveByName } = require('./actors');
const { handleError } = require('./error-handler');
const { broadcast } = require('./events');
const config = require('./config');
// Longest regex pattern grep will accept. Length alone doesn't prevent
// ReDoS (short patterns can still backtrack pathologically), but it caps
// the blast radius of pattern parsing and pairs with safe-regex below.
const GREP_REGEX_MAX_LENGTH = 200;

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

async function saveNote(namespace, title, content, slug, createdBy, metadata, extension, opts) {
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

    const upsert = opts && opts.upsert;

    // Check if an active note already exists at this slug (only when upserting)
    let existing = { rows: [] };
    if (upsert) {
        existing = await pool.query(
            'SELECT id, LENGTH(content) AS content_length FROM documents WHERE namespace = $1 AND LOWER(slug) = LOWER($2) AND deleted_at IS NULL',
            [namespace, resolvedSlug]
        );
    }

    // Check storage quota before writing
    const additionalBytes = existing.rows.length > 0
        ? content.length - (existing.rows[0].content_length || 0)
        : content.length;
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

    let result;
    if (existing.rows.length > 0) {
        // Upsert: update the specific live row we just located (by id).
        // Matching by (namespace, LOWER(slug)) would also hit any soft-deleted
        // row at the same case-insensitive slug — setting deleted_at=NULL on
        // both would produce two live rows and violate the partial unique
        // index `(namespace, slug) WHERE deleted_at IS NULL`, bubbling up as
        // an unhandled 23505 from this UPDATE (which has no try/catch).
        // Using id scopes the update to one row and sidesteps that entirely.
        const existingId = existing.rows[0].id;
        // Fixed positions: $1 title, $2 content, $3 id, $4 kind. Optional sets
        // (metadata, extension) come after, with explicit ::jsonb / ::varchar
        // casts so node-postgres' untyped parameter binds don't trip PG's
        // "could not determine data type" on the assignment target.
        const optionalSets = [];
        const optionalParams = [];
        let paramIndex = 5;
        if (metadataJson) {
            optionalSets.push('metadata = $' + paramIndex + '::jsonb');
            optionalParams.push(metadataJson);
            paramIndex++;
        }
        if (cleanExtension) {
            optionalSets.push('extension = $' + paramIndex + '::varchar');
            optionalParams.push(cleanExtension);
            paramIndex++;
        }
        const extraSets = optionalSets.length ? ', ' + optionalSets.join(', ') : '';
        result = await pool.query(`
            UPDATE documents
            SET title = $1, content = $2, deleted_at = NULL, updated_at = NOW(), kind = $4${extraSets}
            WHERE id = $3
            RETURNING id, namespace, slug, title, created_by_actor_id, created_at, updated_at, metadata, extension
        `, [title, content, existingId, kind, ...optionalParams]);
    } else {
        // Insert-only path. If a row already exists at this slug, surface a clean
        // 409 DUPLICATE_SLUG instead of letting the PG unique-constraint violation
        // become an opaque 500. Callers that want overwrite semantics should pass
        // { upsert: true } (or use editNote for incremental edits).
        try {
            result = await pool.query(`
                INSERT INTO documents (namespace, slug, title, content, created_by_actor_id, kind, metadata, extension)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
                RETURNING id, namespace, slug, title, created_by_actor_id, created_at, updated_at, metadata, extension
            `, [namespace, resolvedSlug, title, content, createdByActorId, kind, metadataJson, cleanExtension]);
        } catch (err) {
            // 23505 = unique_violation in PostgreSQL
            if (err.code === '23505') {
                throw Object.assign(
                    new Error(`Note already exists at slug "${resolvedSlug}" in namespace "${namespace}". Use edit_note to update existing notes, or pass upsert:true to overwrite.`),
                    { statusCode: 409, code: 'DUPLICATE_SLUG' }
                );
            }
            throw err;
        }
    }

    const doc = result.rows[0];

    // Update namespace usage counters
    if (existing.rows.length > 0) {
        const oldBytes = existing.rows[0].content_length || 0;
        updateUsage(namespace, 0, content.length - oldBytes);
    } else {
        updateUsage(namespace, 1, content.length);
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

    // Notify admin dashboard clients that this note changed
    broadcast('note_updated', { namespace, slug: resolvedSlug, operation: 'saved' });

    // Classify the note's cognitive type (fire-and-forget); drives per-type decay in search ranking.
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

// Maximum number of lines a single paginated read can return. Guards against a
// caller asking for 1M lines and OOM'ing the server on a pathological note.
const READ_MAX_LIMIT = 10000;

// Apply line-based pagination to a note's content. Used by both the MCP
// read_note handler and the /documents/read HTTP route.
//
// Returns { text, totalLines, paginated }:
//   text         — content prefixed with "[lines START-END of TOTAL]\n\n" when
//                  paginated; raw content when not.
//   totalLines   — total line count (only meaningful when paginated).
//   paginated    — true when offset or limit was supplied.
//
// Semantics match Claude Code's local Read tool:
//   - 1-indexed (first line = 1).
//   - No params at all => full note returned verbatim (back-compat).
//   - offset without limit => limit defaults to 2000.
//   - limit without offset => offset defaults to 1.
//   - offset past end of note => header with empty body (no placeholder text).
//
// Throws 400 on out-of-range params. No silent clamping — surfacing errors
// beats breeding superstition about what the caller actually got back.
function paginateContent(content, offset, limit) {
    const hasOffset = offset !== undefined && offset !== null;
    const hasLimit = limit !== undefined && limit !== null;

    if (!hasOffset && !hasLimit) {
        return { text: content, paginated: false };
    }

    const startLine = hasOffset ? offset : 1;
    const effectiveLimit = hasLimit ? limit : 2000;

    if (!Number.isInteger(startLine) || startLine < 1) {
        throw Object.assign(
            new Error('offset must be a positive integer (1-indexed)'),
            { statusCode: 400 }
        );
    }
    if (!Number.isInteger(effectiveLimit) || effectiveLimit < 1) {
        throw Object.assign(
            new Error('limit must be a positive integer'),
            { statusCode: 400 }
        );
    }
    if (effectiveLimit > READ_MAX_LIMIT) {
        throw Object.assign(
            new Error(`limit must not exceed ${READ_MAX_LIMIT}`),
            { statusCode: 400 }
        );
    }

    const lines = content.split('\n');
    const totalLines = lines.length;

    // Offset past end: emit header with open-ended range, zero-byte body.
    // Callers use this as the signal to stop paginating.
    if (startLine > totalLines) {
        return {
            text: `[lines ${startLine}- of ${totalLines}]\n\n`,
            totalLines,
            paginated: true
        };
    }

    const startIdx = startLine - 1;
    const endIdx = Math.min(startIdx + effectiveLimit, totalLines);
    const endLine = endIdx;
    const slice = lines.slice(startIdx, endIdx).join('\n');
    const header = `[lines ${startLine}-${endLine} of ${totalLines}]\n\n`;

    return {
        text: header + slice,
        totalLines,
        paginated: true
    };
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

    // Notify admin dashboard clients that this note changed
    broadcast('note_updated', { namespace, slug, operation: 'edited' });

    return {
        ...result.rows[0],
        replacements: replaceAll ? count : 1
    };
}

// Text search across notes — like grep but for the notes database.
// Upper bound on grep context lines per direction. The `limit` on matching
// notes already caps worst-case output size, but this bound protects against
// a single note with many matches flooding the response.
const GREP_MAX_CONTEXT = 50;

// Search notes for text matches and return matching lines with context.
//
// options:
//   contextBefore  — lines of context before each match (default 2, cap 50)
//   contextAfter   — lines of context after each match  (default 2, cap 50)
//   context        — symmetric shortcut; specific params take precedence
//   regex          — treat pattern as case-insensitive regex (default false)
//
// Returns per-doc objects:
//   { namespace, slug, title, matchCount, matches: [{ lineNumber, line, isMatch, newBlock? }] }
//
// `newBlock: true` marks the first line of a non-contiguous context block
// within a note — formatters use this to emit a ripgrep-style `--` separator.
async function grepNotes(pattern, namespace, limit, readableNamespaces, options) {
    if (!pattern) {
        throw Object.assign(new Error('Required field: pattern'), { statusCode: 400 });
    }

    const opts = options || {};
    const regex = opts.regex === true;

    // Resolve context window. `context` sets both sides; the specific params
    // win when both are supplied. Defaults to ±2 for back-compat.
    let contextBefore = opts.contextBefore;
    let contextAfter = opts.contextAfter;
    if (contextBefore === undefined || contextBefore === null) {
        contextBefore = (opts.context !== undefined && opts.context !== null) ? opts.context : 2;
    }
    if (contextAfter === undefined || contextAfter === null) {
        contextAfter = (opts.context !== undefined && opts.context !== null) ? opts.context : 2;
    }

    if (!Number.isInteger(contextBefore) || contextBefore < 0 || contextBefore > GREP_MAX_CONTEXT) {
        throw Object.assign(
            new Error(`context_before must be an integer between 0 and ${GREP_MAX_CONTEXT}`),
            { statusCode: 400 }
        );
    }
    if (!Number.isInteger(contextAfter) || contextAfter < 0 || contextAfter > GREP_MAX_CONTEXT) {
        throw Object.assign(
            new Error(`context_after must be an integer between 0 and ${GREP_MAX_CONTEXT}`),
            { statusCode: 400 }
        );
    }

    const maxResults = limit || 20;

    // PG's `~*` is case-insensitive POSIX regex; ILIKE handles the plain
    // substring case. Both prefilter candidate notes so the JS scan below
    // only walks content we already know contains a match somewhere.
    //
    // Regex mode is guarded in two layers before the pattern touches the
    // matcher or Postgres:
    //   1. Length cap — bounds worst-case parser work and keeps the
    //      surface area of any clever pattern small.
    //   2. safe-regex static analysis — rejects patterns with nested
    //      quantifiers and other shapes known to cause catastrophic
    //      backtracking (ReDoS). Node's regex engine does not bound
    //      runtime, so a pathological pattern run on a long line would
    //      stall the single event loop for every request.
    // Callers are authenticated agents, but a mistake by one agent must
    // not DoS every other agent sharing the process.
    let sqlPattern, sqlOp;
    if (regex) {
        if (pattern.length > GREP_REGEX_MAX_LENGTH) {
            throw Object.assign(
                new Error(`regex pattern exceeds ${GREP_REGEX_MAX_LENGTH} characters`),
                { statusCode: 400 }
            );
        }
        try {
            new RegExp(pattern, 'i');
        } catch (err) {
            throw Object.assign(
                new Error(`Invalid regex pattern: ${err.message}`),
                { statusCode: 400 }
            );
        }
        if (!safeRegex(pattern)) {
            throw Object.assign(
                new Error('regex pattern rejected by safety check (possible catastrophic backtracking). Simplify the pattern or use substring mode.'),
                { statusCode: 400 }
            );
        }
        sqlPattern = pattern;
        sqlOp = '~*';
    } else {
        sqlPattern = `%${pattern}%`;
        sqlOp = 'ILIKE';
    }

    let sql, params;
    if (namespace && namespace !== '*') {
        sql = `
            SELECT id, namespace, slug, title, content, updated_at
            FROM documents
            WHERE namespace = $1 AND deleted_at IS NULL AND (content ${sqlOp} $2 OR title ${sqlOp} $2)
            ORDER BY updated_at DESC
            LIMIT $3
        `;
        params = [namespace, sqlPattern, maxResults];
    } else if (readableNamespaces) {
        // Filter at query level to ensure LIMIT returns correct result count
        sql = `
            SELECT id, namespace, slug, title, content, updated_at
            FROM documents
            WHERE namespace = ANY($1) AND deleted_at IS NULL AND (content ${sqlOp} $2 OR title ${sqlOp} $2)
            ORDER BY updated_at DESC
            LIMIT $3
        `;
        params = [readableNamespaces, sqlPattern, maxResults];
    } else {
        sql = `
            SELECT id, namespace, slug, title, content, updated_at
            FROM documents
            WHERE deleted_at IS NULL AND (content ${sqlOp} $1 OR title ${sqlOp} $1)
            ORDER BY updated_at DESC
            LIMIT $2
        `;
        params = [sqlPattern, maxResults];
    }

    const result = await pool.query(sql, params);

    // Build a per-line matcher once per call. Regex path compiles /pattern/i;
    // substring path lowercases the needle and uses String#includes.
    let matcher;
    if (regex) {
        const re = new RegExp(pattern, 'i');
        matcher = (line) => re.test(line);
    } else {
        const lowerPattern = pattern.toLowerCase();
        matcher = (line) => line.toLowerCase().includes(lowerPattern);
    }

    return result.rows.map(doc => {
        const lines = doc.content.split('\n');
        const matchLineNumbers = new Set();

        for (let i = 0; i < lines.length; i++) {
            if (matcher(lines[i])) {
                matchLineNumbers.add(i);
            }
        }

        // Expand to include context lines on both sides.
        const contextLineNumbers = new Set();
        for (const lineNum of matchLineNumbers) {
            for (let delta = -contextBefore; delta <= contextAfter; delta++) {
                const idx = lineNum + delta;
                if (idx >= 0 && idx < lines.length) {
                    contextLineNumbers.add(idx);
                }
            }
        }

        // Sort and tag the first line of any non-contiguous run so the
        // formatter can emit a `--` separator between blocks.
        const sortedLines = Array.from(contextLineNumbers).sort((a, b) => a - b);
        const matches = [];
        let prevIdx = null;
        for (const idx of sortedLines) {
            const entry = {
                lineNumber: idx + 1,
                line: lines[idx],
                isMatch: matchLineNumbers.has(idx)
            };
            if (prevIdx !== null && idx > prevIdx + 1) {
                entry.newBlock = true;
            }
            matches.push(entry);
            prevIdx = idx;
        }

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

module.exports = { saveNote, listNotes, readNote, deleteNote, restoreNote, editNote, grepNotes, moveNote, paginateContent, titleToSlug, validateSlug, escapeLike, READ_MAX_LIMIT, GREP_MAX_CONTEXT };
