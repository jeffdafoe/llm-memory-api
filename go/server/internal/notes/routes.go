package notes

import (
	"net/http"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
)

// Routes are the /v1/documents routes (Node's routes/documents.js). Node
// does not sanitize these fields, and neither does the port.
type Routes struct {
	Service    *Service
	Router     *httpapi.Router
	Namespaces *permissions.Namespaces
}

// Register adds the routes to the /v1 mux.
func (rt *Routes) Register(mux *httpapi.RawMux) {
	route := func(path, label string, fn httpapi.HandlerFunc) {
		mux.Handle(http.MethodPost, "/documents/"+path, rt.Router.Route("documents", label, fn))
	}
	route("save", "save", rt.save)
	route("list", "list", rt.list)
	route("read", "read", rt.read)
	route("delete", "delete", rt.delete)
	route("restore", "restore", rt.restore)
	route("edit", "edit", rt.edit)
	route("move", "move", rt.move)
	route("grep", "grep", rt.grep)
}

// access validates the namespace and checks the caller's grant on it.
func (rt *Routes) access(r *http.Request, namespace string, op permissions.Operation) error {
	if err := permissions.ValidateNamespace(namespace); err != nil {
		return err
	}
	a := httpapi.ActorOf(r)
	return rt.Namespaces.RequireAccess(r.Context(), a.ID, a.Name, namespace, op)
}

// requireStrings reads the named fields as strings, after the route's
// required check, in order.
func requireStrings(f httpapi.Fields, names ...string) ([]string, error) {
	out := make([]string, len(names))
	for i, n := range names {
		s, err := f.RequireString(n)
		if err != nil {
			return nil, err
		}
		out[i] = s
	}
	return out, nil
}

func allTruthy(f httpapi.Fields, names ...string) bool {
	for _, n := range names {
		if !f.Truthy(n) {
			return false
		}
	}
	return true
}

func (rt *Routes) save(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("namespace")
	}
	if !allTruthy(f, "namespace", "title", "content") {
		return httpapi.BadRequest(w, "Required fields: namespace, title, content")
	}
	v, err := requireStrings(f, "namespace", "title", "content")
	if err != nil {
		return err
	}
	in := SaveInput{Namespace: v[0], Title: v[1], Content: v[2]}
	if f.Truthy("slug") {
		if s, isString := f.String("slug"); isString {
			in.Slug = s
		} else {
			in.SlugNotString = true
		}
	}
	if in.CreatedBy, err = f.OptionalString("created_by"); err != nil {
		return err
	}
	// Only a JSON true turns upsert on.
	in.Upsert = string(f.Raw("upsert")) == "true"

	if err := rt.access(r, in.Namespace, permissions.Write); err != nil {
		return err
	}
	doc, err := rt.Service.Save(r.Context(), in)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, doc)
}

func (rt *Routes) list(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("namespace")
	}
	if !f.Truthy("namespace") {
		return httpapi.BadRequest(w, "Required field: namespace")
	}
	namespace, err := f.RequireString("namespace")
	if err != nil {
		return err
	}
	in := ListInput{Namespace: namespace, Limit: f.Raw("limit"), Offset: f.Raw("offset"), IncludeDeleted: f.Truthy("include_deleted")}
	if in.Prefix, err = f.OptionalString("prefix"); err != nil {
		return err
	}

	if err := rt.access(r, namespace, permissions.Read); err != nil {
		return err
	}
	res, err := rt.Service.List(r.Context(), in)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}

// namespaceAndSlug handles the routes whose body is a namespace and a slug.
func namespaceAndSlug(w http.ResponseWriter, r *http.Request) (namespace, slug string, done bool, err error) {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return "", "", true, httpapi.NoBody("namespace")
	}
	if !allTruthy(f, "namespace", "slug") {
		return "", "", true, httpapi.BadRequest(w, "Required fields: namespace, slug")
	}
	v, err := requireStrings(f, "namespace", "slug")
	if err != nil {
		return "", "", true, err
	}
	return v[0], v[1], false, nil
}

