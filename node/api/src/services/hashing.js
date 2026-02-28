// Centralized hashing for credentials (passphrases, API keys, tokens).
// Wraps the algorithm so it can be swapped without touching callsites.

const crypto = require('crypto');

const ALGORITHM = 'pbkdf2';
const PBKDF2_ITERATIONS = 100000;
const PBKDF2_KEY_LENGTH = 64;
const PBKDF2_DIGEST = 'sha512';
const SALT_BYTES = 32;

function generateSalt() {
    return crypto.randomBytes(SALT_BYTES).toString('hex');
}

function hash(plaintext, salt) {
    return crypto.pbkdf2Sync(plaintext, salt, PBKDF2_ITERATIONS, PBKDF2_KEY_LENGTH, PBKDF2_DIGEST).toString('hex');
}

function verify(plaintext, salt, expectedHash) {
    const computed = hash(plaintext, salt);
    return crypto.timingSafeEqual(Buffer.from(computed, 'hex'), Buffer.from(expectedHash, 'hex'));
}

function generateKey() {
    return crypto.randomBytes(32).toString('hex');
}

module.exports = { generateSalt, hash, verify, generateKey };
