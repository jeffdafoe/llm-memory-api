package admin

import (
	"context"
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
	// Only an actor the caller may see; a hidden one reads as missing.
	visible, err := rt.Visibility.CanSee(r.Context(), httpapi.StateOf(r).ActorID, actorID)
	if err != nil {
		return err
	}
	if !visible {
		httpapi.WriteError(w, http.StatusNotFound, "NOT_FOUND", "Actor not found")
		return nil
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
	// Discussion system. The discussions the actor created are deleted after
	// these steps, so every row that points at them goes here too, other
	// actors' included: their ballots, votes and participation, and the
	// discussion's chat. Other actors' ballots on a vote the actor proposed
	// go before that vote.
	"DELETE FROM discussion_ballots WHERE actor_id = $1",
	"DELETE FROM discussion_ballots WHERE vote_id IN (SELECT id FROM discussion_votes WHERE proposed_by_actor_id = $1 OR discussion_id IN (SELECT id FROM discussions WHERE created_by_actor_id = $1))",
	"DELETE FROM discussion_votes WHERE proposed_by_actor_id = $1 OR discussion_id IN (SELECT id FROM discussions WHERE created_by_actor_id = $1)",
	"DELETE FROM discussion_participants WHERE actor_id = $1 OR discussion_id IN (SELECT id FROM discussions WHERE created_by_actor_id = $1)",
	// Chat and mail: delivery rows first (FK to chat_message_texts), then the texts
	"DELETE FROM chat_messages WHERE to_actor_id = $1",
	"DELETE FROM chat_messages WHERE message_text_id IN (SELECT id FROM chat_message_texts WHERE from_actor_id = $1 OR discussion_id IN (SELECT id FROM discussions WHERE created_by_actor_id = $1))",
	"DELETE FROM chat_message_texts WHERE from_actor_id = $1 OR discussion_id IN (SELECT id FROM discussions WHERE created_by_actor_id = $1)",
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

// actorNamespaceDeleteSteps are the rows keyed by the actor's namespace (its
// name) rather than its id, so no foreign key reaches them (GDPR Art. 17,
// LLM-1022). The grants matter beyond tidiness: a grant left on the namespace
// would apply to a later account that signs up with the same name.
var actorNamespaceDeleteSteps = []string{
	// Grants other actors hold on this namespace
	"DELETE FROM namespace_permissions WHERE namespace = $1",
	// Note shares out of this namespace, whoever granted them
	"DELETE FROM note_permissions WHERE owner_namespace = $1",
	// Other actors' sync state for this namespace
	"DELETE FROM note_synchronization WHERE namespace = $1",
	// The note count and size row (the cleanup job only adjusts it, never
	// removes it)
	"DELETE FROM namespace_usage WHERE namespace = $1",
}

// signupInvitesSQL locks the invites that tie an account to its signup: the
// ones it registered with, and every invite on the access requests behind
// them. Only that provenance counts; a request is not the account's just
// because it carries the same email address, and other requests from that
// address age out under the LLM-1024 retention rules. FOR UPDATE is the lock
// registration takes on an invite before it marks it used, so a redemption
// running alongside either finishes first (and the delete then sees it) or
// waits and finds the invite gone.
const signupInvitesSQL = `
        SELECT id, access_request_id, used_by FROM invite_codes
         WHERE used_by = $1
            OR access_request_id IN (
               SELECT access_request_id FROM invite_codes
                WHERE used_by = $1 AND access_request_id IS NOT NULL)
           FOR UPDATE`

// deleteSignupRecords removes an account's invites and access requests
// inside the delete's transaction (GDPR Art. 17, LLM-1022).
//
// A request goes only when no OTHER account registered with one of its
// invites: an admin can issue more than one invite per request, and the
// other account's record must survive. The account's own invites always go;
// an unused invite goes with its request. invite_codes.access_request_id is a
// NO ACTION foreign key, so the invites are deleted first. An invite the
// account issued (created_by, an admin's name) stays, with the name cleared.
func deleteSignupRecords(ctx context.Context, tx pgx.Tx, name string) error {
	type invite struct {
		id        int
		requestID *int
		usedBy    *string
	}
	rows, err := tx.Query(ctx, signupInvitesSQL, name)
	if err != nil {
		return err
	}
	invites, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (invite, error) {
		var inv invite
		err := row.Scan(&inv.id, &inv.requestID, &inv.usedBy)
		return inv, err
	})
	if err != nil {
		return err
	}
	// A request is the account's alone unless another name redeemed one of
	// its invites.
	shared := map[int]bool{}
	requests := map[int]bool{}
	for _, inv := range invites {
		if inv.requestID == nil {
			continue
		}
		requests[*inv.requestID] = true
		if inv.usedBy != nil && *inv.usedBy != name {
			shared[*inv.requestID] = true
		}
	}
	inviteIDs := []int{}
	for _, inv := range invites {
		mine := inv.usedBy != nil && *inv.usedBy == name
		unusedOnMyRequest := inv.usedBy == nil && inv.requestID != nil && !shared[*inv.requestID]
		if mine || unusedOnMyRequest {
			inviteIDs = append(inviteIDs, inv.id)
		}
	}
	requestIDs := []int{}
	for id := range requests {
		if !shared[id] {
			requestIDs = append(requestIDs, id)
		}
	}
	if _, err := tx.Exec(ctx, "DELETE FROM invite_codes WHERE id = ANY($1::int[])", inviteIDs); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM access_requests WHERE id = ANY($1::int[])", requestIDs); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE invite_codes SET created_by = NULL WHERE created_by = $1", name); err != nil {
		return err
	}
	return nil
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

	ctx := r.Context()
	tx, err := rt.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Re-read and lock the actor inside the transaction. Several steps below
	// delete by name, and names are unique only among live actors: if a
	// second delete of the same id committed after the read above and the
	// name was then registered again, those steps would hit the new account.
	// While this row is locked and still has its name, no other actor can.
	err = tx.QueryRow(ctx, "SELECT name FROM actors WHERE id = $1 FOR UPDATE", actorID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		httpapi.WriteError(w, http.StatusNotFound, "NOT_FOUND", "Actor not found")
		return nil
	}
	if err != nil {
		return err
	}

	rows, err := tx.Query(ctx,
		`SELECT a.id, a.name FROM actors a
         JOIN agent_configuration ac ON ac.actor_id = a.id
         WHERE a.created_by = $1 AND ac.virtual = true
           FOR UPDATE OF a`, actorID)
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

	for _, id := range ids {
		for _, sql := range actorDeleteSteps {
			if _, err := tx.Exec(ctx, sql, id); err != nil {
				return err
			}
		}
	}
	// Discussions they created, after every row that points at them.
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
		for _, sql := range actorNamespaceDeleteSteps {
			if _, err := tx.Exec(ctx, sql, n); err != nil {
				return err
			}
		}
	}
	// Signup records. Virtual agents never register by invite, but a match
	// on their names costs nothing.
	for _, n := range names {
		if err := deleteSignupRecords(ctx, tx, n); err != nil {
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
