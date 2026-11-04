package permissions

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/ttlcache"
)

const adminCacheTTL = 2 * time.Minute

// actionRank orders the hierarchical actions: a grant of a higher action
// includes the lower ones. Any other action name matches only exactly.
var actionRank = map[string]int{"read": 1, "write": 2, "delete": 3}

type AdminGrant struct {
	Resource string
	Action   string
}

// Admin checks admin-dashboard permissions. The single grant '*:*' is
// superadmin.
type Admin struct {
	DB db.DB
	// Errorf receives permission-check failures from RequirePerm.
	Errorf func(format string, args ...any)

	cache ttlcache.Cache[int, []AdminGrant]

	systemMu sync.Mutex
	systemID *int
}

func (a *Admin) now() time.Time {
	if a.cache.Now != nil {
		return a.cache.Now()
	}
	return time.Now()
}

// Grants returns the actor's admin grants in row order.
func (a *Admin) Grants(ctx context.Context, actorID int) ([]AdminGrant, error) {
	if grants, ok := a.cache.Get(actorID); ok {
		return grants, nil
	}

	gen := a.cache.Generation()
	rows, err := a.DB.Query(ctx, "SELECT resource, action FROM admin_permissions WHERE actor_id = $1", actorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	grants := []AdminGrant{}
	for rows.Next() {
		var g AdminGrant
		if err := rows.Scan(&g.Resource, &g.Action); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	a.cache.SetIfCurrent(gen, actorID, grants, a.now().Add(adminCacheTTL))
	return grants, nil
}

// HasPermission reports whether the actor holds resource/action, directly,
// through a '*' resource or action, or through a higher hierarchical action.
func (a *Admin) HasPermission(ctx context.Context, actorID int, resource, action string) (bool, error) {
	grants, err := a.Grants(ctx, actorID)
	if err != nil {
		return false, err
	}
	for _, g := range grants {
		if g.Resource == "*" && g.Action == "*" {
			return true, nil
		}
	}
	required, hierarchical := actionRank[action]
	for _, g := range grants {
		if g.Resource != resource && g.Resource != "*" {
			continue
		}
		if g.Action == "*" {
			return true, nil
		}
		if hierarchical {
			granted, ok := actionRank[g.Action]
			if ok && granted >= required {
				return true, nil
			}
		}
		if g.Action == action {
			return true, nil
		}
	}
	return false, nil
}

// IsSuperadmin reports whether the actor holds '*:*'.
func (a *Admin) IsSuperadmin(ctx context.Context, actorID int) (bool, error) {
	return a.HasPermission(ctx, actorID, "*", "*")
}

// RequirePerm wraps a route so only actors holding resource/action reach it.
func (a *Admin) RequirePerm(resource, action string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actorID := httpapi.StateOf(r).ActorID
		if actorID == 0 {
			httpapi.WriteError(w, http.StatusForbidden, "FORBIDDEN", "Insufficient permissions")
			return
		}
		ok, err := a.HasPermission(r.Context(), actorID, resource, action)
		if err != nil {
			if a.Errorf != nil {
				a.Errorf("Permission check error: %s", err)
			}
			httpapi.WriteError(w, http.StatusInternalServerError, "INTERNAL", "Permission check failed")
			return
		}
		if !ok {
			httpapi.WriteError(w, http.StatusForbidden, "FORBIDDEN", "Insufficient permissions")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// PermissionMap is {resource: [action, ...]} for the login response. Keys are
// written in the order the resources first appear, as a JavaScript object
// built row by row would print them.
type PermissionMap struct {
	order   []string
	actions map[string][]string
}

func (m PermissionMap) Actions(resource string) []string {
	return m.actions[resource]
}

func (m PermissionMap) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, resource := range m.order {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := jsonutil.Marshal(resource)
		if err != nil {
			return nil, err
		}
		value, err := jsonutil.Marshal(m.actions[resource])
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(value)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func (a *Admin) PermissionMap(ctx context.Context, actorID int) (PermissionMap, error) {
	grants, err := a.Grants(ctx, actorID)
	if err != nil {
		return PermissionMap{}, err
	}
	m := PermissionMap{actions: map[string][]string{}}
	for _, g := range grants {
		if _, seen := m.actions[g.Resource]; !seen {
			m.order = append(m.order, g.Resource)
		}
		m.actions[g.Resource] = append(m.actions[g.Resource], g.Action)
	}
	return m, nil
}

// IsTrustedCreator reports whether the actor is the system actor or holds
// agents/create_system_equivalent. Use it wherever "owned by system" is a
// trust signal.
func (a *Admin) IsTrustedCreator(ctx context.Context, actorID int) (bool, error) {
	systemID, err := a.systemActorID(ctx)
	if err != nil {
		return false, err
	}
	if actorID == systemID {
		return true, nil
	}
	return a.HasPermission(ctx, actorID, "agents", "create_system_equivalent")
}

func (a *Admin) systemActorID(ctx context.Context) (int, error) {
	a.systemMu.Lock()
	defer a.systemMu.Unlock()
	if a.systemID != nil {
		return *a.systemID, nil
	}
	var id int
	err := a.DB.QueryRow(ctx, "SELECT id FROM actors WHERE name = 'system'").Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		id = -1
	} else if err != nil {
		return 0, err
	}
	a.systemID = &id
	return id, nil
}

// ClearCache drops one actor's cached grants, or every actor's when actorID
// is 0. A load already in flight is not published afterwards.
func (a *Admin) ClearCache(actorID int) {
	if actorID == 0 {
		a.cache.Clear()
	} else {
		a.cache.Delete(actorID)
	}
}
