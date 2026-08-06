// Dream processing — nightly conversation log analysis.
// Reads conversation logs uploaded by agents (or, for dream_source=notes
// agents, their curated notes — see MEM-137), sends them through a dream
// virtual agent (companion or technical), and saves consolidated insights
// as notes in the agent's namespace.

const pool = require('../db');
const config = require('./config');
const { log, logError } = require('./logger');
const { saveNote, readNote, listNotes } = require('./documents');
const { invokeAgent } = require('./virtual-agent');
const { personContextSlug } = require('./people-slug');
const { normalizeSlugPrefix, slugToDisplay } = require('./sim-conversation-distiller');

// Upper bound on the operator-settable politeness delays between provider calls
// (dream_interagent_delay, MEM-092; dream_interchunk_delay, MEM-118). An hour is
// far past any real pause — the defaults are 2s and 1s.
const MAX_DREAM_DELAY_MS = 3600000;

// dreamDelayMs reads one of those config rows and clamps it.
//
// The clamp is not about a hostile operator, it's about a silent inversion:
// Node stores a timer's delay in a signed 32-bit int, so a value past 2^31-1 ms
// (~24.8 days) overflows, is clamped to 1ms, and only emits a
// TimeoutOverflowWarning. A fat-fingered "pause a very long time" therefore
// becomes "no pause at all" and the run hammers the provider — the opposite of
// what was asked, with nothing in the logs to say so.
//
// Parsing is integer-based and permissive: blank and non-numeric values take the
// default, a fractional value truncates, and a negative one survives to be
// skipped by the caller's `> 0` guard.
//
// Only a genuinely unparseable value falls back. An explicit 0 is kept, because
// both config rows document "0 to disable" (migrations MEM-092, MEM-118) and the
// earlier `parseInt(x) || fallback` treated that 0 as falsy — so setting a row to
// 0 handed back the DEFAULT delay and the documented escape hatch never worked
// (LLM-584). The callers' `> 0` guards already implement the disable path; only
// the parse was swallowing the 0 before they could see it.
//
// This is the reason config.parseNonNegativeFinite is not used here: it parses
// with Number rather than parseInt, which would keep fractions instead of
// truncating them and would reject negatives instead of passing them through.
function dreamDelayMs(key, fallback) {
    let configured = parseInt(config.get(key));
    if (Number.isNaN(configured)) {
        configured = fallback;
    }
    return Math.min(configured, MAX_DREAM_DELAY_MS);
}

// validatePersonSlug — defense for runPersonContextUpdate now that it's
// exported. Pass the input back through the same slugify the dream cron
// uses; if the result differs from the input, the input was not already
// a clean slug (could carry path traversal, whitespace, or unsafe
// characters). Returns the canonical slug or null. Reuses people-slug.js
// rather than duplicating the regex so both paths stay in sync.
function validatePersonSlug(slug) {
    if (typeof slug !== 'string') return null;
    const canonical = personContextSlug(slug);
    if (!canonical || canonical !== slug) return null;
    return canonical;
}

// The single validated boundary for a dream scope's slug prefix. Returns '' for
// an ABSENT prefix (undefined/null/'' → dedicated agent, namespace root) or the
// canonical "<villager>/" prefix for a shared-VA villager. THROWS on a
// present-but-invalid prefix — a non-string (number/object/array), or a
// non-canonical string (LIKE metacharacters, path traversal, wrong case,
// over-length). Distinguishing "absent" from "present but invalid" matters: a
// malformed shared prefix silently coerced to '' would collapse the villager's
// notes into the unscoped namespace and cause cross-villager reads/writes.
// Every path and LIKE pattern in the shared path flows from this. Pure +
// exported so every branch is unit-testable.
function resolveScopePrefix(raw) {
    if (raw === undefined || raw === null || raw === '') {
        return '';
    }
    if (typeof raw !== 'string') {
        throw new Error('invalid slug prefix: not a string');
    }
    // Require the input to be ALREADY canonical: normalize, then demand the
    // result equals the input. normalizeSlugPrefix canonicalizes (adds a missing
    // trailing slash, collapses doubled slashes, trims whitespace), so a bare
    // !canonical check would silently accept non-canonical forms — and two
    // distinct roster values ('constance-scott' and 'constance-scott/') would
    // collapse to one namespace. The write path (the distiller) canonicalizes on
    // store; this read boundary rejects anything that isn't already in canonical
    // form (missing/doubled slash, surrounding whitespace, uppercase, LIKE
    // metacharacters, traversal, over-length).
    const canonical = normalizeSlugPrefix(raw);
    if (!canonical || canonical !== raw) {
        throw new Error('invalid slug prefix: ' + raw);
    }
    return canonical;
}

// Build the context/people note slug for a person under a scope prefix. Safe to
// call with any input: BOTH components are validated — the prefix through
// resolveScopePrefix (absent → namespace root; present-but-invalid → throws)
// and the person slug through validatePersonSlug (must be a canonical
// lowercase-kebab slug; a '../', space, or non-string throws). Neither path
// component can traverse or inject. Exported for direct unit testing of the
// path invariant.
function peopleNotePath(slugPrefix, personSlug) {
    const safePersonSlug = validatePersonSlug(personSlug);
    if (!safePersonSlug) {
        throw new Error('invalid person slug: ' + personSlug);
    }
    return resolveScopePrefix(slugPrefix) + 'context/people/' + safePersonSlug;
}

// Signal patterns that indicate memory-worthy content.
// Used to pre-filter conversation logs before sending to the dream agent,
// keeping only passages around these signals + surrounding context.
const SIGNAL_PATTERNS = [
    // Explicit memory requests
    /\bremember\b/i,
    /\bdon'?t forget\b/i,
    /\bnote that\b/i,
    /\bkeep in mind\b/i,
    // Corrections and feedback
    /\bdon'?t do that\b/i,
    /\bstop doing\b/i,
    /\bnot like that\b/i,
    /\bwrong\b/i,
    /\binstead\b/i,
    /\bactually\b/i,
    /\bno,?\s/i,
    // Preferences and decisions
    /\bfrom now on\b/i,
    /\balways\b/i,
    /\bnever\b/i,
    /\bprefer\b/i,
    /\bI like\b/i,
    /\bI hate\b/i,
    /\bI want\b/i,
    // Temporal / deadline signals
    /\bdeadline\b/i,
    /\bby\s+(monday|tuesday|wednesday|thursday|friday|saturday|sunday|tomorrow|next week)\b/i,
    /\bdue\s+(date|by)\b/i,
    // Reasons and context
    /\bthe reason\b/i,
    /\bbecause\b/i,
    /\bimportant\b/i,
    // Emotional signals (for companion mode)
    /\bfeeling\b/i,
    /\bworried\b/i,
    /\bexcited\b/i,
    /\bfrustrat/i,
    /\bhappy\b/i,
    /\bsad\b/i,
    /\bstress/i,
    /\banxious/i,
    /\bthank you\b/i,
    /\blove\b/i,
    /\bmiss\b/i,
];

// How many context lines to include before and after a signal match
const CONTEXT_LINES = 5;

// Reasoning-preamble markers used by detectReasoningPreamble to guard
// the soul-save path against chat models that emit chain-of-thought
// when asked for plain output. See processDreamChunk's soul block for
// usage. All entries are lowercase substrings; the input is lowercased
// once before matching. Keep this list conservative — markers must be
// distinct enough that legitimate first-person soul prose cannot
// trigger them. Plain "analysis:" is intentionally excluded because a
// soul could legitimately reference analysis as a topic.
const REASONING_PREAMBLE_MARKERS = [
    'thinking process',
    '<thinking',
    '<think',
    '1. **analyze',
    'step 1:',
    'let me analyze',
    'let me think',
    "i'll analyze",
    "first, i'll",
    'first, let me',
    '## analysis',
];

// Returns the first matched marker name, or null if the leading 200
// characters look like clean output. Exported as a module-scope helper
// so it's straightforward to add regression tests when new leakage
// patterns surface.
function detectReasoningPreamble(text) {
    if (!text) return null;
    const leadCheck = text.trim().substring(0, 200).toLowerCase();
    return REASONING_PREAMBLE_MARKERS.find(m => leadCheck.includes(m)) || null;
}

// True when a soul has no usable prior document to evolve and should be rebuilt
// from scratch (backload recent dreams) instead. Two cases: it's empty (deleted
// or first run), or it's a suspiciously short stub — the fingerprint of a
// truncated/degraded write. Detecting the stub at read time matters because the
// soul-writer feeds its own prior output back in as input, so a garbled stub
// left in evolve-only mode compounds every cycle instead of self-healing
// (LLM-420). minChars <= 0 disables the short-stub arm, leaving only the empty
// check. Exported as a module-scope helper so the threshold logic is unit-testable.
function soulNeedsRebuild(existingSoul, minChars) {
    const trimmed = (existingSoul || '').trim();
    if (trimmed === '') return true;
    if (minChars > 0 && trimmed.length < minChars) return true;
    return false;
}

// Assembles the user message for the soul-writer. Kept as a pure, exported
// helper so the rebuild-vs-evolve branching (which document goes in, which
// framing) is unit-testable without standing up the whole dream cron. When
// needsRebuild is true the prior soul is deliberately withheld — replaced by a
// placeholder, plus the from-scratch rebuild framing when a dream backload is
// available — so a degraded/truncated stub can never leak back into the
// writer's input (LLM-420). When a backload is not available (disabled or no
// dreams yet) the day's chunk is used as the snapshot regardless of rebuild.
function buildSoulUserMessage({ agentName, startupInstructions, existingSoul, needsRebuild, backloadDreams, chunkDate, dreamContent }) {
    // Length cap shared with the sim-soul endpoint (LLM-501) — the soul is
    // re-billed on every NPC turn, so both soul writers carry the same
    // condense-don't-grow contract.
    const { SOUL_LENGTH_DIRECTIVE } = require('./sim-soul');
    return '## Agent: ' + agentName + '\n\n'
        + (startupInstructions
            ? '## Character description\n\n' + startupInstructions + '\n\n'
            : '')
        + '## Current soul document\n\n'
        + (needsRebuild ? '(none on file — rebuilding from recent dreams)' : existingSoul)
        + (backloadDreams
            ? '\n\n## Dream snapshot for initial soul rebuild\n\n'
                + 'There is no usable prior soul document. Synthesize an initial soul from the recent dream history below; do not treat this as a single-day incremental update.\n\n'
                + backloadDreams
            : '\n\n## Dream snapshot for ' + chunkDate + '\n\n' + dreamContent)
        + '\n\n' + SOUL_LENGTH_DIRECTIVE;
}

