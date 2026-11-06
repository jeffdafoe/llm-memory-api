package contract

var adminShareCases = []Scenario{
	{Name: "admin shares create", Steps: steps(routeShape(adminSharesCreate), []Step{
		// No grant: contract-admin may share only its own namespace.
		contentPost(adminSharesCreate, `{"owner_namespace":"agent-a","slug_pattern":"notes/"}`),
		contentPost(adminSharesCreate, `{"owner_namespace":"contract-admin"}`),
		contentPost(adminSharesCreate, `{"owner_namespace":"*","slug_pattern":"x"}`),
		contentPost(adminSharesCreate, `{"owner_namespace":"contract-admin","slug_pattern":"notes/","can_delete":true}`),
		contentPost(adminSharesCreate, `{"owner_namespace":"contract-admin","slug_pattern":"notes/"}`),
	})},
	{Name: "admin shares create and revoke with access", Steps: steps(routeShape(adminSharesRevoke), []Step{
		grantAdmin("agent-a", true, true, false),
		{Method: "POST", Path: adminSharesCreate, Auth: "admin-web", Header: jsonType,
			Body:    `{"owner_namespace":"agent-a","slug_pattern":"notes/codebase/"}`,
			Capture: map[string]string{"all": "share.id"}},
		// The same pattern and grantee again replaces the flags.
		contentPost(adminSharesCreate, `{"owner_namespace":"agent-a","slug_pattern":"notes/codebase/","can_read":false,"can_write":1}`),
		contentPost(adminSharesCreate, `{"owner_namespace":"agent-a","slug_pattern":"notes/codebase/","grantee_actor_id":null,"can_delete":true}`),
		{Method: "POST", Path: adminSyncList, Auth: "admin-web", Header: jsonType, Body: `{}`,
			Capture: map[string]string{"aid": "mappings.0.actor_id"}},
		{Method: "POST", Path: adminSharesCreate, Auth: "admin-web", Header: jsonType,
			Body:    `{"owner_namespace":"agent-a","slug_pattern":"notes/codebase/auth-middleware","grantee_actor_id":{{aid}},"can_write":true,"can_delete":true}`,
			Capture: map[string]string{"one": "share.id"}},
		contentPost(adminSharesCreate, `{"owner_namespace":"agent-a","slug_pattern":"notes/codebase/auth-middleware","grantee_actor_id":"{{aid}}","can_read":0}`),
		contentPost(adminSharesCreate, `{"owner_namespace":"agent-a","slug_pattern":"x","grantee_actor_id":"abc"}`),
		contentPost(adminSharesRevoke, `{"id":{{one}}}`),
		contentPost(adminSharesRevoke, `{"id":{{one}}}`),
		contentPost(adminSharesRevoke, `{"id":999999}`),
		contentPost(adminSharesRevoke, `{"id":"abc"}`),
		contentPost(adminSharesRevoke, `{"id":"{{all}}"}`),
	})},
	{Name: "admin shares revoke without write access", Steps: []Step{
		grantAdmin("private-ns", true, false, false),
		{Method: "POST", Path: adminSharesList, Auth: "admin-web", Header: jsonType, Body: `{"owner_namespace":"private-ns"}`,
			Capture: map[string]string{"sid": "shares.0.id"}},
		contentPost(adminSharesRevoke, `{"id":{{sid}}}`),
	}},
	{Name: "admin shares list", Steps: steps(routeShape(adminSharesList), []Step{
		contentPost(adminSharesList, `{}`),
		contentPost(adminSharesList, `{"owner_namespace":"private-ns"}`),
		contentPost(adminSharesList, `{"owner_namespace":"*"}`),
		contentPost(adminSharesList, `{"all":true}`),
		contentPost(adminSharesList, `{"all":false,"owner_namespace":"contract-admin"}`),
	})},
	{Name: "admin shares list with access", Steps: []Step{
		grantAdmin("private-ns", true, false, false),
		contentPost(adminSharesList, `{"owner_namespace":"private-ns"}`),
		contentPost(adminSharesList, `{"all":true}`),
	}},
	{Name: "admin shares list all as a wildcard reader", Steps: []Step{
		grantAdmin("/", true, false, false),
		{SQL: `INSERT INTO note_permissions (owner_namespace, slug_pattern, grantee_actor_id, can_read, granted_by)
               SELECT 'agent-a', 'tasks/', NULL, true, id FROM actors WHERE name = 'agent-a'`},
		contentPost(adminSharesList, `{"all":1}`),
	}},
	{Name: "admin shares shared with me", Steps: steps(routeShape(adminSharesWithMe), routeShape(adminSharesDocuments), []Step{
		contentPost(adminSharesWithMe, `{}`),
		contentPost(adminSharesDocuments, `{}`),
		shareToAdmin("private-ns", "notes/codebase/", true, false, false),
		shareToAdmin("agent-a", "notes/codebase/auth-middleware", true, true, false),
		shareToAdmin("shared", "notes/", false, true, false),
		{SQL: `INSERT INTO note_permissions (owner_namespace, slug_pattern, grantee_actor_id, can_read, granted_by)
               SELECT 'agent-a', 'anne_walker/', NULL, true, id FROM actors WHERE name = 'agent-a'`},
		contentPost(adminSharesWithMe, `{}`),
		contentPost(adminSharesDocuments, `{}`),
	})},
	// A namespace named like an Object.prototype member: Node grouped into a
	// plain object, found the inherited function and threw on .push.
	{Name: "admin shares documents in a namespace named constructor", Steps: []Step{
		{SQL: `INSERT INTO documents (namespace, slug, title, content, kind)
               VALUES ('constructor', 'notes/x', 'X', 'x', 'note');
               INSERT INTO note_permissions (owner_namespace, slug_pattern, grantee_actor_id, can_read, granted_by)
               SELECT 'constructor', 'notes/', NULL, true, id FROM actors WHERE name = 'agent-a'`},
		contentPost(adminSharesDocuments, `{}`),
	}, Known: []KnownDifference{
		{Where: "step 1 POST /v1/admin/shares/documents: *", Reason: "a namespace named like an Object.prototype member groups in Go; Node threw (go-server note, Known differences)"},
		{Where: "table error_log*", Reason: "Node logged the TypeError; Go answers (go-server note, Known differences)"},
		{Where: "table request_log*", Reason: "the request log records the status and length the client got (go-server note, Known differences)"},
	}},
	{Name: "admin shares for slug", Steps: steps(routeShape(adminSharesForSlug), []Step{
		contentPost(adminSharesForSlug, `{"owner_namespace":"private-ns"}`),
		contentPost(adminSharesForSlug, `{"owner_namespace":"private-ns","slug_pattern":"notes/codebase/shared-with-b"}`),
		contentPost(adminSharesForSlug, `{"owner_namespace":"/","slug_pattern":"x"}`),
	})},
	{Name: "admin shares for slug with access", Steps: []Step{
		grantAdmin("private-ns", true, false, false),
		contentPost(adminSharesForSlug, `{"owner_namespace":"private-ns","slug_pattern":"notes/codebase/shared-with-b"}`),
		contentPost(adminSharesForSlug, `{"owner_namespace":"private-ns","slug_pattern":"notes/codebase/"}`),
	}},
}
