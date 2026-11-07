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
	{Name: "admin notes move-prefix", Steps: steps(routeShape(adminNotesMovePrefix), []Step{
		contentPost(adminNotesMovePrefix, `{"namespace":"agent-a","old_prefix":"notes/"}`),
		contentPost(adminNotesMovePrefix, `{"namespace":"agent-a","old_prefix":"notes/","new_prefix":null}`),
		contentPost(adminNotesMovePrefix, `{"namespace":"agent-a","old_prefix":"","new_prefix":"x/"}`),
		contentPost(adminNotesMovePrefix, `{"namespace":"*","old_prefix":"notes/","new_prefix":"x/"}`),
		// contract-admin reads agent-a's notes through no grant: refused.
		contentPost(adminNotesMovePrefix, `{"namespace":"agent-a","old_prefix":"notes/","new_prefix":"x/","dry_run":true}`),
		// The order of the checks: required fields, the namespace, access,
		// then the prefixes.
		contentPost(adminNotesMovePrefix, `{"namespace":"*","old_prefix":"/x"}`),
		contentPost(adminNotesMovePrefix, `{"namespace":"*","old_prefix":"/x","new_prefix":5}`),
		contentPost(adminNotesMovePrefix, `{"namespace":"agent-a","old_prefix":"/x","new_prefix":5,"overwrite_slugs":7}`),
	})},
	{Name: "admin notes move-prefix refusals", Steps: []Step{
		grantAdmin("agent-a", true, true, false),
		contentPost(adminNotesMovePrefix, `{"namespace":"agent-a","old_prefix":5,"new_prefix":"x/"}`),
		contentPost(adminNotesMovePrefix, `{"namespace":"agent-a","old_prefix":"notes/","new_prefix":false}`),
		contentPost(adminNotesMovePrefix, `{"namespace":"agent-a","old_prefix":"/notes","new_prefix":0}`),
		contentPost(adminNotesMovePrefix, `{"namespace":"agent-a","old_prefix":"notes/","new_prefix":"a//b/"}`),
		contentPost(adminNotesMovePrefix, `{"namespace":"agent-a","old_prefix":"notes/","new_prefix":"notes/sub/"}`),
		contentPost(adminNotesMovePrefix, `{"namespace":"agent-a","old_prefix":"notes/","new_prefix":"notes/"}`),
		// The Set is built after the scan, and only for a real move.
		contentPost(adminNotesMovePrefix, `{"namespace":"agent-a","old_prefix":"notes/codebase/","new_prefix":"archive/","overwrite_slugs":7}`),
		contentPost(adminNotesMovePrefix, `{"namespace":"agent-a","old_prefix":"notes/codebase/","new_prefix":"archive/","overwrite_slugs":{"a":1}}`),
		contentPost(adminNotesMovePrefix, `{"namespace":"agent-a","old_prefix":"notes/codebase/","new_prefix":"archive/","overwrite_slugs":true}`),
		contentPost(adminNotesMovePrefix, `{"namespace":"agent-a","old_prefix":"/x","new_prefix":"archive/","overwrite_slugs":7}`),
		contentPost(adminNotesMovePrefix, `{"namespace":"agent-a","old_prefix":"notes/codebase/","new_prefix":"archive/","dry_run":true,"overwrite_slugs":7}`),
		{Settle: true},
	}},
	{Name: "admin notes move-prefix conflicts", Steps: []Step{
		grantAdmin("agent-a", true, true, false),
		contentPost(adminNotesSave, `{"namespace":"agent-a","slug":"archive/auth-middleware","title":"c","content":"conflict"}`),
		contentPost(adminNotesSave, `{"namespace":"agent-a","slug":"notes/codebase/second","title":"s","content":"second"}`),
		// A skipped note keeps its sync mapping; a moved one's follows it.
		{SQL: `INSERT INTO note_synchronization (actor_id, namespace, slug, local_path, created_at)
               SELECT id, 'agent-a', s, 'C:/sync/' || s, '2026-10-01T09:00:00Z' FROM actors,
                      unnest(ARRAY['notes/codebase/auth-middleware', 'notes/codebase/second']) s
               WHERE name = 'agent-a'`},
		{Settle: true},
		contentPost(adminNotesMovePrefix, `{"namespace":"agent-a","old_prefix":"notes/codebase/","new_prefix":"archive/","dry_run":true}`),
		// A string is a Set of its characters: no conflict matches it.
		contentPost(adminNotesMovePrefix, `{"namespace":"agent-a","old_prefix":"notes/codebase/","new_prefix":"archive/","overwrite_slugs":"archive/auth-middleware"}`),
		{Settle: true},
		contentPost(adminNotesList, `{"namespace":"agent-a","prefix":"archive/"}`),
		contentPost(adminNotesList, `{"namespace":"agent-a","prefix":"notes/codebase/"}`),
		contentPost(adminSyncList, `{"actor_id":0}`),
	}},
	{Name: "admin notes move-prefix overwrite", Steps: []Step{
		grantAdmin("agent-a", true, true, false),
		contentPost(adminNotesSave, `{"namespace":"agent-a","slug":"archive/auth-middleware","title":"c","content":"conflict"}`),
		{Settle: true},
		contentPost(adminNotesMovePrefix, `{"namespace":"agent-a","old_prefix":"notes/codebase/","new_prefix":"archive/","overwrite_slugs":["archive/other",5,"archive/auth-middleware"]}`),
		{Settle: true},
		contentPost(adminNotesList, `{"namespace":"agent-a","prefix":"archive/"}`),
	}},
	{Name: "admin notes move-prefix to the root", Steps: []Step{
		grantAdmin("agent-a", true, true, false),
		// Only dry_run true previews: 1 moves.
		contentPost(adminNotesMovePrefix, `{"namespace":"agent-a","old_prefix":"notes/","new_prefix":"","dry_run":1}`),
		{Settle: true},
		contentPost(adminNotesMovePrefix, `{"namespace":"agent-a","old_prefix":"no-such/","new_prefix":"x/"}`),
		{Settle: true},
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
