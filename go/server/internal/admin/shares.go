package admin

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/pgbind"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/pgjson"
)

// hasNoteAccess is note-permissions.js hasNoteAccess: a live share of the
// note (its exact slug, or a folder pattern it starts with) to the actor or
// to everyone, with the operation's flag.
func hasNoteAccess(ctx context.Context, conn db.DB, namespace, slug string, actorID int, op permissions.Operation) (bool, error) {
	column := "can_" + string(op)
	var one int
	err := conn.QueryRow(ctx, `SELECT 1 FROM note_permissions
         WHERE owner_namespace = $1
           AND (slug_pattern = $2 OR $2 LIKE slug_pattern || '%')
           AND (grantee_actor_id = $3 OR grantee_actor_id IS NULL)
           AND revoked_at IS NULL
           AND `+column+` = true
         LIMIT 1`, namespace, slug, actorID).Scan(&one)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// sharesCreate is POST /admin/shares/create: a share of a note or a folder
// (a slug pattern ending in "/") with one actor, or with everyone when
// grantee_actor_id is absent or null. An active share of the same pattern
// to the same grantee has its flags replaced instead.
func (rt *ContentRoutes) sharesCreate(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("owner_namespace")
	}
	if !f.Truthy("owner_namespace") || !f.Truthy("slug_pattern") {
		return httpapi.BadRequest(w, "Required fields: owner_namespace, slug_pattern")
	}
	owner, err := f.RequireString("owner_namespace")
	if err != nil {
		return err
	}
	pattern, err := f.RequireString("slug_pattern")
	if err != nil {
		return err
	}
	if err := permissions.ValidateNamespace(owner); err != nil {
		return err
	}
	if err := rt.requireAccess(r, owner, permissions.Write); err != nil {
		return err
	}

	// grantee_actor_id !== undefined ? grantee_actor_id : null
	grantee := f.Raw("grantee_actor_id")
	shareWithAll := grantee == nil || string(grantee) == "null"
	canRead := string(f.Raw("can_read")) != "false"
	canWrite := f.Truthy("can_write")
	canDelete := f.Truthy("can_delete")
	if shareWithAll && canDelete {
		return apierr.New(http.StatusBadRequest, "Share-with-all cannot grant delete permission")
	}

	ctx, cancel := detached(r)
	defer cancel()
	var existing []pgjson.Row
	if shareWithAll {
		existing, err = pgjson.Query(ctx, rt.DB, `SELECT id FROM note_permissions
         WHERE owner_namespace = $1 AND slug_pattern = $2
           AND grantee_actor_id IS NULL
           AND revoked_at IS NULL`, owner, pattern)
	} else {
		existing, err = pgjson.Query(ctx, rt.DB, `SELECT id FROM note_permissions
         WHERE owner_namespace = $1 AND slug_pattern = $2
           AND grantee_actor_id = $3
           AND revoked_at IS NULL`, owner, pattern, pgbind.Value(grantee))
	}
	if err != nil {
		return err
	}
	var share pgjson.Row
	if len(existing) > 0 {
		id, _ := existing[0].Get("id")
		share, _, err = pgjson.QueryOne(ctx, rt.DB, `UPDATE note_permissions SET can_read = $1, can_write = $2, can_delete = $3
             WHERE id = $4 RETURNING *`, canRead, canWrite, canDelete, string(id))
	} else {
		var granteeParam any
		if !shareWithAll {
			granteeParam = pgbind.Value(grantee)
		}
		share, _, err = pgjson.QueryOne(ctx, rt.DB, `INSERT INTO note_permissions (owner_namespace, slug_pattern, grantee_actor_id, can_read, can_write, can_delete, granted_by)
         VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING *`,
			owner, pattern, granteeParam, canRead, canWrite, canDelete, httpapi.StateOf(r).ActorID)
	}
	if err != nil {
		return err
	}
	details := logger.Fields{{Key: "owner_namespace", Value: owner}, {Key: "slug_pattern", Value: pattern}}
	// JSON.stringify leaves out an undefined grantee_actor_id.
	if grantee != nil {
		details = append(details, logger.Field{Key: "grantee_actor_id", Value: grantee})
	}
	details = append(details, logger.Field{Key: "user_id", Value: User(r).ID})
	rt.LogAdmin("share_create", details)
	return httpapi.WriteJSON(w, http.StatusOK, map[string]pgjson.Row{"share": share})
}

