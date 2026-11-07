package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"testing"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/auth"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

func TestAgentField(t *testing.T) {
	cases := []struct {
		body         string
		agent        string
		given, isErr bool
	}{
		{`{"agent":"Agent-A"}`, "agent-a", true, false},
		{`{"agent":"  "}`, "", false, false},
		{`{}`, "", false, false},
		{`{"agent":0}`, "", false, false},
		{`{"agent":5}`, "", true, true},
		{`{"agent":["a"]}`, "", true, true},
	}
	for _, c := range cases {
		var f httpapi.Fields
		json.Unmarshal([]byte(c.body), &f)
		agent, given, err := agentField(f)
		if agent != c.agent || given != c.given || (err != nil) != c.isErr {
			t.Errorf("%s = %q %v %v", c.body, agent, given, err)
		}
	}
}

func TestParseCostBudget(t *testing.T) {
	cases := []struct {
		raw  string
		want any
		err  string
	}{
		{``, nil, ""}, {`null`, nil, ""}, {`""`, nil, ""},
		{`5`, jsutil.Number("5"), ""}, {`"12.50"`, jsutil.Number("12.5"), ""}, {`" "`, jsutil.Number("0"), ""},
		{`true`, jsutil.Number("1"), ""}, {`[]`, jsutil.Number("0"), ""},
		{`-1`, nil, "Invalid cost_budget_daily: must be a non-negative number up to 99999999.99"},
		{`"abc"`, nil, "Invalid cost_budget_daily: must be a non-negative number up to 99999999.99"},
		{`100000000`, nil, "Invalid cost_budget_daily: must be a non-negative number up to 99999999.99"},
		{`{}`, nil, "Invalid cost_budget_daily: must be a non-negative number up to 99999999.99"},
	}
	for _, c := range cases {
		var raw json.RawMessage
		if c.raw != "" {
			raw = json.RawMessage(c.raw)
		}
		got, err := parseCostBudget(raw, "cost_budget_daily")
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		if !reflect.DeepEqual(got, c.want) || msg != c.err {
			t.Errorf("%s = %v %q", c.raw, got, msg)
		}
	}
}

func TestConfigurationText(t *testing.T) {
	cases := map[string]string{
		`{"temperature":"","top_p":0.5,"other":"","max_completion_tokens":""}`: `{"top_p":0.5,"other":""}`,
		`{"b":1,"2":"x","1":"y"}`: `{"1":"y","2":"x","b":1}`,
		`["a",{"t":""}]`:          `{"0":"a","1":{"t":""}}`,
		`"text"`:                  `"text"`,
		`5`:                       `5`,
		`true`:                    `true`,
	}
	for in, want := range cases {
		got, err := configurationText(json.RawMessage(in))
		if err != nil || got != want {
			t.Errorf("%s = %s %v, want %s", in, got, err, want)
		}
	}
}

func TestParseFloatValue(t *testing.T) {
	cases := map[string]string{`"0.7abc"`: "0.7", `1`: "1", `[2]`: "2", `true`: "NaN", `{}`: "NaN", `""`: "NaN"}
	for in, want := range cases {
		got, err := parseFloatValue(json.RawMessage(in))
		if err != nil || jsutil.NumberString(got) != want {
			t.Errorf("%s = %v %v, want %s", in, got, err, want)
		}
	}
}

