// Package auth verifies the three kinds of credential the server accepts:
// session tokens (agents and admin web sessions), API keys, and the HMAC
// bearer tokens issued to MCP clients.
package auth

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
)

// The PBKDF2 parameters every stored passphrase, session token and API key
// hash was made with. Changing them breaks every existing credential.
const (
	pbkdf2Iterations = 100000
	pbkdf2KeyLength  = 64
)

// Hash derives the stored hash of a credential. The salt is used as the bytes
// of its hex string, not decoded: Node's crypto.pbkdf2 was given the hex
// string itself, so that is what every existing row was hashed with.
func Hash(plaintext, saltHex string) (string, error) {
	derived, err := pbkdf2.Key(sha512.New, plaintext, []byte(saltHex), pbkdf2Iterations, pbkdf2KeyLength)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(derived), nil
}

// Verify reports whether plaintext hashes to expectedHex with saltHex. A
// stored hash that is not valid hex or has the wrong length fails closed.
func Verify(plaintext, saltHex, expectedHex string) bool {
	computed, err := Hash(plaintext, saltHex)
	if err != nil {
		return false
	}
	computedBytes, _ := hex.DecodeString(computed)
	expectedBytes, err := hex.DecodeString(expectedHex)
	if err != nil || len(expectedBytes) != len(computedBytes) {
		return false
	}
	return subtle.ConstantTimeCompare(computedBytes, expectedBytes) == 1
}

// TokenLookupHash is the unsalted SHA-256 of a token, stored next to the
// PBKDF2 hash so a token's row can be found with an index. Tokens are long
// random values, so no salt is needed against precomputation; a per-row salt
// would make the index useless. The PBKDF2 verify stays the real check.
func TokenLookupHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
