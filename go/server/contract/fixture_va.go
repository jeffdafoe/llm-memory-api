//go:build contract

package contract

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// applyVirtualAgentFixture adds vaUser and the virtual agents it may use
// (cases_va.go): one per provider, a key encrypted under the fixture's
// virtual_agent_encryption_key, the model's current _configVersion, and a
// virtual_agent_access row for vaUser. The grant is not agent-a's or
// agent-b's: the chat cases send to everyone as them, and Node would answer
// such a chat through the virtual agents' direct-chat path, which Go's chat
// does not call yet. Their dream mode stays 'none', so no
// transcript is written (its slug is random by design), and the cases' mail is
// short enough that learning extraction skips. Two more: one with no key, one
// with a daily budget of 0.
func applyVirtualAgentFixture(ctx context.Context, tx pgx.Tx, adminID int) error {
	var userID int
	if err := tx.QueryRow(ctx, "INSERT INTO actors (name, created_by) VALUES ($1, $2) RETURNING id", vaUser, adminID).Scan(&userID); err != nil {
		return fmt.Errorf("%s: %w", vaUser, err)
	}
	for _, va := range fixtureVirtualAgents {
		var id int
		if err := tx.QueryRow(ctx, "INSERT INTO actors (name, created_by) VALUES ($1, $2) RETURNING id", va.name, adminID).Scan(&id); err != nil {
			return fmt.Errorf("virtual agent %s: %w", va.name, err)
		}
		var key *string
		if !va.noKey {
			k, err := encryptFixtureKey(va.name)
			if err != nil {
				return err
			}
			key = &k
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO agent_configuration (actor_id, virtual, provider, model, api_key, configuration, startup_instructions, cost_budget_daily)
             VALUES ($1, true, $2, $3, $4, $5, $6, $7)`,
			id, va.provider, va.model, key, fmt.Sprintf(`{"_configVersion":%d}`, va.configVersion),
			"You are "+va.name+", a contract fixture.", va.dailyBudget); err != nil {
			return fmt.Errorf("virtual agent %s configuration: %w", va.name, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO virtual_agent_access (virtual_agent_id, grantee_actor_id) VALUES ($1, $2)", id, userID); err != nil {
			return fmt.Errorf("virtual agent %s access: %w", va.name, err)
		}
	}
	return nil
}

// encryptFixtureKey is the agent's plain key "contract-key-<name>" as
// provider.js encryptApiKey stores it ("<iv>:<tag>:<ciphertext>", hex,
// AES-256-GCM), with an IV drawn from the name so every run builds the same
// template.
func encryptFixtureKey(name string) (string, error) {
	key, err := hex.DecodeString(fixtureConfig["virtual_agent_encryption_key"])
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
	sum := sha256.Sum256([]byte("iv " + name))
	iv := sum[:gcm.NonceSize()]
	sealed := gcm.Seal(nil, iv, []byte("contract-key-"+name), nil)
	ciphertext, tag := sealed[:len(sealed)-gcm.Overhead()], sealed[len(sealed)-gcm.Overhead():]
	return hex.EncodeToString(iv) + ":" + hex.EncodeToString(tag) + ":" + hex.EncodeToString(ciphertext), nil
}