// Cheap detection for the typed-context JSON array format. The whole
// multi-turn discussion history sits on a single line as a JSON array
// of {sender, content} objects (see virtual-agent.js's <Conversation>
// block). prefilterLog and extractSpeakers both need this pattern —
// prefilterLog so the speaker record survives signal-based filtering,
// extractSpeakers so it can parse the array into per-speaker lines.
const JSON_ARRAY_HEAD = /^\s*\[\s*\{\s*"sender"\s*:/;

// A ledger line — the engine's own record of something that actually happened,
// as emitted by sim-conversation-distiller.js's narrateEvent: a speaker label
// followed by narration wholly wrapped in parentheses.
//
//   [Friday 18:26 Constance Scott] (paid Josiah Thorne 1 coin for milk)
//
// Contrast with speech, which the distiller wraps in double quotes and which is
// written by the NPC's own model — a claim, not a fact. The paren-vs-quote
// distinction is the only discriminator the two shapes have, and it is reliable
// because both wrappers are applied by the distiller, not by the model
// (sanitizeSpeech escapes any embedded quote before wrapping).
const LEDGER_LINE = /^\[[^\]]+\]\s+\(.*\)\s*$/;

function logDream(action, details) {
    log('dream', action, details);
}

// Pre-filter a conversation log to only signal-bearing passages.
// Returns a reduced version of the log with signal lines + surrounding context.
function prefilterLog(content) {
    const lines = content.split('\n');

    // Find which lines contain signals
    const signalLineIndices = new Set();
    for (let i = 0; i < lines.length; i++) {
        for (const pattern of SIGNAL_PATTERNS) {
            if (pattern.test(lines[i])) {
                signalLineIndices.add(i);
                break;
            }
        }
    }

    // A day with ledger lines is never "nothing worth dreaming about", even
    // when nobody said anything signal-bearing (LLM-523). SIGNAL_PATTERNS only
    // recognize conversational markers, so a day of pure transactions used to
    // be discarded whole — dream, learnings and people files alike — which is
    // exactly the case this change exists to carry through. Ledger lines are
    // deliberately NOT added to signalLineIndices: they are pinned below
    // without pulling CONTEXT_LINES of surrounding chatter, so a talkative day
    // keeps the same filtered shape it had before.
    const ledgerLineIndices = [];
    for (let i = 0; i < lines.length; i++) {
        if (LEDGER_LINE.test(lines[i])) {
            ledgerLineIndices.push(i);
        }
    }

    if (signalLineIndices.size === 0 && ledgerLineIndices.length === 0) {
        return null; // Nothing said, nothing done — nothing worth dreaming about
    }

    // Expand to include context around each signal
    const includedLines = new Set();
    for (const idx of signalLineIndices) {
        const start = Math.max(0, idx - CONTEXT_LINES);
        const end = Math.min(lines.length - 1, idx + CONTEXT_LINES);
        for (let i = start; i <= end; i++) {
            includedLines.add(i);
        }
    }

    // Always include the typed-context JSON-array conversation record. The
    // array is one long line carrying every speaker's turn, but its
    // utterances often don't contain signal-pattern words, so signal-only
    // filtering can drop it entirely. When that happens extractSpeakers
    // sees no parseable speaker data and the people-update loop runs zero
    // iterations — the visible symptom is per-NPC people-files freezing in
    // place after a multi-agent discussion. Pinning these lines into the
    // filtered output guarantees extractSpeakers always sees the speaker
    // enumeration, while the dream LLM still gets the signal-filtered
    // version of the surrounding narrative.
    for (let i = 0; i < lines.length; i++) {
        if (JSON_ARRAY_HEAD.test(lines[i])) {
            includedLines.add(i);
        }
    }

    // Pin the engine-authored ledger lines found above. A bare economic fact
    // carries no signal word of its own, so it used to survive only when it
    // happened to sit within CONTEXT_LINES of someone's chatter. On Constance
    // Scott's 2026-07-24 that silently dropped "(earned 4 coins working for
    // Ezekiel Crane)" and "(delivered meat to John Ellis for 4 coins)"; on
    // 07-15 it dropped "(offered to work for Josiah Thorne for 4 coins)".
    // These are the deterministic record the people-VA has to weigh talk
    // against, so they are never filtered out. Cheap: a ledger line is one
    // short sentence, and most already survived as context lines.
    for (const idx of ledgerLineIndices) {
        includedLines.add(idx);
    }

    // Build the filtered content, inserting separators where lines are skipped
    const result = [];
    let lastIncluded = -2;
    for (let i = 0; i < lines.length; i++) {
        if (includedLines.has(i)) {
            if (i > lastIncluded + 1 && lastIncluded >= 0) {
                result.push('  [...]');
            }
            result.push(lines[i]);
            lastIncluded = i;
        }
    }

    return result.join('\n');
}

// Assemble the notes-mode dream source text (MEM-137). Each curated note
// gets a header carrying its slug and last-updated date so the dream agent
// can attribute material to a document, mirroring how conversation logs
// carry their own timestamps. Rows arrive from the notes-mode source query
// ordered by updated_at ASC.
function buildNotesLog(rows) {
    return rows.map(r => {
        const updated = r.updated_at instanceof Date
            ? r.updated_at.toISOString().slice(0, 10)
            : String(r.updated_at).slice(0, 10);
        return '## Note: ' + r.slug + ' (updated ' + updated + ')\n\n' + r.content;
    }).join('\n\n---\n\n');
}

// Extract unique speakers from conversation logs and group lines by speaker.
// Handles five formats:
//   memory-sync uploads:    "[HH:MM speaker] message text"
//   VA transcript metadata: "- **From:** speaker"
//   discussion history:     "speaker: message text" or "[timestamp] speaker: message text"
//   typed-context-injection JSON array: '[{"sender":"name","content":"..."}, ...]'
//     — produced by the typed-context VA prompt (the <Discussion> block in
//     virtual-agent.js). The whole message history sits on one line as a
//     JSON array; we parse it and treat each entry as a discrete speaker
//     line. Without this branch the salem NPCs' people-files freeze the
//     moment their conversation traffic shifts to multi-agent discussions.
//   sim-day distiller:      "[Weekday HH:MM Display Name] text"
//     — produced by sim-conversation-distiller.js for sim-mode NPCs.
//     Display name is multi-word ("John Ellis"), so a separate pattern
//     and slug step is needed.
//
// Returns a Map<slug, { display, lines }>:
//   - slug is the filesystem-safe key used for context/people/{slug}.
//     Built via personContextSlug from the display name; identical to
//     the slug loadPeopleContext uses at read time.
//   - display is the human-readable name to show in the people-update
//     prompt (## Person: ...) and the saved note title.
//   - lines is the per-speaker excerpt array fed to the dream-people
//     agent.
//
// Self-skip: lines spoken by the agent itself are dropped so the
// agent's namespace doesn't accumulate context/people/{self}. agentName
// arrives as the actor slug (e.g. "home" in companion mode,
// "zbbs-john-ellis" in sim mode); the salem zbbs- prefix is stripped
// before slugifying so "zbbs-john-ellis" matches "John Ellis" lines.
function extractSpeakers(content, agentName) {
    const lines = content.split('\n');
    const speakerLines = new Map();
    const agentSlugSelf = personContextSlug(String(agentName || '').replace(/^zbbs-/, ''));

    // Pattern for sim distiller format: [Weekday HH:MM Display Name]
    // Captures the display name (group 1). Multi-word names land here;
    // chatPattern's \S+ would only catch the first token.
    const simPattern = /^\[\w+day\s+\d{2}:\d{2}\s+([^\]]+?)\]/;
    // Pattern for memory-sync format: [HH:MM speaker]
    const chatPattern = /^\[(\d{2}:\d{2})\s+(\S+)\]/;
    // Pattern for VA transcript metadata: - **From:** speaker
    const fromPattern = /^-\s+\*\*From:\*\*\s+(\S+)/;
    // Pattern for discussion history: "speaker: message" or "[timestamp] speaker: message"
    // Speaker names are agent identifiers (lowercase, may contain hyphens)
    const discussionPattern = /^(?:\[.*?\]\s+)?([a-z][a-z0-9-]*):(?:\s|$)/;

    let currentSpeaker = null;

    function addSpeaker(displayName, line) {
        const slug = personContextSlug(displayName);
        if (!slug || slug === agentSlugSelf) {
            currentSpeaker = null;
            return;
        }
        currentSpeaker = slug;
        if (!speakerLines.has(slug)) {
            speakerLines.set(slug, { display: displayName, lines: [] });
        }
        if (line) {
            speakerLines.get(slug).lines.push(line);
        }
    }

    for (const line of lines) {
        // Skip section headers and metadata
        if (line.startsWith('##') || line.startsWith('---')) {
            continue;
        }

        // Typed-context JSON array — handle before the chat/discussion
        // patterns because the line opens with '[' and could otherwise
        // confuse the timestamp-prefixed discussionPattern. Failure to
        // parse falls through to the line-based patterns below.
        if (JSON_ARRAY_HEAD.test(line)) {
            try {
                const messages = JSON.parse(line.trim());
                if (Array.isArray(messages)) {
                    for (const msg of messages) {
                        if (!msg || typeof msg.sender !== 'string') {
                            continue;
                        }
                        const text = typeof msg.content === 'string' ? msg.content : '';
                        // Label each message with the speaker so the
                        // dream-people LLM can tell turns apart when
                        // several get joined into one prompt.
                        addSpeaker(msg.sender, '[' + msg.sender + '] ' + text);
                    }
                    // Reset currentSpeaker — the JSON array is its own
                    // self-contained block; don't let stray lines after
                    // it attach to the last sender from the array.
                    currentSpeaker = null;
                    continue;
                }
            } catch (err) {
                // Not a parseable array — fall through to other patterns.
            }
        }

        const simMatch = line.match(simPattern);
        if (simMatch) {
            addSpeaker(simMatch[1].trim(), line);
            continue;
        }

        const chatMatch = line.match(chatPattern);
        if (chatMatch) {
            addSpeaker(chatMatch[2], line);
            continue;
        }

        const fromMatch = line.match(fromPattern);
        if (fromMatch) {
            addSpeaker(fromMatch[1], null);
            continue;
        }

        const discussionMatch = line.match(discussionPattern);
        if (discussionMatch) {
            addSpeaker(discussionMatch[1], line);
            continue;
        }

        // Continuation lines belong to the current speaker
        if (currentSpeaker && line.trim()) {
            if (speakerLines.has(currentSpeaker)) {
                speakerLines.get(currentSpeaker).lines.push(line);
            }
        }
    }

    return speakerLines;
}

// Build a case-insensitive whole-word matcher for a name fragment. Word
// boundaries matter: without them "Anne" matches inside "Annexed" and, worse,
// a short first name matches inside an unrelated word. Metacharacters are
// escaped because display names are operator-editable.
//
// The boundaries are Unicode letter/number classes rather than \w, which is
// ASCII-only: with \w, an accented name ("Zoë Marsh") would treat the accented
// character as a boundary and match inside a longer word (code_review, LLM-523).
// Lookarounds rather than capture groups so adjacent occurrences can't consume
// each other's boundary.
function nameMatcher(fragment) {
    const escaped = String(fragment).replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
    return new RegExp('(?<![\\p{L}\\p{N}_])' + escaped + '(?![\\p{L}\\p{N}_])', 'iu');
}

// Pull the speaker name out of a distilled line's bracket header. Handles every
// header the pipeline produces — "[Weekday HH:MM Name]" from the sim distiller
// (formatTimestamp) and "[HH:MM name]" from memory-sync uploads — by stripping
// a leading weekday and a leading clock time if present, rather than demanding
// one exact shape.
//
// This is a best-effort read, NOT a gate. LEDGER_LINE accepts any bracket
// header and is the authority on what counts as a ledger line; a header shape
// this can't reduce (a date prefix, a non-English weekday) just means the
// caller attributes the line by the names IN it instead of by who recorded it.
// Nothing is dropped on a parse miss. Returns '' when there's no header at all.
function ledgerLineSpeaker(line) {
    const header = line.match(/^\[([^\]]+)\]/);
    if (!header) {
        return '';
    }
    return header[1]
        .replace(/^\w+day\s+/i, '')
        .replace(/^\d{1,2}:\d{2}\s+/, '')
        .trim();
}

