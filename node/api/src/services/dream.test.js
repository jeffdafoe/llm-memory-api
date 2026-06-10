// Tests for buildNotesLog — the notes-mode (dream_source=notes, MEM-137)
// source-text assembler in the dream cron. Run with: node --test (from
// node/api). Uses the built-in node:test runner + node:assert, matching
// sim-conversation-distiller.test.js.
//
// buildNotesLog is the only new pure surface of ZBBS-WORK-391; the sourcing
// branch and first-run window live in SQL + cron flow and are exercised at
// deploy (the conversation-mode path is untouched by the change).

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { buildNotesLog } = require('./dream');

test('single note gets a slug+date header above its content', () => {
    const rows = [{
        slug: 'journal/2026-05-california-trip',
        content: 'We drove the coast road.',
        updated_at: new Date('2026-05-18T14:22:00Z'),
    }];
    assert.equal(
        buildNotesLog(rows),
        '## Note: journal/2026-05-california-trip (updated 2026-05-18)\n\nWe drove the coast road.'
    );
});

test('multiple notes are separated and keep their query order', () => {
    const rows = [
        { slug: 'core-identity', content: 'A', updated_at: new Date('2026-05-16T08:00:00Z') },
        { slug: 'thea-core', content: 'B', updated_at: new Date('2026-05-16T19:30:00Z') },
    ];
    assert.equal(
        buildNotesLog(rows),
        '## Note: core-identity (updated 2026-05-16)\n\nA'
        + '\n\n---\n\n'
        + '## Note: thea-core (updated 2026-05-16)\n\nB'
    );
});

test('string updated_at (non-Date driver output) still renders the date', () => {
    const rows = [{
        slug: 'sirius-identity',
        content: 'C',
        updated_at: '2026-06-06T03:15:00.000Z',
    }];
    assert.equal(
        buildNotesLog(rows),
        '## Note: sirius-identity (updated 2026-06-06)\n\nC'
    );
});
