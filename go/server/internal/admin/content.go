package admin

import (
	"net/http"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/chat"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/discussion"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/memory"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
)

// ContentRoutes are the content, log and live-view admin routes: notes,
// shares, sync mappings, the reindex, the comms views (discussions, mail,
// chat), the logs, the dashboard and the funnel.
type ContentRoutes struct {
	*Common
	Config      *config.Store
	Notes       *notes.Service
	Memory      *memory.Service
	Namespaces  *permissions.Namespaces
	Discussions *discussion.Service
	Chat        *chat.Service

	reindex reindexState
}

// Register adds the routes to the admin mux.
func (rt *ContentRoutes) Register(mux *httpapi.RawMux) {
	rt.handle(mux, "/admin/notes/list", "notes", "read", "notes-list", rt.notesList)
	rt.handle(mux, "/admin/notes/read", "notes", "read", "notes-read", rt.notesRead)
	rt.handle(mux, "/admin/notes/save", "notes", "write", "notes-save", rt.notesSave)
	rt.handle(mux, "/admin/notes/delete", "notes", "delete", "notes-delete", rt.notesDelete)
	rt.handle(mux, "/admin/notes/move", "notes", "write", "notes-move", rt.notesMove)
	rt.handle(mux, "/admin/notes/search", "notes", "read", "notes-search", rt.notesSearch)
	rt.handle(mux, "/admin/notes/namespaces", "notes", "read", "notes-namespaces", rt.notesNamespaces)
	rt.handle(mux, "/admin/notes/reindex", "notes", "write", "notes-reindex", rt.notesReindex)
	// These two have no adminRoute wrapper in Node: nothing in them throws.
	mux.Handle(http.MethodPost, "/admin/notes/reindex-status", rt.Admin.RequirePerm("notes", "read", http.HandlerFunc(rt.notesReindexStatus)))
	mux.Handle(http.MethodPost, "/admin/notes/reindex-clear", rt.Admin.RequirePerm("notes", "write", http.HandlerFunc(rt.notesReindexClear)))
	rt.handle(mux, "/admin/notes/usage", "notes", "read", "notes-usage", rt.notesUsage)
	rt.handle(mux, "/admin/notes/sync/list", "notes", "read", "sync-list", rt.syncList)
	rt.handle(mux, "/admin/notes/sync/save", "notes", "write", "sync-save", rt.syncSave)
	rt.handle(mux, "/admin/notes/sync/delete", "notes", "write", "sync-delete", rt.syncDelete)

	rt.handle(mux, "/admin/shares/create", "notes", "write", "shares-create", rt.sharesCreate)
	rt.handle(mux, "/admin/shares/revoke", "notes", "write", "shares-revoke", rt.sharesRevoke)
	rt.handle(mux, "/admin/shares/list", "notes", "read", "shares-list", rt.sharesList)
	rt.handle(mux, "/admin/shares/shared-with-me", "notes", "read", "shares-shared-with-me", rt.sharesSharedWithMe)
	rt.handle(mux, "/admin/shares/documents", "notes", "read", "shares-documents", rt.sharesDocuments)
	rt.handle(mux, "/admin/shares/for-slug", "notes", "read", "shares-for-slug", rt.sharesForSlug)

	rt.handle(mux, "/admin/discussions", "comms", "read", "discussions-list", rt.discussionsList)
	rt.handle(mux, "/admin/discussions/detail", "comms", "read", "discussions-detail", rt.discussionsDetail)
	rt.handle(mux, "/admin/discussions/create", "comms", "write", "discussions-create", rt.discussionsCreate)
	rt.handle(mux, "/admin/discussions/send", "comms", "write", "discussions-send", rt.discussionsSend)
	rt.handle(mux, "/admin/discussions/conclude", "comms", "write", "discussions-conclude", rt.discussionsConclude)
	rt.handle(mux, "/admin/chat", "comms", "read", "chat-list", rt.chatList)
	rt.handle(mux, "/admin/mail", "comms", "read", "mail-list", rt.mailList)
	rt.handle(mux, "/admin/mail/delete", "comms", "delete", "mail-delete", rt.mailDelete)
	rt.handle(mux, "/admin/chat/delete", "comms", "delete", "chat-delete", rt.chatDelete)
	rt.handle(mux, "/admin/mail/send", "comms", "write", "mail-send", rt.mailSend)

	rt.handle(mux, "/admin/dashboard", "dashboard", "read", "dashboard", rt.dashboardSummary)
	rt.handle(mux, "/admin/api-log", "logs", "read", "api-log", rt.apiLog)
	rt.handle(mux, "/admin/error-log", "logs", "read", "error-log", rt.errorLog)
	rt.handle(mux, "/admin/funnel", "access", "read", "funnel", rt.funnel)
}

// handle registers a route whose handler runs on a context detached from the
// request (30 s bound): Node finished a route, its writes included, after
// the client hung up. The permission check before it reads the request.
func (rt *ContentRoutes) handle(mux *httpapi.RawMux, path, resource, action, label string, fn httpapi.HandlerFunc) {
	rt.Handle(mux, path, resource, action, label, func(w http.ResponseWriter, r *http.Request) error {
		ctx, cancel := detached(r)
		defer cancel()
		return fn(w, r.WithContext(ctx))
	})
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