// Partition one day's excerpts into the two kinds of material the people-VA
// must weigh differently (LLM-523):
//
//   ledger — engine-authored action lines, deterministic fact. These are
//     almost always spoken by the villager THEMSELF ("(paid Josiah Thorne 1
//     coin for milk)"), which is exactly why extractSpeakers never surfaced
//     them: its self-skip drops the agent's own lines so the agent doesn't
//     accumulate a relationship file about itself. Correct for speech, but it
//     meant 100% of ledger events were dropped before the people prompt was
//     built — the people-VA had only talk to go on, and duly wrote an NPC's
//     unbacked verbal promise ("six coins to square us up") into durable
//     memory as a completed payment. A self ledger line is attributed to a
//     person when that person's full display name appears in it; the distiller
//     writes full sanitized names into the narration, so this match is exact.
//   said — quoted speech, model-authored, no better than a claim.
//
// Third-party labeling: extractSpeakers groups by the OTHER party, so anything
// this person said to someone else while the villager was co-present lands in
// this pair's file undifferentiated. A line that names another known speaker
// and does NOT name the villager is labeled as overheard, so it informs the
// character impression without reading as this pair's dealings. The heuristic
// matches a first name only when that first name is unique among the day's
// speakers, and a mislabel costs a softened impression whereas a missing label
// costs a fabricated transaction — so it errs toward labeling.
//
// Takes the speakers map from extractSpeakers (so speech grouping stays in one
// place) plus the unsplit filtered log for the self lines extractSpeakers threw
// away. Pure and exported for unit testing.
//
// Returns Map<slug, { display, ledger: string[], said: string[] }>.
function buildPersonExcerptSections(filtered, selfName, speakers) {
    const sections = new Map();
    for (const [slug, entry] of speakers) {
        sections.set(slug, { display: entry.display, ledger: [], said: [] });
    }

    // selfName arrives as the villager's display name for a shared-VA actor
    // ("Constance Scott") and as the agent slug for a dedicated NPC
    // ("zbbs-josiah-thorne"); slugToDisplay normalizes both to "First Last" and
    // leaves an already-display name untouched.
    const selfDisplay = slugToDisplay(String(selfName || ''));
    const selfSlug = personContextSlug(String(selfName || '').replace(/^zbbs-/, ''));

    // A first name identifies a person only when no one else that day shares
    // it. Two Johns and the token tells us nothing about who was addressed.
    const firstNameCounts = new Map();
    for (const [, entry] of speakers) {
        const first = entry.display.split(/\s+/)[0];
        firstNameCounts.set(first, (firstNameCounts.get(first) || 0) + 1);
    }

    // Matchers are built once per person rather than per line — a day is
    // thousands of lines across a dozen speakers.
    const people = [];
    for (const [slug, entry] of speakers) {
        const first = entry.display.split(/\s+/)[0];
        let firstMatcher = null;
        if (firstNameCounts.get(first) === 1) {
            firstMatcher = nameMatcher(first);
        }
        people.push({
            slug,
            display: entry.display,
            full: nameMatcher(entry.display),
            first: firstMatcher,
        });
    }

    // Self matchers, built the same way. An empty selfDisplay would make
    // nameMatcher('') match every line, so the whole self test is disabled
    // instead — every line would then read as third-party. The callers all
    // supply a real identity (a shared scope throws without one), so this is
    // belt-and-braces.
    let selfFullMatcher = null;
    let selfFirstMatcher = null;
    if (selfDisplay) {
        selfFullMatcher = nameMatcher(selfDisplay);
        const selfFirst = selfDisplay.split(/\s+/)[0];
        if (!firstNameCounts.has(selfFirst)) {
            selfFirstMatcher = nameMatcher(selfFirst);
        }
    }

    function namesPerson(text, person) {
        if (person.full.test(text)) {
            return true;
        }
        if (person.first) {
            return person.first.test(text);
        }
        return false;
    }

    function namesSelf(text) {
        if (selfFullMatcher && selfFullMatcher.test(text)) {
            return true;
        }
        if (selfFirstMatcher) {
            return selfFirstMatcher.test(text);
        }
        return false;
    }

    // Ledger pass over the whole day. A ledger line spoken by the villager is
    // filed under everyone it names; a ledger line spoken by someone else (the
    // engine does not currently push these, but the shape is legal) is filed
    // under that speaker, since it is that person's own recorded act.
    //
    // A non-self speaker normally has a section: this pass and extractSpeakers
    // read the same `filtered` text, and extractSpeakers creates an entry for
    // every non-self bracket-header line it sees, ledger lines included.
    //
    // When the header doesn't resolve to a section — an unslugifiable name, or
    // a header shape ledgerLineSpeaker can't reduce (LEDGER_LINE accepts any
    // bracket header, so it is deliberately the looser of the two) — the line
    // falls through to name-matching rather than being dropped. That makes the
    // header parser's strictness non-load-bearing: a transaction is attributed
    // by whom it NAMES even when we can't tell who recorded it, and an
    // authoritative line is never silently lost (code_review, LLM-523).
    //
    // Name-matching deliberately files a line under EVERY known person it
    // names, not just one. A transaction between two people is part of both
    // relationships, and a ledger line is a fact — showing it to both parties
    // can't fabricate anything. In practice this is near-always a single match:
    // narrateEvent writes from the actor's point of view, so the actor is the
    // speaker in the header and only the counterparty appears in the narration
    // ("(earned 4 coins working for Josiah Thorne)").
    for (const line of filtered.split('\n')) {
        if (!LEDGER_LINE.test(line)) {
            continue;
        }
        const speakerSlug = personContextSlug(ledgerLineSpeaker(line));
        if (speakerSlug && speakerSlug !== selfSlug && sections.has(speakerSlug)) {
            sections.get(speakerSlug).ledger.push(line);
            continue;
        }
        for (const person of people) {
            if (person.full.test(line)) {
                sections.get(person.slug).ledger.push(line);
            }
        }
    }

    // Speech pass. extractSpeakers already grouped these; all that is added is
    // the overheard label. Any ledger line that reached a non-self block above
    // is skipped here so it isn't repeated in both sections.
    for (const [slug, entry] of speakers) {
        const section = sections.get(slug);
        for (const line of entry.lines) {
            if (LEDGER_LINE.test(line)) {
                continue;
            }
            let addressee = null;
            for (const person of people) {
                if (person.slug !== slug && namesPerson(line, person)) {
                    addressee = person.display;
                    break;
                }
            }
            if (addressee && !namesSelf(line)) {
                section.said.push(line + '  (overheard — addressed to ' + addressee + ')');
                continue;
            }
            section.said.push(line);
        }
    }

    return sections;
}

// Find a dream agent by expertise tag. Verifies it exists, is owned by system
// or by a user with 'agents/create_system_equivalent' permission, and has
// provider/model/api_key configured. Returns the agent name or null.
async function findDreamAgent(expertiseTag) {
    const { isTrustedCreator } = require('./admin-permissions');

    const result = await pool.query(
        `SELECT ac.id, ac.name, ac.created_by, agc.provider, agc.model, agc.api_key
         FROM actors ac
         JOIN agent_configuration agc ON agc.actor_id = ac.id
         WHERE ac.expertise @> jsonb_build_array($1::text)`,
        [expertiseTag]
    );

    if (result.rows.length === 0) {
        logDream('error', { message: 'No agent found with expertise: ' + expertiseTag });
        return null;
    }

    // Verify ownership — must be created by system or a trusted creator
    let agent = null;
    for (const row of result.rows) {
        if (await isTrustedCreator(row.created_by)) {
            agent = row;
            break;
        }
    }

    if (!agent) {
        logDream('error', { message: 'Agent with expertise "' + expertiseTag + '" not owned by system or trusted creator' });
        return null;
    }

    if (!agent.api_key || !agent.provider || !agent.model) {
        logDream('error', { message: agent.name + ' (expertise: ' + expertiseTag + ') missing provider/model/api_key' });
        return null;
    }

    return agent.name;
}

// Slugify a title for note storage
function slugify(text) {
    return text.toLowerCase()
        .replace(/[^a-z0-9]+/g, '-')
        .replace(/^-+|-+$/g, '')
        .slice(0, 60);
}

// Split a [since, now] window into per-UTC-day chunks. The first chunk
// starts at `since` (not at the start of that UTC day) so we don't
// re-scan logs already consumed by a prior cron run. Subsequent chunks
// are full UTC days. The final chunk ends at `now`.
//
// Returns [{from: Date, to: Date}, ...]. Empty if since >= now.
//
// Bounds are inclusive on `to`, exclusive on `from`, matching the
// per-chunk SQL query (created_at > from AND created_at <= to). This
// makes setting last_dream_at = chunk.to safely exclude already-
// processed boundary-time logs from the next chunk's window.
function computeDailyChunks(since, now) {
    const sinceMs = (since instanceof Date ? since : new Date(since)).getTime();
    const nowMs = (now instanceof Date ? now : new Date(now)).getTime();
    if (sinceMs >= nowMs) {
        return [];
    }
    const chunks = [];
    const sinceDate = new Date(sinceMs);
    let dayEndMs = Date.UTC(
        sinceDate.getUTCFullYear(),
        sinceDate.getUTCMonth(),
        sinceDate.getUTCDate()
    ) + 24 * 60 * 60 * 1000;
    let cursorMs = sinceMs;
    while (cursorMs < nowMs) {
        const chunkEndMs = Math.min(dayEndMs, nowMs);
        chunks.push({ from: new Date(cursorMs), to: new Date(chunkEndMs) });
        cursorMs = chunkEndMs;
        dayEndMs += 24 * 60 * 60 * 1000;
    }
    return chunks;
}

// Process one (from, to] chunk for one agent: dream → save note → soul →
// people → learnings. Returns a summary object. Throws on dream-call
// failure (the caller catches and decides whether to retry on the next
// cron). Soul, people, and learnings failures are caught here and logged
// but don't fail the chunk — they're auxiliary to the dream note itself.
//
// agentNames: { dreamAgentName, soulAgentName, peopleAgentName, learningsAgentName }
// chunk: { from: Date, to: Date }
// How the people-VA is told to weigh the two sections against each other
// (LLM-523). Mirrors the rule the salem engine applies in its own consolidation
// prompt (LLM-499): the ledger is what happened, speech is what someone said
// happened, and the ledger wins. The direction clause is here because the
// failure it guards against is not the model doubting the ledger but
// misreading it — "(paid Josiah Thorne 1 coin for milk)" was consolidated into
// a jug of milk given as a gift, inverting a purchase into a gratuity.
//
// The non-reciprocal clause is LLM-607, and it is the general form of that same
// misreading. Every ledger line names one movement of coin or goods; nothing in
// the block ever states that a movement came back, because a return is simply
// another line. So a transfer with no answering line looks identical to a debt
// outstanding, and the directive above tells the model to trust that appearance
// completely. Moses James's character document acquired "Constable Marsh takes
// my coin for a 'day's rate' but never delivers nails or any real help — the
// ledger shows I pay him and get nothing", and the constable refunded eight
// coins of collected town rate against it. The same document carried the same
// shape about a gift of flour to a different neighbour, which is why the clause
// is written generally rather than about the levy.
//
// The salem side stops the levy case at its source — a rate payment now narrates
// as a due settled rather than carrying the payer's own words for it — but that
// only covers transfers the engine classifies. Gifts, wages and one-sided
// deliveries have no such marker, so the reading rule has to stand on its own.
const LEDGER_PRECEDENCE_DIRECTIVE = [
    '## How to weigh these',
    '',
    'The ledger is the record of what actually happened — coin and goods that truly changed hands. It is authoritative and complete: if something is not in it, it did not happen. Speech is only what someone said, and people misremember, overstate, and promise more than they deliver. Where the two disagree, the ledger wins.',
    '',
    '- A payment, gift, or delivery counts ONLY if the ledger records it. Someone saying they paid you, or promising to make good on a debt, is not payment. Do not write an unbacked promise into the file as a settled matter.',
    '- Read the ledger direction exactly as written. "(paid Josiah Thorne 1 coin for milk)" means you spent a coin and received milk — a purchase you made, not a gift you were given.',
    '- Not everything that moves one way is owed back. A due, a levy, a gift, a wage — these are settled when they change hands, and the ledger records no return because none was ever coming. Do not turn a one-way line into a debt, and do not write that someone "never delivered" or "gave nothing back" unless the ledger itself shows goods promised and not handed over.',
    '- Lines marked "overheard" were spoken while you were present but addressed to someone else. They tell you about this person\'s character and dealings; they are not your own transactions with them.',
].join('\n');

