package admin

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
)

// adminPermAllowlist is the server-side authority for the admin
// permissions a save may grant. A pair not listed here is dropped by the
// replace-all save, so every capability a consumer checks must be listed,
// plugins:administer included (the salem umbilical reads it through
// /auth/verify's permission map).
var adminPermAllowlist = map[string][]string{
	"dashboard": {"read"},
	"agents":    {"read", "write", "create_system_equivalent"},
	"comms":     {"read", "write", "delete"},
	"notes":     {"read", "write", "delete"},
	"config":    {"read", "write"},
	"actors":    {"read", "write"},
	"templates": {"read", "write", "delete"},
	"logs":      {"read"},
	"access":    {"read", "write"},
	"plugins":   {"administer"},
}

// objectPrototypeMembers are the names a plain JavaScript object inherits.
// ADMIN_PERM_ALLOWLIST[resource] found one of them for such a resource, and
// allowed.includes then threw.
var objectPrototypeMembers = map[string]bool{
	"constructor": true, "__defineGetter__": true, "__defineSetter__": true, "hasOwnProperty": true,
	"__lookupGetter__": true, "__lookupSetter__": true, "isPrototypeOf": true, "propertyIsEnumerable": true,
	"toString": true, "valueOf": true, "__proto__": true, "toLocaleString": true,
}

// validAdminPerm is isValidAdminPerm.
func validAdminPerm(resource, action string) (bool, error) {
	if resource == "*" && action == "*" {
		return true, nil
	}
	if objectPrototypeMembers[resource] {
		return false, errors.New("allowed.includes is not a function")
	}
	for _, a := range adminPermAllowlist[resource] {
		if a == action {
			return true, nil
		}
	}
	return false, nil
}

type adminPermission struct {
	Resource string `json:"resource"`
	Action   string `json:"action"`
}

func positiveActorID(f httpapi.Fields) (int, bool) {
	n, ok := SafeInt(f.Raw("actor_id"))
	if !ok || n <= 0 {
		return 0, false
	}
	return int(n), true
}

// actorsAdminPermissionsRead is POST /admin/actors/admin-permissions/read.
func (rt *Routes) actorsAdminPermissionsRead(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return readError("actor_id")
	}
	actorID, ok := positiveActorID(f)
	if !ok {
		return httpapi.BadRequest(w, "Required field: actor_id (positive integer)")
	}
	rows, err := rt.DB.Query(r.Context(),
		"SELECT resource, action FROM admin_permissions WHERE actor_id = $1 ORDER BY resource, action", actorID)
	if err != nil {
		return err
	}
	defer rows.Close()
	list := []adminPermission{}
	for rows.Next() {
		var p adminPermission
		if err := rows.Scan(&p.Resource, &p.Action); err != nil {
			return err
		}
		list = append(list, p)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string][]adminPermission{"permissions": list})
}

// permString is String(perm.<name> || ”).trim().
func permString(elem any, name string) (string, error) {
	v, err := member(elem, name)
	if err != nil {
		return "", err
	}
	if !jsutil.Truthy(v) {
		return "", nil
	}
	s, err := jsutil.ValueString(v)
	if err != nil {
		return "", err
	}
	return jsutil.Trim(s), nil
}

