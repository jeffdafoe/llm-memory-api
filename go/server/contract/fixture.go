//go:build contract

package contract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/auth"
)

// The fixture's actors and their plain credentials. The template stores only
// the hashes, as the servers do; scenarios use these by name (Credentials).
const (
	AdminName     = "contract-admin"
	AgentAName    = "agent-a"
	AgentBName    = "agent-b"
	adminPassword = "contract-admin-password"
)

// fixedSecret is a stable 64-hex value per name, used for tokens, keys and
// salts, so the plain values are constants and every run builds the same
// template.
func fixedSecret(name string) string {
	sum := sha256.Sum256([]byte("llm-909 contract fixture: " + name))
	return hex.EncodeToString(sum[:])
}

// fixedSalt is a 32-hex salt, the length hashing.js generateSalt makes.
func fixedSalt(name string) string {
	return fixedSecret("salt " + name)[:32]
}

// Credentials returns the fixture's plain credentials by name. A Step names
// one in Auth (sent as a bearer token) or as {{name}} in a path, header or
// body.
func Credentials() map[string]string {
	creds := map[string]string{
		"admin-password":     adminPassword,
		"admin-web":          fixedSecret("admin web session"),
		"agent-a-passphrase": fixedSecret("agent-a passphrase"),
		"agent-a-session":    fixedSecret("agent-a api session"),
		"agent-a-key":        fixedSecret("agent-a api key"),
		"agent-b-passphrase": fixedSecret("agent-b passphrase"),
		"agent-b-session":    fixedSecret("agent-b api session"),
		"agent-b-key":        fixedSecret("agent-b api key"),
		// Stored, but refused: the session expired a day before the
		// template was built, and the key is revoked.
		"agent-b-expired-session": fixedSecret("agent-b expired session"),
		"agent-b-revoked-key":     fixedSecret("agent-b revoked key"),
	}
	// Each fixture virtual agent's API key, for /agent/tick as itself.
	for _, va := range fixtureVirtualAgents {
		creds[va.name+"-key"] = vaKey(va.name)
	}
	return creds
}

// Fixed values for the two secrets seed.sql leaves empty, so Node and Go
// sign MCP bearer tokens and encrypt virtual-agent keys the same way.
var fixtureConfig = map[string]string{
	"mcp_oauth_bearer_secret":      fixedSecret("mcp_oauth_bearer_secret"),
	"virtual_agent_encryption_key": fixedSecret("virtual_agent_encryption_key"),
	// No cron runs during a scenario.
	"dream_cron_schedule":   "",
	"cleanup_cron_schedule": "",
	// Minutes; the ping has no off switch, so it is pushed past any run.
	// Node passes minutes * 60000 to setInterval, whose limit is 2^31-1 ms
	// (about 35791 minutes); a larger value overflows and Node runs the
	// ping every millisecond, which starved its event loop and made a
	// server miss its 60 s /health start check.
	"virtual_agent_error_ping_interval": "35000",
	// Seconds: one retry, 1 s later, so the virtual-agent retry cases end
	// within a settle.
	"virtual_agent_retry_backoff":              "1",
	"virtual_agent_retry_backoff_client_error": "1",
}

