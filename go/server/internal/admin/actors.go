package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/auth"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
)

// isoTime is a timestamptz as node-postgres put it in JSON (a Date's
// toISOString), or null.
type isoTime struct{ t *time.Time }

func (v isoTime) MarshalJSON() ([]byte, error) {
	if v.t == nil {
		return []byte("null"), nil
	}
	return json.Marshal(jsutil.DateISO(*v.t))
}

// parseActorID is parseActorId: actor_id as a positive safe integer, or the
// 400 written directly and ok false.
func parseActorID(w http.ResponseWriter, f httpapi.Fields) (id int, ok bool) {
	n, valid := SafeInt(f.Raw("actor_id"))
	if !valid || n <= 0 {
		httpapi.WriteError(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid actor_id")
		return 0, false
	}
	return int(n), true
}

// actorExists is resolveById's 404, written directly. ok is false when the
// handler should stop.
func (rt *Routes) actorExists(w http.ResponseWriter, r *http.Request, id int) (bool, error) {
	_, found, err := rt.Actors.ResolveByID(r.Context(), id)
	if err != nil {
		return false, err
	}
	if !found {
		httpapi.WriteError(w, http.StatusNotFound, "NOT_FOUND", "Actor not found")
		return false, nil
	}
	return true, nil
}

// actorVisible is actorExists for an actor the caller may also see: a
// hidden actor gets the same 404 as a missing one.
func (rt *Routes) actorVisible(w http.ResponseWriter, r *http.Request, id int) (bool, error) {
	_, found, err := rt.Actors.ResolveByID(r.Context(), id)
	if err != nil {
		return false, err
	}
	if found {
		found, err = rt.Visibility.CanSee(r.Context(), httpapi.StateOf(r).ActorID, id)
		if err != nil {
			return false, err
		}
	}
	if !found {
		httpapi.WriteError(w, http.StatusNotFound, "NOT_FOUND", "Actor not found")
		return false, nil
	}
	return true, nil
}

// requireSuperadmin answers 403 with message unless the caller holds *:*.
func (rt *Routes) requireSuperadmin(w http.ResponseWriter, r *http.Request, message string) (bool, error) {
	super, err := rt.Admin.IsSuperadmin(r.Context(), httpapi.StateOf(r).ActorID)
	if err != nil {
		return false, err
	}
	if !super {
		httpapi.WriteError(w, http.StatusForbidden, "FORBIDDEN", message)
		return false, nil
	}
	return true, nil
}

type actorListRow struct {
	ID              int      `json:"id"`
	Name            string   `json:"name"`
	CreatedAt       isoTime  `json:"created_at"`
	VisibleToOthers bool     `json:"visible_to_others"`
	CreatedBy       *int     `json:"created_by"`
	IsAgent         bool     `json:"is_agent"`
	IsUser          bool     `json:"is_user"`
	Realms          []string `json:"realms"`
	Status          *string  `json:"status"`
	LastSeen        isoTime  `json:"last_seen"`
	RegisteredAt    isoTime  `json:"registered_at"`
	Provider        *string  `json:"provider"`
	Model           *string  `json:"model"`
	Virtual         *bool    `json:"virtual"`
	Personality     *string  `json:"personality"`
	ActiveSince     isoTime  `json:"active_since"`
	CreatedByName   *string  `json:"created_by_name"`
	PricingInfo     *string  `json:"pricing_info,omitempty"`
}

// actorsList is POST /admin/actors/list: the actors the caller may see,
// with each agent's pricing text.
func (rt *Routes) actorsList(w http.ResponseWriter, r *http.Request) error {
	visible, err := rt.Visibility.VisibleActorIDs(r.Context(), httpapi.StateOf(r).ActorID)
	if err != nil {
		return err
	}
	sql := `SELECT a.id, a.name, a.created_at, a.visible_to_others, a.created_by,
                (ac.actor_id IS NOT NULL) AS is_agent,
                (a.password_hash IS NOT NULL) AS is_user,
                a.realms,
                s.status, s.last_seen, s.registered_at,
                s.provider, s.model, s.virtual, s.personality,
                s.active_since, ac.configuration,
                creator.name AS created_by_name
         FROM actors a
         LEFT JOIN agent_configuration ac ON ac.actor_id = a.id
         LEFT JOIN agent_status s ON s.actor_id = a.id
         LEFT JOIN actors creator ON creator.id = a.created_by`
	var args []any
	if !visible.All {
		sql += " WHERE a.id = ANY($1)"
		args = append(args, visible.IDs)
	}
	sql += " ORDER BY a.name"
	rows, err := rt.DB.Query(r.Context(), sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	list := []*actorListRow{}
	var configs []*string
	for rows.Next() {
		var (
			row                                 actorListRow
			createdAt                           time.Time
			lastSeen, registeredAt, activeSince *time.Time
			configuration                       *string
		)
		if err := rows.Scan(&row.ID, &row.Name, &createdAt, &row.VisibleToOthers, &row.CreatedBy,
			&row.IsAgent, &row.IsUser, &row.Realms, &row.Status, &lastSeen, &registeredAt,
			&row.Provider, &row.Model, &row.Virtual, &row.Personality, &activeSince, &configuration,
			&row.CreatedByName); err != nil {
			return err
		}
		row.CreatedAt = isoTime{&createdAt}
		row.LastSeen = isoTime{lastSeen}
		row.RegisteredAt = isoTime{registeredAt}
		row.ActiveSince = isoTime{activeSince}
		list = append(list, &row)
		configs = append(configs, configuration)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// OpenRouter's pricing text reads the catalog cache, so fill it first.
	for _, row := range list {
		if row.Provider != nil && *row.Provider == "openrouter" {
			rt.Providers.FetchCatalog(r.Context())
			break
		}
	}
	for i, row := range list {
		if row.Provider == nil || *row.Provider == "" || row.Model == nil || *row.Model == "" {
			continue
		}
		// The configuration column is text: JSON that does not parse is {}.
		var conf any = jsutil.NewObject()
		if c := configs[i]; c != nil && *c != "" {
			if parsed, err := jsutil.Parse([]byte(*c)); err == nil {
				conf = parsed
			}
		}
		text, err := rt.Providers.FormatPricing(*row.Provider, *row.Model, conf)
		if err != nil {
			return err
		}
		row.PricingInfo = &text
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string]any{"actors": list})
}

type namespacePermission struct {
	Namespace string `json:"namespace"`
	CanRead   bool   `json:"can_read"`
	CanWrite  bool   `json:"can_write"`
	CanDelete bool   `json:"can_delete"`
}

// actorsPermissionsRead is POST /admin/actors/permissions/read: one actor's
// namespace grants.
func (rt *Routes) actorsPermissionsRead(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return readError("actor_id")
	}
	actorID, ok := parseActorID(w, f)
	if !ok {
		return nil
	}
	if ok, err := rt.actorVisible(w, r, actorID); !ok {
		return err
	}
	rows, err := rt.DB.Query(r.Context(),
		"SELECT namespace, can_read, can_write, can_delete FROM namespace_permissions WHERE actor_id = $1 ORDER BY namespace", actorID)
	if err != nil {
		return err
	}
	defer rows.Close()
	list := []namespacePermission{}
	for rows.Next() {
		var p namespacePermission
		if err := rows.Scan(&p.Namespace, &p.CanRead, &p.CanWrite, &p.CanDelete); err != nil {
			return err
		}
		list = append(list, p)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string]any{"permissions": list})
}

