package memory

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/sanitize"
)

// Routes are the /v1/memory routes (Node's routes/memory.js).
type Routes struct {
	Service    *Service
	Router     *httpapi.Router
	Namespaces *permissions.Namespaces
}

// Register adds the routes to the /v1 mux.
func (rt *Routes) Register(mux *httpapi.RawMux) {
	mux.Handle(http.MethodPost, "/memory/ingest", rt.Router.Route("memory", "ingest", rt.ingest))
	mux.Handle(http.MethodPost, "/memory/ingest/status", rt.Router.Route("memory", "ingest-status", rt.ingestStatus))
	mux.Handle(http.MethodPost, "/memory/search", rt.Router.Route("memory", "search", rt.search))
	mux.Handle(http.MethodPost, "/memory/cleanup", rt.Router.Route("memory", "cleanup", rt.cleanup))
	mux.Handle(http.MethodPost, "/memory/delete", rt.Router.Route("memory", "delete", rt.delete))
}

// actor is the caller as the permission checks name it.
type actor struct {
	id   int
	name string
	kind string // "agent" or "user"
}

func actorOf(r *http.Request) actor {
	st := httpapi.StateOf(r)
	a := actor{id: st.ActorID, name: st.AuthenticatedAgent, kind: "agent"}
	if a.name == "" {
		a.kind = "user"
		if st.AuthenticatedUser != nil && st.AuthenticatedUser.Username != "" {
			a.name = st.AuthenticatedUser.Username
		} else {
			a.name = "unknown"
		}
	}
	return a
}

// noBody is the TypeError Node threw when a route read a field from an
// undefined req.body (no JSON body). The text is V8's, so the error_log row
// matches.
func noBody(destructured string) error {
	return errors.New("Cannot destructure property '" + destructured + "' of 'req.body' as it is undefined.")
}

// notString refuses a truthy field of a JSON type the route cannot use.
// Node passed such a value on, where it failed in some later step or was
// used as text; the port refuses it (go-server note, Known differences).
func notString(name string) error {
	return apierr.New(http.StatusBadRequest, fmt.Sprintf("%s must be a string", name))
}

// badRequest answers a missing required field. Node wrote these 400s with
// res.status(400).json() instead of throwing, so the route wrapper never
// logged them to error_log.
func badRequest(w http.ResponseWriter, message string) error {
	httpapi.WriteError(w, http.StatusBadRequest, "BAD_REQUEST", message)
	return nil
}

// requireString reads a string field after the route's required-fields
// check: a truthy field that is not a string is refused.
func requireString(f httpapi.Fields, name string) (string, error) {
	s, ok := f.String(name)
	if !ok {
		return "", notString(name)
	}
	return s, nil
}

func (rt *Routes) ingest(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return noBody("namespace")
	}
	// Each field is sanitized before the required check, so one that cleans
	// down to "" counts as missing.
	sourceFile, sfOK := f.String("source_file")
	sourceFile = sanitize.Identifier(sourceFile)
	content, cOK := f.String("content")
	content = sanitize.Content(content)
	if !f.Truthy("namespace") || (sfOK && sourceFile == "") || !f.Truthy("source_file") || (cOK && content == "") || !f.Truthy("content") {
		return badRequest(w, "Required fields: namespace, source_file, content")
	}
	namespace, err := requireString(f, "namespace")
	if err != nil {
		return err
	}
	if !sfOK {
		return notString("source_file")
	}
	if !cOK {
		return notString("content")
	}

	if err := permissions.ValidateNamespace(namespace); err != nil {
		return err
	}
	a := actorOf(r)
	if err := rt.Namespaces.RequireAccess(r.Context(), a.id, a.name, namespace, permissions.Write); err != nil {
		return err
	}
	res, err := rt.Service.Ingest(r.Context(), namespace, sourceFile, content)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}

