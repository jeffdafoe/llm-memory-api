package admin

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/memory"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/pgbind"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/pgjson"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/sanitize"
)

// maxListNamespaces is MAX_LIST_NAMESPACES: the most namespaces one batched
// /admin/notes/list may name, each one listNotes query.
const maxListNamespaces = 500

// defaultAdminListLimit is the admin listing's `limit || 200`.
var defaultAdminListLimit = json.RawMessage("200")

func adminListLimit(f httpapi.Fields) json.RawMessage {
	if f.Truthy("limit") {
		return f.Raw("limit")
	}
	return defaultAdminListLimit
}

// listResult is one namespace of a batched listing: {namespace, ...data}.
type listResult struct {
	Namespace string             `json:"namespace"`
	Notes     []notes.ListedNote `json:"notes"`
}

// notesList is POST /admin/notes/list: one namespace, or with `namespaces`
// several in one call (LLM-829). Every namespace of a batch is checked
// before any is read, so one unreadable namespace refuses the whole batch.
func (rt *ContentRoutes) notesList(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("namespace")
	}
	if raw := f.Raw("namespaces"); raw != nil {
		elems, isArray := f.Array("namespaces")
		names := make([]string, 0, len(elems))
		valid := isArray
		for _, e := range elems {
			var s string
			if len(e) == 0 || e[0] != '"' || json.Unmarshal(e, &s) != nil || s == "" {
				valid = false
				break
			}
			names = append(names, s)
		}
		if !valid {
			return httpapi.BadRequest(w, "namespaces must be an array of namespace names")
		}
		if len(names) > maxListNamespaces {
			return httpapi.BadRequest(w, fmt.Sprintf("At most %d namespaces per request", maxListNamespaces))
		}
		for _, ns := range names {
			if err := permissions.ValidateNamespace(ns); err != nil {
				return err
			}
			if err := rt.requireAccess(r, ns, permissions.Read); err != nil {
				return err
			}
		}
		prefix, err := f.OptionalString("prefix")
		if err != nil {
			return err
		}
		// One query per namespace, in sequence, as Node held one connection.
		results := make([]listResult, 0, len(names))
		for _, ns := range names {
			rows, err := rt.Notes.ListLive(r.Context(), notes.ListInput{
				Namespace: ns, Limit: adminListLimit(f), Offset: f.Raw("offset"), Prefix: prefix,
			})
			if err != nil {
				return err
			}
			results = append(results, listResult{Namespace: ns, Notes: rows})
		}
		return httpapi.WriteJSON(w, http.StatusOK, map[string][]listResult{"results": results})
	}
	if !f.Truthy("namespace") {
		return httpapi.BadRequest(w, "Required field: namespace")
	}
	namespace, err := f.RequireString("namespace")
	if err != nil {
		return err
	}
	if err := permissions.ValidateNamespace(namespace); err != nil {
		return err
	}
	if err := rt.requireAccess(r, namespace, permissions.Read); err != nil {
		return err
	}
	prefix, err := f.OptionalString("prefix")
	if err != nil {
		return err
	}
	res, err := rt.Notes.List(r.Context(), notes.ListInput{
		Namespace: namespace, Limit: adminListLimit(f), Offset: f.Raw("offset"), Prefix: prefix,
	})
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}

// noteAccess is the read, save and delete check: namespace access, else a
// share of the note (note_permissions) with that operation. A refusal is
// Node's 403, written directly.
func (rt *ContentRoutes) noteAccess(w http.ResponseWriter, r *http.Request, namespace, slug string, op permissions.Operation) (bool, error) {
	id, name := caller(r)
	ok, err := rt.Namespaces.HasAccess(r.Context(), id, name, namespace, op)
	if err != nil || ok {
		return ok, err
	}
	ok, err = hasNoteAccess(r.Context(), rt.DB, namespace, slug, id, op)
	if err != nil || ok {
		return ok, err
	}
	httpapi.WriteError(w, http.StatusForbidden, "FORBIDDEN", "Insufficient permissions")
	return false, nil
}