// member is p.<name> for an element of a request-body array: an object's
// own member, else undefined (nil). A null element is V8's TypeError.
func member(elem any, name string) (any, error) {
	switch v := elem.(type) {
	case nil:
		return nil, errors.New("Cannot read properties of null (reading '" + name + "')")
	case *jsutil.Object:
		value, _ := v.Get(name)
		return value, nil
	}
	return nil, nil
}

// actorsPermissionsSave is POST /admin/actors/permissions/save: a full
// replace of one actor's namespace grants. Superadmin only: granting
// cross-actor namespace access (the '/' wildcard included) is an admin
// operation, not a per-owned-resource one.
func (rt *Routes) actorsPermissionsSave(w http.ResponseWriter, r *http.Request) error {
	if ok, err := rt.requireSuperadmin(w, r, "Managing namespace permissions requires superadmin"); !ok {
		return err
	}
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return readError("actor_id")
	}
	actorID, ok := parseActorID(w, f)
	if !ok {
		return nil
	}
	raw, isArray := f.Array("permissions")
	if !isArray {
		return httpapi.BadRequest(w, "Required field: permissions (array)")
	}
	if ok, err := rt.actorExists(w, r, actorID); !ok {
		return err
	}

	normalized := make([]namespacePermission, 0, len(raw))
	for _, elemRaw := range raw {
		elem, err := jsutil.Parse(elemRaw)
		if err != nil {
			return err
		}
		var p namespacePermission
		ns, err := member(elem, "namespace")
		if err != nil {
			return err
		}
		if s, isString := ns.(string); isString {
			p.Namespace = jsutil.Trim(s)
		}
		for _, field := range []struct {
			name string
			dst  *bool
		}{{"can_read", &p.CanRead}, {"can_write", &p.CanWrite}, {"can_delete", &p.CanDelete}} {
			v, _ := member(elem, field.name)
			*field.dst = jsutil.Truthy(v)
		}
		normalized = append(normalized, p)
	}
	seen := map[string]bool{}
	for _, p := range normalized {
		if p.Namespace == "/" {
			continue
		}
		if err := permissions.ValidateNamespace(p.Namespace); err != nil {
			return httpapi.BadRequest(w, err.Error())
		}
		if p.Namespace == "" {
			return httpapi.BadRequest(w, "Empty namespace not allowed")
		}
	}
	for _, p := range normalized {
		if seen[p.Namespace] {
			return httpapi.BadRequest(w, "Duplicate namespace entries")
		}
		seen[p.Namespace] = true
	}

	ctx := r.Context()
	tx, err := rt.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "DELETE FROM namespace_permissions WHERE actor_id = $1", actorID); err != nil {
		return err
	}
	for _, p := range normalized {
		if _, err := tx.Exec(ctx,
			"INSERT INTO namespace_permissions (actor_id, namespace, can_read, can_write, can_delete) VALUES ($1, $2, $3, $4, $5)",
			actorID, p.Namespace, p.CanRead, p.CanWrite, p.CanDelete); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	rt.Namespaces.ClearCache(actorID)
	rt.LogAdmin("permissions.save", logger.Fields{{Key: "actor_id", Value: actorID}, {Key: "count", Value: len(normalized)}})
	return httpapi.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type visibilityGrant struct {
	TargetActorID *int    `json:"target_actor_id"`
	TargetName    *string `json:"target_name"`
	TargetIsAgent bool    `json:"target_is_agent"`
	TargetIsUser  bool    `json:"target_is_user"`
}

// actorsVisibilityRead is POST /admin/actors/visibility/read: one actor's
// visibility grants. A row with no target is the wildcard.
func (rt *Routes) actorsVisibilityRead(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return readError("actor_id")
	}
	actorID, ok := parseActorID(w, f)
	if !ok {
		return nil
	}
	if ok, err := rt.actorVisible(w, r, actorID); !ok {
		return err
	}
	rows, err := rt.DB.Query(r.Context(),
		`SELECT avc.target_actor_id, a.name AS target_name,
                (ac.actor_id IS NOT NULL) AS target_is_agent,
                (a.password_hash IS NOT NULL) AS target_is_user
         FROM actor_visibility_configuration avc
         LEFT JOIN actors a ON a.id = avc.target_actor_id
         LEFT JOIN agent_configuration ac ON ac.actor_id = a.id
         WHERE avc.actor_id = $1
         ORDER BY a.name NULLS FIRST`, actorID)
	if err != nil {
		return err
	}
	defer rows.Close()
	wildcard := false
	grants := []visibilityGrant{}
	for rows.Next() {
		var g visibilityGrant
		if err := rows.Scan(&g.TargetActorID, &g.TargetName, &g.TargetIsAgent, &g.TargetIsUser); err != nil {
			return err
		}
		if g.TargetActorID == nil {
			wildcard = true
			continue
		}
		grants = append(grants, g)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, struct {
		Wildcard bool              `json:"wildcard"`
		Grants   []visibilityGrant `json:"grants"`
	}{wildcard, grants})
}