func (rt *Routes) ingestStatus(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return noBody("namespace")
	}
	a := actorOf(r)
	if f.Truthy("namespace") {
		namespace, err := requireString(f, "namespace")
		if err != nil {
			return err
		}
		if err := permissions.ValidateNamespace(namespace); err != nil {
			return err
		}
		if err := rt.Namespaces.RequireAccess(r.Context(), a.id, a.name, namespace, permissions.Read); err != nil {
			return err
		}
		res, err := rt.Service.Status(r.Context(), namespace)
		if err != nil {
			return err
		}
		return httpapi.WriteJSON(w, http.StatusOK, res)
	}

	// Node passes the actor TYPE where the name goes here, so the implicit
	// own namespace is "agent" or "user", and a wildcard reader (null list)
	// makes the filter throw. Kept as Node does it until LLM-918 fixes both.
	readable, all, err := rt.Namespaces.ReadableNamespaces(r.Context(), a.id, a.kind)
	if err != nil {
		return err
	}
	res, err := rt.Service.Status(r.Context(), "")
	if err != nil {
		return err
	}
	// The throw happened inside the filter callback, which never runs for
	// an empty list.
	if all && len(res.Files) > 0 {
		return errors.New("Cannot read properties of null (reading 'includes')")
	}
	allowed := map[string]bool{}
	for _, ns := range readable {
		allowed[ns] = true
	}
	filtered := StatusResult{Files: []StatusFile{}}
	for _, file := range res.Files {
		if allowed[file.Namespace] {
			filtered.Files = append(filtered.Files, file)
		}
	}
	return httpapi.WriteJSON(w, http.StatusOK, filtered)
}

func (rt *Routes) search(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return errors.New("Cannot read properties of undefined (reading 'query')")
	}
	query, qOK := f.String("query")
	query = sanitize.Content(query)
	// A slug prefix that is not a string was ignored in Node (searchMemory
	// tests typeof); one that sanitizes to "" is no filter.
	prefix := ""
	if p, isString := f.String("slug_prefix"); isString {
		prefix = sanitize.Identifier(p)
	}
	if !f.Truthy("query") || (qOK && query == "") {
		return badRequest(w, "Required field: query")
	}
	if !qOK {
		return notString("query")
	}

	in := SearchInput{Query: query, Limit: jsutil.ToNumber(f.Raw("limit")), SlugPrefix: prefix}
	a := actorOf(r)
	if f.Truthy("namespace") {
		namespace, err := requireString(f, "namespace")
		if err != nil {
			return err
		}
		in.Namespace = namespace
		if namespace != "*" {
			if err := permissions.ValidateNamespace(namespace); err != nil {
				return err
			}
			if err := rt.Namespaces.RequireAccess(r.Context(), a.id, a.name, namespace, permissions.Read); err != nil {
				return err
			}
		}
	}
	if in.wildcard() {
		readable, all, err := rt.Namespaces.ReadableNamespaces(r.Context(), a.id, a.name)
		if err != nil {
			return err
		}
		in.Readable = readable
		in.FilterReadable = !all
		in.ActorID = a.id
	}
	res, err := rt.Service.Search(r.Context(), in)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}

func (rt *Routes) cleanup(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return noBody("namespace")
	}
	valid, isArray := f.Array("valid_source_files")
	if !f.Truthy("namespace") || !isArray {
		return badRequest(w, "Required fields: namespace (string), valid_source_files (array)")
	}
	namespace, err := requireString(f, "namespace")
	if err != nil {
		return err
	}
	if err := permissions.ValidateNamespace(namespace); err != nil {
		return err
	}
	a := actorOf(r)
	if err := rt.Namespaces.RequireAccess(r.Context(), a.id, a.name, namespace, permissions.Delete); err != nil {
		return err
	}
	res, err := rt.Service.Cleanup(r.Context(), namespace, valid)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}

func (rt *Routes) delete(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return noBody("namespace")
	}
	sourceFile, sfOK := f.String("source_file")
	sourceFile = sanitize.Identifier(sourceFile)
	if !f.Truthy("namespace") || !f.Truthy("source_file") || (sfOK && sourceFile == "") {
		return badRequest(w, "Required fields: namespace, source_file")
	}
	namespace, err := requireString(f, "namespace")
	if err != nil {
		return err
	}
	if !sfOK {
		return notString("source_file")
	}
	if err := permissions.ValidateNamespace(namespace); err != nil {
		return err
	}
	a := actorOf(r)
	if err := rt.Namespaces.RequireAccess(r.Context(), a.id, a.name, namespace, permissions.Delete); err != nil {
		return err
	}
	res, err := rt.Service.Delete(r.Context(), namespace, sourceFile)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}
