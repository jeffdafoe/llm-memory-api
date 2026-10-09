// Tests for the public OAuth client rules (LLM-733). Run with: node --test
// (from node/api). Config is stubbed for the signing secret.

const { test } = require('node:test');
const assert = require('node:assert/strict');

const config = require('./config');
config.get = (key) => (key === 'mcp_oauth_bearer_secret' ? 'test-secret' : undefined);

const {
    ALLOWED_REDIRECT_URIS,
    isCimdClientId,
    issueDcrClientId,
    verifyDcrClientId,
    publicClientKind
} = require('./oauth-public-clients');

const CALLBACK = ALLOWED_REDIRECT_URIS[0];

test('CIMD client_ids: https on an Anthropic origin, with a path, nothing else', () => {
    assert.equal(isCimdClientId('https://claude.ai/oauth/mcp-oauth-client-metadata'), true);
    assert.equal(isCimdClientId('https://claude.com/x'), true);
    for (const id of [
        'https://claude.ai/',
        'https://claude.ai',
        'http://claude.ai/x',
        'https://claude.ai:8443/x',
        'https://evil@claude.ai/x',
        'https://claude.ai/x#frag',
        'https://claude.ai.evil.example/x',
        'https://evil.example/claude.ai/x',
        'remi',
        '',
        undefined
    ]) {
        assert.equal(isCimdClientId(id), false, String(id));
    }
});

test('a DCR client_id round-trips', () => {
    const id = issueDcrClientId([CALLBACK], 1760000000);
    assert.deepEqual(verifyDcrClientId(id), { redirectUris: [CALLBACK], issuedAt: 1760000000 });
    assert.equal(publicClientKind(id), 'dcr');
});

test('a tampered DCR client_id is refused', () => {
    const id = issueDcrClientId([CALLBACK], 1760000000);
    const [prefix, payload, signature] = id.split('.');
    assert.equal(verifyDcrClientId(`${prefix}.${payload}.${signature.slice(0, -1)}A`), null);
    assert.equal(verifyDcrClientId(`${prefix}.${payload}x.${signature}`), null);
    assert.equal(verifyDcrClientId(`${prefix}.${payload}`), null);
    assert.equal(verifyDcrClientId(id.replace('dcr.', 'xyz.')), null);
});

test('even a validly signed payload must match the registration format', () => {
    // issueDcrClientId signs whatever it is given, standing in for a future
    // issuer bug or a leaked key; the verifier still refuses these.
    const bad = [
        [['https://evil.example/cb'], 1],
        [[], 1],
        [[CALLBACK, CALLBACK], 1],
        [[...ALLOWED_REDIRECT_URIS, 'https://claude.ai/other'], 1],
        [[CALLBACK], -1],
        [[CALLBACK], 1.5],
        [[CALLBACK], '1'],
        [[CALLBACK], undefined],
        [[42], 1]
    ];
    for (const [uris, issuedAt] of bad) {
        assert.equal(verifyDcrClientId(issueDcrClientId(uris, issuedAt)), null, JSON.stringify([uris, issuedAt]));
    }
});