// actorsAdminPermissionsSave is POST /admin/actors/admin-permissions/save:
// replaces an actor's admin permissions with the allowed, deduplicated
// pairs of the request. Superadmin only, or any account with agents:write
// could grant itself *:*.
func (rt *Routes) actorsAdminPermissionsSave(w http.ResponseWriter, r *http.Request) error {
	if ok, err := rt.requireSuperadmin(w, r, "Managing admin permissions requires superadmin"); !ok {
		return err
	}
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return readError("actor_id")
	}
	actorID, idOK := positiveActorID(f)
	raw, isArray := f.Array("permissions")
	if !idOK || !isArray {
		return httpapi.BadRequest(w, "Required fields: actor_id (positive integer), permissions[]")
	}
	var one int
	err := rt.DB.QueryRow(r.Context(), "SELECT id FROM actors WHERE id = $1", actorID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		httpapi.WriteError(w, http.StatusNotFound, "NOT_FOUND", "Actor not found")
		return nil
	}
	if err != nil {
		return err
	}

	validated := []adminPermission{}
	seen := map[string]bool{}
	for _, elemRaw := range raw {
		elem, err := jsutil.Parse(elemRaw)
		if err != nil {
			return err
		}
		resource, err := permString(elem, "resource")
		if err != nil {
			return err
		}
		action, err := permString(elem, "action")
		if err != nil {
			return err
		}
		if resource == "" || action == "" {
			continue
		}
		valid, err := validAdminPerm(resource, action)
		if err != nil {
			return err
		}
		key := resource + ":" + action
		if !valid || seen[key] {
			continue
		}
		seen[key] = true
		validated = append(validated, adminPermission{resource, action})
	}

	// One transaction, where Node ran the delete and each insert on their
	// own: a crash part way no longer leaves the actor with a partial set.
	// The end state on success is the same.
	ctx := r.Context()
	tx, err := rt.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "DELETE FROM admin_permissions WHERE actor_id = $1", actorID); err != nil {
		return err
	}
	for _, p := range validated {
		if _, err := tx.Exec(ctx, "INSERT INTO admin_permissions (actor_id, resource, action) VALUES ($1, $2, $3)", actorID, p.Resource, p.Action); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	rt.Admin.ClearCache(actorID)
	rt.LogAdmin("admin_permissions_save", logger.Fields{{Key: "actor_id", Value: actorID}, {Key: "count", Value: len(validated)}, {Key: "user_id", Value: User(r).ID}})

	resp := struct {
		Message            string                     `json:"message"`
		Count              int                        `json:"count"`
		UpdatedPermissions *permissions.PermissionMap `json:"updated_permissions,omitempty"`
	}{Message: "Admin permissions saved", Count: len(validated)}
	// Editing one's own permissions returns the new map, so the dashboard
	// can refresh.
	if actorID == httpapi.StateOf(r).ActorID {
		perms, err := rt.Admin.PermissionMap(ctx, actorID)
		if err != nil {
			return err
		}
		resp.UpdatedPermissions = &perms
	}
	return httpapi.WriteJSON(w, http.StatusOK, resp)
}

// actorDeleteSteps are the per-actor statements of actors/delete, in
// dependency order.
var actorDeleteSteps = []string{
	// Discussion system
	"DELETE FROM discussion_ballots WHERE actor_id = $1",
	"DELETE FROM discussion_votes WHERE proposed_by_actor_id = $1",
	"DELETE FROM discussion_participants WHERE actor_id = $1",
	// Chat and mail: delivery rows first (FK to chat_message_texts), then the texts
	"DELETE FROM chat_messages WHERE to_actor_id = $1",
	"DELETE FROM chat_messages WHERE message_text_id IN (SELECT id FROM chat_message_texts WHERE from_actor_id = $1)",
	"DELETE FROM chat_message_texts WHERE from_actor_id = $1",
	"DELETE FROM mail WHERE from_actor_id = $1 OR to_actor_id = $1",
	// Sessions and keys
	"DELETE FROM sessions WHERE actor_id = $1",
	"DELETE FROM agent_api_keys WHERE actor_id = $1",
	// Permissions
	"DELETE FROM agent_permissions WHERE actor_id = $1",
	"DELETE FROM namespace_permissions WHERE actor_id = $1",
	"DELETE FROM admin_permissions WHERE actor_id = $1",
	"DELETE FROM actor_visibility_configuration WHERE actor_id = $1 OR target_actor_id = $1",
	"DELETE FROM note_permissions WHERE grantee_actor_id = $1 OR granted_by = $1",
	"DELETE FROM virtual_agent_access WHERE virtual_agent_id = $1 OR grantee_actor_id = $1",
	// Usage and sync. virtual_agent_calls.actor_id is NOT NULL with no ON
	// DELETE, so it is deleted, not detached.
	"DELETE FROM virtual_agent_usage WHERE actor_id = $1",
	"DELETE FROM virtual_agent_calls WHERE actor_id = $1",
	"DELETE FROM note_synchronization WHERE actor_id = $1",
	// Logs keep their history, detached from the actor
	"UPDATE system_errors SET actor_id = NULL WHERE actor_id = $1",
	"UPDATE error_log SET actor_id = NULL WHERE actor_id = $1",
	"UPDATE request_log SET actor_id = NULL WHERE actor_id = $1",
	// Documents the actor created elsewhere stay, without the creator
	"UPDATE documents SET created_by_actor_id = NULL WHERE created_by_actor_id = $1",
}