// applyFixture writes the harness data on top of seed.sql: config, an admin
// user with *:* and a web session, and two agents, each with a passphrase,
// an API session, an API key and namespace grants. agent-a has every MCP
// permission and wildcard namespace access; agent-b can read "shared" and
// read and write "agent-a". Then the notes and chunks searches run against
// (fixture_memory.go), the mail and virtual agent (fixture_mail.go), the
// discussion and chat messages (fixture_chat.go), the OAuth and
// registration rows (fixture_oauth.go), and what the /v1/agent routes read
// (fixture_agent.go).
func applyFixture(ctx context.Context, conn *pgx.Conn) error {
	creds := Credentials()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	for key, value := range fixtureConfig {
		tag, err := tx.Exec(ctx, "UPDATE config SET value = $1 WHERE key = $2", value, key)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("config key %q is not in seed.sql", key)
		}
	}

	hash := func(plain, saltName string) (string, string, error) {
		salt := fixedSalt(saltName)
		h, err := auth.Hash(plain, salt)
		return h, salt, err
	}

	pwHash, pwSalt, err := hash(adminPassword, "admin password")
	if err != nil {
		return err
	}
	var adminID int
	if err := tx.QueryRow(ctx,
		"INSERT INTO actors (name, password_hash, password_salt) VALUES ($1, $2, $3) RETURNING id",
		AdminName, pwHash, pwSalt).Scan(&adminID); err != nil {
		return fmt.Errorf("admin: %w", err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO admin_permissions (actor_id, resource, action) VALUES ($1, '*', '*')", adminID); err != nil {
		return err
	}
	if err := insertSession(ctx, tx, adminID, creds["admin-web"], "admin web session", "web"); err != nil {
		return err
	}

	agents := map[string]int{}
	for _, name := range []string{AgentAName, AgentBName} {
		h, salt, err := hash(creds[name+"-passphrase"], name+" passphrase")
		if err != nil {
			return err
		}
		var id int
		if err := tx.QueryRow(ctx,
			"INSERT INTO actors (name, token_hash, token_salt) VALUES ($1, $2, $3) RETURNING id",
			name, h, salt).Scan(&id); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		agents[name] = id
		// Agent login requires this row (routes/agent.js joins it).
		if _, err := tx.Exec(ctx, "INSERT INTO agent_configuration (actor_id) VALUES ($1)", id); err != nil {
			return err
		}
		if err := insertSession(ctx, tx, id, creds[name+"-session"], name+" api session", "api"); err != nil {
			return err
		}
		key := creds[name+"-key"]
		kh, ks, err := hash(key, name+" api key")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			"INSERT INTO agent_api_keys (actor_id, key_hash, key_salt, key_lookup_hash, label) VALUES ($1, $2, $3, $4, 'contract')",
			id, kh, ks, auth.TokenLookupHash(key)); err != nil {
			return err
		}
	}

	bID := agents[AgentBName]
	if err := insertSessionExpiring(ctx, tx, bID, creds["agent-b-expired-session"], "agent-b expired session", "api", "NOW() - INTERVAL '1 day'"); err != nil {
		return err
	}
	revoked := creds["agent-b-revoked-key"]
	rh, rs, err := hash(revoked, "agent-b revoked key")
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO agent_api_keys (actor_id, key_hash, key_salt, key_lookup_hash, label, revoked_at) VALUES ($1, $2, $3, $4, 'contract revoked', NOW())",
		bID, rh, rs, auth.TokenLookupHash(revoked)); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx,
		"INSERT INTO agent_permissions (actor_id, permission_id) SELECT $1, id FROM permissions ORDER BY id",
		agents[AgentAName]); err != nil {
		return err
	}
	grants := []struct {
		actor             string
		namespace         string
		read, write, dele bool
	}{
		{AgentAName, "/", true, true, true},
		{AgentBName, "shared", true, false, false},
		{AgentBName, AgentAName, true, true, false},
	}
	for _, g := range grants {
		if _, err := tx.Exec(ctx,
			"INSERT INTO namespace_permissions (actor_id, namespace, can_read, can_write, can_delete) VALUES ($1, $2, $3, $4, $5)",
			agents[g.actor], g.namespace, g.read, g.write, g.dele); err != nil {
			return err
		}
	}
	if err := applyMemoryFixture(ctx, tx, adminID, agents); err != nil {
		return err
	}
	if err := applyMailFixture(ctx, tx, adminID, agents); err != nil {
		return err
	}
	if err := applyVirtualAgentFixture(ctx, tx, adminID); err != nil {
		return err
	}
	if err := applyVAChatFixture(ctx, tx, adminID, agents); err != nil {
		return err
	}
	if err := applyChatFixture(ctx, tx, adminID, agents); err != nil {
		return err
	}
	if err := applyDiscussionFixture(ctx, tx, adminID, agents); err != nil {
		return err
	}
	if err := applyOAuthFixture(ctx, tx, agents); err != nil {
		return err
	}
	if err := applyAgentFixture(ctx, tx, adminID, agents); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// insertSession stores a session the way the login routes do: PBKDF2 hash
// and salt plus the SHA-256 lookup hash. It expires 30 days after the
// template is built, far from the 12-hour refresh window.
func insertSession(ctx context.Context, tx pgx.Tx, actorID int, token, saltName, kind string) error {
	return insertSessionExpiring(ctx, tx, actorID, token, saltName, kind, "NOW() + INTERVAL '30 days'")
}

// insertSessionExpiring takes the expiry as a constant SQL expression from
// this file, never from a scenario.
func insertSessionExpiring(ctx context.Context, tx pgx.Tx, actorID int, token, saltName, kind, expires string) error {
	salt := fixedSalt(saltName)
	h, err := auth.Hash(token, salt)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO sessions (actor_id, token_hash, token_salt, token_lookup_hash, kind, expires_at)
		 VALUES ($1, $2, $3, $4, $5, `+expires+`)`,
		actorID, h, salt, auth.TokenLookupHash(token), kind)
	return err
}
