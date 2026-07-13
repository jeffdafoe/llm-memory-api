// Run with: node --test (from node/api). Uses the built-in node:test runner.
//
// Covers chunkByHeading — in particular the rule that a heading with no prose
// under it must NOT become its own chunk (the title-only chunk that used to
// out-rank real body sections in search and render as an empty "— Title —" in
// the recall tool).

const { test } = require('node:test');
const assert = require('node:assert');
const { chunkByHeading } = require('./chunker');

test('an H1 title immediately followed by an H2 does not emit a title-only chunk', () => {
    // This is the sim NPC dream-note shape: "# A Day …" straight into a section
    // heading with no prose between them.
    const content = [
        '# A Day of Trade and Hospitality at the Inn',
        '',
        '## Notable scenes',
        'Hannah had a series of conversations with the villagers.',
        '',
        '## Decisions',
        'Hannah decided to trade flour for nails.',
    ].join('\n');

    const chunks = chunkByHeading(content);

    // No chunk should be just the title line.
    for (const c of chunks) {
        assert.notStrictEqual(
            c.chunk_text.trim(),
            '# A Day of Trade and Hospitality at the Inn',
            'title-only chunk must not be emitted'
        );
        assert.ok(
            /[a-z]/.test(c.chunk_text.replace(/^#{1,3}\s.*$/gm, '')),
            `chunk must carry prose, got: ${JSON.stringify(c.chunk_text)}`
        );
    }

    // The two real sections survive as chunks with their bodies.
    assert.strictEqual(chunks.length, 2);
    assert.strictEqual(chunks[0].heading, '## Notable scenes');
    assert.match(chunks[0].chunk_text, /series of conversations/);
    assert.strictEqual(chunks[1].heading, '## Decisions');
    assert.match(chunks[1].chunk_text, /trade flour for nails/);
});

test('a normal multi-section note keeps one chunk per section', () => {
    const content = [
        '## Intro',
        'First paragraph.',
        '',
        '## Body',
        'Second paragraph.',
    ].join('\n');

    const chunks = chunkByHeading(content);

    assert.strictEqual(chunks.length, 2);
    assert.strictEqual(chunks[0].heading, '## Intro');
    assert.match(chunks[0].chunk_text, /First paragraph/);
    assert.strictEqual(chunks[1].heading, '## Body');
    assert.match(chunks[1].chunk_text, /Second paragraph/);
});

test('an H1 with prose directly under it (no subheading) is one chunk', () => {
    const content = [
        '# Title',
        'Prose right under the title.',
    ].join('\n');

    const chunks = chunkByHeading(content);

    assert.strictEqual(chunks.length, 1);
    assert.strictEqual(chunks[0].heading, '# Title');
    assert.match(chunks[0].chunk_text, /Prose right under the title/);
});

test('content with no headings becomes a single null-heading chunk', () => {
    const content = 'Just a paragraph of prose with no headings at all.';

    const chunks = chunkByHeading(content);

    assert.strictEqual(chunks.length, 1);
    assert.strictEqual(chunks[0].heading, null);
    assert.strictEqual(chunks[0].chunk_text, content);
});

test('a trailing heading with no body under it is dropped', () => {
    const content = [
        '## Real section',
        'Has content.',
        '',
        '## Empty trailer',
    ].join('\n');

    const chunks = chunkByHeading(content);

    assert.strictEqual(chunks.length, 1);
    assert.strictEqual(chunks[0].heading, '## Real section');
    assert.match(chunks[0].chunk_text, /Has content/);
});

test('a note made entirely of headings stays searchable via the fallback', () => {
    // No prose anywhere — must not vanish from the index.
    const content = [
        '# Title Only',
        '## Section A',
        '## Section B',
    ].join('\n');

    const chunks = chunkByHeading(content);

    assert.strictEqual(chunks.length, 1);
    assert.strictEqual(chunks[0].heading, '# Title Only');
    assert.strictEqual(chunks[0].chunk_text, content.trim());
});

test('empty / whitespace-only content produces no chunks', () => {
    assert.deepStrictEqual(chunkByHeading(''), []);
    assert.deepStrictEqual(chunkByHeading('   \n\n  '), []);
});
