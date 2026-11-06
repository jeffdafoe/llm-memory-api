package contract

import (
	"fmt"
	"strings"
)

// adminContentCases cover the admin notes, reindex, sync-mapping and share
// routes (admin.ContentRoutes). Each scenario is one route: its successes,
// refusals and the shapes Express answers before the route, as steps.
var adminContentCases = concat(adminNotesCases, adminReindexCases, adminSyncCases, adminShareCases)

const (
	adminNotesList       = "/v1/admin/notes/list"
	adminNotesRead       = "/v1/admin/notes/read"
	adminNotesSave       = "/v1/admin/notes/save"
	adminNotesDelete     = "/v1/admin/notes/delete"
	adminNotesMove       = "/v1/admin/notes/move"
	adminNotesSearch     = "/v1/admin/notes/search"
	adminNotesNamespaces = "/v1/admin/notes/namespaces"
	adminNotesUsage      = "/v1/admin/notes/usage"
	adminReindex         = "/v1/admin/notes/reindex"
	adminReindexStatus   = "/v1/admin/notes/reindex-status"
	adminReindexClear    = "/v1/admin/notes/reindex-clear"
	adminSyncList        = "/v1/admin/notes/sync/list"
	adminSyncSave        = "/v1/admin/notes/sync/save"
	adminSyncDelete      = "/v1/admin/notes/sync/delete"
	adminSharesCreate    = "/v1/admin/shares/create"
	adminSharesRevoke    = "/v1/admin/shares/revoke"
	adminSharesList      = "/v1/admin/shares/list"
	adminSharesWithMe    = "/v1/admin/shares/shared-with-me"
	adminSharesDocuments = "/v1/admin/shares/documents"
	adminSharesForSlug   = "/v1/admin/shares/for-slug"
)

// grantAdmin gives contract-admin namespace access, which the fixture does
// not (it reads only its own namespace). It runs before the scenario's
// first request, so no permission cache holds the old grants.
func grantAdmin(namespace string, read, write, del bool) Step {
	return Step{SQL: fmt.Sprintf(
		`INSERT INTO namespace_permissions (actor_id, namespace, can_read, can_write, can_delete)
         SELECT id, '%s', %t, %t, %t FROM actors WHERE name = 'contract-admin'`, namespace, read, write, del)}
}

// shareToAdmin shares a note pattern of a namespace with contract-admin.
func shareToAdmin(namespace, pattern string, read, write, del bool) Step {
	return Step{SQL: fmt.Sprintf(
		`INSERT INTO note_permissions (owner_namespace, slug_pattern, grantee_actor_id, can_read, can_write, can_delete, granted_by)
         SELECT '%s', '%s', a.id, %t, %t, %t, a.id FROM actors a WHERE a.name = 'contract-admin'`, namespace, pattern, read, write, del)}
}

// contentPost is a JSON POST as contract-admin.
func contentPost(path, body string) Step {
	return Step{Method: "POST", Path: path, Auth: "admin-web", Header: jsonType, Body: body}
}

// routeShape is the steps every admin route gets: no JSON body, the limited
// admin (no notes grant), an agent session (requireUser), OPTIONS, GET and
// an encoded spelling of the path.
func routeShape(path string) []Step {
	last := strings.LastIndex(path, "/")
	encoded := path[:last+1] + fmt.Sprintf("%%%02X", path[last+1]) + path[last+2:]
	return []Step{
		{Label: "no body " + path, Method: "POST", Path: path, Auth: "admin-web"},
		{Label: "limited admin " + path, Method: "POST", Path: path, Auth: "admin-limited-web", Header: jsonType, Body: `{}`},
		{Label: "agent " + path, Method: "POST", Path: path, Auth: "agent-a-session", Header: jsonType, Body: `{}`},
		{Label: "options " + path, Method: "OPTIONS", Path: path, Auth: "admin-web"},
		{Label: "get " + path, Method: "GET", Path: path, Auth: "admin-web"},
		{Label: "encoded " + encoded, Method: "POST", Path: encoded, Auth: "admin-web", Header: jsonType, Body: `{}`},
	}
}

func steps(groups ...[]Step) []Step {
	var out []Step
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

func manyNamespaces(n int) string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf(`"ns%d"`, i)
	}
	return "[" + strings.Join(names, ",") + "]"
}
