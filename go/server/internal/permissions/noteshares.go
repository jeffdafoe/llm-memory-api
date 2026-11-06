package permissions

import (
	"context"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
)

// NoteShares are the note-level grants in note_permissions (Node's
// services/note-permissions.js): one note or a folder prefix (a pattern
// ending in "/") of an owner namespace, shared with one actor or, with a
// NULL grantee, with everyone.
type NoteShares struct {
	DB db.DB
}

// HasNoteAccess reports whether the actor holds op on the note through a
// live share of it or of a prefix of its slug. A nil slug (a tool call with
// none) is bound as NULL and matches nothing, as node-postgres bound
// undefined.
func (n *NoteShares) HasNoteAccess(ctx context.Context, ownerNamespace string, slug *string, actorID int, op Operation) (bool, error) {
	column := "can_" + string(op)
	rows, err := n.DB.Query(ctx, `SELECT 1 FROM note_permissions
         WHERE owner_namespace = $1
           AND (slug_pattern = $2 OR $2 LIKE slug_pattern || '%')
           AND (grantee_actor_id = $3 OR grantee_actor_id IS NULL)
           AND revoked_at IS NULL
           AND `+column+` = true
         LIMIT 1`, ownerNamespace, slug, actorID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	found := rows.Next()
	return found, rows.Err()
}

// OwnerShare is a live share of an owner namespace, with the names it joins.
// Only the columns a caller reads are kept.
type OwnerShare struct {
	SlugPattern    string
	GranteeActorID *int
	GranteeName    *string
	GrantedByName  string
}

// ListSharesByOwner lists the live shares of a namespace, by slug pattern
// and grantee.
func (n *NoteShares) ListSharesByOwner(ctx context.Context, ownerNamespace string) ([]OwnerShare, error) {
	rows, err := n.DB.Query(ctx, `SELECT np.slug_pattern, np.grantee_actor_id, a.name AS grantee_name, g.name AS granted_by_name
         FROM note_permissions np
         LEFT JOIN actors a ON np.grantee_actor_id = a.id
         JOIN actors g ON np.granted_by = g.id
         WHERE np.owner_namespace = $1 AND np.revoked_at IS NULL
         ORDER BY np.slug_pattern, np.grantee_actor_id`, ownerNamespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var shares []OwnerShare
	for rows.Next() {
		var s OwnerShare
		if err := rows.Scan(&s.SlugPattern, &s.GranteeActorID, &s.GranteeName, &s.GrantedByName); err != nil {
			return nil, err
		}
		shares = append(shares, s)
	}
	return shares, rows.Err()
}

// SharedDocument is a live note another namespace shares with an actor.
type SharedDocument struct {
	Namespace string
	Slug      string
	Title     *string
	UpdatedAt *time.Time
	CanRead   bool
	CanWrite  bool
	CanDelete bool
}

// ListSharedDocuments lists the live notes shared with the actor (directly
// or with everyone) for reading, by namespace and slug. A note reached by
// two shares with different grants is listed once per grant combination,
// as SELECT DISTINCT returned it.
func (n *NoteShares) ListSharedDocuments(ctx context.Context, actorID int) ([]SharedDocument, error) {
	rows, err := n.DB.Query(ctx, `SELECT DISTINCT d.namespace, d.slug, d.title, d.updated_at,
                np.can_read, np.can_write, np.can_delete
         FROM note_permissions np
         JOIN documents d ON d.namespace = np.owner_namespace
           AND (np.slug_pattern = d.slug OR (np.slug_pattern LIKE '%/' AND d.slug LIKE np.slug_pattern || '%'))
         WHERE (np.grantee_actor_id = $1 OR np.grantee_actor_id IS NULL)
           AND np.revoked_at IS NULL AND np.can_read = true
           AND d.deleted_at IS NULL
         ORDER BY d.namespace, d.slug`, actorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var docs []SharedDocument
	for rows.Next() {
		var d SharedDocument
		if err := rows.Scan(&d.Namespace, &d.Slug, &d.Title, &d.UpdatedAt, &d.CanRead, &d.CanWrite, &d.CanDelete); err != nil {
			return nil, err
		}
		docs = append(docs, d)
	}
	return docs, rows.Err()
}
