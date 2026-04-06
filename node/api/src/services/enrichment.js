// Note enrichment — optional LLM-powered keyword/tag/relation generation.
// When enabled, each saved note is analyzed by a virtual agent ("memory-enrichment")
// which generates keywords, tags, and suggested relations to other notes.
// Keywords and tags are stored in the note's metadata JSONB column.
// Relations are created via the existing note_relations system (MEM-091).
//
// Inspired by the A-Mem paper (arxiv.org/html/2502.12110v11) which showed that
// LLM-enriched note metadata + link generation significantly improves search quality.

const pool = require('../db');
const config = require('./config');
const { log } = require('./logger');
const { handleError } = require('./error-handler');

// Agent name for enrichment. If this agent doesn't exist, enrichment silently skips.
const ENRICHMENT_AGENT = 'memory-enrichment';

// Kinds to skip — these are either raw data (conversations), system-managed
// (context, dreams), or not worth the LLM cost to enrich.
const SKIP_KINDS = new Set(['conversation', 'context', 'dream']);

// Debounce window in milliseconds — prevents re-enrichment on rapid successive saves.
const DEBOUNCE_MS = 60000;

// Determine note kind from slug prefix (mirrors slugToKind in documents.js
// but inlined here to avoid circular imports).
function kindFromSlug(slug) {
    if (slug.startsWith('instructions/')) return 'instruction';
    if (slug.startsWith('notes/codebase/')) return 'reference';
    if (slug.startsWith('conversations/')) return 'conversation';
    if (slug.startsWith('context/')) return 'context';
    if (slug.startsWith('dreams/')) return 'dream';
    if (slug.startsWith('tasks/')) return 'task';
    if (slug.startsWith('learnings/')) return 'learning';
    if (slug.startsWith('notes/')) return 'note';
    return 'note';
}

function logEnrich(action, details) {
    log('enrichment', action, details);
}

// Build the user message for the enrichment agent.
// Includes the note content (truncated) and top-k similar notes for context.
function buildUserMessage(namespace, slug, title, content, similarNotes) {
    var truncatedContent = content.length > 4000
        ? content.substring(0, 4000) + '\n\n[... truncated]'
        : content;

    var contextSection = '';
    if (similarNotes && similarNotes.length > 0) {
        contextSection = '\n\n## Related notes in the system\n\n';
        for (var note of similarNotes) {
            var preview = (note.chunk_text || '').substring(0, 200);
            contextSection += '- **' + note.namespace + '/' + note.source_file + '**: ' + preview + '\n';
        }
    }

    return '## Note to enrich\n\n'
        + '**Namespace:** ' + namespace + '\n'
        + '**Slug:** ' + slug + '\n'
        + '**Title:** ' + title + '\n\n'
        + truncatedContent
        + contextSection
        + '\n\nAnalyze this note and return a JSON object with:\n'
        + '- `cognitive_type`: one of "semantic" (facts, definitions, knowledge), '
        + '"episodic" (events, things that happened), '
        + '"procedural" (decisions, conventions, how-tos, institutional knowledge), '
        + 'or "reflective" (synthesized insights, lessons learned, analysis)\n'
        + '- `keywords`: array of 3-8 key concepts (single words or short phrases)\n'
        + '- `tags`: array of 2-5 categorization labels\n'
        + '- `relations`: array of suggested connections to the related notes listed above (aim for 3-8). '
        + 'Each relation should have `target_namespace`, `target_slug`, and `relation_type` '
        + '(one of: depends-on, references, supersedes, led-to, related, subtask-of). '
        + 'Be generous with connections — if two notes share a topic, project, or concept, link them.\n\n'
        + 'Return ONLY valid JSON, no markdown fences, no explanation.';
}

// Parse and validate the enrichment response from the LLM.
// Returns { keywords, tags, relations } or null on failure.
// maxRelations caps how many relations to keep (default 10).
function parseResponse(text, maxRelations) {
    var relationCap = maxRelations || 10;
    if (!text) return null;

    // Strip markdown code fences if present
    var cleaned = text.trim();
    if (cleaned.startsWith('```')) {
        cleaned = cleaned.replace(/^```(?:json)?\s*\n?/, '').replace(/\n?```\s*$/, '');
    }

    var parsed;
    try {
        parsed = JSON.parse(cleaned);
    } catch (e) {
        return null;
    }

    if (!parsed || typeof parsed !== 'object') return null;

    // Validate cognitive type
    var VALID_COGNITIVE_TYPES = ['semantic', 'episodic', 'procedural', 'reflective'];
    var cognitiveType = null;
    if (typeof parsed.cognitive_type === 'string' && VALID_COGNITIVE_TYPES.includes(parsed.cognitive_type.toLowerCase())) {
        cognitiveType = parsed.cognitive_type.toLowerCase();
    }

    // Validate and cap keywords
    var keywords = [];
    if (Array.isArray(parsed.keywords)) {
        for (var kw of parsed.keywords) {
            if (typeof kw === 'string' && kw.length > 0 && kw.length < 100) {
                keywords.push(kw.trim());
            }
            if (keywords.length >= 10) break;
        }
    }

    // Validate and cap tags
    var tags = [];
    if (Array.isArray(parsed.tags)) {
        for (var tag of parsed.tags) {
            if (typeof tag === 'string' && tag.length > 0 && tag.length < 50) {
                tags.push(tag.trim());
            }
            if (tags.length >= 10) break;
        }
    }

    // Validate relations
    var VALID_TYPES = ['depends-on', 'references', 'supersedes', 'led-to', 'related', 'subtask-of'];
    var relations = [];
    if (Array.isArray(parsed.relations)) {
        for (var rel of parsed.relations) {
            if (rel && typeof rel === 'object'
                && typeof rel.target_namespace === 'string'
                && typeof rel.target_slug === 'string'
                && typeof rel.relation_type === 'string'
                && VALID_TYPES.includes(rel.relation_type)) {
                relations.push({
                    target_namespace: rel.target_namespace,
                    target_slug: rel.target_slug,
                    relation_type: rel.relation_type
                });
            }
            if (relations.length >= relationCap) break;
        }
    }

    return { keywords, tags, relations, cognitiveType };
}

