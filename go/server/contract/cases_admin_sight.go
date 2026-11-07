package contract

// adminSightCases send the limited admin (no realm, so it sees itself and
// the two actors it created) to the routes that read or change one actor's
// settings, the virtual-agent access list and call detail, about actors
// outside that sight. A hidden actor reads as a missing one (404); another
// actor's profile visibility needs ownership (403).
var adminSightCases = []Scenario{
	withIDs("admin limited admin reads only actors it can see",
		verifyID("a", "agent-a-session"),
		limitedIDs(),
		adminPost(actorsPermissionsRead, "admin-limited-web", `{"actor_id":{{a}}}`),
		adminPost(actorsPermissionsRead, "admin-limited-web", `{"actor_id":{{limited_agent_id}}}`),
		adminPost(actorsVisibilityRead, "admin-limited-web", `{"actor_id":{{a}}}`),
		adminPost(actorsVisibilityRead, "admin-limited-web", `{"actor_id":{{limited_agent_id}}}`),
		adminPost(actorsAdminPermsRead, "admin-limited-web", `{"actor_id":{{a}}}`),
		adminPost(actorsAdminPermsRead, "admin-limited-web", `{"actor_id":999999}`),
		adminPost(actorsAdminPermsRead, "admin-limited-web", `{"actor_id":{{limited_id}}}`)),
	withIDs("admin limited admin reads an actor it was granted sight of",
		grantLimitedSight,
		verifyID("a", "agent-a-session"),
		adminPost(actorsPermissionsRead, "admin-limited-web", `{"actor_id":{{a}}}`),
		adminPost(actorsVisibilityRead, "admin-limited-web", `{"actor_id":{{a}}}`),
		adminPost(actorsAdminPermsRead, "admin-limited-web", `{"actor_id":{{a}}}`)),
	withIDs("admin profile visibility of another actor",
		verifyID("a", "agent-a-session"),
		limitedIDs(),
		adminPost(adminProfilePath, "admin-limited-web", `{"actor_id":{{a}}}`),
		adminPost(adminProfilePath, "admin-limited-web", `{"actor_id":"{{a}}","visible_to_others":false}`),
		adminPost(adminProfilePath, "admin-limited-web", `{"actor_id":"{{limited_agent_id}}","visible_to_others":false}`),
		adminPost(adminProfilePath, "admin-limited-web", `{"actor_id":{{limited_agent_id}}}`),
		adminPost(adminProfilePath, "admin-limited-web", `{"actor_id":{{limited_id}},"visible_to_others":"yes"}`),
		adminPost(adminProfilePath, "admin-web", `{"actor_id":{{a}},"visible_to_others":0}`),
		adminPost(adminProfilePath, "admin-web", `{"actor_id":{{a}}}`)),
	withIDs("admin va access list holds only visible agents",
		limitedIDs(),
		adminPost(vaAccessListPath, "admin-limited-web", `{}`),
		verifyID("a", "agent-a-session"),
		adminPost(vaAccessGrantPath, "admin-limited-web", `{"virtual_agent_id":{{limited_va_id}},"grantee_actor_id":{{limited_agent_id}}}`),
		// A rule naming a grantee the limited admin cannot see stays out of
		// its list (agent-a's name must not show), not out of the superadmin's.
		adminPost(vaAccessGrantPath, "admin-limited-web", `{"virtual_agent_id":{{limited_va_id}},"grantee_actor_id":{{a}}}`),
		adminPost(vaAccessGrantPath, "admin-limited-web", `{"virtual_agent_id":{{limited_va_id}}}`),
		adminPost(vaAccessListPath, "admin-limited-web", `{}`),
		adminPost(vaAccessListPath, "admin-web", `{}`)),
	withIDs("admin agents call detail of a hidden agent",
		vaUsage,
		Step{SQL: `INSERT INTO virtual_agent_calls (id, actor_id, provider, model, status, created_at)
            SELECT 9103, id, 'anthropic', 'claude-haiku-4-5', 'success', NOW() FROM actors WHERE name = 'limited-va'`},
		adminPost(agentsCallDetailPath, "admin-limited-web", `{"call_id":9101}`),
		adminPost(agentsCallDetailPath, "admin-limited-web", `{"call_id":9103}`),
		adminPost(agentsCallDetailPath, "admin-web", `{"call_id":9101}`)),
}