// namespaceAndSlug reads the required namespace and slug.
func namespaceAndSlug(w http.ResponseWriter, f httpapi.Fields) (namespace, slug string, done bool, err error) {
	if !f.Truthy("namespace") || !f.Truthy("slug") {
		return "", "", true, httpapi.BadRequest(w, "Required fields: namespace, slug")
	}
	if namespace, err = f.RequireString("namespace"); err != nil {
		return "", "", true, err
	}
	if slug, err = f.RequireString("slug"); err != nil {
		return "", "", true, err
	}
	return namespace, slug, false, nil
}

// notesRead is POST /admin/notes/read: the note as readNote returns it.
func (rt *ContentRoutes) notesRead(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("namespace")
	}
	namespace, slug, done, err := namespaceAndSlug(w, f)
	if done {
		return err
	}
	if err := permissions.ValidateNamespace(namespace); err != nil {
		return err
	}
	if ok, err := rt.noteAccess(w, r, namespace, slug, permissions.Read); !ok {
		return err
	}
	note, err := rt.Notes.Read(r.Context(), namespace, slug)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string]notes.Note{"note": note})
}

// notesSave is POST /admin/notes/save: an upsert at the slug, with no
// creator. The slug is sanitized as an identifier, title and content as
// content; content may be "" here and is then refused by the save.
func (rt *ContentRoutes) notesSave(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("namespace")
	}
	in := notes.SaveInput{Upsert: true}
	slugTruthy := f.Truthy("slug")
	if s, isString := f.String("slug"); isString {
		in.Slug = sanitize.Identifier(s)
		slugTruthy = in.Slug != ""
	} else if slugTruthy {
		in.SlugNotString = true
	}
	title, titleIsString := f.String("title")
	title = sanitize.Content(title)
	titleTruthy := f.Truthy("title") && (!titleIsString || title != "")
	if !f.Truthy("namespace") || !slugTruthy || !titleTruthy || f.Raw("content") == nil {
		return httpapi.BadRequest(w, "Required fields: namespace, slug, title, content")
	}
	var err error
	if in.Namespace, err = f.RequireString("namespace"); err != nil {
		return err
	}
	if !titleIsString {
		return httpapi.NotString("title")
	}
	in.Title = title
	// content === undefined is the only missing case; null and "" reach
	// saveNote, which refuses them (Required fields: title, content).
	if f.Truthy("content") {
		content, err := f.RequireString("content")
		if err != nil {
			return err
		}
		in.Content = sanitize.Content(content)
	} else if s, isString := f.String("content"); isString {
		in.Content = s
	}
	if in.Extension, err = f.OptionalString("extension"); err != nil {
		return err
	}
	if err := permissions.ValidateNamespace(in.Namespace); err != nil {
		return err
	}
	if ok, err := rt.noteAccess(w, r, in.Namespace, in.Slug, permissions.Write); !ok {
		return err
	}
	doc, err := rt.Notes.Save(r.Context(), in)
	if err != nil {
		return err
	}
	rt.LogAdmin("note_save", logger.Fields{{Key: "namespace", Value: in.Namespace}, {Key: "slug", Value: in.Slug}, {Key: "user_id", Value: User(r).ID}})
	return httpapi.WriteJSON(w, http.StatusOK, map[string]notes.SavedNote{"note": doc})
}

type deletedResponse struct {
	Deleted   bool   `json:"deleted"`
	Namespace string `json:"namespace"`
	Slug      string `json:"slug"`
}

// notesDelete is POST /admin/notes/delete: a soft delete.
func (rt *ContentRoutes) notesDelete(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("namespace")
	}
	namespace, slug, done, err := namespaceAndSlug(w, f)
	if done {
		return err
	}
	if err := permissions.ValidateNamespace(namespace); err != nil {
		return err
	}
	if ok, err := rt.noteAccess(w, r, namespace, slug, permissions.Delete); !ok {
		return err
	}
	if _, err := rt.Notes.Delete(r.Context(), namespace, slug, nil); err != nil {
		return err
	}
	rt.LogAdmin("note_delete", logger.Fields{{Key: "namespace", Value: namespace}, {Key: "slug", Value: slug}, {Key: "user_id", Value: User(r).ID}})
	return httpapi.WriteJSON(w, http.StatusOK, deletedResponse{Deleted: true, Namespace: namespace, Slug: slug})
}