// Assemble the people-VA user message. Split out of runPersonContextUpdate so
// the prompt structure — the thing LLM-523 is actually fixing — is directly
// testable; runPersonContextUpdate itself reaches invokeAgent/readNote through
// destructured requires that can't be stubbed (see dream-shared-cron.test.js).
//
// ledger: array of engine-authored action lines for this pair (may be empty).
// said:   speech excerpts as one string (empty triggers consolidation-only).
function buildPersonUserMessage({ selfLabel, display, today, existingFile, ledger, said }) {
    // Empty excerpts trigger consolidation-only mode. The prompt recognizes
    // this signal and either consolidates redundant bullets or returns the file
    // unchanged if it's already tight. The sentinel wording is load-bearing —
    // /admin/dream/consolidate-people drives that mode by passing no excerpts
    // at all.
    let saidBlock = '(no new excerpts since last update — please consolidate any redundant bullets if present, or return file unchanged if already tight)';
    if (said && said.trim()) {
        saidBlock = said;
    }

    // The ledger section. Its absence is stated rather than left blank, because
    // "nothing changed hands" is itself the fact that stops a spoken promise
    // from being remembered as a settled payment.
    let ledgerBlock = '(nothing — no coin or goods changed hands between you and ' + display + ' today)';
    if (Array.isArray(ledger) && ledger.length > 0) {
        ledgerBlock = ledger.join('\n');
    }

    return '## Agent: ' + selfLabel + '\n'
        + '## Person: ' + display + '\n'
        + '## Today\'s date: ' + today + '\n\n'
        + '## Current relationship file\n\n'
        + (existingFile || '(empty — first encounter)')
        + '\n\n## What the ledger records\n\n'
        + ledgerBlock
        + '\n\n## What was said in your hearing\n\n'
        + saidBlock
        + '\n\n' + LEDGER_PRECEDENCE_DIRECTIVE;
}

// runPersonContextUpdate — single (agent, person) people-VA invocation,
// reads the existing context/people/{slug} note, runs the appropriate
// people VA, optionally writes the result back. Used by:
//   1. processDreamChunk (per-day dream chunk) — `excerpts` is today's
//      speech lines for this person and opts.ledger is the matching
//      engine-authored action lines, both from buildPersonExcerptSections.
//   2. /admin/dream/consolidate-people endpoint — `excerpts` is empty
//      and opts.ledger absent, so the VA acts as a consolidate-only pass
//      against bloated files.
//
// opts.ledger = array of ledger lines for this pair (LLM-523). Rendered as
// its own authoritative section; absent or empty renders an explicit
// "nothing changed hands" so a spoken promise can't be read as settled.
// opts.dryRun = true skips the write and returns the proposed updated
// file in the result so the caller can inspect.
//
// Returns { existingFile, updatedFile, written, changed }. updatedFile
// is the trimmed VA output (or null if the VA returned empty).
async function runPersonContextUpdate(agentName, peopleAgentName, slug, display, excerpts, today, opts) {
    opts = opts || {};
    const dryRun = !!opts.dryRun;

    // The relationship note is scoped under a shared-VA villager's subtree via
    // opts.slugPrefix ("<villager>/context/people/..."), or namespace root for a
    // dedicated agent. The prefix is validated where the path is built, in
    // peopleNotePath below (absent → root; present-but-invalid → throws), so
    // there's no separate guard here. selfLabel is who the relationship is formed
    // FOR in the people-VA prompt — the villager for a shared actor, not the
    // pooled agent (salem-vendor).
    const selfLabel = opts.selfLabel || agentName;

    // Defend the path input now that this helper is exported. Reject
    // anything that doesn't slug-roundtrip cleanly (path traversal,
    // whitespace, mixed case, etc.). Same regex that the cron's natural
    // slug-creation path uses, so input from the cron continues to pass.
    const safeSlug = validatePersonSlug(slug);
    if (!safeSlug) {
        throw new Error('runPersonContextUpdate: invalid person slug: ' + slug);
    }
    slug = safeSlug;
    const peopleSlug = peopleNotePath(opts.slugPrefix, slug);

    // Sanitize display name before it's interpolated into the user
    // message. Note titles are operator-editable in admin UI, so a
    // multi-line display name could inject extra "## section" markers
    // into the prompt and confuse the VA. Collapse whitespace, trim.
    const safeDisplay = String(display || '').replace(/[\r\n]+/g, ' ').replace(/\s+/g, ' ').trim() || slug;
    display = safeDisplay;

    // Read the current relationship file (empty string if first encounter).
    let existingFile = '';
    try {
        const note = await readNote(agentName, peopleSlug);
        existingFile = note.content || '';
    } catch (e) {
        // No existing file — first encounter.
    }

    const peopleUserMessage = buildPersonUserMessage({
        selfLabel, display, today, existingFile,
        ledger: opts.ledger,
        said: excerpts,
    });

    const { text: rawUpdatedFile, truncated: peopleTruncated, finish_reason: peopleFinish } = await invokeAgent(peopleAgentName, {
        userMessage: peopleUserMessage,
        context: 'people',
        skipRateLimit: true,
        skipCostLimit: true,
        skipRetry: false,
    });

    if (peopleTruncated) {
        // Length-stop: the relationship file came back cut off. The writer
        // rewrites the whole file each pass, so a truncated version drops the
        // bullets it didn't reach — and that lossy file would feed back in as
        // input next time. Leave the prior file intact.
        logDream('person-context-error', {
            agent: agentName, person: display, slug, today,
            reason: 'truncated', finishReason: peopleFinish,
        });
        return { existingFile, updatedFile: '', written: false, changed: false, emptyResponse: false, truncated: true };
    }

    const updatedFile = rawUpdatedFile && rawUpdatedFile.trim();
    const changed = !!(updatedFile && updatedFile !== existingFile.trim());
    const emptyResponse = !updatedFile;
    let written = false;
    if (emptyResponse) {
        // Empty VA response is unusual — log it so it's visible in the
        // dream journal alongside chunk-people-updated/error events.
        // The cron's chunk-people-error catch wraps the call, so an
        // explicit log here keeps the empty case from being silently
        // swallowed by the trim-no-op path below.
        logDream('person-context-empty-response', {
            agent: agentName, person: display, slug, today,
        });
    }

    if (changed && !dryRun) {
        await saveNote(
            agentName,
            'People — ' + display,
            updatedFile,
            peopleSlug,
            peopleAgentName,
            null, null, { upsert: true }
        );
        written = true;
    }

    return { existingFile, updatedFile, written, changed, emptyResponse };
}

