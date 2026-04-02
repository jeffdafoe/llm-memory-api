function chunkByHeading(content) {
    const lines = content.split('\n');
    const chunks = [];
    let currentHeading = null;
    let currentLines = [];

    for (const line of lines) {
        if (line.match(/^#{1,3}\s/)) {
            if (currentLines.length > 0) {
                const text = currentLines.join('\n').trim();
                if (text) {
                    chunks.push({
                        heading: currentHeading,
                        chunk_text: text
                    });
                }
            }
            currentHeading = line.trim();
            currentLines = [line];
        } else {
            currentLines.push(line);
        }
    }

    if (currentLines.length > 0) {
        const text = currentLines.join('\n').trim();
        if (text) {
            chunks.push({
                heading: currentHeading,
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
