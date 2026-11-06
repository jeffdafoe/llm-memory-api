// Package visibility decides which actors a viewer may see (Node's
// services/actor-visibility.js): the agent roster (/agent/status, the MCP
// agent_status tool) and the admin actor views.
//
// Three layers:
//  1. A superadmin ('*:*') sees every actor, across realms.
//  2. Otherwise the viewer sees only actors sharing at least one realm.
//  3. Within the realm, actor_visibility_configuration grants visibility: a
//     row with a NULL target is a wildcard, other rows name actors.
//
// Without grant rows a viewer sees only itself and the actors it created
// (created_by), which are its own resources and are visible across realms.
package visibility

import (
	"context"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/ttlcache"
)

const cacheTTL = 2 * time.Minute

// Superadmin reports whether an actor holds '*:*' (permissions.Admin).
type Superadmin interface {
	IsSuperadmin(ctx context.Context, actorID int) (bool, error)
}

// Visible is the actors one viewer may see. All means no filter applies.
// IDs keeps Node's Set order: the viewer, then the others as found.
type Visible struct {
	All bool
	IDs []int
}

// Has reports whether the actor is visible.
func (v Visible) Has(actorID int) bool {
	if v.All {
		return true
	}
	for _, id := range v.IDs {
		if id == actorID {
			return true
		}
	}
	return false
}

// Visibility resolves and caches each viewer's visible actors for two
// minutes.
type Visibility struct {
	DB    db.DB
	Admin Superadmin

	cache ttlcache.Cache[int, Visible]
}

// VisibleActorIDs returns the actors the viewer may see. A viewer ID that
// is not a positive integer sees nothing.
func (v *Visibility) VisibleActorIDs(ctx context.Context, actorID int) (Visible, error) {
	if actorID <= 0 {
		return Visible{IDs: []int{}}, nil
	}
	if cached, ok := v.cache.Get(actorID); ok {
		return cached, nil
	}
	gen := v.cache.Generation()
	visible, err := v.load(ctx, actorID)
	if err != nil {
		return Visible{}, err
	}
	v.cache.SetIfCurrent(gen, actorID, visible, v.now().Add(cacheTTL))
	return visible, nil
}

// CanSee reports whether the viewer may see the target. Every actor sees
// itself.
func (v *Visibility) CanSee(ctx context.Context, viewerID, targetID int) (bool, error) {
	if viewerID <= 0 || targetID <= 0 {
		return false, nil
	}
	if viewerID == targetID {
		return true, nil
	}
	visible, err := v.VisibleActorIDs(ctx, viewerID)
	if err != nil {
		return false, err
	}
	return visible.Has(targetID), nil
}

// ClearCache drops one viewer's entry, or every entry for 0. Call it after
// a visibility grant changes.
func (v *Visibility) ClearCache(actorID int) {
	if actorID != 0 {
		v.cache.Delete(actorID)
		return
	}
	v.cache.Clear()
}

func (v *Visibility) now() time.Time {
	if v.cache.Now != nil {
		return v.cache.Now()
	}
	return time.Now()
}

// set is an int set that keeps insertion order, as a JavaScript Set does.
type set struct {
	seen map[int]bool
	ids  []int
}

func newSet() *set {
	return &set{seen: map[int]bool{}}
}

func (s *set) add(id int) {
	if !s.seen[id] {
		s.seen[id] = true
		s.ids = append(s.ids, id)
	}
}

func (v *Visibility) load(ctx context.Context, actorID int) (Visible, error) {
	super, err := v.Admin.IsSuperadmin(ctx, actorID)
	if err != nil {
		return Visible{}, err
	}
	if super {
		return Visible{All: true}, nil
	}

	// The realm peers bound the grants: the actors whose realms overlap the
	// viewer's (the && operator), and the viewer.
	peers := newSet()
	peers.add(actorID)
	peerIDs, err := v.ids(ctx, `SELECT a2.id FROM actors a1
         JOIN actors a2 ON a1.realms && a2.realms
         WHERE a1.id = $1`, actorID)
	if err != nil {
		return Visible{}, err
	}
	for _, id := range peerIDs {
		peers.add(id)
	}

	created, err := v.ids(ctx, "SELECT id FROM actors WHERE created_by = $1", actorID)
	if err != nil {
		return Visible{}, err
	}

	rows, err := v.DB.Query(ctx, "SELECT target_actor_id FROM actor_visibility_configuration WHERE actor_id = $1", actorID)
	if err != nil {
		return Visible{}, err
	}
	var targets []*int
	for rows.Next() {
		var target *int
		if err := rows.Scan(&target); err != nil {
			rows.Close()
			return Visible{}, err
		}
		targets = append(targets, target)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Visible{}, err
	}

	// A wildcard row: every realm peer, plus the created actors.
	for _, target := range targets {
		if target == nil {
			out := newSet()
			for _, id := range peers.ids {
				out.add(id)
			}
			for _, id := range created {
				out.add(id)
			}
			return Visible{IDs: out.ids}, nil
		}
	}

	out := newSet()
	out.add(actorID)
	for _, id := range created {
		out.add(id)
	}
	// Explicit grants count only inside the realm.
	for _, target := range targets {
		if peers.seen[*target] {
			out.add(*target)
		}
	}
	return Visible{IDs: out.ids}, nil
}

func (v *Visibility) ids(ctx context.Context, sql string, args ...any) ([]int, error) {
	rows, err := v.DB.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
