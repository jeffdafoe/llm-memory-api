package memory

import (
	"errors"
	"net/http"

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

func (rt *Routes) ingest(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("namespace")
	}
	// Each field is sanitized before the required check, so one that cleans
	// down to "" counts as missing.
	sourceFile, sfOK := f.String("source_file")
	sourceFile = sanitize.Identifier(sourceFile)
	content, cOK := f.String("content")
	content = sanitize.Content(content)
	if !f.Truthy("namespace") || (sfOK && sourceFile == "") || !f.Truthy("source_file") || (cOK && content == "") || !f.Truthy("content") {
		return httpapi.BadRequest(w, "Required fields: namespace, source_file, content")
	}
	namespace, err := f.RequireString("namespace")
	if err != nil {
		return err
	}
	if !sfOK {
		return httpapi.NotString("source_file")
	}
	if !cOK {
		return httpapi.NotString("content")
	}

	if err := permissions.ValidateNamespace(namespace); err != nil {
		return err
	}
	a := httpapi.ActorOf(r)
	if err := rt.Namespaces.RequireAccess(r.Context(), a.ID, a.Name, namespace, permissions.Write); err != nil {
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
		return httpapi.NoBody("namespace")
	}
	a := httpapi.ActorOf(r)
	if f.Truthy("namespace") {
		namespace, err := f.RequireString("namespace")
		if err != nil {
			return err
		}
		if err := permissions.ValidateNamespace(namespace); err != nil {
			return err
		}
		if err := rt.Namespaces.RequireAccess(r.Context(), a.ID, a.Name, namespace, permissions.Read); err != nil {
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
	readable, all, err := rt.Namespaces.ReadableNamespaces(r.Context(), a.ID, a.Kind)
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
		return httpapi.BadRequest(w, "Required field: query")
	}
	if !qOK {
		return httpapi.NotString("query")
	}

	in := SearchInput{Query: query, Limit: jsutil.ToNumber(f.Raw("limit")), SlugPrefix: prefix}
	a := httpapi.ActorOf(r)
	if f.Truthy("namespace") {
		namespace, err := f.RequireString("namespace")
		if err != nil {
			return err
		}
		in.Namespace = namespace
		if namespace != "*" {
			if err := permissions.ValidateNamespace(namespace); err != nil {
				return err
			}
			if err := rt.Namespaces.RequireAccess(r.Context(), a.ID, a.Name, namespace, permissions.Read); err != nil {
				return err
			}
		}
	}
	if in.wildcard() {
		readable, all, err := rt.Namespaces.ReadableNamespaces(r.Context(), a.ID, a.Name)
		if err != nil {
			return err
		}
		in.Readable = readable
		in.FilterReadable = !all
		in.ActorID = a.ID
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
		return httpapi.NoBody("namespace")
	}
	valid, isArray := f.Array("valid_source_files")
	if !f.Truthy("namespace") || !isArray {
		return httpapi.BadRequest(w, "Required fields: namespace (string), valid_source_files (array)")
	}
	namespace, err := f.RequireString("namespace")
	if err != nil {
		return err
	}
	if err := permissions.ValidateNamespace(namespace); err != nil {
		return err
	}
	a := httpapi.ActorOf(r)
	if err := rt.Namespaces.RequireAccess(r.Context(), a.ID, a.Name, namespace, permissions.Delete); err != nil {
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
		return httpapi.NoBody("namespace")
	}
	sourceFile, sfOK := f.String("source_file")
	sourceFile = sanitize.Identifier(sourceFile)
	if !f.Truthy("namespace") || !f.Truthy("source_file") || (sfOK && sourceFile == "") {
		return httpapi.BadRequest(w, "Required fields: namespace, source_file")
	}
	namespace, err := f.RequireString("namespace")
	if err != nil {
		return err
	}
	if !sfOK {
		return httpapi.NotString("source_file")
	}
	if err := permissions.ValidateNamespace(namespace); err != nil {
		return err
	}
	a := httpapi.ActorOf(r)
	if err := rt.Namespaces.RequireAccess(r.Context(), a.ID, a.Name, namespace, permissions.Delete); err != nil {
		return err
	}
	res, err := rt.Service.Delete(r.Context(), namespace, sourceFile)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}