async function processDreamChunk(agent, agentNames, chunk, scope) {
    const { dreamAgentName, soulAgentName, peopleAgentName, learningsAgentName } = agentNames;
    const { from, to } = chunk;
    const chunkDateStr = from.toISOString().slice(0, 10);
    const notesMode = agent.dream_source === 'notes';

    // Scope selects the slug subtree and the self-identity. slugPrefix is
    // empty for a dedicated agent (all notes at namespace root) and a
    // "<villager>/" prefix for a shared-VA villager, so every read and write
    // below lands under the same subtree recall searches. selfName is the
    // identity whose own speech is skipped in extractSpeakers and whose name
    // labels the dream/people/learnings prompts — the villager for a shared
    // actor, else the agent itself. Defaulted so dedicated callers are
    // byte-identical.
    //
    // This function is the shared path's real security boundary: it builds every
    // note slug and the conversation-source LIKE pattern from the prefix. It does
    // not trust the caller (scope is an arbitrary internal object) — the prefix
    // runs through resolveScopePrefix, which returns '' for an absent prefix and
    // throws on any present-but-invalid one (non-string, or non-canonical with
    // LIKE metacharacters / traversal). A malformed prefix can never silently
    // become the unscoped namespace.
    scope = scope || {};
    const slugPrefix = resolveScopePrefix(scope.slugPrefix);
    // A shared scope (non-empty prefix) MUST carry a real self identity — the
    // villager's display name. Falling back to the pooled agent (salem-vendor)
    // would make the villager's own lines read as counterparty speech and spawn
    // a bogus self relationship note, so throw rather than silently default. A
    // dedicated scope ('' prefix) legitimately uses the agent's own name.
    if (slugPrefix && !(typeof scope.selfName === 'string' && scope.selfName.trim())) {
        throw new Error('processDreamChunk: shared scope requires a non-empty selfName');
    }
    // Trim the shared self identity so a whitespace-padded display name matches
    // the canonical speaker name in extractSpeakers and reads cleanly in prompts.
    const selfName = slugPrefix ? scope.selfName.trim() : (scope.selfName || agent.name);

    let logs;
    if (notesMode) {
        // Notes-sourced dreaming (MEM-137): the raw material is the agent's
        // own curated notes, windowed by updated_at instead of created_at —
        // so a later human edit to a note re-enters it as fresh material on
        // the next run. The prefix exclusions are load-bearing: without them
        // the cron would dream about its own dreams/soul/learnings output and
        // feed back on itself (the same spiral that bloated the technical
        // souls). conversations/% is excluded because dream_source selects
        // ONE source — agents with real conversation logs use the default.
        logs = await pool.query(
            `SELECT slug, content, updated_at FROM documents
             WHERE namespace = $1 AND deleted_at IS NULL
             AND slug NOT LIKE 'conversations/%'
             AND slug NOT LIKE 'dreams/%'
             AND slug NOT LIKE 'context/%'
             AND slug NOT LIKE 'learnings/%'
             AND updated_at > $2 AND updated_at <= $3
             ORDER BY updated_at ASC`,
            [agent.name, from, to]
        );
    } else {
        logs = await pool.query(
            `SELECT slug, content, created_at FROM documents
             WHERE namespace = $1 AND slug LIKE $4 AND deleted_at IS NULL
             AND created_at > $2 AND created_at <= $3
             ORDER BY created_at ASC`,
            [agent.name, from, to, slugPrefix + 'conversations/%']
        );
    }
    if (logs.rows.length === 0) {
        logDream('chunk-no-logs', { agent: agent.name, source: agent.dream_source, chunkDate: chunkDateStr });
        return { skipped: true, reason: 'no logs', chunkDate: chunkDateStr };
    }

    // In notes mode each note gets a slug+date header so the dream agent can
    // tell documents apart (conversation logs carry their own timestamps;
    // curated notes don't). No signal prefilter either: SIGNAL_PATTERNS are
    // conversational markers ("remember", "don't do that") that deliberate
    // prose rarely contains — filtering would drop most of the material as
    // "no signals". Curated notes are already distilled; feed them whole.
    let fullLog;
    let filtered;
    if (notesMode) {
        fullLog = buildNotesLog(logs.rows);
        filtered = fullLog;
    } else {
        fullLog = logs.rows.map(r => r.content).join('\n\n---\n\n');
        filtered = prefilterLog(fullLog);
    }
    if (!filtered) {
        logDream('chunk-no-signals', { agent: agent.name, chunkDate: chunkDateStr, logCount: logs.rows.length });
        return { skipped: true, reason: 'no signals', chunkDate: chunkDateStr, logCount: logs.rows.length };
    }

    logDream('chunk-processing', {
        agent: agent.name,
        mode: agent.dream_mode,
        source: agent.dream_source,
        chunkDate: chunkDateStr,
        logCount: logs.rows.length,
        originalSize: fullLog.length,
        filteredSize: filtered.length,
    });

    const userMessage = (notesMode
        ? 'Curated notes written or updated by agent "' + agent.name + '" on ' + chunkDateStr
            + ' (this agent\'s memory lives in hand-curated notes — journals, identity documents, session summaries — rather than conversation logs):\n\n'
        : 'Conversation logs for agent "' + selfName + '" on ' + chunkDateStr + ':\n\n')
        + filtered
        + '\n\nAlso provide a brief title summarizing the overarching subject of the day.';

    const { text: response } = await invokeAgent(dreamAgentName, {
        userMessage,
        context: 'dream',
        skipRateLimit: true,
        skipCostLimit: true,
        skipRetry: false,
    });

    const titleMatch = response.match(/^#\s+(.+)$/m) || response.match(/^title:\s*(.+)$/im);
    const title = titleMatch ? titleMatch[1].trim() : 'Dream consolidation';
    const content = response;

    // Slug uses the chunk's date so catching up multiple days produces
    // distinct dated notes (rather than overwriting the same NOW-dated slug).
    const slug = slugPrefix + 'dreams/' + chunkDateStr + '-' + slugify(title);
    await saveNote(agent.name, title + ' (' + chunkDateStr + ')', content, slug, dreamAgentName);
    logDream('chunk-saved', { agent: agent.name, slug, contentLength: content.length });

    // Soul synthesis — runs after each chunk per Jeff's call. The current
    // soul note is the prior chunk's output, so consecutive chunks build
    // on each other naturally rather than needing a single end-of-run pass.
    // Skipped under a shared-VA scope: a shared villager's soul lives in the
    // engine's actor_narrative_state.about_me (LLM-199), not a note, and a
    // namespace-root context/soul would collide across every pooled villager.
    // The shared path also passes a null soul agent, so this is defense in
    // depth for any future caller.
    if (soulAgentName && !slugPrefix) {
        try {
            let existingSoul = '';
            try {
                const soulNote = await readNote(agent.name, 'context/soul');
                existingSoul = soulNote.content || '';
            } catch (e) {
                // No soul yet — first run for this agent.
            }

            // When there's no usable prior soul, backload the N most recent
            // dreams instead of just feeding the chunk we just saved. Lets a
            // rebuilt soul come back from accumulated personality rather than
            // starting flat and slowly filling in over many cycles. "No usable
            // prior soul" means empty (deleted or first run) OR a suspiciously
            // short stub (dream_soul_min_chars) — the latter is the fingerprint
            // of a truncated/degraded write, which must not be fed back in and
            // "evolved" (it would compound; the writer reads its own prior
            // output as input). The just-saved chunk is included as the first
            // backload entry since listNotes orders by updated_at DESC. After
            // the rebuild the soul is long again, so subsequent cycles resume
            // the normal per-chunk update path. Cost guards on the soul agent
            // call protect against runaway prompt sizes.
            let soulMinChars = 0;
            try {
                soulMinChars = parseInt(config.get('dream_soul_min_chars'), 10) || 0;
            } catch (e) {
                // Config key missing (deploy ordering: service started before
                // migration ran). Treat as disabled — same contract as
                // dream_backload_count below.
                soulMinChars = 0;
            }
            let backloadDreams = null;
            // existingSoul is already a string (initialized '' and only ever
            // set from soulNote.content || ''); normalize once anyway so the
            // trimmed length is derived in one place and shared by the
            // emptiness check, the rebuild helper, and the degraded log.
            const normalizedSoul = typeof existingSoul === 'string' ? existingSoul : '';
            const trimmedSoulLen = normalizedSoul.trim().length;
            const soulIsEmpty = trimmedSoulLen === 0;
            const needsRebuild = soulNeedsRebuild(normalizedSoul, soulMinChars);
            if (needsRebuild && !soulIsEmpty) {
                // Non-empty soul below the health floor: a degraded/truncated
                // stub we're rerouting to a from-scratch rebuild. Log the
                // self-heal so it's visible in the dream journal rather than a
                // silent reroute.
                logDream('chunk-soul-degraded-rebuild', {
                    agent: agent.name,
                    chunkDate: chunkDateStr,
                    size: trimmedSoulLen,
                    minChars: soulMinChars,
                });
            }
            if (needsRebuild) {
                let backloadCount = 0;
                try {
                    backloadCount = parseInt(config.get('dream_backload_count'), 10) || 0;
                } catch (e) {
                    // Config key missing (deploy ordering: service started before
                    // migration ran). Treat as disabled rather than crash.
                    backloadCount = 0;
                }
                // Hard cap at 20 regardless of config — sanity bound on the
                // sequential read burst.
                backloadCount = Math.max(0, Math.min(backloadCount, 20));
                if (backloadCount > 0) {
                    const list = await listNotes(agent.name, backloadCount, 0, 'dreams/');
                    if (list.notes && list.notes.length > 0) {
                        const dreamReads = await Promise.all(
                            list.notes.map(n => readNote(agent.name, n.slug).catch(() => null))
                        );
                        backloadDreams = dreamReads
                            .filter(d => d && d.content)
                            .map(d => `### ${d.slug}\n\n${d.content}`)
                            .join('\n\n---\n\n');
                    }
                }
            }

            const soulUserMessage = buildSoulUserMessage({
                agentName: agent.name,
                startupInstructions: agent.startup_instructions,
                existingSoul: normalizedSoul,
                needsRebuild,
                backloadDreams,
                chunkDate: chunkDateStr,
                dreamContent: content,
            });

            const { text: updatedSoul, usage: soulUsage, truncated: soulTruncated, finish_reason: soulFinish } = await invokeAgent(soulAgentName, {
                userMessage: soulUserMessage,
                context: 'soul',
                skipRateLimit: true,
                skipCostLimit: true,
                skipRetry: false,
            });

            if (soulTruncated) {
                // Length-stop: the writer hit its token ceiling and the soul
                // came back cut off mid-thought (observed: gemini-2.5-pro's
                // thinking budget eats the output cap, leaving a partial like
                // "...Stabilize, Observe,"). The partial has non-empty text, so
                // it would sail past the checks below — saving it would poison
                // every future tick's system prompt AND compound on the next
                // dream cycle, since the writer reads its own prior output as
                // input. Skip the save; the prior soul stays intact. Same
                // contract as the reasoning-preamble guard below.
                logDream('chunk-soul-error', {
                    agent: agent.name,
                    chunkDate: chunkDateStr,
                    reason: 'truncated',
                    finishReason: soulFinish,
                    size: (updatedSoul || '').length,
                });
            } else if (updatedSoul && updatedSoul.trim()) {
                const trimmedSoul = updatedSoul.trim();
                // Reject reasoning-preamble leakage. Some chat models
                // (observed: qwen3.5-flash) ignore the "Output ONLY"
                // instruction and emit their analytical chain-of-thought
                // as plain text before the soul body. Saving that would
                // poison every future tick's system prompt AND compound
                // on the next dream cycle, since the soul-writer reads
                // its own prior output as input. Detect via leading
                // characters and skip the save so the existing soul
                // stays intact rather than being overwritten with garbage.
                // Soul content (including the preamble) is intentionally
                // kept out of the error log to avoid leaking model
                // reasoning or character state into operational logs.
                const matchedMarker = detectReasoningPreamble(trimmedSoul);
                if (matchedMarker) {
                    logDream('chunk-soul-error', {
                        agent: agent.name,
                        chunkDate: chunkDateStr,
                        reason: 'reasoning-preamble-detected',
                        marker: matchedMarker,
                        size: trimmedSoul.length,
                    });
                } else {
                    await saveNote(agent.name, 'Soul', trimmedSoul, 'context/soul', soulAgentName, null, null, { upsert: true });
                    logDream('chunk-soul-updated', { agent: agent.name, chunkDate: chunkDateStr, size: trimmedSoul.length });
                }
            } else {
                // Empty/whitespace soul response — the writer returned no
                // visible text (observed: gemini-2.5-pro spending its whole
                // budget on thinking tokens, so output_tokens > 0 but the body
                // is empty). Without this the branch skips silently: no save
                // and no log, making an empty soul response indistinguishable
                // from the step never running. Log it so it's visible in the
                // dream journal, mirroring the people path's
                // person-context-empty-response. The prior soul is left intact.
                logDream('chunk-soul-empty-response', {
                    agent: agent.name,
                    chunkDate: chunkDateStr,
                    outputTokens: soulUsage?.output_tokens ?? 0,
                });
            }
        } catch (soulErr) {
            // Soul failure doesn't block the chunk's dream/people output.
            logDream('chunk-soul-error', { agent: agent.name, chunkDate: chunkDateStr, error: soulErr.message });
        }
    }

    // People synthesis — runs whenever a people-agent is configured for the
    // dream mode (currently companion and sim). Per-chunk for the same
    // reason soul does: per-day relationship updates compose better than
    // one massive end-of-run pass over weeks of conversation.
    //
    // Skipped in notes mode: extractSpeakers parses conversation formats
    // (chat timestamps, discussion lines, sim distiller output) and would
    // misparse curated prose into junk context/people/* files — e.g. any
    // "word:" line in a journal becomes a phantom speaker. Notes-sourced
    // dreaming deliberately writes only dreams/* and context/soul.
    if (peopleAgentName && !notesMode) {
        try {
            const speakers = extractSpeakers(filtered, selfName);
            // Split each person's day into ledger vs. speech before prompting
            // (LLM-523). The ledger half is recovered from the villager's OWN
            // lines, which extractSpeakers self-skips — so a person can now have
            // material worth prompting on even when they said nothing.
            const sections = buildPersonExcerptSections(filtered, selfName, speakers);
            for (const [slug, entry] of sections) {
                const { display, ledger, said } = entry;
                if (ledger.length === 0 && said.length === 0) {
                    continue;
                }
                try {
                    const result = await runPersonContextUpdate(
                        agent.name, peopleAgentName, slug, display,
                        said.join('\n'), chunkDateStr,
                        { slugPrefix, selfLabel: selfName, ledger }
                    );
                    if (result.written) {
                        logDream('chunk-people-updated', {
                            agent: agent.name,
                            chunkDate: chunkDateStr,
                            person: display,
                            size: result.updatedFile.length,
                        });
                    }
                } catch (personErr) {
                    logDream('chunk-people-error', {
                        agent: agent.name,
                        chunkDate: chunkDateStr,
                        person: display,
                        error: personErr.message,
                    });
                }
            }
        } catch (peopleErr) {
            logDream('chunk-people-error', { agent: agent.name, chunkDate: chunkDateStr, error: peopleErr.message });
        }
    }

    // Daily learnings synthesis. Replaces the per-turn extractLearnings path
    // for sim agents, where every in-world tick is tool-use and the per-turn
    // extractor is silenced by its !isToolUse gate. Distills the day's
    // filtered conversation into a single learnings note keyed by date.
    // Skipped in notes mode for the same containment reason as people above
    // (and learnings/% is an excluded source prefix — writing it would feed
    // the next run's input).
    if (learningsAgentName && !notesMode) {
        const learningsSlug = slugPrefix + 'learnings/' + chunkDateStr + '-sim-day';
        try {
            let existingFile = '';
            try {
                const note = await readNote(agent.name, learningsSlug);
                existingFile = note.content || '';
            } catch (e) {
                // No existing learnings file for this day — first pass.
            }

            const learningsUserMessage = '## Agent: ' + selfName + '\n'
                + '## Date: ' + chunkDateStr + '\n\n'
                + (existingFile
                    ? '## Existing learnings for today (refine, integrate; do not duplicate)\n\n' + existingFile + '\n\n'
                    : '')
                + '## Day\'s conversation excerpts\n\n'
                + filtered;

            const { text: extractionResult, truncated: learningsTruncated, finish_reason: learningsFinish } = await invokeAgent(learningsAgentName, {
                userMessage: learningsUserMessage,
                context: 'learnings',
                skipRateLimit: true,
                skipCostLimit: true,
                skipRetry: false,
            });

            const trimmed = extractionResult ? extractionResult.trim() : '';
            if (learningsTruncated) {
                // Length-stop: the day's learnings synthesis was cut off. It
                // upserts (integrates with and replaces the existing note), so a
                // truncated version would drop earlier bullets. Keep the prior note.
                logDream('chunk-learnings-error', {
                    agent: agent.name,
                    chunkDate: chunkDateStr,
                    reason: 'truncated',
                    finishReason: learningsFinish,
                });
            } else if (trimmed && trimmed.toUpperCase() !== 'NONE') {
                await saveNote(
                    agent.name,
                    'Learnings — ' + chunkDateStr,
                    trimmed,
                    learningsSlug,
                    learningsAgentName,
                    null, null, { upsert: true }
                );
                logDream('chunk-learnings-updated', {
                    agent: agent.name,
                    chunkDate: chunkDateStr,
                    size: trimmed.length,
                });
            } else {
                logDream('chunk-learnings-none', { agent: agent.name, chunkDate: chunkDateStr });
            }
        } catch (learningsErr) {
            logDream('chunk-learnings-error', {
                agent: agent.name,
                chunkDate: chunkDateStr,
                error: learningsErr.message,
            });
        }
    }

    return {
        processed: true,
        chunkDate: chunkDateStr,
        slug,
        title,
        logCount: logs.rows.length,
        filteredSize: filtered.length,
        responseSize: response.length,
    };
}

// Run the per-day chunk loop for a single dreaming identity. Shared by the
// dedicated per-agent path and the shared-VA per-actor path: the only
// differences are the scope (namespace-root vs a villager slug prefix) and
// where progress is stamped, both passed in. advanceCursor(chunkTo) persists
// the cursor after each successful chunk so a later failure doesn't lose
// earlier work; a failed chunk stops this identity's remaining chunks (so we
// never skip past unprocessed logs) and the next cron retries it. Returns the
// per-chunk result array.
async function runChunkLoop(agent, agentNames, chunks, scope, advanceCursor, lock) {
    const interChunkDelay = dreamDelayMs('dream_interchunk_delay', 1000);
    const chunkResults = [];

    for (let i = 0; i < chunks.length; i++) {
        const chunk = chunks[i];
        // Outside the try on purpose: a lost lock aborts the run rather than
        // being recorded as this chunk's error. Checked here because the next
        // statements are a model call and a cursor advance — the two things
        // that must not happen while another run may be doing the same.
        throwIfRunLockLost(lock);
        try {
            const r = await processDreamChunk(agent, agentNames, chunk, scope);
            chunkResults.push(r);
            // Re-checked here because the chunk above spends minutes in a model
            // call, which is ample time for the lock's session to die. The
            // cursor write is the one operation that loses data when two runs
            // race, so it gets the check closest to it. (The model call itself
            // can't be interrupted — only the write can be withheld.)
            throwIfRunLockLost(lock);
            await advanceCursor(chunk.to);
        } catch (chunkErr) {
            // The post-model check above throws from INSIDE this try — a lost
            // lock is an aborted run, not this chunk's failure.
            if (chunkErr instanceof RunLockLostError) {
                throw chunkErr;
            }
            const chunkDate = chunk.from.toISOString().slice(0, 10);
            const errDetail = { agent: agent.name, chunkDate, error: chunkErr.message };
            if (scope.slugPrefix) {
                errDetail.prefix = scope.slugPrefix;
            }
            logDream('chunk-error', errDetail);
            logError('dream', 'chunk-error', {
                agent: agent.name,
                message: chunkErr.message,
                detail: chunkErr.stack,
            });
            chunkResults.push({ chunkDate, error: chunkErr.message });
            break;
        }
        // Inter-chunk pause for the same identity — politeness to the provider
        // when catching up multiple days back-to-back.
        if (i + 1 < chunks.length && interChunkDelay > 0) {
            await new Promise(resolve => setTimeout(resolve, interChunkDelay));
        }
    }

    return chunkResults;
}

// Validate a shared-VA roster row's slug_prefix at the dream boundary. Returns
// the canonical prefix, or null for anything that must be skipped: a
// NULL/non-string value (explicit type guard so a bad row can't throw and abort
// the roster), or a non-canonical value the distiller's normalizer rejects —
// LIKE metacharacters, path traversal, wrong case, over-length. Pure and
// exported so the roster-boundary decision is unit-testable.
function validateRosterPrefix(rowPrefix) {
    // Delegate to the throwing boundary and convert to the roster loop's
    // skip-this-row contract: null for anything invalid (non-string,
    // non-canonical, LIKE metacharacters, traversal) OR absent (''), since a
    // roster row must carry a real villager prefix.
    try {
        return resolveScopePrefix(rowPrefix) || null;
    } catch (e) {
        return null;
    }
}

// Count shared-VA actors that saw any failure this run — an actor-level error
// (invalid prefix, per-actor exception) OR a failed chunk nested inside an
// otherwise-formed actor result. A skipped chunk (no logs / no signals) is not
// a failure and is not counted. Exported so the cron's failure signal is
// unit-testable without standing up the whole pipeline.
function countFailedActors(actorResults) {
    return actorResults.filter(r =>
        r.error || (Array.isArray(r.chunks) && r.chunks.some(c => c.error))
    ).length;
}

// Dream every shared-VA villager enrolled under one pooled agent
// (dream_mode='sim-shared', e.g. salem-vendor). Each villager is a slug
// prefix in the pooled namespace — it has no actors row of its own — so it
// gets a dedicated-VA-equivalent dream/learnings/people pass scoped under its
// prefix, cursored on its own sim_shared_actor.last_dream_at. Soul is
// deliberately omitted (null soul agent + the slug-prefix guard in
// processDreamChunk): a shared villager's soul is the engine's
// actor_narrative_state.about_me (LLM-199), not a note. The roster only holds
// villagers that have landed at least one non-empty day (the distiller
// enrolls on first material), so no empty-namespace villager reaches here.
async function processSharedAgent(agent, simAgents, results, lock) {
    const { simAgentName, simPeopleAgentName, simLearningsAgentName } = simAgents;
    if (!simAgentName) {
        results.push({ agent: agent.name, mode: 'sim-shared', failedActorCount: 1, error: 'dream-sim agent not available' });
        return;
    }

    // sim-shared is conversation-sourced only. processDreamChunk's notes-mode
    // source query is NOT slug-scoped, so a misconfigured dream_source would
    // read the pooled agent's cross-villager curated notes and write them into
    // one villager's subtree — a cross-villager leak. Reject the whole agent
    // rather than process it under the wrong source.
    const dreamSource = agent.dream_source || 'conversation';
    if (dreamSource !== 'conversation') {
        logDream('shared-unsupported-source', { agent: agent.name, source: dreamSource });
        logError('dream', 'shared-unsupported-source', {
            agent: agent.name,
            message: 'dream_source must be conversation for sim-shared (got ' + dreamSource + ')',
        });
        results.push({
            agent: agent.name,
            mode: 'sim-shared',
            failedActorCount: 1,
            error: 'dream_source must be conversation for sim-shared (got ' + dreamSource + ')',
        });
        return;
    }

    const agentNames = {
        dreamAgentName: simAgentName,
        soulAgentName: null,
        peopleAgentName: simPeopleAgentName,
        learningsAgentName: simLearningsAgentName,
    };

    // Accumulate villager results by reference so an unexpected roster/setup
    // failure still preserves the villagers already processed in the single
    // aggregate result below.
    const actorResults = [];
    let setupError = null;
    let rosterSize = 0;
    try {
        rosterSize = await dreamSharedRoster(agent, agentNames, actorResults, lock);
    } catch (err) {
        if (err instanceof RunLockLostError) {
            throw err;
        }
        // A roster-query or other setup failure — record it and fall through so
        // any partial actorResults are still reported, not discarded.
        logDream('shared-agent-error', { agent: agent.name, error: err.message });
        logError('dream', 'shared-agent-error', { agent: agent.name, message: err.message, detail: err.stack });
        setupError = err.message;
    }

    if (rosterSize === 0 && actorResults.length === 0 && !setupError) {
        // Empty roster — nothing enrolled yet (a legit, non-error state).
        logDream('shared-no-roster', { agent: agent.name });
        results.push({ agent: agent.name, mode: 'sim-shared', actorCount: 0 });
        return;
    }

    // failedActorCount = how many villagers this pooled agent saw fail — an
    // actor-level error OR any failed chunk counts the villager once (actor
    // granularity; per-villager plannedChunks/completedChunks carry the finer
    // detail), plus 1 for a setup failure. Per-villager failures are also
    // written to the error_log via logError, matching the dedicated per-agent
    // contract: one bad villager never fails the whole run.
    const failedActorCount = countFailedActors(actorResults) + (setupError ? 1 : 0);
    const summary = {
        agent: agent.name,
        mode: 'sim-shared',
        actorCount: actorResults.length,
        failedActorCount,
        actors: actorResults,
    };
    if (setupError) {
        summary.error = setupError;
    }
    results.push(summary);
}

// Query the villager roster for one pooled shared-VA agent and dream each
// villager, appending one result per villager to actorResults (populated by
// reference). Separated from processSharedAgent so a roster/setup failure here
// is caught by the caller — which still emits one aggregate result preserving
// whatever villagers were already processed. Returns the roster size.
async function dreamSharedRoster(agent, agentNames, actorResults, lock) {
    const roster = await pool.query(
        `SELECT slug_prefix, display_name, last_dream_at
         FROM sim_shared_actor
         WHERE shared_actor_id = $1
         ORDER BY slug_prefix ASC`,
        [agent.actor_id]
    );
    if (roster.rows.length === 0) {
        return 0;
    }

    const interActorDelay = dreamDelayMs('dream_interagent_delay', 2000);

    for (let i = 0; i < roster.rows.length; i++) {
        const actorRow = roster.rows[i];
        const rowPrefix = actorRow.slug_prefix;
        // Declared before the try so the catch below can still reference it.
        let slugPrefix = '';
        // Before the try, so a lost lock aborts the roster instead of being
        // filed as this villager's error (see runChunkLoop).
        throwIfRunLockLost(lock);
        try {
            // Validate the roster prefix at this boundary — DB/operator-
            // controlled state, not trusted just because the distiller normally
            // writes it canonically. Kept inside the per-actor try so one bad row
            // (including a NULL/non-string slug_prefix) skips only THIS villager,
            // never aborting the rest of the roster. The canonical form drives
            // every path built below; the stored value keys the cursor UPDATE so
            // it targets the exact roster row.
            const validated = validateRosterPrefix(rowPrefix);
            if (!validated) {
                logDream('shared-actor-invalid-prefix', { agent: agent.name, prefix: rowPrefix });
                logError('dream', 'shared-actor-invalid-prefix', {
                    agent: agent.name,
                    message: 'invalid roster slug prefix: ' + String(rowPrefix),
                });
                actorResults.push({ prefix: rowPrefix, error: 'invalid slug prefix' });
                continue;
            }
            slugPrefix = validated;
            // A shared villager needs a real display name (its self-identity for
            // the self-skip + prompt labels). An empty one would fall back to the
            // pooled agent (salem-vendor) in processDreamChunk, making the
            // villager's own lines read as counterparty speech and spawning a
            // bogus self relationship note — so skip the row rather than dream it
            // under the wrong identity. (sim_shared_actor.display_name is NOT
            // NULL, and the distiller stores a validated non-empty label, so this
            // is defense against a hand-edited/corrupt row.)
            const displayName = actorRow.display_name;
            if (!(typeof displayName === 'string' && displayName.trim())) {
                logDream('shared-actor-invalid-display', { agent: agent.name, prefix: slugPrefix });
                logError('dream', 'shared-actor-invalid-display', {
                    agent: agent.name,
                    message: 'missing display name for roster prefix ' + slugPrefix,
                });
                actorResults.push({ prefix: slugPrefix, error: 'missing display name' });
                continue;
            }
            const scope = { slugPrefix, selfName: displayName };
            // First run for this villager (no cursor yet): start at its
            // earliest conversation note so a freshly-pushed backlog — an
            // LLM-515 push-cursor backfill, or a villager enrolled several days
            // before its first dream — is consolidated rather than skipped.
            // Steady state, last_dream_at carries the window forward.
            let since = actorRow.last_dream_at;
            if (!since) {
                const earliest = await pool.query(
                    `SELECT MIN(created_at) AS min_created FROM documents
                     WHERE namespace = $1 AND slug LIKE $2 AND deleted_at IS NULL`,
                    [agent.name, slugPrefix + 'conversations/%']
                );
                if (!earliest.rows[0] || !earliest.rows[0].min_created) {
                    logDream('shared-no-conversations', { agent: agent.name, prefix: slugPrefix });
                    actorResults.push({ prefix: slugPrefix, skipped: true, reason: 'no conversation notes' });
                    continue;
                }
                const minCreated = earliest.rows[0].min_created instanceof Date
                    ? earliest.rows[0].min_created
                    : new Date(earliest.rows[0].min_created);
                // Window is exclusive on `from`; back off 1ms to include the
                // earliest note itself.
                since = new Date(minCreated.getTime() - 1);
            }

            const chunks = computeDailyChunks(since, new Date());
            if (chunks.length === 0) {
                logDream('shared-no-window', { agent: agent.name, prefix: slugPrefix, since });
                actorResults.push({ prefix: slugPrefix, skipped: true, reason: 'last_dream_at is in the future' });
                continue;
            }

            logDream('shared-chunks-planned', {
                agent: agent.name,
                prefix: slugPrefix,
                count: chunks.length,
                from: chunks[0].from.toISOString(),
                to: chunks[chunks.length - 1].to.toISOString(),
            });

            const chunkResults = await runChunkLoop(
                agent, agentNames, chunks, scope,
                (chunkTo) => pool.query(
                    `UPDATE sim_shared_actor SET last_dream_at = $1
                     WHERE shared_actor_id = $2 AND slug_prefix = $3`,
                    [chunkTo, agent.actor_id, rowPrefix]
                ),
                lock
            );
            // Report planned vs completed separately — runChunkLoop stops on the
            // first failed chunk, so chunks.length alone would overstate progress
            // to cron monitoring. A chunk result carries an `error` field only
            // when it failed.
            actorResults.push({
                prefix: slugPrefix,
                plannedChunks: chunks.length,
                completedChunks: chunkResults.filter(r => !r.error).length,
                chunks: chunkResults,
            });
        } catch (actorErr) {
            if (actorErr instanceof RunLockLostError) {
                throw actorErr;
            }
            const prefix = slugPrefix || rowPrefix;
            logDream('shared-actor-error', { agent: agent.name, prefix, error: actorErr.message });
            logError('dream', 'shared-actor-error', {
                agent: agent.name,
                context: prefix,
                message: actorErr.message,
                detail: actorErr.stack,
            });
            actorResults.push({ prefix, error: actorErr.message });
        }

        // Delay between villagers — same politeness as the inter-agent delay,
        // since each villager is a full dedicated-VA-equivalent pass.
        if (i + 1 < roster.rows.length && interActorDelay > 0) {
            await new Promise(resolve => setTimeout(resolve, interActorDelay));
        }
    }

    return roster.rows.length;
}

// Losing the run lock aborts the WHOLE run, so it needs its own type: every
// loop below catches per-item errors and carries on to the next agent/villager/
// chunk, which is right for a bad roster row and wrong for a lost lock. Each of
// those catches rethrows this one.
class RunLockLostError extends Error {}

// Advisory lock key for the dream run (LLM-532). Advisory lock keys are
// arbitrary integers, but each lock needs its own and a key must never be
// reused — two jobs sharing a key would exclude each other for no reason.
// Convention for new locks: <ticket number>001. Registry for this database:
//   532001 — the dream run (LLM-532). The only advisory lock in the app.
const DREAM_RUN_LOCK_KEY = 532001;

// Take the dream-run lock. Returns a handle with release(), or null when
// another run already holds it.
//
// The lock is what stops two overlapping runs from racing on the same
// last_dream_at cursors: both would read the same cursor, plan overlapping day
// windows, and whichever wrote last would advance the cursor past days the
// other run never processed — permanently outside every future window, with no
// error raised. The nightly cron runs inside the API service process while
// operator/verification runs are separate processes, so the guard has to be
// cross-process; an in-process flag would not see the other run.
//
// Advisory rather than a `running` flag column, deliberately: Postgres drops
// the lock when the session ends, so a crashed or killed run leaves nothing
// stuck and needs no stale-lock sweep.
//
// The subtlety worth knowing: advisory locks are SESSION-scoped, and this app
// talks to Postgres through a connection pool. The lock therefore lives on a
// client checked out explicitly with pool.connect() and held for the entire
// run. Taking it via pool.query() would hand that connection straight back to
// the pool and drop the lock with it, leaving a guard that silently does
// nothing.
async function acquireDreamRunLock() {
    const client = await pool.connect();
    let lockLost = false;
    let released = false;

    // If this connection dies mid-run its session ends and Postgres releases
    // the lock — at which point another run can acquire it while this one is
    // still advancing cursors, which is exactly the race the lock exists to
    // prevent. So the listener records that the lock is GONE, and
    // throwIfRunLockLost() below turns that into an aborted run.
    //
    // It is also what keeps that event handled at all: pg's Pool REMOVES its
    // own idle error listener for as long as a client is checked out
    // (pg-pool's _acquireClient / _release pair), and an 'error' event with no
    // listener is thrown by EventEmitter — which on a checked-out connection
    // would take the API service process down, not just the run.
    function onConnectionError(err) {
        lockLost = true;
        logDream('lock-connection-error', { error: err.message });
    }
    client.on('error', onConnectionError);

    // Idempotent: a second call is a no-op rather than a second unlock on a
    // connection that has already gone back to the pool (and may by then
    // belong to someone else).
    async function release() {
        if (released) {
            return;
        }
        released = true;
        let releaseError = null;
        try {
            // AS ok is load-bearing: unaliased, Postgres names the column
            // after the function, and every successful unlock would read as
            // "not held".
            const result = await client.query('SELECT pg_advisory_unlock($1) AS ok', [DREAM_RUN_LOCK_KEY]);
            if (!result.rows[0] || result.rows[0].ok !== true) {
                // false means this session did not hold the lock — it was lost
                // mid-run. Nothing to clean up, but it explains how a second run
                // could have started, so it must not pass silently.
                lockLost = true;
                logDream('lock-release-not-held', {});
            }
        } catch (err) {
            logDream('lock-release-error', { error: err.message });
            releaseError = err;
        }
        // Removed only now, so a connection error DURING the unlock is still
        // reported as lock-connection-error rather than swallowed.
        client.removeListener('error', onConnectionError);
        // A truthy argument makes pg destroy the connection instead of
        // returning it to the pool: the session state after a failed unlock is
        // unknown, and ending the session is itself a guaranteed lock release.
        client.release(releaseError);
    }

    try {
        const result = await client.query('SELECT pg_try_advisory_lock($1) AS ok', [DREAM_RUN_LOCK_KEY]);
        if (result.rows[0] && result.rows[0].ok === true) {
            return { lost: () => lockLost, release };
        }
        // Not granted: nothing to unlock, and the connection is healthy, so it
        // goes back to the pool intact.
        client.removeListener('error', onConnectionError);
        client.release();
        return null;
    } catch (err) {
        client.removeListener('error', onConnectionError);
        // The acquisition statement failed, so this connection is suspect —
        // destroy it rather than hand a possibly-broken one to the next
        // borrower.
        client.release(err);
        throw err;
    }
}

// Abort the run if its lock is gone. Called at every point where the run is
// about to start a new unit of work or write a cursor: once the lock is lost a
// second run can be underway, and two runs racing on last_dream_at is what
// silently loses a day. Failing loudly here is the point — a run that keeps
// going without its lock is the bug this ticket fixes, wearing a lock's
// clothes. It cannot interrupt an in-flight query, only stop the next one.
//
// The exact guarantee, so nobody reads more into it than it gives: NO further
// unit of work or cursor write is started after a lock loss THIS PROCESS HAS
// OBSERVED. The check and the cursor write are not atomic with lock ownership
// — a session that dies in the microseconds between them still lets that one
// write through. Making that impossible would mean issuing the cursor UPDATE
// on the lock-holding client itself (a write on the session that holds the
// lock either happens under the lock or fails with the session), which is a
// larger change than the guard and is deliberately not done here.
function throwIfRunLockLost(lock) {
    if (lock && lock.lost()) {
        throw new RunLockLostError('dream run lock lost: its Postgres session ended mid-run, so another run may already hold the lock');
    }
}

// Run the dream processing job, serialized against any other run (see
// acquireDreamRunLock). Returns a summary object with counts and any errors.
async function runDream() {
    // Check global switch. Before the lock: a disabled run has nothing to
    // exclude and shouldn't check out a connection to discover that.
    if (config.get('dream_processing_enabled') !== 'true') {
        logDream('skip', { reason: 'dream_processing_enabled is false' });
        return { skipped: true, reason: 'disabled' };
    }

    const lock = await acquireDreamRunLock();
    if (!lock) {
        logDream('skip', { reason: 'another dream run holds the lock' });
        return { skipped: true, reason: 'already running' };
    }

    try {
        const result = await runDreamAgents(lock);
        // A loss between the last work boundary and here would otherwise be
        // reported as a clean run. Any observed loss fails the run: the cursors
        // this run wrote may have raced another run's.
        throwIfRunLockLost(lock);
        return result;
    } finally {
        await lock.release();
    }
}

// The run itself — every cursor read and write below is serialized by the
// caller's run lock, and aborts if that lock is lost mid-run.
async function runDreamAgents(lock) {
    // Find dream agents by expertise tag
    const companionAgentName = await findDreamAgent('dream-companion');
    const technicalAgentName = await findDreamAgent('dream-technical');
    const simAgentName = await findDreamAgent('dream-sim');
    const companionSoulAgentName = await findDreamAgent('dream-companion-soul');
    const technicalSoulAgentName = await findDreamAgent('dream-technical-soul');
    const simSoulAgentName = await findDreamAgent('dream-sim-soul');
    const companionPeopleAgentName = await findDreamAgent('dream-companion-people');
    const simPeopleAgentName = await findDreamAgent('dream-sim-people');
    const simLearningsAgentName = await findDreamAgent('dream-sim-learnings');

    if (!companionAgentName && !technicalAgentName && !simAgentName) {
        logDream('abort', { reason: 'No dream agent found or valid' });
        return { error: 'No valid dream agents found. At least one of dream-companion, dream-technical, or dream-sim must exist and be created by a trusted creator.' };
    }

    // Find agents with dream mode enabled. sim-shared (pooled salem-vendor-
    // style agents) is included but handled on a separate per-villager path
    // below — it fans out over the sim_shared_actor roster instead of dreaming
    // one identity across the whole namespace.
    const agents = await pool.query(
        `SELECT ac.name, ac.id AS actor_id, agc.dream_mode, agc.dream_source,
                agc.last_dream_at, agc.startup_instructions
         FROM agent_configuration agc
         JOIN actors ac ON ac.id = agc.actor_id
         WHERE agc.dream_mode IN ('companion', 'technical', 'sim', 'sim-shared')`
    );

    if (agents.rows.length === 0) {
        logDream('skip', { reason: 'No agents with dream mode enabled' });
        return { processed: 0, reason: 'No agents with dream mode enabled' };
    }

    logDream('start', { agents: agents.rows.map(a => a.name + ':' + a.dream_mode) });

    const results = [];

    for (const agent of agents.rows) {
        // Before the try: a lost lock ends the run rather than being recorded
        // as this agent's error (see runChunkLoop).
        throwIfRunLockLost(lock);
        try {
            if (agent.dream_mode === 'sim-shared') {
                // Pooled shared-VA agent: fan out over its villager roster on a
                // separate per-actor path (own cursors, slug-prefixed notes, no
                // soul). The helper paces itself with an inter-villager delay,
                // so skip straight to the next agent afterward.
                await processSharedAgent(
                    agent,
                    { simAgentName, simPeopleAgentName, simLearningsAgentName },
                    results,
                    lock
                );
                continue;
            }

            // Pick the right dream/soul/people/learnings agents for this dream_mode.
            let dreamAgentName = null;
            let soulAgentName = null;
            let peopleAgentName = null;
            let learningsAgentName = null;
            if (agent.dream_mode === 'companion') {
                dreamAgentName = companionAgentName;
                soulAgentName = companionSoulAgentName;
                peopleAgentName = companionPeopleAgentName;
            } else if (agent.dream_mode === 'technical') {
                dreamAgentName = technicalAgentName;
                soulAgentName = technicalSoulAgentName;
            } else if (agent.dream_mode === 'sim') {
                dreamAgentName = simAgentName;
                soulAgentName = simSoulAgentName;
                peopleAgentName = simPeopleAgentName;
                learningsAgentName = simLearningsAgentName;
            }
            if (!dreamAgentName) {
                results.push({ agent: agent.name, error: 'dream-' + agent.dream_mode + ' agent not available' });
                continue;
            }
            const agentNames = { dreamAgentName, soulAgentName, peopleAgentName, learningsAgentName };

            // Split the work since last_dream_at into per-UTC-day chunks so an
            // agent that's fallen behind doesn't try to fit weeks of logs into
            // one model call (which is what tripped home with deepseek's 163K
            // window). First-run agents process the previous 24h — except in
            // notes mode, where the first run starts at the earliest curated
            // note so the soul accretes over the full history in authored
            // order. (Day-chunks with no notes skip cheaply, so a long span
            // costs only the days that actually have material.)
            let since = agent.last_dream_at;
            if (!since && agent.dream_source === 'notes') {
                const earliest = await pool.query(
                    `SELECT MIN(updated_at) AS min_updated FROM documents
                     WHERE namespace = $1 AND deleted_at IS NULL
                     AND slug NOT LIKE 'conversations/%'
                     AND slug NOT LIKE 'dreams/%'
                     AND slug NOT LIKE 'context/%'
                     AND slug NOT LIKE 'learnings/%'`,
                    [agent.name]
                );
                if (!earliest.rows[0] || !earliest.rows[0].min_updated) {
                    logDream('no-notes', { agent: agent.name });
                    results.push({ agent: agent.name, skipped: true, reason: 'dream_source=notes but namespace has no source notes' });
                    continue;
                }
                // The chunk window is exclusive on `from` (updated_at > from),
                // so back off 1ms to include the earliest note itself.
                // Coerce defensively — a custom pg type parser could hand
                // back a string instead of a Date (same guard buildNotesLog
                // applies to updated_at).
                const minUpdated = earliest.rows[0].min_updated instanceof Date
                    ? earliest.rows[0].min_updated
                    : new Date(earliest.rows[0].min_updated);
                since = new Date(minUpdated.getTime() - 1);
            }
            if (!since) {
                since = new Date(Date.now() - 24 * 60 * 60 * 1000);
            }
            const chunks = computeDailyChunks(since, new Date());

            if (chunks.length === 0) {
                logDream('no-window', { agent: agent.name, since });
                results.push({ agent: agent.name, skipped: true, reason: 'last_dream_at is in the future' });
                continue;
            }

            logDream('chunks-planned', {
                agent: agent.name,
                count: chunks.length,
                from: chunks[0].from.toISOString(),
                to: chunks[chunks.length - 1].to.toISOString(),
            });

            // Dedicated agent: one identity across the whole namespace, cursor
            // on agent_configuration.last_dream_at.
            const chunkResults = await runChunkLoop(
                agent, agentNames, chunks,
                { slugPrefix: '', selfName: agent.name },
                (chunkTo) => pool.query(
                    'UPDATE agent_configuration SET last_dream_at = $1 WHERE actor_id = $2',
                    [chunkTo, agent.actor_id]
                ),
                lock
            );

            results.push({
                agent: agent.name,
                mode: agent.dream_mode,
                chunkCount: chunks.length,
                chunks: chunkResults,
            });
        } catch (err) {
            if (err instanceof RunLockLostError) {
                throw err;
            }
            logDream('error', { agent: agent.name, error: err.message });
            // Also surface in the admin error_log so per-agent failures aren't
            // silently swallowed (the outer cron-level catch only fires if
            // runDream itself throws, not for individual agents).
            logError('dream', 'agent-error', {
                agent: agent.name,
                message: err.message,
                detail: err.stack,
            });
            // A throw out of processSharedAgent (e.g. the roster query failing
            // before any actor result is recorded) must still register as a
            // shared failure so the run-level failedSharedActorCount counts it.
            const failureResult = { agent: agent.name, error: err.message };
            if (agent.dream_mode === 'sim-shared') {
                failureResult.mode = 'sim-shared';
                failureResult.failedActorCount = 1;
            }
            results.push(failureResult);
        }

        // Delay between agents to avoid hammering the provider
        const interDelay = dreamDelayMs('dream_interagent_delay', 2000);
        if (interDelay > 0) {
            await new Promise(resolve => setTimeout(resolve, interDelay));
        }
    }

    // Aggregate a run-level failure signal for shared-VA dreaming so the
    // scheduler can record a persistent-failure run rather than a clean
    // 'complete'. Scoped to sim-shared deliberately: the dedicated per-agent
    // contract (capture errors into results, resolve the run) is unchanged —
    // widening the whole cron's success semantics is a separate ticket.
    const failedSharedActorCount = results
        .filter(r => r.mode === 'sim-shared')
        .reduce((sum, r) => sum + (r.failedActorCount || 0), 0);

    logDream('complete', { processed: results.length, failedSharedActorCount });
    return { processed: results.length, failedSharedActorCount, results };
}

// Map a runDream result to the scheduler's log events. The completion event
// carries the run status ('completed-with-errors' when any shared-VA actor
// failed, else 'ok') — that status field IS the authoritative, self-contained
// failure signal, so a consumer reading the completion record never needs to
// race it against a separate event. When there are failures a shared-failures
// event is also emitted, which the scheduler records in the error_log table for
// monitoring (best-effort/fire-and-forget, like every other error record — no
// cross-event persistence ordering is relied upon). Pure + exported so the
// status mapping is unit-testable without the cron runtime.
function planCronReport(result) {
    const sharedFailures = result ? (result.failedSharedActorCount || 0) : 0;
    const events = [];
    if (sharedFailures > 0) {
        events.push({ kind: 'shared-failures', count: sharedFailures });
    }
    events.push({ kind: 'complete', status: sharedFailures > 0 ? 'completed-with-errors' : 'ok' });
    return events;
}

// Start the dream scheduler. Reads dream_cron_schedule from config
// and schedules runDream() accordingly. Called once at server startup.
let scheduledTask = null;

function startDreamScheduler() {
    const cron = require('node-cron');
    const schedule = config.get('dream_cron_schedule') || '';

    if (!schedule) {
        logDream('scheduler', { message: 'No dream_cron_schedule configured, scheduler disabled' });
        return;
    }

    if (!cron.validate(schedule)) {
        logDream('scheduler-error', { message: 'Invalid cron expression: ' + schedule });
        return;
    }

    // Stop any existing scheduled task (in case of hot reload)
    if (scheduledTask) {
        scheduledTask.stop();
    }

    scheduledTask = cron.schedule(schedule, async () => {
        logDream('cron-trigger', { schedule });
        try {
            const result = await runDream();
            // Emit the mapped events (see planCronReport): a completion event
            // whose `status` field self-describes the run — so a shared-actor
            // failure is never a clean-looking completion — plus, on failure, an
            // error_log record for monitoring. No cross-event persistence
            // ordering is assumed; the status lives in the completion record
            // itself. runDream still resolves (one bad villager never blocks the
            // others).
            for (const ev of planCronReport(result)) {
                if (ev.kind === 'shared-failures') {
                    logError('dream', 'cron-shared-actor-failures', {
                        message: ev.count + ' shared-VA actor(s) failed this dream run',
                    });
                } else {
                    logDream('cron-complete', { result, status: ev.status });
                }
            }
        } catch (err) {
            logDream('cron-error', { error: err.message });
            logError('dream', 'cron-error', { message: err.message, detail: err.stack });
        }
    });

    logDream('scheduler', { message: 'Dream scheduler started', schedule });
}

module.exports = { runDream, prefilterLog, extractSpeakers, buildPersonExcerptSections, buildPersonUserMessage, buildNotesLog, startDreamScheduler, runPersonContextUpdate, findDreamAgent, detectReasoningPreamble, soulNeedsRebuild, buildSoulUserMessage, countFailedActors, peopleNotePath, validateRosterPrefix, resolveScopePrefix, planCronReport, dreamDelayMs };
