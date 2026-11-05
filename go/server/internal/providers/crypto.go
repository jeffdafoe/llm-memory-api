package providers

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
)

// Agent API keys are stored as "<iv>:<tag>:<ciphertext>", hex, AES-256-GCM
// under the hex key in config virtual_agent_encryption_key.

var (
	errAuthenticate = errors.New("Unsupported state or unable to authenticate data")
	errKeyLength    = errors.New("Invalid key length")
)

// nodeHex is Buffer.from(s, 'hex'): pairs are decoded up to the first pair
// that is not hex, and an odd last character is dropped.
func nodeHex(s string) []byte {
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

func (p *Providers) encryptionKey() ([]byte, error) {
	encKey, err := p.Config.Get("virtual_agent_encryption_key")
	if err != nil {
		return nil, err
	}
	if encKey == "" {
		return nil, errors.New("virtual_agent_encryption_key not configured")
	}
	key := nodeHex(encKey)
	if len(key) != 32 {
		return nil, errKeyLength
	}
	return key, nil
}

// DecryptAPIKey returns the plaintext key. The checks run in Node's order:
// the key setting, the format, an empty IV, the key length, the tag length.
func (p *Providers) DecryptAPIKey(encrypted string) (string, error) {
	key, keyErr := p.encryptionKey()
	if keyErr != nil && !errors.Is(keyErr, errKeyLength) {
		return "", keyErr
	}
	parts := strings.Split(encrypted, ":")
	if len(parts) != 3 {
		return "", errors.New("Invalid encrypted API key format")
	}
	iv := nodeHex(parts[0])
	tag := nodeHex(parts[1])
	ciphertext := nodeHex(parts[2])
	if len(iv) == 0 {
		return "", errors.New("Invalid initialization vector")
	}
	if keyErr != nil {
		return "", keyErr
	}
	switch len(tag) {
	case 4, 8, 12, 13, 14, 15, 16:
	default:
		return "", fmt.Errorf("Invalid authentication tag length: %d", len(tag))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	// Go's GCM takes 12- to 16-byte tags, and a nonce other than 12 bytes
	// only with a 16-byte tag. encryptAPIKey writes 12 and 16; a shorter tag
	// is refused as unauthenticated.
	var gcm cipher.AEAD
	switch {
	case len(iv) == 12 && len(tag) >= 12:
		gcm, err = cipher.NewGCMWithTagSize(block, len(tag))
	case len(tag) == 16:
		gcm, err = cipher.NewGCMWithNonceSize(block, len(iv))
	default:
		return "", errAuthenticate
	}
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, iv, append(append([]byte(nil), ciphertext...), tag...), nil)
	if err != nil {
		return "", errAuthenticate
	}
	return httpapi.DecodeUTF8(plain), nil
}

// EncryptAPIKey encrypts a key with a random 12-byte IV.
func (p *Providers) EncryptAPIKey(plaintext string) (string, error) {
	key, err := p.encryptionKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, iv, []byte(plaintext), nil)
	ciphertext, tag := sealed[:len(sealed)-16], sealed[len(sealed)-16:]
	return hex.EncodeToString(iv) + ":" + hex.EncodeToString(tag) + ":" + hex.EncodeToString(ciphertext), nil
}
