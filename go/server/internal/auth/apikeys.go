package auth

import (
	"context"
	"fmt"
	"regexp"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
)

// Every API key ever issued is 64 hex characters. Anything else is refused
// before any hashing, and in particular before the legacy PBKDF2 scan.
var apiKeyShape = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// LooksLikeAPIKey reports whether token has the shape of an API key.
func LooksLikeAPIKey(token string) bool {
	return apiKeyShape.MatchString(token)
}

// APIKeyMatch is the key row a presented key resolved to.
type APIKeyMatch struct {
	ID      int
	ActorID int
	Agent   string
}

// APIKeys resolves presented API keys (bearer tokens, OAuth client secrets).
type APIKeys struct {
	DB db.DB
	// Warnf receives the legacy-scan warning. It may be nil.
	Warnf func(format string, args ...any)
}

// Find resolves a key to its unrevoked agent_api_keys row, or nil. With
// actorID > 0 only that actor's keys are searched (the OAuth
// client-credentials case, where client_id names the actor).
//
// The row is found by key_lookup_hash, then confirmed with one PBKDF2 verify.
// Keys issued before key_lookup_hash existed (MEM-136) have it NULL and are
// checked one by one; API keys never expire, so a successful legacy match
// stamps the lookup hash and moves that key to the indexed path. The
// last_used_at stamp (and that heal) run in the background.
func (k *APIKeys) Find(ctx context.Context, token string, actorID int) (*APIKeyMatch, error) {
	if !LooksLikeAPIKey(token) {
		return nil, nil
	}
	lookup := TokenLookupHash(token)

	fastSQL := `SELECT ak.id, ak.actor_id, ak.key_hash, ak.key_salt, ac.name AS agent
         FROM agent_api_keys ak
         JOIN actors ac ON ac.id = ak.actor_id
         WHERE ak.key_lookup_hash = $1 AND ak.revoked_at IS NULL`
	fastArgs := []any{lookup}
	if actorID > 0 {
		fastSQL += ` AND ak.actor_id = $2`
		fastArgs = append(fastArgs, actorID)
	}
	fastSQL += `
         LIMIT 1`
	fast, err := k.query(ctx, fastSQL, fastArgs...)
	if err != nil {
		return nil, err
	}
	if len(fast) > 0 {
		row := fast[0]
		if !Verify(token, row.salt, row.hash) {
			return nil, nil
		}
		db.Background(func(ctx context.Context) {
			k.DB.Exec(ctx, "UPDATE agent_api_keys SET last_used_at = NOW() WHERE id = $1", row.match.ID)
		})
		match := row.match
		return &match, nil
	}

	legacySQL := `SELECT ak.id, ak.actor_id, ak.key_hash, ak.key_salt, ac.name AS agent
         FROM agent_api_keys ak
         JOIN actors ac ON ac.id = ak.actor_id
         WHERE ak.key_lookup_hash IS NULL AND ak.revoked_at IS NULL`
	var legacyArgs []any
	if actorID > 0 {
		legacySQL += ` AND ak.actor_id = $1`
		legacyArgs = append(legacyArgs, actorID)
	}
	legacy, err := k.query(ctx, legacySQL, legacyArgs...)
	if err != nil {
		return nil, err
	}
	if len(legacy) > 0 && k.Warnf != nil {
		k.Warnf("api-keys: legacy fallback scanning %d pre-MEM-136 row(s) (self-heals on successful auth)", len(legacy))
	}
	for _, row := range legacy {
		if Verify(token, row.salt, row.hash) {
			id := row.match.ID
			db.Background(func(ctx context.Context) {
				k.DB.Exec(ctx, "UPDATE agent_api_keys SET key_lookup_hash = $1, last_used_at = NOW() WHERE id = $2", lookup, id)
			})
			match := row.match
			return &match, nil
		}
	}
	return nil, nil
}

type keyRow struct {
	match APIKeyMatch
	hash  string
	salt  string
}

func (k *APIKeys) query(ctx context.Context, sql string, args ...any) ([]keyRow, error) {
	rows, err := k.DB.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("api key lookup: %w", err)
	}
	defer rows.Close()
	var out []keyRow
	for rows.Next() {
		var r keyRow
		if err := rows.Scan(&r.match.ID, &r.match.ActorID, &r.hash, &r.salt, &r.match.Agent); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
