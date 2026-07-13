// A markdown H1–H3 heading line. Same test used to split sections and to
// detect a section that carries no prose.
function isHeadingLine(line) {
    return /^#{1,3}\s/.test(line);
}

// True when `lines` holds at least one non-blank line that is NOT a heading —
// i.e. real prose under the heading(s), not just stacked heading lines.
function hasProse(lines) {
    return lines.some(l => l.trim() !== '' && !isHeadingLine(l));
}

function chunkByHeading(content) {
    const lines = content.split('\n');
    const chunks = [];
    let currentHeading = null;
    let currentLines = [];

    // Emit the accumulated section, but ONLY if it has prose under its heading.
    // A heading with no body — e.g. a note whose "# Title" line is immediately
    // followed by a "## Section" line — would otherwise become a chunk whose
    // chunk_text is just the title. That title-only chunk is short and on-topic,
    // so it out-ranks the note's real body chunks in search, and the recall tool
    // strips its heading and renders an empty "— Title —" with no content
    // (observed for the sim NPC dream notes, which are all "# A Day of … at the
    // Inn" followed straight by "## Notable scenes"). Dropping it lets the body
    // sections be the hits. The whole-note fallback below keeps a heading-only
    // note searchable.
    const flush = () => {
        if (currentLines.length === 0) {
            return;
        }
        const text = currentLines.join('\n').trim();
        if (text && hasProse(currentLines)) {
            chunks.push({
                heading: currentHeading,
                chunk_text: text
            });
        }
    };

    for (const line of lines) {
        if (isHeadingLine(line)) {
            flush();
            currentHeading = line.trim();
            currentLines = [line];
        } else {
            currentLines.push(line);
        }
    }
    flush();

    // Fallback: a note built entirely of headings with no prose produces no
    // chunks above and would become unsearchable. Preserve findability by
    // emitting the whole trimmed content as one chunk (its heading text is all
    // it has to match on).
    if (chunks.length === 0) {
        const text = content.trim();
        if (text) {
            const firstHeading = lines.find(isHeadingLine);
            chunks.push({
                heading: firstHeading ? firstHeading.trim() : null,
                chunk_text: text
            });
        }
    }

    return chunks;
}

// Chunk conversation logs by message windows.
// Conversation format: lines like "[HH:MM speaker] text"
// Splits into windows of `windowSize` messages with `overlap` messages carried
// over between chunks for context continuity. The header block (everything
// before the first message line) is prepended to each chunk. If `maxChars`
// is set, the window closes early when accumulated text exceeds that limit.
function chunkConversation(content, windowSize, overlap, maxChars) {
    windowSize = windowSize || 5;
    overlap = overlap || 2;
    maxChars = maxChars || 0; // 0 = no char limit

    const lines = content.split('\n');
    const messagePattern = /^\[\d{2}:\d{2}\s/;

    // Separate header (session metadata, --- divider) from message lines
    const headerLines = [];
    const messageLines = [];
    let pastHeader = false;

    for (const line of lines) {
        if (!pastHeader && !messagePattern.test(line)) {
            headerLines.push(line);
        } else {
            pastHeader = true;
            messageLines.push(line);
        }
    }

    // Group into individual messages (a message may span multiple lines)
    const messages = [];
    let currentMsg = [];

    for (const line of messageLines) {
        if (messagePattern.test(line) && currentMsg.length > 0) {
            messages.push(currentMsg.join('\n'));
            currentMsg = [line];
        } else {
            currentMsg.push(line);
        }
    }
    if (currentMsg.length > 0) {
        messages.push(currentMsg.join('\n'));
    }

    if (messages.length === 0) {
        // No messages found — fall back to single chunk
        const text = content.trim();
        if (text) {
            return [{ heading: null, chunk_text: text }];
        }
        return [];
    }

    const header = headerLines.join('\n').trim();
    const chunks = [];
    let start = 0;

    while (start < messages.length) {
        // Build window: take up to windowSize messages, but stop early if
        // accumulated text exceeds maxChars (produces more focused embeddings).
        let end = Math.min(start + windowSize, messages.length);
        if (maxChars > 0) {
            let charCount = 0;
            for (let i = start; i < end; i++) {
                charCount += messages[i].length;
                if (charCount > maxChars && i > start) {
                    end = i;
                    break;
                }
            }
        }
        const windowMessages = messages.slice(start, end);

        // Build chunk text with header for context
        const parts = [];
        if (header) {
            parts.push(header);
            parts.push('');
        }
        parts.push(windowMessages.join('\n'));

        const chunkText = parts.join('\n').trim();
        if (chunkText) {
            // Extract a heading from the first message's timestamp for reference
            const firstLine = windowMessages[0] || '';
            const timeMatch = firstLine.match(/^\[(\d{2}:\d{2})/);
            const heading = timeMatch ? 'Conversation at ' + timeMatch[1] : null;

            chunks.push({
                heading: heading,
                chunk_text: chunkText
            });
        }

        // Advance by actual window size minus overlap, but always advance at least 1.
        // Uses (end - start) instead of windowSize because the char limit may have
        // closed the window early.
        const actualWindow = end - start;
        const step = Math.max(1, actualWindow - overlap);
        start += step;
    }

    return chunks;
}

module.exports = { chunkByHeading, chunkConversation };
