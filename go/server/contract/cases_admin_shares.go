package contract

var adminShareCases = []Scenario{
	{Name: "admin shares create", Steps: steps(routeShape(adminSharesCreate), []Step{
		// No grant: contract-admin may share only its own namespace.
		adminPost(adminSharesCreate, `{"owner_namespace":"agent-a","slug_pattern":"notes/"}`),
		adminPost(adminSharesCreate, `{"owner_namespace":"contract-admin"}`),
		adminPost(adminSharesCreate, `{"owner_namespace":"*","slug_pattern":"x"}`),
		adminPost(adminSharesCreate, `{"owner_namespace":"contract-admin","slug_pattern":"notes/","can_delete":true}`),
		adminPost(adminSharesCreate, `{"owner_namespace":"contract-admin","slug_pattern":"notes/"}`),
	})},
	{Name: "admin shares create and revoke with access", Steps: steps(routeShape(adminSharesRevoke), []Step{
		grantAdmin("agent-a", true, true, false),
		{Method: "POST", Path: adminSharesCreate, Auth: "admin-web", Header: jsonType,
			Body:    `{"owner_namespace":"agent-a","slug_pattern":"notes/codebase/"}`,
			Capture: map[string]string{"all": "share.id"}},
		// The same pattern and grantee again replaces the flags.
		adminPost(adminSharesCreate, `{"owner_namespace":"agent-a","slug_pattern":"notes/codebase/","can_read":false,"can_write":1}`),
		adminPost(adminSharesCreate, `{"owner_namespace":"agent-a","slug_pattern":"notes/codebase/","grantee_actor_id":null,"can_delete":true}`),
		{Method: "POST", Path: adminSyncList, Auth: "admin-web", Header: jsonType, Body: `{}`,
			Capture: map[string]string{"aid": "mappings.0.actor_id"}},
		{Method: "POST", Path: adminSharesCreate, Auth: "admin-web", Header: jsonType,
			Body:    `{"owner_namespace":"agent-a","slug_pattern":"notes/codebase/auth-middleware","grantee_actor_id":{{aid}},"can_write":true,"can_delete":true}`,
			Capture: map[string]string{"one": "share.id"}},
		adminPost(adminSharesCreate, `{"owner_namespace":"agent-a","slug_pattern":"notes/codebase/auth-middleware","grantee_actor_id":"{{aid}}","can_read":0}`),
		adminPost(adminSharesCreate, `{"owner_namespace":"agent-a","slug_pattern":"x","grantee_actor_id":"abc"}`),
		adminPost(adminSharesRevoke, `{"id":{{one}}}`),
		adminPost(adminSharesRevoke, `{"id":{{one}}}`),
		adminPost(adminSharesRevoke, `{"id":999999}`),
		adminPost(adminSharesRevoke, `{"id":"abc"}`),
		adminPost(adminSharesRevoke, `{"id":"{{all}}"}`),
	})},
	{Name: "admin shares revoke without write access", Steps: []Step{
		grantAdmin("private-ns", true, false, false),
		{Method: "POST", Path: adminSharesList, Auth: "admin-web", Header: jsonType, Body: `{"owner_namespace":"private-ns"}`,
			Capture: map[string]string{"sid": "shares.0.id"}},
		adminPost(adminSharesRevoke, `{"id":{{sid}}}`),
	}},
	{Name: "admin shares list", Steps: steps(routeShape(adminSharesList), []Step{
		adminPost(adminSharesList, `{}`),
		adminPost(adminSharesList, `{"owner_namespace":"private-ns"}`),
		adminPost(adminSharesList, `{"owner_namespace":"*"}`),
		adminPost(adminSharesList, `{"all":true}`),
		adminPost(adminSharesList, `{"all":false,"owner_namespace":"contract-admin"}`),
	})},
	{Name: "admin shares list with access", Steps: []Step{
		grantAdmin("private-ns", true, false, false),
		adminPost(adminSharesList, `{"owner_namespace":"private-ns"}`),
		adminPost(adminSharesList, `{"all":true}`),
	}},
	{Name: "admin shares list all as a wildcard reader", Steps: []Step{
		grantAdmin("/", true, false, false),
		{SQL: `INSERT INTO note_permissions (owner_namespace, slug_pattern, grantee_actor_id, can_read, granted_by)
               SELECT 'agent-a', 'tasks/', NULL, true, id FROM actors WHERE name = 'agent-a'`},
		adminPost(adminSharesList, `{"all":1}`),
	}},
	{Name: "admin shares shared with me", Steps: steps(routeShape(adminSharesWithMe), routeShape(adminSharesDocuments), []Step{
		adminPost(adminSharesWithMe, `{}`),
		adminPost(adminSharesDocuments, `{}`),
		shareToAdmin("private-ns", "notes/codebase/", true, false, false),
		shareToAdmin("agent-a", "notes/codebase/auth-middleware", true, true, false),
		shareToAdmin("shared", "notes/", false, true, false),
		{SQL: `INSERT INTO note_permissions (owner_namespace, slug_pattern, grantee_actor_id, can_read, granted_by)
               SELECT 'agent-a', 'anne_walker/', NULL, true, id FROM actors WHERE name = 'agent-a'`},
		adminPost(adminSharesWithMe, `{}`),
		adminPost(adminSharesDocuments, `{}`),
	})},
	{Name: "admin shares for slug", Steps: steps(routeShape(adminSharesForSlug), []Step{
		adminPost(adminSharesForSlug, `{"owner_namespace":"private-ns"}`),
		adminPost(adminSharesForSlug, `{"owner_namespace":"private-ns","slug_pattern":"notes/codebase/shared-with-b"}`),
		adminPost(adminSharesForSlug, `{"owner_namespace":"/","slug_pattern":"x"}`),
	})},
	{Name: "admin shares for slug with access", Steps: []Step{
		grantAdmin("private-ns", true, false, false),
		adminPost(adminSharesForSlug, `{"owner_namespace":"private-ns","slug_pattern":"notes/codebase/shared-with-b"}`),
		adminPost(adminSharesForSlug, `{"owner_namespace":"private-ns","slug_pattern":"notes/codebase/"}`),
	}},
}