// actorsDelete is POST /admin/actors/delete: deletes an actor, the virtual
// agents it created, and everything that belongs to them, in one
// transaction. Not oneself; only an actor one created, unless superadmin.
func (rt *Routes) actorsDelete(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return readError("actor_id")
	}
	actorID, ok := positiveActorID(f)
	if !ok {
		return httpapi.BadRequest(w, "Required field: actor_id (positive integer)")
	}
	if actorID == httpapi.StateOf(r).ActorID {
		return httpapi.BadRequest(w, "Cannot delete your own account")
	}
	var name string
	err := rt.DB.QueryRow(r.Context(), "SELECT id, name FROM actors WHERE id = $1", actorID).Scan(new(int), &name)
	if errors.Is(err, pgx.ErrNoRows) {
		httpapi.WriteError(w, http.StatusNotFound, "NOT_FOUND", "Actor not found")
		return nil
	}
	if err != nil {
		return err
	}
	if ok, err := rt.RequireOwnershipID(w, r, actorID); !ok {
		return err
	}

	rows, err := rt.DB.Query(r.Context(),
		`SELECT a.id, a.name FROM actors a
         JOIN agent_configuration ac ON ac.actor_id = a.id
         WHERE a.created_by = $1 AND ac.virtual = true`, actorID)
	if err != nil {
		return err
	}
	ids := []int{actorID}
	names := []string{name}
	vaIDs := []int{}
	vaNames := []string{}
	for rows.Next() {
		var id int
		var n string
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return err
		}
		vaIDs = append(vaIDs, id)
		vaNames = append(vaNames, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	ids = append(ids, vaIDs...)
	names = append(names, vaNames...)

	ctx := r.Context()
	tx, err := rt.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, id := range ids {
		for _, sql := range actorDeleteSteps {
			if _, err := tx.Exec(ctx, sql, id); err != nil {
				return err
			}
		}
	}
	// Discussions they created, after their participants, votes and ballots.
	for _, id := range ids {
		if _, err := tx.Exec(ctx, "DELETE FROM discussions WHERE created_by_actor_id = $1", id); err != nil {
			return err
		}
	}
	// Each actor's namespace is its name.
	for _, n := range names {
		if _, err := tx.Exec(ctx, "DELETE FROM memory_chunks WHERE namespace = $1", n); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM documents WHERE namespace = $1", n); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, "UPDATE actors SET created_by = NULL WHERE created_by = ANY($1::int[])", ids); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(ctx, "DELETE FROM agent_configuration WHERE actor_id = $1", id); err != nil {
			return err
		}
	}
	// The owned virtual agents first, then the actor.
	for _, id := range append(vaIDs, actorID) {
		if _, err := tx.Exec(ctx, "DELETE FROM actors WHERE id = $1", id); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	rt.Actors.Clear()
	for _, id := range ids {
		rt.Admin.ClearCache(id)
		rt.Visibility.ClearCache(id)
		// Node left a deleted actor's cached sessions and keys working for
		// up to the cache's five minutes; the port drops them at once.
		rt.SessionAuth.ForgetActor(id)
	}
	rt.LogAdmin("actor_delete", logger.Fields{
		{Key: "actor_id", Value: actorID},
		{Key: "actor_name", Value: name},
		{Key: "deleted_virtual_agents", Value: vaNames},
		{Key: "user_id", Value: User(r).ID},
	})
	type deleted struct {
		Actor         string   `json:"actor"`
		VirtualAgents []string `json:"virtual_agents"`
	}
	return httpapi.WriteJSON(w, http.StatusOK, struct {
		Message string  `json:"message"`
		Deleted deleted `json:"deleted"`
	}{"Actor deleted", deleted{name, vaNames}})
}
