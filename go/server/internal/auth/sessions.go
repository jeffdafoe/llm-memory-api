package auth

import (
	"context"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
)

// Session kinds in the sessions.kind column.
const (
	SessionKindAPI = "api"
	SessionKindWeb = "web"
)

// Sliding expiry: a session used within 12 hours of expiring is pushed out to
// 24 hours from now. That bounds the writes to one per 12 hours per session.
const (
	sessionTTL       = 24 * time.Hour
	slidingThreshold = 12 * time.Hour
)

// SessionRow is a valid session joined with its actor.
type SessionRow struct {
	ID        string
	ActorID   int
	Name      string
	Realms    []string
	ExpiresAt time.Time
}

type sessionCandidate struct {
	SessionRow
	tokenHash string
	tokenSalt string
}

// Sessions validates and deletes session tokens.
type Sessions struct {
	DB  db.DB
	Now func() time.Time
}

func (s *Sessions) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

const sessionColumns = `s.id::text, s.actor_id, s.token_hash, s.token_salt, s.expires_at, ac.name, ac.realms`

// Validate resolves a token to its live session of the given kind, or nil.
//
// The row is found by token_lookup_hash, then confirmed with PBKDF2. Sessions
// created before token_lookup_hash existed (MEM-131) have it NULL and are
// checked one by one; LLM-899 tracks dropping that fallback.
func (s *Sessions) Validate(ctx context.Context, token, kind string) (*SessionRow, error) {
	lookup := TokenLookupHash(token)
	fast, err := s.querySessions(ctx,
		`SELECT `+sessionColumns+`
         FROM sessions s
         JOIN actors ac ON ac.id = s.actor_id
         WHERE s.kind = $1
           AND s.token_lookup_hash = $2
           AND s.expires_at > NOW()
         LIMIT 1`,
		kind, lookup)
	if err != nil {
		return nil, err
	}
	if len(fast) > 0 {
		row := fast[0]
		if !Verify(token, row.tokenSalt, row.tokenHash) {
			// A lookup-hash match that fails PBKDF2 fails closed; it does not
			// fall through to the legacy scan.
			return nil, nil
		}
		if err := s.maybeExtend(ctx, &row.SessionRow); err != nil {
			return nil, err
		}
		return &row.SessionRow, nil
	}

	legacy, err := s.querySessions(ctx,
		`SELECT `+sessionColumns+`
         FROM sessions s
         JOIN actors ac ON ac.id = s.actor_id
         WHERE s.kind = $1
           AND s.token_lookup_hash IS NULL
           AND s.expires_at > NOW()`,
		kind)
	if err != nil {
		return nil, err
	}
	for i := range legacy {
		row := legacy[i]
		if Verify(token, row.tokenSalt, row.tokenHash) {
			if err := s.maybeExtend(ctx, &row.SessionRow); err != nil {
				return nil, err
			}
			return &row.SessionRow, nil
		}
	}
	return nil, nil
}

func (s *Sessions) querySessions(ctx context.Context, sql string, args ...any) ([]sessionCandidate, error) {
	rows, err := s.DB.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sessionCandidate
	for rows.Next() {
		var c sessionCandidate
		var realms []string
		if err := rows.Scan(&c.ID, &c.ActorID, &c.tokenHash, &c.tokenSalt, &c.ExpiresAt, &c.Name, &realms); err != nil {
			return nil, err
		}
		c.Realms = realms
		out = append(out, c)
	}
	return out, rows.Err()
}

// maybeExtend pushes expires_at out to 24 hours from now when the session is
// within 12 hours of expiring. The new time is cut to milliseconds, the
// precision the Node server wrote with.
func (s *Sessions) maybeExtend(ctx context.Context, row *SessionRow) error {
	now := s.now()
	if row.ExpiresAt.After(now.Add(slidingThreshold)) {
		return nil
	}
	newExpiry := now.Add(sessionTTL).Truncate(time.Millisecond)
	if _, err := s.DB.Exec(ctx, "UPDATE sessions SET expires_at = $1 WHERE id = $2", newExpiry, row.ID); err != nil {
		return err
	}
	row.ExpiresAt = newExpiry
	return nil
}

// DeleteByToken deletes the one session of this actor and kind that the token
// belongs to. It reports whether a session was deleted.
//
// The legacy fallback keeps the expires_at bound: it PBKDF2-verifies row by
// row, and an actor with thousands of stale sessions must not turn a logout
// into a long scan.
func (s *Sessions) DeleteByToken(ctx context.Context, token, kind string, actorID int) (bool, error) {
	lookup := TokenLookupHash(token)
	fast, err := s.queryHashes(ctx,
		`SELECT id::text, token_hash, token_salt FROM sessions
         WHERE actor_id = $1 AND kind = $2 AND token_lookup_hash = $3
         LIMIT 1`,
		actorID, kind, lookup)
	if err != nil {
		return false, err
	}
	if len(fast) > 0 {
		row := fast[0]
		if !Verify(token, row.salt, row.hash) {
			return false, nil
		}
		_, err := s.DB.Exec(ctx, "DELETE FROM sessions WHERE id = $1", row.id)
		return err == nil, err
	}

	legacy, err := s.queryHashes(ctx,
		`SELECT id::text, token_hash, token_salt FROM sessions
         WHERE actor_id = $1 AND kind = $2 AND token_lookup_hash IS NULL
           AND expires_at > NOW()`,
		actorID, kind)
	if err != nil {
		return false, err
	}
	for _, row := range legacy {
		if Verify(token, row.salt, row.hash) {
			_, err := s.DB.Exec(ctx, "DELETE FROM sessions WHERE id = $1", row.id)
			return err == nil, err
		}
	}
	return false, nil
}

type hashRow struct {
	id   string
	hash string
	salt string
}

func (s *Sessions) queryHashes(ctx context.Context, sql string, args ...any) ([]hashRow, error) {
	rows, err := s.DB.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hashRow
	for rows.Next() {
		var r hashRow
		if err := rows.Scan(&r.id, &r.hash, &r.salt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
