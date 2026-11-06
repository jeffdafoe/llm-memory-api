package admin

import (
	"net/http"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/memory"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
)

// ContentRoutes are the content, log and live-view admin routes: notes,
// shares, sync mappings, the reindex, and (in later files) the comms views,
// the logs, the dashboard and the funnel.
type ContentRoutes struct {
	*Common
	Config     *config.Store
	Notes      *notes.Service
	Memory     *memory.Service
	Namespaces *permissions.Namespaces

	reindex reindexState
}

// Register adds the routes to the admin mux.
func (rt *ContentRoutes) Register(mux *httpapi.RawMux) {
	rt.Handle(mux, "/admin/notes/list", "notes", "read", "notes-list", rt.notesList)
	rt.Handle(mux, "/admin/notes/read", "notes", "read", "notes-read", rt.notesRead)
	rt.Handle(mux, "/admin/notes/save", "notes", "write", "notes-save", rt.notesSave)
	rt.Handle(mux, "/admin/notes/delete", "notes", "delete", "notes-delete", rt.notesDelete)
	rt.Handle(mux, "/admin/notes/move", "notes", "write", "notes-move", rt.notesMove)
	rt.Handle(mux, "/admin/notes/search", "notes", "read", "notes-search", rt.notesSearch)
	rt.Handle(mux, "/admin/notes/namespaces", "notes", "read", "notes-namespaces", rt.notesNamespaces)
	rt.Handle(mux, "/admin/notes/reindex", "notes", "write", "notes-reindex", rt.notesReindex)
	// These two have no adminRoute wrapper in Node: nothing in them throws.
	mux.Handle(http.MethodPost, "/admin/notes/reindex-status", rt.Admin.RequirePerm("notes", "read", http.HandlerFunc(rt.notesReindexStatus)))
	mux.Handle(http.MethodPost, "/admin/notes/reindex-clear", rt.Admin.RequirePerm("notes", "write", http.HandlerFunc(rt.notesReindexClear)))
	rt.Handle(mux, "/admin/notes/usage", "notes", "read", "notes-usage", rt.notesUsage)
	rt.Handle(mux, "/admin/notes/sync/list", "notes", "read", "sync-list", rt.syncList)
	rt.Handle(mux, "/admin/notes/sync/save", "notes", "write", "sync-save", rt.syncSave)
	rt.Handle(mux, "/admin/notes/sync/delete", "notes", "write", "sync-delete", rt.syncDelete)

	rt.Handle(mux, "/admin/shares/create", "notes", "write", "shares-create", rt.sharesCreate)
	rt.Handle(mux, "/admin/shares/revoke", "notes", "write", "shares-revoke", rt.sharesRevoke)
	rt.Handle(mux, "/admin/shares/list", "notes", "read", "shares-list", rt.sharesList)
	rt.Handle(mux, "/admin/shares/shared-with-me", "notes", "read", "shares-shared-with-me", rt.sharesSharedWithMe)
	rt.Handle(mux, "/admin/shares/documents", "notes", "read", "shares-documents", rt.sharesDocuments)
	rt.Handle(mux, "/admin/shares/for-slug", "notes", "read", "shares-for-slug", rt.sharesForSlug)
}

// caller is the admin user as Node's routes name it for namespace checks:
// req.actorId and req.authenticatedUser.username, actor type 'user'.
func caller(r *http.Request) (id int, name string) {
	return httpapi.StateOf(r).ActorID, User(r).Username
}

// requireAccess is requireAccess(req.actorId, username, 'user', ns, op).
func (rt *ContentRoutes) requireAccess(r *http.Request, namespace string, op permissions.Operation) error {
	id, name := caller(r)
	return rt.Namespaces.RequireAccess(r.Context(), id, name, namespace, op)
}

// readable is getReadableNamespaces for the caller: nil and all true for a
// wildcard reader (Node's null).
func (rt *ContentRoutes) readable(r *http.Request) (namespaces []string, all bool, err error) {
	id, name := caller(r)
	return rt.Namespaces.ReadableNamespaces(r.Context(), id, name)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
