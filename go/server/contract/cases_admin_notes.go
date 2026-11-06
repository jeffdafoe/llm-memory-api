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
		adminPost(adminNotesList, `{"namespace":"agent-a"}`),
		adminPost(adminNotesList, `{"namespace":"contract-admin"}`),
		adminPost(adminNotesList, `{"namespaces":["contract-admin","agent-a"]}`),
		adminPost(adminNotesList, `{}`),
		adminPost(adminNotesList, `{"namespace":""}`),
		adminPost(adminNotesList, `{"namespace":"*"}`),
		adminPost(adminNotesList, `{"namespaces":"agent-a"}`),
		adminPost(adminNotesList, `{"namespaces":null}`),
		adminPost(adminNotesList, `{"namespaces":["agent-a",""]}`),
		adminPost(adminNotesList, `{"namespaces":["agent-a",7]}`),
		adminPost(adminNotesList, `{"namespaces":`+manyNamespaces(501)+`}`),
		adminPost(adminNotesList, `{"namespaces":[]}`),
		adminPost(adminNotesList, `{"namespaces":["contract-admin","*"]}`),
	})},
	{Name: "admin notes list with access", Steps: []Step{
		grantAdmin("/", true, false, false),
		adminPost(adminNotesList, `{"namespace":"agent-a"}`),
		adminPost(adminNotesList, `{"namespace":"agent-a","prefix":"notes/","limit":2,"offset":1}`),
		adminPost(adminNotesList, `{"namespace":"agent-a","limit":0}`),
		adminPost(adminNotesList, `{"namespaces":["shared","agent-a","private-ns"],"limit":3}`),
		adminPost(adminNotesList, `{"namespaces":["agent-a"],"prefix":"anne_"}`),
		adminPost(adminNotesList, `{"namespaces":["agent-a"],"limit":"x"}`),
	}},
	{Name: "admin notes read", Steps: steps(routeShape(adminNotesRead), []Step{
		shareToAdmin("private-ns", "notes/codebase/", true, false, false),
		adminPost(adminNotesRead, `{"namespace":"agent-a","slug":"notes/codebase/auth-middleware"}`),
		adminPost(adminNotesRead, `{"namespace":"agent-a"}`),
		adminPost(adminNotesRead, `{"namespace":"*","slug":"x"}`),
		// Read through the folder share.
		adminPost(adminNotesRead, `{"namespace":"private-ns","slug":"notes/codebase/not-shared"}`),
		adminPost(adminNotesRead, `{"namespace":"private-ns","slug":"notes/other"}`),
		adminPost(adminNotesRead, `{"namespace":"private-ns","slug":"notes/codebase/missing"}`),
		{Settle: true},
	})},
	{Name: "admin notes read with access", Steps: []Step{
		grantAdmin("agent-a", true, false, false),
		adminPost(adminNotesRead, `{"namespace":"agent-a","slug":"Notes/Codebase/Auth-Middleware"}`),
		adminPost(adminNotesRead, `{"namespace":"agent-a","slug":"no/such/note"}`),
		{Settle: true},
	}},
	{Name: "admin notes save", Steps: steps(routeShape(adminNotesSave), []Step{
		shareToAdmin("private-ns", "notes/codebase/shared-with-b", true, true, false),
		adminPost(adminNotesSave, `{"namespace":"agent-a","slug":"notes/x","title":"T","content":"c"}`),
		adminPost(adminNotesSave, `{"namespace":"agent-a","slug":"notes/x","title":"T"}`),
		adminPost(adminNotesSave, `{"namespace":"agent-a","slug":"  %%  ","title":"T","content":"c"}`),
		adminPost(adminNotesSave, `{"namespace":"agent-a","slug":"notes/x","title":"`+bs+`u200b","content":"c"}`),
		adminPost(adminNotesSave, `{"namespace":"*","slug":"notes/x","title":"T","content":"c"}`),
		// Saved through the share's write flag.
		adminPost(adminNotesSave, `{"namespace":"private-ns","slug":"notes/codebase/shared-with-b","title":"Shared","content":"## Shared\nEdited by the admin."}`),
		{Settle: true},
	})},
	{Name: "admin notes save with access", Steps: []Step{
		grantAdmin("agent-a", true, true, false),
		adminPost(adminNotesSave, `{"namespace":"agent-a","slug":"  notes/admin-made!  ","title":"Admin`+bs+`u0007 made","content":"# Admin\n\nMade`+bs+`u200b here.","extension":".MD"}`),
		{Settle: true},
		adminPost(adminNotesSave, `{"namespace":"agent-a","slug":"notes/admin-made","title":"Again","content":"## Again\nReplaced.","extension":"x"}`),
		{Settle: true},
		adminPost(adminNotesSave, `{"namespace":"agent-a","slug":"notes/empty","title":"T","content":""}`),
		adminPost(adminNotesSave, `{"namespace":"agent-a","slug":"notes/empty","title":"T","content":null}`),
		adminPost(adminNotesSave, `{"namespace":"agent-a","slug":"agent-a/notes/x","title":"T","content":"c"}`),
		adminPost(adminNotesSave, `{"namespace":"agent-a","slug":"notes//x","title":"T","content":"c"}`),
	}},
	{Name: "admin notes delete", Steps: steps(routeShape(adminNotesDelete), []Step{
		shareToAdmin("private-ns", "notes/codebase/not-shared", true, false, true),
		adminPost(adminNotesDelete, `{"namespace":"agent-a","slug":"notes/codebase/auth-middleware"}`),
		adminPost(adminNotesDelete, `{"slug":"x"}`),
		adminPost(adminNotesDelete, `{"namespace":"private-ns","slug":"notes/codebase/not-shared"}`),
		{Settle: true},
	})},
	{Name: "admin notes delete with access", Steps: []Step{
		grantAdmin("agent-a", true, true, true),
		adminPost(adminNotesDelete, `{"namespace":"agent-a","slug":"notes/codebase/auth-middleware"}`),
		{Settle: true},
		adminPost(adminNotesDelete, `{"namespace":"agent-a","slug":"notes/codebase/auth-middleware"}`),
	}},
	{Name: "admin notes move", Steps: steps(routeShape(adminNotesMove), []Step{
		adminPost(adminNotesMove, `{"namespace":"agent-a","slug":"notes/codebase/auth-middleware","new_slug":"notes/moved"}`),
		adminPost(adminNotesMove, `{"namespace":"agent-a","slug":"x"}`),
		adminPost(adminNotesMove, `{"namespace":"agent-a","slug":"x","new_slug":"y","new_namespace":"*"}`),
	})},
	{Name: "admin notes move with access", Steps: []Step{
		grantAdmin("agent-a", true, true, false),
		// Write on agent-a only: a move into shared is refused.
		adminPost(adminNotesMove, `{"namespace":"agent-a","slug":"notes/codebase/auth-middleware","new_slug":"notes/moved","new_namespace":"shared"}`),
		adminPost(adminNotesMove, `{"namespace":"agent-a","slug":"notes/codebase/auth-middleware","new_slug":"notes/moved","new_namespace":"agent-a"}`),
		{Settle: true},
		adminPost(adminNotesMove, `{"namespace":"agent-a","slug":"notes/moved","new_slug":"/bad"}`),
		adminPost(adminNotesMove, `{"namespace":"agent-a","slug":"no/such","new_slug":"x"}`),
		adminPost(adminNotesMove, `{"namespace":"agent-a","slug":"notes/moved","new_slug":"notes/codebase/dream-lock"}`),
	}},
	{Name: "admin notes search", Steps: steps(routeShape(adminNotesSearch), []Step{
		adminPost(adminNotesSearch, `{}`),
		adminPost(adminNotesSearch, `{"query":""}`),
		adminPost(adminNotesSearch, `{"query":"auth middleware work","namespace":"agent-a"}`),
		adminPost(adminNotesSearch, `{"query":"x","namespace":"/"}`),
		adminPost(adminNotesSearch, `{"query":"x","limit":{"toString":1}}`),
	})},
	{Name: "admin notes search across namespaces", Steps: []Step{
		shareToAdmin("private-ns", "notes/codebase/shared-with-b", true, false, false),
		adminPost(adminNotesSearch, `{"query":"auth middleware work","limit":"3"}`),
		{Settle: true},
	}},
	{Name: "admin notes search as a wildcard reader", Steps: []Step{
		grantAdmin("/", true, false, false),
		adminPost(adminNotesSearch, `{"query":"dream lock","namespace":"*","limit":2}`),
		{Settle: true},
	}},
	{Name: "admin notes namespaces", Steps: steps(routeShape(adminNotesNamespaces), []Step{
		adminPost(adminNotesNamespaces, `{}`),
	})},
	{Name: "admin notes namespaces with access", Steps: []Step{
		grantAdmin("shared", true, false, false),
		grantAdmin("quota-ns", true, false, false),
		adminPost(adminNotesNamespaces, `{}`),
	}},
	{Name: "admin notes namespaces as a wildcard reader", Steps: []Step{
		grantAdmin("/", true, false, false),
		adminPost(adminNotesNamespaces, `{}`),
	}},
	{Name: "admin notes usage", Steps: steps(routeShape(adminNotesUsage), []Step{
		adminPost(adminNotesUsage, `{}`),
		adminPost(adminNotesUsage, `{"namespace":"quota-ns"}`),
		adminPost(adminNotesUsage, `{"namespace":"no-such-ns"}`),
		adminPost(adminNotesUsage, `{"namespace":""}`),
		adminPost(adminNotesUsage, `{"namespace":["quota-ns"]}`),
	})},
}