// notesMove is POST /admin/notes/move: rename a note, optionally into
// another namespace (write access on both).
func (rt *ContentRoutes) notesMove(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("namespace")
	}
	if !f.Truthy("namespace") || !f.Truthy("slug") || !f.Truthy("new_slug") {
		return httpapi.BadRequest(w, "Required fields: namespace, slug, new_slug")
	}
	var in notes.MoveInput
	var err error
	if in.Namespace, err = f.RequireString("namespace"); err != nil {
		return err
	}
	if in.Slug, err = f.RequireString("slug"); err != nil {
		return err
	}
	if s, isString := f.String("new_slug"); isString {
		in.NewSlug = s
	} else {
		in.NewSlugNotString = true
	}
	if in.NewNamespace, err = f.OptionalString("new_namespace"); err != nil {
		return err
	}
	if err := permissions.ValidateNamespace(in.Namespace); err != nil {
		return err
	}
	if in.NewNamespace != "" {
		if err := permissions.ValidateNamespace(in.NewNamespace); err != nil {
			return err
		}
	}
	if err := rt.requireAccess(r, in.Namespace, permissions.Write); err != nil {
		return err
	}
	if in.NewNamespace != "" && in.NewNamespace != in.Namespace {
		if err := rt.requireAccess(r, in.NewNamespace, permissions.Write); err != nil {
			return err
		}
	}
	doc, err := rt.Notes.Move(r.Context(), in)
	if err != nil {
		return err
	}
	target := in.NewNamespace
	if target == "" {
		target = in.Namespace
	}
	rt.LogAdmin("note_move", logger.Fields{
		{Key: "namespace", Value: in.Namespace}, {Key: "slug", Value: in.Slug}, {Key: "new_slug", Value: in.NewSlug},
		{Key: "new_namespace", Value: target}, {Key: "user_id", Value: User(r).ID},
	})
	return httpapi.WriteJSON(w, http.StatusOK, map[string]any{"note": doc})
}

// notesSearch is POST /admin/notes/search: one namespace, or every readable
// one when namespace is absent or "*". The query is not sanitized here.
func (rt *ContentRoutes) notesSearch(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("query")
	}
	if !f.Truthy("query") {
		return httpapi.BadRequest(w, "Required field: query")
	}
	query, err := f.RequireString("query")
	if err != nil {
		return err
	}
	namespace := "*"
	if f.Truthy("namespace") {
		if namespace, err = f.RequireString("namespace"); err != nil {
			return err
		}
	}
	in := memory.SearchInput{Query: query, Namespace: namespace, ActorID: httpapi.StateOf(r).ActorID}
	// limit || 10, then Number() inside searchMemory.
	if f.Truthy("limit") {
		in.Limit, in.LimitErr = jsutil.ToNumberE(f.Raw("limit"))
	} else {
		in.Limit = 10
	}
	if namespace != "*" {
		if err := permissions.ValidateNamespace(namespace); err != nil {
			return err
		}
		if err := rt.requireAccess(r, namespace, permissions.Read); err != nil {
			return err
		}
	} else {
		readable, all, err := rt.readable(r)
		if err != nil {
			return err
		}
		in.Readable = readable
		in.FilterReadable = !all
	}
	res, err := rt.Memory.Search(r.Context(), in)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}

// notesNamespaces is POST /admin/notes/namespaces: live note counts and
// stored bytes per namespace, limited to the readable ones.
func (rt *ContentRoutes) notesNamespaces(w http.ResponseWriter, r *http.Request) error {
	rows, err := pgjson.Query(r.Context(), rt.DB,
		`SELECT d.namespace, COUNT(*) AS count, COALESCE(nu.total_bytes, 0) AS total_bytes
         FROM documents d
         LEFT JOIN namespace_usage nu ON nu.namespace = d.namespace
         WHERE d.deleted_at IS NULL
         GROUP BY d.namespace, nu.total_bytes
         ORDER BY d.namespace`)
	if err != nil {
		return err
	}
	readable, all, err := rt.readable(r)
	if err != nil {
		return err
	}
	if !all {
		rows = filterRows(rows, "namespace", readable)
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string][]pgjson.Row{"namespaces": rows})
}

// filterRows keeps the rows whose column is one of allowed.
func filterRows(rows []pgjson.Row, column string, allowed []string) []pgjson.Row {
	out := []pgjson.Row{}
	for _, row := range rows {
		if s, ok := row.String(column); ok && contains(allowed, s) {
			out = append(out, row)
		}
	}
	return out
}

