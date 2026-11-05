package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
)

// GenerateSalt is a new random salt: 32 bytes as hex, as Node's generateSalt.
func GenerateSalt() string {
	return randomHex(32)
}

// GenerateKey is a new random API key: 32 bytes as hex (64 characters).
func GenerateKey() string {
	return randomHex(32)
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// dummySalt is hashed against when the name is unknown, so a miss takes as
// long as a wrong password and the response time does not tell which agent
// names exist.
var dummySalt = GenerateSalt()

// PasswordAccount is the account a password login matched.
type PasswordAccount struct {
	ID   int
	Name string
}

// VerifyPasswordLogin checks an agent name and password (Node's
// services/password-login.js), shared by the connector login on /authorize
// and the dashboard login. username must already be normalized
// (sanitize.AgentName). The password is the one chosen at signup; accounts
// without one (virtual agents, sim NPCs) never match. Returns nil on no match.
func VerifyPasswordLogin(ctx context.Context, conn db.DB, username, password string) (*PasswordAccount, error) {
	var (
		id         int
		name       string
		storedHash *string
		salt       *string
	)
	err := conn.QueryRow(ctx,
		"SELECT id, name, password_hash, password_salt FROM actors WHERE name = $1 AND password_hash IS NOT NULL",
		username).Scan(&id, &name, &storedHash, &salt)
	if errors.Is(err, pgx.ErrNoRows) {
		Hash(password, dummySalt)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Node handed a NULL salt to crypto.pbkdf2, which threw.
	if salt == nil {
		return nil, errors.New(`The "salt" argument must be of type string or an instance of ArrayBuffer, Buffer, TypedArray, or DataView. Received null`)
	}
	hashText := ""
	if storedHash != nil {
		hashText = *storedHash
	}
	if !verifyLoose(password, *salt, hashText) {
		return nil, nil
	}
	return &PasswordAccount{ID: id, Name: name}, nil
}

// verifyLoose is Node's verify: the stored hash is read with
// Buffer.from(hex), which stops at the first byte that is not a hex pair
// instead of failing, so only the decoded prefix is compared.
func verifyLoose(plaintext, salt, expectedHex string) bool {
	computed, err := Hash(plaintext, salt)
	if err != nil {
		return false
	}
	computedBytes, _ := hex.DecodeString(computed)
	expected := nodeHexPrefix(expectedHex)
	if len(expected) != len(computedBytes) {
		return false
	}
	return subtle.ConstantTimeCompare(computedBytes, expected) == 1
}

// nodeHexPrefix decodes hex the way Buffer.from(s, 'hex') does: pairs from
// the start until the first pair that is not two hex digits.
func nodeHexPrefix(s string) []byte {
	out := make([]byte, 0, len(s)/2)
	for i := 0; i+1 < len(s); i += 2 {
		b, err := hex.DecodeString(s[i : i+2])
		if err != nil {
			break
		}
		out = append(out, b[0])
	}
	return out
}