var adminReindexCases = []Scenario{
	// The route reads no body, so its no-body step already starts a reindex;
	// the status steps come once it is done (a status mid-run depends on
	// timing).
	{Name: "admin notes reindex", Steps: steps(routeShape(adminReindex), []Step{{Settle: true}}, routeShape(adminReindexStatus), routeShape(adminReindexClear), []Step{
		adminPost(adminReindexStatus, `{}`),
		adminPost(adminReindexClear, `{}`),
		adminPost(adminReindex, `{}`),
		{Settle: true},
		adminPost(adminReindexStatus, `{}`),
		adminPost(adminReindexClear, `{}`),
		adminPost(adminReindexStatus, `{}`),
	})},
}

var adminSyncCases = []Scenario{
	{Name: "admin notes sync", Steps: steps(routeShape(adminSyncList), routeShape(adminSyncSave), routeShape(adminSyncDelete), []Step{
		{Method: "POST", Path: adminSyncList, Auth: "admin-web", Header: jsonType, Body: `{}`,
			Capture: map[string]string{"aid": "mappings.0.actor_id", "mid": "mappings.0.id"}},
		adminPost(adminSyncList, `{"actor_id":{{aid}}}`),
		adminPost(adminSyncList, `{"actor_id":"{{aid}}"}`),
		adminPost(adminSyncList, `{"actor_id":999999}`),
		adminPost(adminSyncSave, `{"actor_id":{{aid}},"namespace":"agent-a","local_path":"C:/sync/agent-a"}`),
		adminPost(adminSyncSave, `{"actor_id":{{aid}},"namespace":"agent-a","slug":"","local_path":"C:/sync/again"}`),
		adminPost(adminSyncSave, `{"actor_id":{{aid}},"namespace":"shared","slug":"notes/","local_path":"C:/sync/shared"}`),
		adminPost(adminSyncSave, `{"actor_id":999999,"namespace":"agent-a","local_path":"x"}`),
		adminPost(adminSyncSave, `{"actor_id":"abc","namespace":"agent-a","local_path":"x"}`),
		adminPost(adminSyncSave, `{"actor_id":{{aid}},"namespace":"agent-a"}`),
		adminPost(adminSyncDelete, `{"id":{{mid}}}`),
		adminPost(adminSyncDelete, `{"id":999999}`),
		adminPost(adminSyncDelete, `{}`),
		adminPost(adminSyncList, `{}`),
	})},
}