// notesUsage is POST /admin/notes/usage: namespace_usage with the actor of
// the same name, for one namespace (bound as sent) or all, largest first.
func (rt *ContentRoutes) notesUsage(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("namespace")
	}
	var rows []pgjson.Row
	var err error
	if f.Truthy("namespace") {
		rows, err = pgjson.Query(r.Context(), rt.DB, `
            SELECT nu.namespace, nu.note_count, nu.total_bytes, nu.updated_at,
                   ac.name AS agent
            FROM namespace_usage nu
            LEFT JOIN actors ac ON ac.name = nu.namespace
            WHERE nu.namespace = $1
            ORDER BY nu.namespace`, pgbind.Value(f.Raw("namespace")))
	} else {
		rows, err = pgjson.Query(r.Context(), rt.DB, `
            SELECT nu.namespace, nu.note_count, nu.total_bytes, nu.updated_at,
                   ac.name AS agent
            FROM namespace_usage nu
            LEFT JOIN actors ac ON ac.name = nu.namespace
            ORDER BY nu.total_bytes DESC`)
	}
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string][]pgjson.Row{"usage": rows})
}

// syncList is POST /admin/notes/sync/list: note_synchronization rows, for
// one actor (bound as sent) or all.
func (rt *ContentRoutes) syncList(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("actor_id")
	}
	var rows []pgjson.Row
	var err error
	if f.Truthy("actor_id") {
		rows, err = pgjson.Query(r.Context(), rt.DB, `
            SELECT ns.id, ns.actor_id, a.name AS actor_name, ns.namespace, ns.slug, ns.local_path, ns.created_at
            FROM note_synchronization ns
            JOIN actors a ON a.id = ns.actor_id
            WHERE ns.actor_id = $1
            ORDER BY ns.namespace, ns.slug`, pgbind.Value(f.Raw("actor_id")))
	} else {
		rows, err = pgjson.Query(r.Context(), rt.DB, `
            SELECT ns.id, ns.actor_id, a.name AS actor_name, ns.namespace, ns.slug, ns.local_path, ns.created_at
            FROM note_synchronization ns
            JOIN actors a ON a.id = ns.actor_id
            ORDER BY a.name, ns.namespace, ns.slug`)
	}
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string][]pgjson.Row{"mappings": rows})
}

// syncSave is POST /admin/notes/sync/save: create a mapping, or change the
// local path of an existing one. An empty slug syncs the whole namespace.
// Every value is bound as sent.
func (rt *ContentRoutes) syncSave(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("actor_id")
	}
	if !f.Truthy("actor_id") || !f.Truthy("namespace") || !f.Truthy("local_path") {
		return httpapi.BadRequest(w, "Required fields: actor_id, namespace, slug, local_path")
	}
	// slug || ''
	var slug any = ""
	if f.Truthy("slug") {
		slug = pgbind.Value(f.Raw("slug"))
	}
	actorID := pgbind.Value(f.Raw("actor_id"))
	// resolveById: the actor by id, bound as sent.
	if _, found, err := pgjson.QueryOne(r.Context(), rt.DB, "SELECT id, name FROM actors WHERE id = $1", actorID); err != nil {
		return err
	} else if !found {
		httpapi.WriteError(w, http.StatusNotFound, "NOT_FOUND", "Actor not found")
		return nil
	}
	ctx := r.Context()
	row, _, err := pgjson.QueryOne(ctx, rt.DB, `
        INSERT INTO note_synchronization (actor_id, namespace, slug, local_path)
        VALUES ($1, $2, $3, $4)
        ON CONFLICT (actor_id, namespace, slug) DO UPDATE SET local_path = $4
        RETURNING *`, actorID, pgbind.Value(f.Raw("namespace")), slug, pgbind.Value(f.Raw("local_path")))
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string]pgjson.Row{"mapping": row})
}

// syncDelete is POST /admin/notes/sync/delete: one mapping by id, whether
// or not it exists.
func (rt *ContentRoutes) syncDelete(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("id")
	}
	if !f.Truthy("id") {
		return httpapi.BadRequest(w, "Required field: id")
	}
	ctx := r.Context()
	if _, err := rt.DB.Exec(ctx, "DELETE FROM note_synchronization WHERE id = $1", pgbind.Value(f.Raw("id"))); err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