// Main enrichment function. Called fire-and-forget from saveNote.
// Silently skips if enrichment is disabled, the agent doesn't exist,
// or the note kind shouldn't be enriched.
async function enrichNote(namespace, slug, title, content, existingMetadata) {
    // Config check — skip if not enabled
    try {
        if (config.get('note_enrichment_enabled') !== 'true') return;
    } catch (e) {
        // Config key doesn't exist yet (pre-migration) — skip silently
        return;
    }

    // Kind check — skip conversations, context docs, dreams
    var kind = kindFromSlug(slug);
    if (SKIP_KINDS.has(kind)) return;

    // Debounce check — skip if recently enriched
    var meta = existingMetadata || {};
    if (meta._enriched_at) {
        var lastEnriched = new Date(meta._enriched_at).getTime();
        if (Date.now() - lastEnriched < DEBOUNCE_MS) return;
    }

    // Agent existence check — skip if no enrichment agent configured
    var { loadAgent, invokeAgent } = require('./virtual-agent');
    var agent = await loadAgent(ENRICHMENT_AGENT);
    if (!agent) return;
    if (!agent.provider || !agent.model || !agent.api_key) return;

    logEnrich('start', { namespace, slug, kind });

    // Configurable neighbor count for relation context
    var neighborCount = 25;
    try {
        var configuredCount = parseInt(config.get('enrichment_neighbor_count'), 10);
        if (configuredCount > 0) neighborCount = configuredCount;
    } catch (e) {
        // Config key doesn't exist yet — use default
    }

    // Fetch similar notes for relation context, respecting visibility.
    // Resolve the namespace owner's actor to get their readable namespaces.
    var similarNotes = [];
    try {
        var { searchMemory } = require('./memory');
        var { resolveByName } = require('./actors');
        var { getReadableNamespaces } = require('./namespace-permissions');

        var searchQuery = title + ' ' + content.substring(0, 500);

        // Resolve the namespace owner to get visibility-correct results
        var actor = await resolveByName(namespace);
        var readableNamespaces = null;
        var actorId = null;
        if (actor) {
            actorId = actor.id;
            readableNamespaces = await getReadableNamespaces(actor.id, namespace, 'agent');
        }

        var searchResults = await searchMemory(searchQuery, '*', neighborCount, readableNamespaces, actorId);
        // Filter out the current note itself
        similarNotes = (searchResults.results || []).filter(function(r) {
            return !(r.namespace === namespace && r.source_file.toLowerCase() === slug.toLowerCase());
        });
    } catch (e) {
        // Search failure shouldn't block enrichment — just proceed without context
        logEnrich('search-failed', { namespace, slug, error: e.message });
    }

    // Call the enrichment agent
    var userMessage = buildUserMessage(namespace, slug, title, content, similarNotes);
    var response;
    try {
        response = await invokeAgent(ENRICHMENT_AGENT, {
            userMessage: userMessage,
            skipRateLimit: true,
            skipCostLimit: false,
            skipRetry: true,
            context: 'note-enrichment'
        });
    } catch (e) {
        logEnrich('agent-error', { namespace, slug, error: e.message });
        return;
    }

    // Parse and validate the response — relation cap is configurable
    var maxRelations = 10;
    try {
        var configuredMax = parseInt(config.get('enrichment_max_relations'), 10);
        if (configuredMax > 0) maxRelations = configuredMax;
    } catch (e) {
        // Config key doesn't exist yet — use default
    }

    var enrichment = parseResponse(response.text, maxRelations);
    if (!enrichment) {
        logEnrich('parse-failed', { namespace, slug, responsePreview: (response.text || '').substring(0, 200) });
        return;
    }

    logEnrich('parsed', {
        namespace, slug,
        cognitiveType: enrichment.cognitiveType,
        keywords: enrichment.keywords.length,
        tags: enrichment.tags.length,
        relations: enrichment.relations.length
    });

    // Merge keywords/tags into existing metadata.
    // Union-merge with deduplication, preserving all other metadata fields.
    var merged = Object.assign({}, meta);

    var existingKeywords = Array.isArray(merged.keywords) ? merged.keywords : [];
    var allKeywords = Array.from(new Set(existingKeywords.concat(enrichment.keywords)));
    if (allKeywords.length > 20) allKeywords = allKeywords.slice(0, 20);
    merged.keywords = allKeywords;

    var existingTags = Array.isArray(merged.tags) ? merged.tags : [];
    var allTags = Array.from(new Set(existingTags.concat(enrichment.tags)));
    if (allTags.length > 20) allTags = allTags.slice(0, 20);
    merged.tags = allTags;

    // Store cognitive type (overwrites on re-enrichment)
    if (enrichment.cognitiveType) {
        merged.cognitive_type = enrichment.cognitiveType;
    }

    merged._enriched_at = new Date().toISOString();

    // Check metadata size limit (10KB)
    var serialized = JSON.stringify(merged);
    if (serialized.length > 10240) {
        logEnrich('metadata-too-large', { namespace, slug, size: serialized.length });
        return;
    }

    // Write metadata via direct SQL UPDATE (not saveNote — avoids recursion)
    try {
        await pool.query(
            'UPDATE documents SET metadata = $1 WHERE namespace = $2 AND LOWER(slug) = LOWER($3) AND deleted_at IS NULL',
            [serialized, namespace, slug]
        );
    } catch (e) {
        logEnrich('metadata-write-failed', { namespace, slug, error: e.message });
        return;
    }

    // Create suggested relations
    if (enrichment.relations.length > 0) {
        var { createRelation } = require('./relations');
        for (var rel of enrichment.relations) {
            try {
                await createRelation(
                    namespace, slug,
                    rel.target_namespace, rel.target_slug,
                    rel.relation_type,
                    ENRICHMENT_AGENT,
                    { source: 'enrichment' },
                    true // autoExtracted
                );
            } catch (e) {
                // Skip invalid relations (self-reference, missing target, etc.)
                logEnrich('relation-failed', {
                    namespace, slug,
                    target: rel.target_namespace + '/' + rel.target_slug,
                    type: rel.relation_type,
                    error: e.message
                });
            }
        }
    }

    logEnrich('complete', {
        namespace, slug,
        keywords: enrichment.keywords,
        tags: enrichment.tags,
        relations: enrichment.relations.length,
        cost: response.cost ? response.cost.toFixed(6) : 'unknown'
    });
}