func (rt *Routes) read(w http.ResponseWriter, r *http.Request) error {
	namespace, slug, done, err := namespaceAndSlug(w, r)
	if done {
		return err
	}
	f, _ := httpapi.BodyFields(r)
	if err := rt.access(r, namespace, permissions.Read); err != nil {
		return err
	}
	note, err := rt.Service.Read(r.Context(), namespace, slug)
	if err != nil {
		return err
	}
	// Without offset or limit the content goes out whole, as before
	// pagination existed (the admin UI and memory-sync read it that way).
	page, err := PaginateContent(note.Content, f.Raw("offset"), f.Raw("limit"))
	if err != nil {
		return err
	}
	if page != nil {
		note.Content = page.Text
		note.TotalLines = &page.TotalLines
	}
	return httpapi.WriteJSON(w, http.StatusOK, note)
}

func (rt *Routes) delete(w http.ResponseWriter, r *http.Request) error {
	namespace, slug, done, err := namespaceAndSlug(w, r)
	if done {
		return err
	}
	if err := rt.access(r, namespace, permissions.Delete); err != nil {
		return err
	}
	res, err := rt.Service.Delete(r.Context(), namespace, slug, nil)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}

func (rt *Routes) restore(w http.ResponseWriter, r *http.Request) error {
	namespace, slug, done, err := namespaceAndSlug(w, r)
	if done {
		return err
	}
	if err := rt.access(r, namespace, permissions.Write); err != nil {
		return err
	}
	res, err := rt.Service.Restore(r.Context(), namespace, slug)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}

func (rt *Routes) edit(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("namespace")
	}
	newRaw := f.Raw("new_string")
	if !allTruthy(f, "namespace", "slug", "old_string") || newRaw == nil {
		return httpapi.BadRequest(w, "Required fields: namespace, slug, old_string, new_string")
	}
	v, err := requireStrings(f, "namespace", "slug", "old_string")
	if err != nil {
		return err
	}
	in := EditInput{Namespace: v[0], Slug: v[1], OldString: v[2], ReplaceAll: f.Truthy("replace_all")}
	// null passes the route's check and fails in the service, as in Node.
	if string(newRaw) != "null" {
		s, err := f.RequireString("new_string")
		if err != nil {
			return err
		}
		in.NewString = &s
	}

	if err := rt.access(r, in.Namespace, permissions.Write); err != nil {
		return err
	}
	res, err := rt.Service.Edit(r.Context(), in)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}

func (rt *Routes) move(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("namespace")
	}
	if !allTruthy(f, "namespace", "slug", "new_slug") {
		return httpapi.BadRequest(w, "Required fields: namespace, slug, new_slug. Optional: new_namespace")
	}
	v, err := requireStrings(f, "namespace", "slug")
	if err != nil {
		return err
	}
	in := MoveInput{Namespace: v[0], Slug: v[1]}
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
	// Write on the source (to remove) and on the target (to create).
	a := httpapi.ActorOf(r)
	if err := rt.Namespaces.RequireAccess(r.Context(), a.ID, a.Name, in.Namespace, permissions.Write); err != nil {
		return err
	}
	if in.NewNamespace != "" && in.NewNamespace != in.Namespace {
		if err := rt.Namespaces.RequireAccess(r.Context(), a.ID, a.Name, in.NewNamespace, permissions.Write); err != nil {
			return err
		}
	}
	res, err := rt.Service.Move(r.Context(), in)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}

func (rt *Routes) grep(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("pattern")
	}
	if !f.Truthy("pattern") {
		return httpapi.BadRequest(w, "Required field: pattern")
	}
	pattern, err := f.RequireString("pattern")
	if err != nil {
		return err
	}
	in := GrepInput{
		Pattern:       pattern,
		Limit:         f.Raw("limit"),
		ContextBefore: f.Raw("context_before"),
		ContextAfter:  f.Raw("context_after"),
		Context:       f.Raw("context"),
		Regex:         string(f.Raw("regex")) == "true",
	}
	if in.Namespace, err = f.OptionalString("namespace"); err != nil {
		return err
	}
	a := httpapi.ActorOf(r)
	if in.Namespace != "" && in.Namespace != "*" {
		if err := rt.access(r, in.Namespace, permissions.Read); err != nil {
			return err
		}
	} else {
		readable, all, err := rt.Namespaces.ReadableNamespaces(r.Context(), a.ID, a.Name)
		if err != nil {
			return err
		}
		in.Readable, in.FilterReadable = readable, !all
	}
	results, err := rt.Service.Grep(r.Context(), in)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, struct {
		Results []GrepResult `json:"results"`
	}{results})
}
