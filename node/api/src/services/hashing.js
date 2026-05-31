// Centralized hashing for credentials (passphrases, API keys, tokens).
// Wraps the algorithm so it can be swapped without touching callsites.

const crypto = require('crypto');
const { promisify } = require('util');

// Async (threadpool) PBKDF2. The sync variant (pbkdf2Sync) runs the whole
// ~30ms key derivation ON the single event-loop thread, so any burst of auth
// calls — or one pathological per-row scan — freezes every other request for
// the duration. crypto.pbkdf2 runs on libuv's threadpool instead, keeping the
// loop free. Consequence: hash() and verify() are now async and EVERY caller
// must `await` them (a missing await yields a truthy Promise — an auth bypass).
const pbkdf2Async = promisify(crypto.pbkdf2);

const ALGORITHM = 'pbkdf2';
const PBKDF2_ITERATIONS = 100000;
const PBKDF2_KEY_LENGTH = 64;
const PBKDF2_DIGEST = 'sha512';
const SALT_BYTES = 32;

function generateSalt() {
    return crypto.randomBytes(SALT_BYTES).toString('hex');
}

async function hash(plaintext, salt) {
    const derived = await pbkdf2Async(plaintext, salt, PBKDF2_ITERATIONS, PBKDF2_KEY_LENGTH, PBKDF2_DIGEST);
    return derived.toString('hex');
}

async function verify(plaintext, salt, expectedHash) {
    const computed = await hash(plaintext, salt);
    const computedBuf = Buffer.from(computed, 'hex');
    const expectedBuf = Buffer.from(expectedHash || '', 'hex');
    // timingSafeEqual throws on differing buffer lengths, so a corrupted or
    // truncated stored hash would turn every auth/session check into a 500.
    // A length mismatch can't be a valid match anyway — fail closed.
    if (computedBuf.length !== expectedBuf.length) {
        return false;
    }
    return crypto.timingSafeEqual(computedBuf, expectedBuf);
}

function generateKey() {
    return crypto.randomBytes(32).toString('hex');
}

// Deterministic SHA-256 hex of a token, used as an indexed lookup key
// for session validation. The slow PBKDF2 hash above is for at-rest
// protection (DB exfiltration → can't recover tokens); the fast lookup
// hash is for finding the candidate row without iterating PBKDF2 over
// every session. After the indexed SELECT returns a row, callers still
// run the PBKDF2 verify on it as the authoritative check.
//
// Plain SHA-256 (no salt) is intentional: tokens are 48 bytes of CSPRNG
// output, so they have ~384 bits of entropy and don't need a salt to
// resist precomputation. Any salt would defeat the indexed-lookup
// purpose since it would be per-row.
function tokenLookupHash(token) {
    return crypto.createHash('sha256').update(token).digest('hex');
}

module.exports = { generateSalt, hash, verify, generateKey, tokenLookupHash };