// actorsVisibilitySave is POST /admin/actors/visibility/save: a full
// replace of one actor's visibility grants, for oneself or an agent one
// created. The caller can only share sight it already has (the delegation
// bound), or ownership alone would let any signup with agents:write grant
// itself wildcard visibility; a superadmin skips the bound.
func (rt *Routes) actorsVisibilitySave(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return readError("actor_id")
	}
	actorID, ok := parseActorID(w, f)
	if !ok {
		return nil
	}
	wildcardRaw := string(f.Raw("wildcard"))
	grantsRaw, isArray := f.Array("grants")
	if wildcardRaw != "true" && wildcardRaw != "false" || !isArray {
		return httpapi.BadRequest(w, "Required fields: wildcard (bool), grants (array of actor IDs)")
	}
	wildcard := wildcardRaw == "true"
	if ok, err := rt.actorExists(w, r, actorID); !ok {
		return err
	}
	if ok, err := rt.RequireOwnershipID(w, r, actorID); !ok {
		return err
	}

	normalized := make([]int, 0, len(grantsRaw))
	for _, g := range grantsRaw {
		n, valid := SafeInt(g)
		if !valid || n <= 0 {
			return httpapi.BadRequest(w, "Invalid grant actor ID")
		}
		normalized = append(normalized, int(n))
	}
	// Deduplicated in first-seen order (a JavaScript Set), without the
	// actor itself.
	seen := map[int]bool{}
	deduped := []int{}
	for _, g := range normalized {
		if seen[g] || g == actorID {
			seen[g] = true
			continue
		}
		seen[g] = true
		deduped = append(deduped, g)
	}

	if len(deduped) > 0 {
		var count int
		if err := rt.DB.QueryRow(r.Context(), "SELECT count(*) FROM actors WHERE id = ANY($1)", deduped).Scan(&count); err != nil {
			return err
		}
		if count != len(deduped) {
			return httpapi.BadRequest(w, "One or more target actors do not exist")
		}
	}

	callerID := httpapi.StateOf(r).ActorID
	callerVisible, err := rt.Visibility.VisibleActorIDs(r.Context(), callerID)
	if err != nil {
		return err
	}
	if !callerVisible.All {
		if wildcard {
			// Realm-wide sight is not something self or created ownership
			// confers: the caller must hold the wildcard itself.
			var one int
			err := rt.DB.QueryRow(r.Context(),
				"SELECT 1 FROM actor_visibility_configuration WHERE actor_id = $1 AND target_actor_id IS NULL", callerID).Scan(&one)
			if errors.Is(err, pgx.ErrNoRows) {
				httpapi.WriteError(w, http.StatusForbidden, "FORBIDDEN", "Granting wildcard visibility requires wildcard visibility yourself")
				return nil
			}
			if err != nil {
				return err
			}
		} else {
			for _, g := range deduped {
				if !callerVisible.Has(g) {
					httpapi.WriteError(w, http.StatusForbidden, "FORBIDDEN", "You can only grant visibility of actors you can see yourself")
					return nil
				}
			}
		}
	}

	ctx := r.Context()
	tx, err := rt.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "DELETE FROM actor_visibility_configuration WHERE actor_id = $1", actorID); err != nil {
		return err
	}
	if wildcard {
		if _, err := tx.Exec(ctx, "INSERT INTO actor_visibility_configuration (actor_id, target_actor_id) VALUES ($1, NULL)", actorID); err != nil {
			return err
		}
	} else {
		for _, target := range deduped {
			if _, err := tx.Exec(ctx, "INSERT INTO actor_visibility_configuration (actor_id, target_actor_id) VALUES ($1, $2)", actorID, target); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	rt.Visibility.ClearCache(actorID)
	rt.LogAdmin("visibility.save", logger.Fields{{Key: "actor_id", Value: actorID}, {Key: "wildcard", Value: wildcard}, {Key: "count", Value: len(deduped)}})
	return httpapi.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// actorsPassword is POST /admin/actors/password: sets or clears (password
// null or absent) an actor's dashboard password, for oneself or an agent
// one created.
func (rt *Routes) actorsPassword(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("actor_id")
	}
	actorID, ok := parseActorID(w, f)
	if !ok {
		return nil
	}
	actor, found, err := rt.Actors.ResolveByID(r.Context(), actorID)
	if err != nil {
		return err
	}
	if !found {
		httpapi.WriteError(w, http.StatusNotFound, "NOT_FOUND", "Actor not found")
		return nil
	}
	if ok, err := rt.RequireOwnershipID(w, r, actorID); !ok {
		return err
	}
	userID := User(r).ID

	raw := f.Raw("password")
	if raw == nil || string(raw) == "null" {
		ctx := r.Context()
		if _, err := rt.DB.Exec(ctx, "UPDATE actors SET password_hash = NULL, password_salt = NULL WHERE id = $1", actorID); err != nil {
			return err
		}
		rt.LogAdmin("actor_password_clear", logger.Fields{{Key: "actor_id", Value: actorID}, {Key: "actor_name", Value: actor.Name}, {Key: "user_id", Value: userID}})
		return httpapi.WriteJSON(w, http.StatusOK, struct {
			Message string `json:"message"`
			IsUser  bool   `json:"is_user"`
		}{"UI access removed", false})
	}

	minText, err := rt.Config.Get("minimum_password_length")
	if err != nil {
		return err
	}
	minLen, ok := jsutil.ParseInt(minText)
	if !ok || minLen == 0 {
		minLen = 10
	}
	password, isString := f.String("password")
	if !isString || float64(jsutil.UTF16Len(password)) < minLen {
		return httpapi.BadRequest(w, "Password must be at least "+jsutil.NumberString(minLen)+" characters")
	}
	salt := auth.GenerateSalt()
	hash, err := auth.Hash(password, salt)
	if err != nil {
		return err
	}
	ctx := r.Context()
	if _, err := rt.DB.Exec(ctx, "UPDATE actors SET password_hash = $1, password_salt = $2 WHERE id = $3", hash, salt, actorID); err != nil {
		return err
	}
	rt.LogAdmin("actor_password_set", logger.Fields{{Key: "actor_id", Value: actorID}, {Key: "actor_name", Value: actor.Name}, {Key: "user_id", Value: userID}})
	return httpapi.WriteJSON(w, http.StatusOK, struct {
		Message string `json:"message"`
		IsUser  bool   `json:"is_user"`
	}{"Password updated", true})
}

// actorsNamespaces is POST /admin/actors/namespaces: the distinct document
// namespaces, for the permissions dropdown.
func (rt *Routes) actorsNamespaces(w http.ResponseWriter, r *http.Request) error {
	rows, err := rt.DB.Query(r.Context(), "SELECT DISTINCT namespace FROM documents WHERE namespace != '/' ORDER BY namespace")
	if err != nil {
		return err
	}
	defer rows.Close()
	list := []string{}
	for rows.Next() {
		var ns string
		if err := rows.Scan(&ns); err != nil {
			return err
		}
		list = append(list, ns)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string][]string{"namespaces": list})
}

// actorsSearchable is POST /admin/actors/searchable: actors that opted in
// to being found (visible_to_others) whose name contains query, for the
// share dialog. % and _ in query are LIKE wildcards, as in Node.
func (rt *Routes) actorsSearchable(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("query")
	}
	query, err := f.OptionalString("query")
	if err != nil {
		return err
	}
	rows, err := rt.DB.Query(r.Context(),
		`SELECT id, name FROM actors
         WHERE visible_to_others = true AND name ILIKE $1 AND id != $2
         ORDER BY name LIMIT 20`, "%"+query+"%", httpapi.StateOf(r).ActorID)
	if err != nil {
		return err
	}
	defer rows.Close()
	type found struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	list := []found{}
	for rows.Next() {
		var a found
		if err := rows.Scan(&a.ID, &a.Name); err != nil {
			return err
		}
		list = append(list, a)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string][]found{"actors": list})
}

// strictlyEquals is Node's value === n for a request-body value: true only
// for a JSON number with that value.
func strictlyEquals(raw json.RawMessage, n int) bool {
	if len(raw) == 0 || !(raw[0] == '-' || raw[0] >= '0' && raw[0] <= '9') {
		return false
	}
	return jsutil.ToNumber(raw) == float64(n)
}
