package simapi

import (
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/sanitize"
)

// PermissionGate wraps a route in an admin-permission check
// (permissions.Admin.RequirePerm).
type PermissionGate interface {
	RequirePerm(resource, action string, next http.Handler) http.Handler
}

// Routes are the /v1/sim routes.
type Routes struct {
	Service *Service
	Router  *httpapi.Router
	Admin   PermissionGate
}

// engineAgent is the salem engine's service account.
const engineAgent = "salem-engine"

// Mount puts the /sim routes in front of the /v1 mux, as routes/sim.js
// was mounted: POST /sim/raw-turns first (operator-gated, plugins/
// administer), then every other request under /sim, any method, through
// the salem-engine gate (the engine router's router.use), so another
// caller gets 403 there even for OPTIONS or a path with no route. For the
// engine, the engine routes (conversation-day, soul), Express's automatic
// OPTIONS answers of both routers, and otherwise the rest of /v1 (the
// admin router in Node).
func (rt *Routes) Mount(v1 http.Handler) http.Handler {
	rawTurns := rt.Admin.RequirePerm("plugins", "administer", rt.Router.Route("sim", "raw_turns", rt.rawTurns))
	afterEngine := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions && httpapi.RawPath(r) == "/v1/sim/raw-turns" {
			httpapi.WriteOptions(w, "POST")
			return
		}
		v1.ServeHTTP(w, r)
	})
	engine := httpapi.NewRawMux("/v1/sim", afterEngine)
	engine.Handle(http.MethodPost, "/conversation-day", rt.Router.Route("sim", "conversation_day", rt.conversationDay))
	engine.Handle(http.MethodPost, "/soul", rt.Router.Route("sim", "soul", rt.soul))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := httpapi.RawPath(r)
		if r.Method == http.MethodPost && p == "/v1/sim/raw-turns" {
			rawTurns.ServeHTTP(w, r)
			return
		}
		if p != "/v1/sim" && !strings.HasPrefix(p, "/v1/sim/") {
			v1.ServeHTTP(w, r)
			return
		}
		// The blanket /v1 auth admits every actor; these routes cost an LLM
		// call or write into another agent's namespace, and only the engine
		// calls them. An admin web session has no agent, so it is refused too.
		if httpapi.StateOf(r).AuthenticatedAgent != engineAgent {
			httpapi.WriteError(w, http.StatusForbidden, "FORBIDDEN", "This route is restricted to the salem-engine service account")
			return
		}
		engine.ServeHTTP(w, r)
	})
}

// body is req.body || {} as a parsed JSON value.
func body(r *http.Request) (any, error) {
	raw := httpapi.StateOf(r).Body
	if raw == nil {
		return jsutil.NewObject(), nil
	}
	return jsutil.Parse(raw)
}

func member(v any, key string) any {
	if o, ok := v.(*jsutil.Object); ok {
		return o.Value(key)
	}
	return jsutil.Undefined
}

func (rt *Routes) conversationDay(w http.ResponseWriter, r *http.Request) error {
	b, err := body(r)
	if err != nil {
		return err
	}
	in := DistillInput{
		Day: member(b, "day"), Events: member(b, "events"),
		SlugPrefix: member(b, "slug_prefix"), Actor: member(b, "actor"),
	}
	if agent, ok := member(b, "agent").(string); ok {
		in.Agent, in.AgentIsString = sanitize.AgentName(agent), true
	}
	result, err := rt.Service.Distill(r.Context(), in)
	if err != nil {
		return err
	}
	httpapi.WriteJSON(w, http.StatusOK, result)
	return nil
}

func (rt *Routes) soul(w http.ResponseWriter, r *http.Request) error {
	b, err := body(r)
	if err != nil {
		return err
	}
	result, err := rt.Service.SynthesizeSoul(r.Context(), SoulInput{
		CharacterDescription: member(b, "character_description"),
		CurrentSoul:          member(b, "current_soul"),
		DaySnapshot:          member(b, "day_snapshot"),
		Day:                  member(b, "day"),
	})
	if err != nil {
		return err
	}
	httpapi.WriteJSON(w, http.StatusOK, result)
	return nil
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// present is `v !== undefined && v !== null && v !== ”`.
func present(v any) bool {
	return v != nil && v != jsutil.Undefined && v != ""
}

// safeInt is util.js's safeInt: a number or a non-blank string whose
// Number() is a safe integer; ok false otherwise.
func safeInt(v any) (float64, bool) {
	switch x := v.(type) {
	case string:
		if jsutil.Trim(x) == "" {
			return 0, false
		}
	case nil, bool, []any, *jsutil.Object:
		return 0, false
	}
	if v == jsutil.Undefined {
		return 0, false
	}
	n, err := jsutil.ValueNumber(v)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) || math.Abs(n) > 1<<53-1 {
		return 0, false
	}
	return n, true
}

