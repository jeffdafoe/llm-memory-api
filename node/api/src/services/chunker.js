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

module.exports = { chunkByHeading };