// sharesRevoke is POST /admin/shares/revoke: revoke one active share, with
// write access to its owner namespace.
func (rt *ContentRoutes) sharesRevoke(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("id")
	}
	if !f.Truthy("id") {
		return httpapi.BadRequest(w, "Required field: id")
	}
	id := pgbind.Value(f.Raw("id"))
	var owner string
	err := rt.DB.QueryRow(r.Context(), "SELECT owner_namespace FROM note_permissions WHERE id = $1 AND revoked_at IS NULL", id).Scan(&owner)
	if isNoRows(err) {
		httpapi.WriteError(w, http.StatusNotFound, "NOT_FOUND", "Share not found")
		return nil
	}
	if err != nil {
		return err
	}
	if err := rt.requireAccess(r, owner, permissions.Write); err != nil {
		return err
	}
	ctx, cancel := detached(r)
	defer cancel()
	share, found, err := pgjson.QueryOne(ctx, rt.DB,
		"UPDATE note_permissions SET revoked_at = NOW() WHERE id = $1 AND revoked_at IS NULL RETURNING *", id)
	if err != nil {
		return err
	}
	if !found {
		return apierr.New(http.StatusNotFound, "Share not found or already revoked")
	}
	rt.LogAdmin("share_revoke", logger.Fields{{Key: "share_id", Value: f.Raw("id")}, {Key: "user_id", Value: User(r).ID}})
	return httpapi.WriteJSON(w, http.StatusOK, map[string]pgjson.Row{"share": share})
}

// sharesList is POST /admin/shares/list: the shares of one owner namespace,
// or with `all` every active share in a namespace the caller may read.
func (rt *ContentRoutes) sharesList(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("owner_namespace")
	}
	if f.Truthy("all") {
		shares, err := pgjson.Query(r.Context(), rt.DB,
			`SELECT np.*, o.name AS owner_name, a.name AS grantee_name, g.name AS granted_by_name
         FROM note_permissions np
         JOIN actors o ON o.name = np.owner_namespace
         LEFT JOIN actors a ON np.grantee_actor_id = a.id
         JOIN actors g ON np.granted_by = g.id
         WHERE np.revoked_at IS NULL
         ORDER BY np.owner_namespace, np.slug_pattern, np.grantee_actor_id`)
		if err != nil {
			return err
		}
		readable, all, err := rt.readable(r)
		if err != nil {
			return err
		}
		if !all {
			shares = filterRows(shares, "owner_namespace", readable)
		}
		return httpapi.WriteJSON(w, http.StatusOK, map[string][]pgjson.Row{"shares": shares})
	}
	if !f.Truthy("owner_namespace") {
		return httpapi.BadRequest(w, "Required field: owner_namespace (or all: true)")
	}
	owner, err := f.RequireString("owner_namespace")
	if err != nil {
		return err
	}
	if err := permissions.ValidateNamespace(owner); err != nil {
		return err
	}
	if err := rt.requireAccess(r, owner, permissions.Read); err != nil {
		return err
	}
	shares, err := pgjson.Query(r.Context(), rt.DB,
		`SELECT np.*, a.name AS grantee_name, g.name AS granted_by_name
         FROM note_permissions np
         LEFT JOIN actors a ON np.grantee_actor_id = a.id
         JOIN actors g ON np.granted_by = g.id
         WHERE np.owner_namespace = $1 AND np.revoked_at IS NULL
         ORDER BY np.slug_pattern, np.grantee_actor_id`, owner)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string][]pgjson.Row{"shares": shares})
}

// sharesSharedWithMe is POST /admin/shares/shared-with-me: the readable
// shares to the caller or to everyone.
func (rt *ContentRoutes) sharesSharedWithMe(w http.ResponseWriter, r *http.Request) error {
	shares, err := pgjson.Query(r.Context(), rt.DB,
		`SELECT np.*, g.name AS granted_by_name
         FROM note_permissions np
         JOIN actors g ON np.granted_by = g.id
         WHERE (np.grantee_actor_id = $1 OR np.grantee_actor_id IS NULL)
           AND np.revoked_at IS NULL
           AND np.can_read = true
         ORDER BY np.owner_namespace, np.slug_pattern`, httpapi.StateOf(r).ActorID)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string][]pgjson.Row{"shares": shares})
}