func TestAgentsRequiredFields(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	cases := []struct {
		name string
		fn   func(*Routes) func(*httptest.ResponseRecorder, string) error
		body string
		want string
	}{
		{"instructions read", call((*Routes).agentsInstructionsRead), `{"agent":" "}`, "Required field: agent"},
		{"instructions save, no content", call((*Routes).agentsInstructionsSave), `{"agent":"a"}`, "Required fields: agent, content"},
		{"expertise not an array", call((*Routes).agentsExpertiseSave), `{"agent":"a","expertise":"go"}`, "Required fields: agent, expertise (array of strings)"},
		{"reset", call((*Routes).agentsResetPassphrase), `{}`, "Required field: agent"},
		{"read", call((*Routes).agentsRead), `{"agent":null}`, "Required field: agent"},
		{"update", call((*Routes).agentsUpdate), `{"agent":""}`, "Required field: agent"},
		{"usage", call((*Routes).agentsUsage), `{}`, "Required field: agent"},
		{"call detail zero", call((*Routes).agentsCallDetail), `{"call_id":0}`, "Required field: call_id"},
		{"call detail text", call((*Routes).agentsCallDetail), `{"call_id":"x"}`, "Required field: call_id"},
		{"create, no name", call((*Routes).actorsCreate), `{"name":"   "}`, "Required field: name"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		if err := c.fn(rt)(rec, c.body); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		check(t, c.name, rec, 400, errorBody("BAD_REQUEST", c.want))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func call(fn func(*Routes, http.ResponseWriter, *http.Request) error) func(*Routes) func(*httptest.ResponseRecorder, string) error {
	return func(rt *Routes) func(*httptest.ResponseRecorder, string) error {
		return func(rec *httptest.ResponseRecorder, body string) error {
			return fn(rt, rec, request(body))
		}
	}
}

// Fields of another JSON type are refused after the required check; no
// body at all is V8's TypeError, as in Node.
func TestAgentsFieldTypes(t *testing.T) {
	rt, mock, _ := newRoutes(t, map[string]string{"minimum_password_length": "10"})
	cases := []struct {
		name string
		fn   func(*Routes) func(*httptest.ResponseRecorder, string) error
		body string
		want string
	}{
		{"agent", call((*Routes).agentsRead), `{"agent":7}`, "agent must be a string"},
		{"content", call((*Routes).agentsInstructionsSave), `{"agent":"a","content":5}`, "content must be a string"},
		{"name", call((*Routes).actorsCreate), `{"name":5}`, "name must be a string"},
		{"password", call((*Routes).actorsCreate), `{"name":"x","ui_access":true,"password":12345678901}`, "password must be a string"},
		{"provider", call((*Routes).actorsCreate), `{"name":"x","provider":{"a":1}}`, "provider must be a string"},
		{"no body", call((*Routes).agentsUpdate), ``, "Cannot read properties of undefined (reading 'agent')"},
		{"no body create", call((*Routes).actorsCreate), ``, "Cannot destructure property 'name' of 'req.body' as it is undefined."},
	}
	for _, c := range cases {
		err := c.fn(rt)(httptest.NewRecorder(), c.body)
		if err == nil || err.Error() != c.want {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// actors/create's checks before any query, in Node's order.
func TestActorsCreateValidation(t *testing.T) {
	rt, mock, _ := newRoutes(t, map[string]string{"minimum_password_length": "12"})
	email := `"`
	for i := 0; i < 256; i++ {
		email += "a"
	}
	email += `"`
	cases := []struct{ body, want string }{
		{`{"name":"x","ui_access":1}`, "Password must be at least 12 characters"},
		{`{"name":"x","ui_access":true,"password":"short"}`, "Password must be at least 12 characters"},
		{`{"name":"x","max_tokens":0}`, "max_tokens must be a positive integer"},
		{`{"name":"x","max_tokens":"1.5"}`, "max_tokens must be a positive integer"},
		{`{"name":"x","temperature":"hot"}`, "temperature must be a number between 0 and 2"},
		{`{"name":"x","temperature":2.5}`, "temperature must be a number between 0 and 2"},
		{`{"name":"x","email":` + email + `}`, "email must be 255 characters or less"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		if err := rt.actorsCreate(rec, request(c.body)); err != nil {
			t.Fatalf("%s: %v", c.body, err)
		}
		check(t, c.body, rec, 400, errorBody("BAD_REQUEST", c.want))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The caller's own agent: visible to itself, and owned once the ownership
// check has read the caller's admin grants (none, so not a superadmin).
func ownAgent(mock pgxmock.PgxPoolIface, firstOwnershipCheck bool) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, name FROM actors WHERE name = $1")).WithArgs("admin").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(2, "admin"))
	if firstOwnershipCheck {
		grants(mock, 2)
	}
}

func TestAgentsResetPassphrase(t *testing.T) {
	rt, mock, tokens := newRoutes(t, nil)
	ownAgent(mock, true)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE actors SET token_hash = $1, token_salt = $2, passphrase_rotated_at = NOW() WHERE id = $3 RETURNING id")).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), 2).WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(2))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM sessions WHERE actor_id = $1 AND kind = $2")).
		WithArgs(2, auth.SessionKindAPI).WillReturnResult(pgxmock.NewResult("DELETE", 3))
	mock.ExpectCommit()
	mock.ExpectRollback()
	rec := httptest.NewRecorder()
	if err := rt.agentsResetPassphrase(rec, request(`{"agent":"Admin"}`)); err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 200 || body["agent"] != "admin" || body["message"] != "Passphrase reset. All sessions invalidated." || body["passphrase"] == "" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if !reflect.DeepEqual(tokens.agents, []string{"admin"}) {
		t.Fatalf("forgot %v", tokens.agents)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A JSON null is written, as Node wrote it, and then fails as reading its
// length did.
func TestAgentsInstructionsSaveNull(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	ownAgent(mock, true)
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE agent_configuration SET startup_instructions = $1 WHERE actor_id = $2 RETURNING actor_id")).
		WithArgs((*string)(nil), 2).WillReturnRows(pgxmock.NewRows([]string{"actor_id"}).AddRow(2))
	err := rt.agentsInstructionsSave(httptest.NewRecorder(), request(`{"agent":"admin","content":null}`))
	if err == nil || err.Error() != "Cannot read properties of null (reading 'length')" {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentsExpertiseSave(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	ownAgent(mock, true)
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE actors SET expertise = $1 WHERE id = $2 RETURNING id")).
		WithArgs(`["go","postgres"]`, 2).WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(2))
	rec := httptest.NewRecorder()
	if err := rt.agentsExpertiseSave(rec, request(`{"agent":"admin","expertise":[" Go ","",5,"POSTGRES",null,"  "]}`)); err != nil {
		t.Fatal(err)
	}
	check(t, "expertise", rec, 200, `{"agent":"admin","expertise":["go","postgres"],"message":"Expertise saved"}`)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentsUpdate(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	first := true
	// The actor cache answers the name after the first lookup.
	existing := func() {
		if first {
			ownAgent(mock, true)
		}
		first = false
		mock.ExpectQuery(regexp.QuoteMeta("SELECT actor_id, virtual FROM agent_configuration WHERE actor_id = $1")).WithArgs(2).
			WillReturnRows(pgxmock.NewRows([]string{"actor_id", "virtual"}).AddRow(2, false))
	}

	cases := []struct{ body, want string }{
		{`{"agent":"admin"}`, "No fields to update"},
		{`{"agent":"admin","realms":"x"}`, "No fields to update"},
		{`{"agent":"admin","max_tokens":-1}`, "max_tokens must be a positive integer"},
		{`{"agent":"admin","storage_quota":-1}`, "storage_quota must be a non-negative integer"},
		{`{"agent":"admin","dream_mode":null}`, "dream_mode must be none, companion, technical, sim, or sim-shared"},
		{`{"agent":"admin","dream_source":"dreams"}`, "dream_source must be conversation or notes"},
	}
	for _, c := range cases {
		existing()
		rec := httptest.NewRecorder()
		if err := rt.agentsUpdate(rec, request(c.body)); err != nil {
			t.Fatalf("%s: %v", c.body, err)
		}
		check(t, c.body, rec, 400, errorBody("BAD_REQUEST", c.want))
	}

	existing()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_configuration SET personality = $1, model = $2, cost_budget_daily = $3, cache_prompts = $4, learning_enabled = $5, max_tokens = $6, temperature = $7, storage_quota = $8, dream_mode = $9 WHERE actor_id = $10")).
		WithArgs(nil, "m", jsutil.Number("2.5"), false, true, int64(100), jsutil.Number("NaN"), nil, "sim", 2).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE actors SET realms = $1 WHERE id = $2")).WithArgs(`{"a","1"}`, 2).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	rec := httptest.NewRecorder()
	body := `{"agent":"admin","personality":"","model":"m","cost_budget_daily":"2.5","cache_prompts":"yes","learning_enabled":0,` +
		`"max_tokens":"100","temperature":true,"storage_quota":"","dream_mode":"sim","realms":["a",1]}`
	if err := rt.agentsUpdate(rec, request(body)); err != nil {
		t.Fatal(err)
	}
	check(t, "update", rec, 200, `{"agent":"admin","updated":true}`)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
