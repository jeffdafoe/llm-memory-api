// Tests for the direct-chat history scope (LLM-687): the history_scope request
// field, the scene-scope decision and the fail-closed history load. Run with:
// node --test (from node/api). Uses the built-in node:test runner + node:assert,
// matching virtual-agent-rate-limit.test.js.

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { parseHistoryScope, isSceneScopedHistory, historyLoadFor } = require('./virtual-agent-history-scope');

const SCENE = '0b6f3c9e-2d1a-4c5b-9e8f-7a6b5c4d3e2f';

test('history_scope absent or null parses to null', () => {
    assert.deepEqual(parseHistoryScope(undefined), { value: null });
    assert.deepEqual(parseHistoryScope(null), { value: null });
});

test('history_scope "scene" parses to "scene"', () => {
    assert.deepEqual(parseHistoryScope('scene'), { value: 'scene' });
});

test('any other history_scope value is an error', () => {
    for (const raw of ['agent-pair', 'Scene', ' scene', '', 1, true, {}, ['scene']]) {
        const parsed = parseHistoryScope(raw);
        assert.ok(parsed.error, 'expected an error for ' + JSON.stringify(raw));
        assert.equal(parsed.value, undefined);
    }
});

test('a request for scene scope scopes any agent', () => {
    assert.equal(isSceneScopedHistory('zbbs-new-switchboard', 'scene', false), true);
});

test('a stateful NPC with no history_scope keeps agent-pair history', () => {
    assert.equal(isSceneScopedHistory('zbbs-prudence-ward', null, false), false);
});

test('the shared VAs stay scene-scoped when the caller sends no history_scope', () => {
    for (const agent of ['salem-visitor', 'salem-vendor', 'salem-generic']) {
        assert.equal(isSceneScopedHistory(agent, null, false), true, agent);
    }
});

test('the court stays scene-scoped when the caller sends no history_scope', () => {
    assert.equal(isSceneScopedHistory('salem-magistrate', null, true), true);
});

test('a scene-scoped turn with a scene_id loads that scene only', () => {
    assert.deepEqual(historyLoadFor(true, SCENE), { skip: false, sceneId: SCENE });
});

test('a scene-scoped turn with no scene_id gets empty history, never the unscoped window', () => {
    assert.deepEqual(historyLoadFor(true, null), { skip: true, sceneId: null });
    assert.deepEqual(historyLoadFor(true, undefined), { skip: true, sceneId: null });
    assert.deepEqual(historyLoadFor(true, ''), { skip: true, sceneId: null });
});

test('an agent-pair turn loads unscoped history even when it carries a scene_id', () => {
    assert.deepEqual(historyLoadFor(false, SCENE), { skip: false, sceneId: null });
    assert.deepEqual(historyLoadFor(false, null), { skip: false, sceneId: null });
});