// sharesDocuments is POST /admin/shares/documents: the live notes shared
// with the caller, grouped by namespace.
func (rt *ContentRoutes) sharesDocuments(w http.ResponseWriter, r *http.Request) error {
	docs, err := pgjson.Query(r.Context(), rt.DB,
		`SELECT DISTINCT d.namespace, d.slug, d.title, d.updated_at,
                np.can_read, np.can_write, np.can_delete
         FROM note_permissions np
         JOIN documents d ON d.namespace = np.owner_namespace
           AND (np.slug_pattern = d.slug OR (np.slug_pattern LIKE '%/' AND d.slug LIKE np.slug_pattern || '%'))
         WHERE (np.grantee_actor_id = $1 OR np.grantee_actor_id IS NULL)
           AND np.revoked_at IS NULL AND np.can_read = true
           AND d.deleted_at IS NULL
         ORDER BY d.namespace, d.slug`, httpapi.StateOf(r).ActorID)
	if err != nil {
		return err
	}
	groups := jsGroups{}
	for _, d := range docs {
		ns, _ := d.String("namespace")
		groups.add(ns, d)
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string]jsGroups{"namespaces": groups})
}

// sharesForSlug is POST /admin/shares/for-slug: the active shares of one
// exact slug pattern, for the share dialog.
func (rt *ContentRoutes) sharesForSlug(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("owner_namespace")
	}
	if !f.Truthy("owner_namespace") || !f.Truthy("slug_pattern") {
		return httpapi.BadRequest(w, "Required fields: owner_namespace, slug_pattern")
	}
	owner, err := f.RequireString("owner_namespace")
	if err != nil {
		return err
	}
	pattern, err := f.RequireString("slug_pattern")
	if err != nil {
		return err
	}
	if err := permissions.ValidateNamespace(owner); err != nil {
		return err
	}
	if err := rt.requireAccess(r, owner, permissions.Read); err != nil {
		return err
	}
	shares, err := pgjson.Query(r.Context(), rt.DB,
		`SELECT np.*, a.name AS grantee_name
         FROM note_permissions np
         LEFT JOIN actors a ON np.grantee_actor_id = a.id
         WHERE np.owner_namespace = $1 AND np.slug_pattern = $2 AND np.revoked_at IS NULL
         ORDER BY np.grantee_actor_id`, owner, pattern)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string][]pgjson.Row{"shares": shares})
}

// jsGroups is a plain object of arrays keyed by name, in JavaScript's
// property order: names that are array indices ("0", "17") first in numeric
// order, then the rest in insertion order.
type jsGroups struct {
	keys   []string
	values map[string][]pgjson.Row
}

func (g *jsGroups) add(key string, row pgjson.Row) {
	if g.values == nil {
		g.values = map[string][]pgjson.Row{}
	}
	if _, ok := g.values[key]; !ok {
		g.keys = append(g.keys, key)
	}
	g.values[key] = append(g.values[key], row)
}

// arrayIndex reports whether key is a canonical array index (0 to 2^32-2).
func arrayIndex(key string) (uint64, bool) {
	n, err := strconv.ParseUint(key, 10, 32)
	if err != nil || n == 1<<32-1 || strconv.FormatUint(n, 10) != key {
		return 0, false
	}
	return n, true
}

func (g jsGroups) MarshalJSON() ([]byte, error) {
	keys := append([]string(nil), g.keys...)
	sort.SliceStable(keys, func(i, j int) bool {
		a, aIdx := arrayIndex(keys[i])
		b, bIdx := arrayIndex(keys[j])
		if aIdx && bIdx {
			return a < b
		}
		return aIdx && !bIdx
	})
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		name, err := jsonutil.Marshal(k)
		if err != nil {
			return nil, err
		}
		b.Write(name)
		b.WriteByte(':')
		rows, err := jsonutil.Marshal(g.values[k])
		if err != nil {
			return nil, err
		}
		b.Write(rows)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
