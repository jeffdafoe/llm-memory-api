// Package actors maps agent names to integer actor IDs. External APIs speak
// names; every table keys on actors.id. Lookups are cached for five minutes
// because actor rows rarely change.
package actors

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/ttlcache"
)

const cacheTTL = 5 * time.Minute

type Actor struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// cacheKey holds either a name or an ID. Actor names are never empty, so
// {id: n} cannot collide with a name key.
type cacheKey struct {
	name string
	id   int
}

// Cache resolves actors by name or ID. A miss is not cached, so a newly
// created actor is visible on the next lookup.
type Cache struct {
	db      db.DB
	entries ttlcache.Cache[cacheKey, Actor]
}

func NewCache(conn db.DB) *Cache {
	return &Cache{db: conn}
}

func (c *Cache) cachedByName(name string) (Actor, bool) {
	return c.entries.Get(cacheKey{name: name})
}

func (c *Cache) cachedByID(id int) (Actor, bool) {
	return c.entries.Get(cacheKey{id: id})
}

// store caches the actor under its name and its ID, unless the cache was
// cleared after gen was read.
func (c *Cache) store(gen uint64, actor Actor) {
	expires := c.now().Add(cacheTTL)
	if c.entries.SetIfCurrent(gen, cacheKey{name: actor.Name}, actor, expires) {
		c.entries.SetIfCurrent(gen, cacheKey{id: actor.ID}, actor, expires)
	}
}

func (c *Cache) now() time.Time {
	if c.entries.Now != nil {
		return c.entries.Now()
	}
	return time.Now()
}

// ResolveByName returns the actor and true, or false when no actor has the name.
func (c *Cache) ResolveByName(ctx context.Context, name string) (Actor, bool, error) {
	if actor, ok := c.cachedByName(name); ok {
		return actor, true, nil
	}
	gen := c.entries.Generation()
	var actor Actor
	err := c.db.QueryRow(ctx, "SELECT id, name FROM actors WHERE name = $1", name).Scan(&actor.ID, &actor.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return Actor{}, false, nil
	}
	if err != nil {
		return Actor{}, false, err
	}
	c.store(gen, actor)
	return actor, true, nil
}

// ResolveByID returns the actor and true, or false when no actor has the ID.
func (c *Cache) ResolveByID(ctx context.Context, id int) (Actor, bool, error) {
	if actor, ok := c.cachedByID(id); ok {
		return actor, true, nil
	}
	gen := c.entries.Generation()
	var actor Actor
	err := c.db.QueryRow(ctx, "SELECT id, name FROM actors WHERE id = $1", id).Scan(&actor.ID, &actor.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return Actor{}, false, nil
	}
	if err != nil {
		return Actor{}, false, err
	}
	c.store(gen, actor)
	return actor, true, nil
}

// ResolveMultipleByName resolves several names with at most one query. Names
// with no actor are absent from the result.
func (c *Cache) ResolveMultipleByName(ctx context.Context, names []string) (map[string]Actor, error) {
	result := map[string]Actor{}
	var uncached []string
	for _, name := range names {
		if actor, ok := c.cachedByName(name); ok {
			result[name] = actor
		} else {
			uncached = append(uncached, name)
		}
	}
	if len(uncached) == 0 {
		return result, nil
	}
	gen := c.entries.Generation()

	rows, err := c.db.Query(ctx, "SELECT id, name FROM actors WHERE name = ANY($1)", uncached)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var actor Actor
		if err := rows.Scan(&actor.ID, &actor.Name); err != nil {
			return nil, err
		}
		c.store(gen, actor)
		result[actor.Name] = actor
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// RequireByName is ResolveByName with a 404 for an unknown name.
func (c *Cache) RequireByName(ctx context.Context, name string) (Actor, error) {
	actor, ok, err := c.ResolveByName(ctx, name)
	if err != nil {
		return Actor{}, err
	}
	if !ok {
		return Actor{}, apierr.New(http.StatusNotFound, fmt.Sprintf("Agent \"%s\" is not registered", name))
	}
	return actor, nil
}

// Clear drops every cached actor, including any lookup still in flight.
func (c *Cache) Clear() {
	c.entries.Clear()
}

// CanAccessVirtualAgent reports whether an actor may trigger a virtual agent
// over chat or mail. Granted to the VA's creator, to a superadmin (the single
// '*:*' admin permission), or through a virtual_agent_access row that is
// public (NULL grantee) or names the actor.
//
// A bare admin_permissions row is not enough: every self-signup gets some at
// registration. Sharing a realm is not a grant either.
func CanAccessVirtualAgent(ctx context.Context, conn db.DB, actorID, virtualAgentID int) (bool, error) {
	var hasAccess bool
	err := conn.QueryRow(ctx, `
        SELECT EXISTS(
            SELECT 1 FROM actors WHERE id = $2 AND created_by = $1
            UNION ALL
            SELECT 1 FROM admin_permissions WHERE actor_id = $1 AND resource = '*' AND action = '*'
            UNION ALL
            SELECT 1 FROM virtual_agent_access
            WHERE virtual_agent_id = $2
              AND (grantee_actor_id IS NULL OR grantee_actor_id = $1)
        ) AS has_access
    `, actorID, virtualAgentID).Scan(&hasAccess)
	return hasAccess, err
}
