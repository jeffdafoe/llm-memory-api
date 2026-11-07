package admin

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/frontmatter"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/pgbind"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/pgjson"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/sanitize"
)

// templateKinds are the kinds a template may have.
var templateKinds = []string{"welcome", "welcome-note"}

// invalidKind is Node's 400 for a kind outside templateKinds: the value as
// String() wrote it.
func invalidKind(w http.ResponseWriter, raw json.RawMessage) error {
	v, err := jsutil.Parse(raw)
	if err != nil {
		return err
	}
	s, err := jsutil.ValueString(v)
	if err != nil {
		return err
	}
	return httpapi.BadRequest(w, "Invalid kind: "+s)
}

// isTemplateKind is TEMPLATE_KINDS.has(value).
func isTemplateKind(raw json.RawMessage) bool {
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return false
	}
	return contains(templateKinds, s)
}

// templatesList is POST /admin/templates/list: every template without its
// content, by name, of one kind when `kind` is given.
func (rt *Routes) templatesList(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("kind")
	}
	sql := "SELECT id, name, kind, description, created_at, updated_at FROM templates"
	var args []any
	if f.Truthy("kind") {
		if !isTemplateKind(f.Raw("kind")) {
			return invalidKind(w, f.Raw("kind"))
		}
		sql += " WHERE kind = $1"
		args = append(args, pgbind.Value(f.Raw("kind")))
	}
	sql += " ORDER BY name"
	rows, err := pgjson.Query(r.Context(), rt.DB, sql, args...)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string][]pgjson.Row{"templates": rows})
}

// templatesRead is POST /admin/templates/read: one template.
func (rt *Routes) templatesRead(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("id")
	}
	if !f.Truthy("id") {
		return httpapi.BadRequest(w, "Required field: id")
	}
	row, found, err := pgjson.QueryOne(r.Context(), rt.DB, "SELECT * FROM templates WHERE id = $1", pgbind.Value(f.Raw("id")))
	if err != nil {
		return err
	}
	if !found {
		return notFound(w, "Template not found")
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string]pgjson.Row{"template": row})
}

// contentField is sanitize.content(req.body.<name>): a string without the
// dangerous characters. isString is false for any other JSON type, which
// sanitize.content returned as it was.
func contentField(f httpapi.Fields, name string) (value string, truthy, isString bool) {
	if s, ok := f.String(name); ok {
		s = sanitize.Content(s)
		return s, s != "", true
	}
	return "", f.Truthy(name), false
}

// templatesSave is POST /admin/templates/save: updates the template `id`
// names, or creates one. A welcome template's header must have a subject.
func (rt *Routes) templatesSave(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("id")
	}
	name, nameTruthy, nameIsString := contentField(f, "name")
	description, descriptionTruthy, descriptionIsString := contentField(f, "description")
	content, contentTruthy, contentIsString := contentField(f, "content")
	if !nameTruthy || !contentTruthy {
		return httpapi.BadRequest(w, "Required fields: name, content")
	}
	kind := "welcome"
	if f.Truthy("kind") {
		if !isTemplateKind(f.Raw("kind")) {
			return invalidKind(w, f.Raw("kind"))
		}
		kind, _ = f.String("kind")
	}
	if kind == "welcome" {
		// Node read the header with content.match, which a value of another
		// type does not have.
		if !contentIsString {
			return httpapi.NotString("content")
		}
		if header, _ := frontmatter.Parse(content); header["subject"] == "" {
			return httpapi.BadRequest(w, `Welcome templates require a subject in frontmatter (e.g. ---\nsubject: Welcome, {agent}\n---)`)
		}
	}

	// Node stored a value of another type as text. The port refuses it,
	// after the checks Node made before that write: the 404 of an update
	// and the 409 of a new name.
	var typeErr error
	switch {
	case !nameIsString:
		typeErr = httpapi.NotString("name")
	case descriptionTruthy && !descriptionIsString:
		typeErr = httpapi.NotString("description")
	case !contentIsString:
		typeErr = httpapi.NotString("content")
	}
	var descriptionValue any
	if descriptionTruthy {
		descriptionValue = description
	}

	ctx, cancel := detached(r)
	defer cancel()
	var (
		row   pgjson.Row
		found bool
		err   error
	)
	if f.Truthy("id") {
		id := pgbind.Value(f.Raw("id"))
		if typeErr != nil {
			var one int
			err := rt.DB.QueryRow(ctx, "SELECT 1 FROM templates WHERE id = $1", id).Scan(&one)
			if errors.Is(err, pgx.ErrNoRows) {
				return notFound(w, "Template not found")
			}
			if err != nil {
				return err
			}
			return typeErr
		}
		row, found, err = pgjson.QueryOne(ctx, rt.DB,
			"UPDATE templates SET name = $1, kind = $2, description = $3, content = $4, updated_at = NOW() WHERE id = $5 RETURNING *",
			name, kind, descriptionValue, content, id)
		if err != nil {
			return err
		}
		if !found {
			return notFound(w, "Template not found")
		}
	} else {
		var nameArg any = name
		if !nameIsString {
			nameArg = pgbind.Value(f.Raw("name"))
		}
		var existing int
		err := rt.DB.QueryRow(ctx, "SELECT id FROM templates WHERE name = $1", nameArg).Scan(&existing)
		if err == nil {
			httpapi.WriteError(w, http.StatusConflict, "CONFLICT", "A template with that name already exists")
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if typeErr != nil {
			return typeErr
		}
		row, _, err = pgjson.QueryOne(ctx, rt.DB,
			"INSERT INTO templates (name, kind, description, content) VALUES ($1, $2, $3, $4) RETURNING *",
			name, kind, descriptionValue, content)
		if err != nil {
			return err
		}
	}
	id, _ := row.Get("id")
	rt.LogAdmin("template_save", logger.Fields{
		{Key: "template_id", Value: id}, {Key: "name", Value: name}, {Key: "kind", Value: kind}, {Key: "user_id", Value: User(r).ID},
	})
	return httpapi.WriteJSON(w, http.StatusOK, map[string]pgjson.Row{"template": row})
}

// templatesDelete is POST /admin/templates/delete.
func (rt *Routes) templatesDelete(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("id")
	}
	if !f.Truthy("id") {
		return httpapi.BadRequest(w, "Required field: id")
	}
	ctx, cancel := detached(r)
	defer cancel()
	var name string
	err := rt.DB.QueryRow(ctx, "DELETE FROM templates WHERE id = $1 RETURNING name", pgbind.Value(f.Raw("id"))).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound(w, "Template not found")
	}
	if err != nil {
		return err
	}
	rt.LogAdmin("template_delete", logger.Fields{
		{Key: "template_id", Value: f.Raw("id")}, {Key: "name", Value: name}, {Key: "user_id", Value: User(r).ID},
	})
	return httpapi.WriteJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}