// Generate "related" edges between notes that share keywords or tags.
// Runs as a batch job — scans all enriched notes and creates relations
// for pairs that share a configurable minimum number of keywords/tags.
// Does not use LLM calls — pure SQL/JS.
async function generateKeywordRelations(minShared) {
    var threshold = minShared || 3;
    var { createRelation } = require('./relations');

    logEnrich('keyword-relations-start', { threshold });

    // Fetch all notes that have keywords or tags in metadata
    var result = await pool.query(`
        SELECT namespace, slug, metadata
        FROM documents
        WHERE deleted_at IS NULL
          AND metadata IS NOT NULL
          AND (metadata->>'keywords' IS NOT NULL OR metadata->>'tags' IS NOT NULL)
          AND kind NOT IN ('conversation', 'context', 'instruction')
    `);

    // Build a map of note -> Set of terms (keywords + tags combined)
    var notes = [];
    for (var row of result.rows) {
        var meta = row.metadata;
        if (typeof meta === 'string') {
            try { meta = JSON.parse(meta); } catch (e) { continue; }
        }
        var terms = new Set();
        if (Array.isArray(meta.keywords)) {
            for (var kw of meta.keywords) {
                terms.add(kw.toLowerCase().trim());
            }
        }
        if (Array.isArray(meta.tags)) {
            for (var tag of meta.tags) {
                terms.add(tag.toLowerCase().trim());
            }
        }
        if (terms.size > 0) {
            notes.push({ namespace: row.namespace, slug: row.slug, terms: terms });
        }
    }

    logEnrich('keyword-relations-indexed', { noteCount: notes.length });

    // Compare all pairs — O(n^2) but n is small (hundreds, not millions)
    var created = 0;
    var skipped = 0;
    for (var i = 0; i < notes.length; i++) {
        for (var j = i + 1; j < notes.length; j++) {
            var a = notes[i];
            var b = notes[j];

            // Count shared terms
            var shared = 0;
            for (var term of a.terms) {
                if (b.terms.has(term)) shared++;
            }

            if (shared >= threshold) {
                try {
                    await createRelation(
                        a.namespace, a.slug,
                        b.namespace, b.slug,
                        'related',
                        null, // no specific creator
                        { source: 'keyword-overlap', shared_count: shared },
                        true // auto_extracted
                    );
                    created++;
                } catch (e) {
                    // Skip duplicates, self-references, etc.
                    skipped++;
                }
            }
        }
    }

    logEnrich('keyword-relations-complete', { created, skipped, totalPairs: notes.length * (notes.length - 1) / 2 });
    return { created, skipped, noteCount: notes.length };
}

module.exports = { enrichNote, generateKeywordRelations };