// rawTurns is POST /sim/raw-turns: the full virtual_agent_calls rows behind
// sim turns, newest first, for operators debugging through the salem
// umbilical. Every filter is optional and bound. A bounding filter (scene,
// conversation, agent, sim actor) returns its whole set by default (at
// most 500); without one the default is 5 (at most 50). One extra row is
// fetched to set has_more without a COUNT.
func (rt *Routes) rawTurns(w http.ResponseWriter, r *http.Request) error {
	b, err := body(r)
	if err != nil {
		return err
	}
	var conditions []string
	var params []any
	bounded := false
	add := func(cond string, v any) {
		params = append(params, v)
		conditions = append(conditions, cond+strconv.Itoa(len(params)))
	}

	if v := member(b, "scene_id"); present(v) {
		scene, err := jsutil.ValueString(v)
		if err != nil {
			return err
		}
		if !uuidPattern.MatchString(scene) {
			return httpapi.BadRequest(w, "scene_id must be a UUID")
		}
		add("c.scene_id = $", scene)
		bounded = true
	}
	for _, f := range []struct{ field, column string }{{"agent", "ac.name"}, {"conversation", "c.conversation_id"}} {
		v := member(b, f.field)
		if !present(v) {
			continue
		}
		s, ok := v.(string)
		if !ok {
			return httpapi.BadRequest(w, f.field+" must be a string")
		}
		if jsutil.UTF16Len(s) > 200 {
			return httpapi.BadRequest(w, f.field+" is too long")
		}
		add(f.column+" = $", s)
		bounded = true
	}
	if v := member(b, "sim_actor"); present(v) {
		s, ok := v.(string)
		if !ok {
			return httpapi.BadRequest(w, "sim_actor must be a string")
		}
		// Trimmed and capped as chat stores it; blank is no filter.
		s = jsutil.Trim(s)
		if jsutil.UTF16Len(s) > 100 {
			return httpapi.BadRequest(w, "sim_actor is too long")
		}
		if s != "" {
			add("c.sim_actor_id = $", s)
			bounded = true
		}
	}
	for _, f := range []struct{ field, op string }{{"since", ">="}, {"until", "<"}} {
		v := member(b, f.field)
		if !present(v) {
			continue
		}
		ms, ok, err := jsutil.DateValue(v, time.Local)
		if err != nil {
			return err
		}
		if !ok {
			return httpapi.BadRequest(w, f.field+" must be a valid timestamp")
		}
		add("c.created_at "+f.op+" $", jsutil.DateISO(time.UnixMilli(int64(ms))))
	}
	if v := member(b, "status"); present(v) {
		if v != "success" && v != "error" {
			return httpapi.BadRequest(w, "status must be 'success' or 'error'")
		}
		add("c.status = $", v)
	}

	defaultLimit, maxLimit := 5.0, 50.0
	if bounded {
		defaultLimit, maxLimit = 500, 500
	}
	limit, ok := safeInt(member(b, "limit"))
	if !ok {
		limit = defaultLimit
	}
	limit = math.Min(math.Max(limit, 1), maxLimit)

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}
	params = append(params, int(limit)+1)
	turns, err := rt.Service.RawTurns(r.Context(), where, len(params), params)
	if err != nil {
		return err
	}
	hasMore := len(turns) > int(limit)
	if hasMore {
		turns = turns[:int(limit)]
	}
	httpapi.WriteJSON(w, http.StatusOK, rawTurnsResponse{Turns: turns, Returned: len(turns), HasMore: hasMore})
	return nil
}

type rawTurnsResponse struct {
	Turns    []Turn `json:"turns"`
	Returned int    `json:"returned"`
	HasMore  bool   `json:"has_more"`
}
