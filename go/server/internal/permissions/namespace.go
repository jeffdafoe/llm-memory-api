// Package permissions enforces namespace access (read/write/delete per
// namespace) and admin-dashboard permissions (resource/action grants).
package permissions

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
)

// Namespace permissions change more often than actors, so their cache is
// shorter than the actor cache's five minutes.
const namespaceCacheTTL = 2 * time.Minute

// WildcardNamespace in a grant row means every namespace.
const WildcardNamespace = "/"

type Operation string

const (
	Read   Operation = "read"
	Write  Operation = "write"
	Delete Operation = "delete"
)

type grant struct {
	namespace string
	read      bool
	write     bool
	delete    bool
}

func (g grant) allows(op Operation) bool {
	switch op {
	case Read:
		return g.read
	case Write:
		return g.write
	case Delete:
		return g.delete
	}
	return false
}

type namespaceEntry struct {
	grants  []grant // in row order, which getReadableNamespaces preserves
	expires time.Time
}

// Namespaces checks namespace access.
type Namespaces struct {
	DB  db.DB
	Now func() time.Time

	mu    sync.Mutex
	cache map[int]namespaceEntry
}

func (n *Namespaces) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

func (n *Namespaces) grants(ctx context.Context, actorID int) ([]grant, error) {
	n.mu.Lock()
	entry, ok := n.cache[actorID]
	n.mu.Unlock()
	if ok && entry.expires.After(n.now()) {
		return entry.grants, nil
	}

	rows, err := n.DB.Query(ctx,
		"SELECT namespace, can_read, can_write, can_delete FROM namespace_permissions WHERE actor_id = $1",
		actorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Later rows for the same namespace replace earlier ones, as Map.set did.
	var grants []grant
	index := map[string]int{}
	for rows.Next() {
		var g grant
		if err := rows.Scan(&g.namespace, &g.read, &g.write, &g.delete); err != nil {
			return nil, err
		}
		if i, seen := index[g.namespace]; seen {
			grants[i] = g
			continue
		}
		index[g.namespace] = len(grants)
		grants = append(grants, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	n.mu.Lock()
	if n.cache == nil {
		n.cache = map[int]namespaceEntry{}
	}
	n.cache[actorID] = namespaceEntry{grants: grants, expires: n.now().Add(namespaceCacheTTL)}
	n.mu.Unlock()
	return grants, nil
}

func find(grants []grant, namespace string) (grant, bool) {
	for _, g := range grants {
		if g.namespace == namespace {
			return g, true
		}
	}
	return grant{}, false
}

// HasAccess reports whether the actor may perform op on namespace. Every
// actor has full access to the namespace named after it, with no grant row.
func (n *Namespaces) HasAccess(ctx context.Context, actorID int, actorName, namespace string, op Operation) (bool, error) {
	if actorName != "" && namespace == actorName {
		return true, nil
	}
	grants, err := n.grants(ctx, actorID)
	if err != nil {
		return false, err
	}
	if g, ok := find(grants, WildcardNamespace); ok && g.allows(op) {
		return true, nil
	}
	if g, ok := find(grants, namespace); ok && g.allows(op) {
		return true, nil
	}
	return false, nil
}

// RequireAccess is HasAccess with a 403 on denial.
func (n *Namespaces) RequireAccess(ctx context.Context, actorID int, actorName, namespace string, op Operation) error {
	ok, err := n.HasAccess(ctx, actorID, actorName, namespace, op)
	if err != nil {
		return err
	}
	if !ok {
		return apierr.New(http.StatusForbidden,
			fmt.Sprintf("Actor \"%s\" does not have %s access to namespace \"%s\"", actorName, op, namespace))
	}
	return nil
}

// ReadableNamespaces lists the namespaces the actor may read, for filtering a
// search across namespaces. all is true for a wildcard read grant, and then
// the list is nil: no filter applies.
func (n *Namespaces) ReadableNamespaces(ctx context.Context, actorID int, actorName string) (namespaces []string, all bool, err error) {
	grants, err := n.grants(ctx, actorID)
	if err != nil {
		return nil, false, err
	}
	if g, ok := find(grants, WildcardNamespace); ok && g.read {
		return nil, true, nil
	}
	namespaces = []string{}
	own := false
	for _, g := range grants {
		if g.namespace != WildcardNamespace && g.read {
			namespaces = append(namespaces, g.namespace)
			if g.namespace == actorName {
				own = true
			}
		}
	}
	if actorName != "" && !own {
		namespaces = append(namespaces, actorName)
	}
	return namespaces, false, nil
}

// ClearCache drops one actor's cached grants, or every actor's when actorID
// is 0. Call it after changing grants.
func (n *Namespaces) ClearCache(actorID int) {
	n.mu.Lock()
	if actorID == 0 {
		n.cache = nil
	} else {
		delete(n.cache, actorID)
	}
	n.mu.Unlock()
}

// ValidateNamespace refuses the two reserved sentinels: "/" is the wildcard
// grant and "*" the wildcard search qualifier.
func ValidateNamespace(namespace string) error {
	if namespace == "/" || namespace == "*" {
		return apierr.New(http.StatusBadRequest,
			fmt.Sprintf("\"%s\" is a reserved namespace and cannot be used for content", namespace))
	}
	return nil
}
