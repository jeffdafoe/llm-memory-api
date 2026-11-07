package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/pgbind"
)

// plainError is the {"error": "<text>"} shape the virtual-agent access
// routes answer their 400s with.
func plainError(w http.ResponseWriter, status int, message string) error {
	return httpapi.WriteJSON(w, status, map[string]string{"error": message})
}

type vaAccessRow struct {
	ID               int     `json:"id"`
	VirtualAgentID   int     `json:"virtual_agent_id"`
	VirtualAgentName string  `json:"virtual_agent_name"`
	GranteeActorID   *int    `json:"grantee_actor_id"`
	GranteeName      *string `json:"grantee_name"`
	CreatedAt        isoTime `json:"created_at"`
}

// vaAccessList is POST /admin/virtual-agent-access/list: the access rules of
// the virtual agents the caller may see, and of those only the ones open to
// everyone (no grantee) or naming a grantee the caller may see, so a hidden
// actor's name never shows.
func (rt *Routes) vaAccessList(w http.ResponseWriter, r *http.Request) error {
	visible, err := rt.Visibility.VisibleActorIDs(r.Context(), httpapi.StateOf(r).ActorID)
	if err != nil {
		return err
	}
	sql := `
        SELECT vaa.id, vaa.virtual_agent_id, va.name AS virtual_agent_name,
               vaa.grantee_actor_id, ga.name AS grantee_name, vaa.created_at
        FROM virtual_agent_access vaa
        JOIN actors va ON va.id = vaa.virtual_agent_id
        LEFT JOIN actors ga ON ga.id = vaa.grantee_actor_id`
	var args []any
	if !visible.All {
		sql += " WHERE vaa.virtual_agent_id = ANY($1) AND (vaa.grantee_actor_id IS NULL OR vaa.grantee_actor_id = ANY($1))"
		args = append(args, visible.IDs)
	}
	sql += " ORDER BY va.name, ga.name NULLS FIRST"
	rows, err := rt.DB.Query(r.Context(), sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	list := []vaAccessRow{}
	for rows.Next() {
		var (
			row       vaAccessRow
			createdAt time.Time
		)
		if err := rows.Scan(&row.ID, &row.VirtualAgentID, &row.VirtualAgentName, &row.GranteeActorID, &row.GranteeName, &createdAt); err != nil {
			return err
		}
		row.CreatedAt = isoTime{&createdAt}
		list = append(list, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string][]vaAccessRow{"access": list})
}

// vaAccessGrant is POST /admin/virtual-agent-access/grant: lets
// grantee_actor_id (everyone when absent) use a virtual agent. Only the
// agent's creator or a superadmin.
func (rt *Routes) vaAccessGrant(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("virtual_agent_id")
	}
	if !f.Truthy("virtual_agent_id") {
		return plainError(w, http.StatusBadRequest, "virtual_agent_id required")
	}
	vaRaw := f.Raw("virtual_agent_id")
	vaID := pgbind.Value(vaRaw)

	var virtual *bool
	err := rt.DB.QueryRow(r.Context(), "SELECT virtual FROM agent_configuration WHERE actor_id = $1", vaID).Scan(&virtual)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if virtual == nil || !*virtual {
		return plainError(w, http.StatusBadRequest, "Not a virtual agent")
	}
	if ok, err := rt.RequireOwnership(w, r, vaID, strictlyEquals(vaRaw, httpapi.StateOf(r).ActorID)); !ok {
		return err
	}

	// grantee_actor_id || null
	var grantee any
	if f.Truthy("grantee_actor_id") {
		grantee = pgbind.Value(f.Raw("grantee_actor_id"))
	}
	ctx := r.Context()
	tag, err := rt.DB.Exec(ctx,
		"INSERT INTO virtual_agent_access (virtual_agent_id, grantee_actor_id) VALUES ($1, $2) ON CONFLICT DO NOTHING RETURNING id",
		vaID, grantee)
	if err != nil {
		return err
	}
	fields := logger.Fields{{Key: "virtual_agent_id", Value: json.RawMessage(vaRaw)}}
	// JSON.stringify drops an undefined member.
	if raw, present := f["grantee_actor_id"]; present {
		fields = append(fields, logger.Field{Key: "grantee_actor_id", Value: json.RawMessage(raw)})
	}
	fields = append(fields, logger.Field{Key: "user_id", Value: User(r).ID})
	rt.LogAdmin("va_access_grant", fields)
	return httpapi.WriteJSON(w, http.StatusOK, map[string]bool{"granted": tag.RowsAffected() > 0})
}

// vaAccessRevoke is POST /admin/virtual-agent-access/revoke: deletes one
// access rule, for the agent's creator or a superadmin. A rule that does
// not exist is reported revoked.
func (rt *Routes) vaAccessRevoke(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("id")
	}
	if !f.Truthy("id") {
		return plainError(w, http.StatusBadRequest, "id required")
	}
	idRaw := f.Raw("id")
	id := pgbind.Value(idRaw)
	var vaID int
	err := rt.DB.QueryRow(r.Context(), "SELECT virtual_agent_id FROM virtual_agent_access WHERE id = $1", id).Scan(&vaID)
	if errors.Is(err, pgx.ErrNoRows) {
		return httpapi.WriteJSON(w, http.StatusOK, map[string]bool{"revoked": true})
	}
	if err != nil {
		return err
	}
	if ok, err := rt.RequireOwnershipID(w, r, vaID); !ok {
		return err
	}
	ctx := r.Context()
	if _, err := rt.DB.Exec(ctx, "DELETE FROM virtual_agent_access WHERE id = $1", id); err != nil {
		return err
	}
	rt.LogAdmin("va_access_revoke", logger.Fields{{Key: "access_id", Value: json.RawMessage(idRaw)}, {Key: "user_id", Value: User(r).ID}})
	return httpapi.WriteJSON(w, http.StatusOK, map[string]bool{"revoked": true})
}
