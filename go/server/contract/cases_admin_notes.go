package contract

// bs is one backslash, for JSON escapes in request bodies (\u200b).
const bs = `\`

// A grant must come before the scenario's first request that checks
// contract-admin's namespace access: both servers cache the grants for two
// minutes, so a grant made later is not seen. Shares (note_permissions) are
// not cached.
var adminNotesCases = []Scenario{
	{Name: "admin notes list", Steps: steps(routeShape(adminNotesList), []Step{
		// No grant: only contract-admin's own namespace is readable.
		contentPost(adminNotesList, `{"namespace":"agent-a"}`),
		contentPost(adminNotesList, `{"namespace":"contract-admin"}`),
		contentPost(adminNotesList, `{"namespaces":["contract-admin","agent-a"]}`),
		contentPost(adminNotesList, `{}`),
		contentPost(adminNotesList, `{"namespace":""}`),
		contentPost(adminNotesList, `{"namespace":"*"}`),
		contentPost(adminNotesList, `{"namespaces":"agent-a"}`),
		contentPost(adminNotesList, `{"namespaces":null}`),
		contentPost(adminNotesList, `{"namespaces":["agent-a",""]}`),
		contentPost(adminNotesList, `{"namespaces":["agent-a",7]}`),
		contentPost(adminNotesList, `{"namespaces":`+manyNamespaces(501)+`}`),
		contentPost(adminNotesList, `{"namespaces":[]}`),
		contentPost(adminNotesList, `{"namespaces":["contract-admin","*"]}`),
	})},
	{Name: "admin notes list with access", Steps: []Step{
		grantAdmin("/", true, false, false),
		contentPost(adminNotesList, `{"namespace":"agent-a"}`),
		contentPost(adminNotesList, `{"namespace":"agent-a","prefix":"notes/","limit":2,"offset":1}`),
		contentPost(adminNotesList, `{"namespace":"agent-a","limit":0}`),
		contentPost(adminNotesList, `{"namespaces":["shared","agent-a","private-ns"],"limit":3}`),
		contentPost(adminNotesList, `{"namespaces":["agent-a"],"prefix":"anne_"}`),
		contentPost(adminNotesList, `{"namespaces":["agent-a"],"limit":"x"}`),
	}},
	{Name: "admin notes read", Steps: steps(routeShape(adminNotesRead), []Step{
		shareToAdmin("private-ns", "notes/codebase/", true, false, false),
		contentPost(adminNotesRead, `{"namespace":"agent-a","slug":"notes/codebase/auth-middleware"}`),
		contentPost(adminNotesRead, `{"namespace":"agent-a"}`),
		contentPost(adminNotesRead, `{"namespace":"*","slug":"x"}`),
		// Read through the folder share.
		contentPost(adminNotesRead, `{"namespace":"private-ns","slug":"notes/codebase/not-shared"}`),
		contentPost(adminNotesRead, `{"namespace":"private-ns","slug":"notes/other"}`),
		contentPost(adminNotesRead, `{"namespace":"private-ns","slug":"notes/codebase/missing"}`),
		{Settle: true},
	})},
	{Name: "admin notes read with access", Steps: []Step{
		grantAdmin("agent-a", true, false, false),
		contentPost(adminNotesRead, `{"namespace":"agent-a","slug":"Notes/Codebase/Auth-Middleware"}`),
		contentPost(adminNotesRead, `{"namespace":"agent-a","slug":"no/such/note"}`),
		{Settle: true},
	}},
	{Name: "admin notes save", Steps: steps(routeShape(adminNotesSave), []Step{
		shareToAdmin("private-ns", "notes/codebase/shared-with-b", true, true, false),
		contentPost(adminNotesSave, `{"namespace":"agent-a","slug":"notes/x","title":"T","content":"c"}`),
		contentPost(adminNotesSave, `{"namespace":"agent-a","slug":"notes/x","title":"T"}`),
		contentPost(adminNotesSave, `{"namespace":"agent-a","slug":"  %%  ","title":"T","content":"c"}`),
		contentPost(adminNotesSave, `{"namespace":"agent-a","slug":"notes/x","title":"`+bs+`u200b","content":"c"}`),
		contentPost(adminNotesSave, `{"namespace":"*","slug":"notes/x","title":"T","content":"c"}`),
		// Saved through the share's write flag.
		contentPost(adminNotesSave, `{"namespace":"private-ns","slug":"notes/codebase/shared-with-b","title":"Shared","content":"## Shared\nEdited by the admin."}`),
		{Settle: true},
	})},
	{Name: "admin notes save with access", Steps: []Step{
		grantAdmin("agent-a", true, true, false),
		contentPost(adminNotesSave, `{"namespace":"agent-a","slug":"  notes/admin-made!  ","title":"Admin`+bs+`u0007 made","content":"# Admin\n\nMade`+bs+`u200b here.","extension":".MD"}`),
		{Settle: true},
		contentPost(adminNotesSave, `{"namespace":"agent-a","slug":"notes/admin-made","title":"Again","content":"## Again\nReplaced.","extension":"x"}`),
		{Settle: true},
		contentPost(adminNotesSave, `{"namespace":"agent-a","slug":"notes/empty","title":"T","content":""}`),
		contentPost(adminNotesSave, `{"namespace":"agent-a","slug":"notes/empty","title":"T","content":null}`),
		contentPost(adminNotesSave, `{"namespace":"agent-a","slug":"agent-a/notes/x","title":"T","content":"c"}`),
		contentPost(adminNotesSave, `{"namespace":"agent-a","slug":"notes//x","title":"T","content":"c"}`),
	}},
	{Name: "admin notes delete", Steps: steps(routeShape(adminNotesDelete), []Step{
		shareToAdmin("private-ns", "notes/codebase/not-shared", true, false, true),
		contentPost(adminNotesDelete, `{"namespace":"agent-a","slug":"notes/codebase/auth-middleware"}`),
		contentPost(adminNotesDelete, `{"slug":"x"}`),
		contentPost(adminNotesDelete, `{"namespace":"private-ns","slug":"notes/codebase/not-shared"}`),
		{Settle: true},
	})},
	{Name: "admin notes delete with access", Steps: []Step{
		grantAdmin("agent-a", true, true, true),
		contentPost(adminNotesDelete, `{"namespace":"agent-a","slug":"notes/codebase/auth-middleware"}`),
		{Settle: true},
		contentPost(adminNotesDelete, `{"namespace":"agent-a","slug":"notes/codebase/auth-middleware"}`),
	}},
	{Name: "admin notes move", Steps: steps(routeShape(adminNotesMove), []Step{
		contentPost(adminNotesMove, `{"namespace":"agent-a","slug":"notes/codebase/auth-middleware","new_slug":"notes/moved"}`),
		contentPost(adminNotesMove, `{"namespace":"agent-a","slug":"x"}`),
		contentPost(adminNotesMove, `{"namespace":"agent-a","slug":"x","new_slug":"y","new_namespace":"*"}`),
	})},
	{Name: "admin notes move with access", Steps: []Step{
		grantAdmin("agent-a", true, true, false),
		// Write on agent-a only: a move into shared is refused.
		contentPost(adminNotesMove, `{"namespace":"agent-a","slug":"notes/codebase/auth-middleware","new_slug":"notes/moved","new_namespace":"shared"}`),
		contentPost(adminNotesMove, `{"namespace":"agent-a","slug":"notes/codebase/auth-middleware","new_slug":"notes/moved","new_namespace":"agent-a"}`),
		{Settle: true},
		contentPost(adminNotesMove, `{"namespace":"agent-a","slug":"notes/moved","new_slug":"/bad"}`),
		contentPost(adminNotesMove, `{"namespace":"agent-a","slug":"no/such","new_slug":"x"}`),
		contentPost(adminNotesMove, `{"namespace":"agent-a","slug":"notes/moved","new_slug":"notes/codebase/dream-lock"}`),
	}},
	{Name: "admin notes search", Steps: steps(routeShape(adminNotesSearch), []Step{
		contentPost(adminNotesSearch, `{}`),
		contentPost(adminNotesSearch, `{"query":""}`),
		contentPost(adminNotesSearch, `{"query":"auth middleware work","namespace":"agent-a"}`),
		contentPost(adminNotesSearch, `{"query":"x","namespace":"/"}`),
		contentPost(adminNotesSearch, `{"query":"x","limit":{"toString":1}}`),
	})},
	{Name: "admin notes search across namespaces", Steps: []Step{
		shareToAdmin("private-ns", "notes/codebase/shared-with-b", true, false, false),
		contentPost(adminNotesSearch, `{"query":"auth middleware work","limit":"3"}`),
		{Settle: true},
	}},
	{Name: "admin notes search as a wildcard reader", Steps: []Step{
		grantAdmin("/", true, false, false),
		contentPost(adminNotesSearch, `{"query":"dream lock","namespace":"*","limit":2}`),
		{Settle: true},
	}},
	{Name: "admin notes namespaces", Steps: steps(routeShape(adminNotesNamespaces), []Step{
		contentPost(adminNotesNamespaces, `{}`),
	})},
	{Name: "admin notes namespaces with access", Steps: []Step{
		grantAdmin("shared", true, false, false),
		grantAdmin("quota-ns", true, false, false),
		contentPost(adminNotesNamespaces, `{}`),
	}},
	{Name: "admin notes namespaces as a wildcard reader", Steps: []Step{
		grantAdmin("/", true, false, false),
		contentPost(adminNotesNamespaces, `{}`),
	}},
	{Name: "admin notes usage", Steps: steps(routeShape(adminNotesUsage), []Step{
		contentPost(adminNotesUsage, `{}`),
		contentPost(adminNotesUsage, `{"namespace":"quota-ns"}`),
		contentPost(adminNotesUsage, `{"namespace":"no-such-ns"}`),
		contentPost(adminNotesUsage, `{"namespace":""}`),
		contentPost(adminNotesUsage, `{"namespace":["quota-ns"]}`),
	})},
}

var adminReindexCases = []Scenario{
	// The route reads no body, so its no-body step already starts a reindex;
	// the status steps come once it is done (a status mid-run depends on
	// timing).
	{Name: "admin notes reindex", Steps: steps(routeShape(adminReindex), []Step{{Settle: true}}, routeShape(adminReindexStatus), routeShape(adminReindexClear), []Step{
		contentPost(adminReindexStatus, `{}`),
		contentPost(adminReindexClear, `{}`),
		contentPost(adminReindex, `{}`),
		{Settle: true},
		contentPost(adminReindexStatus, `{}`),
		contentPost(adminReindexClear, `{}`),
		contentPost(adminReindexStatus, `{}`),
	})},
}

var adminSyncCases = []Scenario{
	{Name: "admin notes sync", Steps: steps(routeShape(adminSyncList), routeShape(adminSyncSave), routeShape(adminSyncDelete), []Step{
		{Method: "POST", Path: adminSyncList, Auth: "admin-web", Header: jsonType, Body: `{}`,
			Capture: map[string]string{"aid": "mappings.0.actor_id", "mid": "mappings.0.id"}},
		contentPost(adminSyncList, `{"actor_id":{{aid}}}`),
		contentPost(adminSyncList, `{"actor_id":"{{aid}}"}`),
		contentPost(adminSyncList, `{"actor_id":999999}`),
		contentPost(adminSyncSave, `{"actor_id":{{aid}},"namespace":"agent-a","local_path":"C:/sync/agent-a"}`),
		contentPost(adminSyncSave, `{"actor_id":{{aid}},"namespace":"agent-a","slug":"","local_path":"C:/sync/again"}`),
		contentPost(adminSyncSave, `{"actor_id":{{aid}},"namespace":"shared","slug":"notes/","local_path":"C:/sync/shared"}`),
		contentPost(adminSyncSave, `{"actor_id":999999,"namespace":"agent-a","local_path":"x"}`),
		contentPost(adminSyncSave, `{"actor_id":"abc","namespace":"agent-a","local_path":"x"}`),
		contentPost(adminSyncSave, `{"actor_id":{{aid}},"namespace":"agent-a"}`),
		contentPost(adminSyncDelete, `{"id":{{mid}}}`),
		contentPost(adminSyncDelete, `{"id":999999}`),
		contentPost(adminSyncDelete, `{}`),
		contentPost(adminSyncList, `{}`),
	})},
}
