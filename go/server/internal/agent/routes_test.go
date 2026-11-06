package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/auth"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/syserrors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/visibility"
)

type fakeEvents struct {
	names []string
	data  []any
}

func (f *fakeEvents) Broadcast(name string, data any) {
	f.names = append(f.names, name)
	f.data = append(f.data, data)
}

func newRoutes(t *testing.T, values map[string]string) (*Routes, pgxmock.PgxPoolIface, *fakeEvents) {
	mock := testutil.NewMock(t)
	cache := actors.NewCache(mock)
	log := logger.New(mock, cache, "")
	store := config.NewStore()
	for k, v := range values {
		store.Set(k, v)
	}
	ev := &fakeEvents{}
	sessions := &auth.Sessions{DB: mock}
	return &Routes{
		DB:          mock,
		Config:      store,
		Log:         log,
		Router:      &httpapi.Router{Log: log},
		Events:      ev,
		Sessions:    sessions,
		SessionAuth: &fakeCache{},
		Notes: &notes.Service{DB: mock, Config: store, Actors: cache,
			Errors: &syserrors.Recorder{DB: mock, Actors: cache, Errorf: log.Errorf}, Events: ev},
		Visibility: &visibility.Visibility{DB: mock, Admin: notSuper{}},
		Now:        func() time.Time { return time.Date(2026, 11, 6, 12, 0, 0, 123456789, time.UTC) },
	}, mock, ev
}

// fakeCache records what logout and rotate drop from the token cache.
type fakeCache struct {
	tokens, agents []string
}

func (f *fakeCache) Forget(token string)      { f.tokens = append(f.tokens, token) }
func (f *fakeCache) ForgetAgent(agent string) { f.agents = append(f.agents, agent) }

type notSuper struct{}

func (notSuper) IsSuperadmin(context.Context, int) (bool, error) { return false, nil }

func request(body, agent string, actorID int) *http.Request {
	r := httptest.NewRequest("POST", "/v1/agent/x", nil)
	st := &httpapi.State{AuthenticatedAgent: agent, ActorID: actorID}
	if body != "" {
		st.Body = json.RawMessage(body)
	}
	return httpapi.WithStateValue(r, st)
}

func errorBody(code, message string) string {
	m, _ := json.Marshal(message)
	return `{"error":{"code":"` + code + `","message":` + string(m) + `}}`
}

// The 400s and 401s Node wrote with res.status(...).json(), in its order,
// before any query.
func TestDirectErrors(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	type handler func(http.ResponseWriter, *http.Request) error
	cases := []struct {
		name      string
		fn        handler
		body      string
		agent     string
		status    int
		code, msg string
	}{
		{"login empty", rt.login, `{}`, "", 400, "BAD_REQUEST", "Required fields: agent, passphrase"},
		{"login agent sanitized away", rt.login, `{"agent":"!!","passphrase":"p"}`, "", 400, "BAD_REQUEST", "Required fields: agent, passphrase"},
		{"login no passphrase", rt.login, `{"agent":"a","passphrase":""}`, "", 400, "BAD_REQUEST", "Required fields: agent, passphrase"},
		{"rotate missing", rt.rotate, `{}`, "a", 400, "BAD_REQUEST", "Required field: current_passphrase"},
		{"heartbeat admin", rt.heartbeat, `{}`, "", 401, "UNAUTHORIZED", "Agent session required"},
		{"expertise admin", rt.expertise, `{}`, "", 401, "UNAUTHORIZED", "Agent session required"},
		{"expertise not array", rt.expertise, `{"expertise":"x"}`, "a", 400, "BAD_REQUEST", "Required field: expertise (array of strings)"},
		{"profile admin", rt.profile, `{}`, "", 401, "UNAUTHORIZED", "Agent session required"},
		{"profile none", rt.profile, `{"other":1}`, "a", 400, "BAD_REQUEST", "At least one field required: provider, model"},
		{"profile array body", rt.profile, `["provider"]`, "a", 400, "BAD_REQUEST", "At least one field required: provider, model"},
		{"activity admin", rt.activityStart, ``, "", 401, "UNAUTHORIZED", "Agent session required"},
		{"activity stop admin", rt.activityStop, ``, "", 401, "UNAUTHORIZED", "Agent session required"},
		{"signup admin", rt.signupEvent, ``, "", 401, "UNAUTHORIZED", "Agent session required"},
		{"instructions read admin", rt.instructionsRead, ``, "", 401, "UNAUTHORIZED", "Agent session required"},
		{"instructions save admin", rt.instructionsSave, ``, "", 401, "UNAUTHORIZED", "Agent session required"},
		{"instructions save missing", rt.instructionsSave, `{}`, "a", 400, "BAD_REQUEST", "Required field: content"},
		{"sync mappings admin", rt.syncMappings, ``, "", 401, "UNAUTHORIZED", "Agent session required"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		if err := c.fn(rec, request(c.body, c.agent, 7)); err != nil {
			t.Errorf("%s: err %v", c.name, err)
			continue
		}
		if rec.Code != c.status || rec.Body.String() != errorBody(c.code, c.msg) {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// With no JSON body, a route that read a field threw V8's TypeError, which
// the route wrapper logged as a 500. A field of the wrong type is refused.
func TestReturnedErrors(t *testing.T) {
	rt, _, _ := newRoutes(t, nil)
	cases := []struct {
		name  string
		fn    func(http.ResponseWriter, *http.Request) error
		body  string
		agent string
		want  string
	}{
		{"login no body", rt.login, ``, "", "Cannot destructure property 'passphrase' of 'req.body' as it is undefined."},
		{"rotate no body", rt.rotate, ``, "a", "Cannot destructure property 'current_passphrase' of 'req.body' as it is undefined."},
		{"expertise no body", rt.expertise, ``, "a", "Cannot destructure property 'expertise' of 'req.body' as it is undefined."},
		{"profile no body", rt.profile, ``, "a", "Cannot read properties of undefined (reading 'provider')"},
		{"instructions save no body", rt.instructionsSave, ``, "a", "Cannot read properties of undefined (reading 'content')"},
		{"login agent number", rt.login, `{"agent":5,"passphrase":"p"}`, "", "agent must be a string"},
		{"login passphrase number", rt.login, `{"agent":"a","passphrase":5}`, "", "passphrase must be a string"},
		{"login subsystem object", rt.login, `{"agent":"a","passphrase":"p","subsystem":{}}`, "", "subsystem must be a string"},
		{"rotate number", rt.rotate, `{"current_passphrase":5}`, "a", "current_passphrase must be a string"},
		{"profile number", rt.profile, `{"provider":5}`, "a", "provider must be a string"},
		{"profile model array", rt.profile, `{"provider":"p","model":[]}`, "a", "model must be a string"},
		{"instructions save number", rt.instructionsSave, `{"content":5}`, "a", "content must be a string"},
		{"signup bad event", rt.signupEvent, `{"event":"other","value":"x"}`, "a", "event must be one of: guide_shown, client, copy"},
		{"signup no body", rt.signupEvent, ``, "a", "event must be one of: guide_shown, client, copy"},
		{"signup bad value", rt.signupEvent, `{"event":"copy","value":"Bad"}`, "a", "value must be 1-32 lowercase letters, digits, hyphens or underscores, starting with a letter"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		err := c.fn(rec, request(c.body, c.agent, 7))
		if err == nil || err.Error() != c.want {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestLoginUnknownAgent(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE a.name = $1 AND a.token_hash IS NOT NULL")).
		WithArgs("ghost").
		WillReturnRows(pgxmock.NewRows([]string{"actor_id", "agent", "token_hash", "token_salt", "status"}))
	rec := httptest.NewRecorder()
	if err := rt.login(rec, request(`{"agent":" Ghost ","passphrase":"p"}`, "", 0)); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 403 || rec.Body.String() != errorBody("INVALID_CREDENTIALS", "Invalid agent or passphrase") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestLoginOpensSession(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	salt := "00ff"
	hash, _ := auth.Hash("right-phrase", salt)
	name, status := "agent-a", "active"
	mock.ExpectQuery(regexp.QuoteMeta("WHERE a.name = $1 AND a.token_hash IS NOT NULL")).
		WithArgs("agent-a").
		WillReturnRows(pgxmock.NewRows([]string{"actor_id", "agent", "token_hash", "token_salt", "status"}).
			AddRow(11, &name, &hash, &salt, &status))
	expires := time.Date(2026, 11, 7, 12, 0, 0, 123000000, time.UTC)
	sub := "sync"
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO sessions (actor_id, token_hash, token_salt, token_lookup_hash, kind, expires_at, subsystem)")).
		WithArgs(11, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), "api", expires, &sub).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE actors SET last_seen = NOW() WHERE id = $1")).
		WithArgs(11).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	rec := httptest.NewRecorder()
	if err := rt.login(rec, request(`{"agent":"agent-a","passphrase":"right-phrase","subsystem":" sync! "}`, "", 0)); err != nil {
		t.Fatal(err)
	}
	var got loginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Agent != "agent-a" || len(got.SessionToken) != 64 || got.ExpiresAt != "2026-11-07T12:00:00.123Z" {
		t.Errorf("%s", rec.Body)
	}
	if !strings.HasPrefix(rec.Body.String(), `{"agent":"agent-a","session_token":"`) {
		t.Errorf("key order: %s", rec.Body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestLoginWrongPassphrase(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	salt := "00ff"
	hash, _ := auth.Hash("right-phrase", salt)
	name, status := "agent-a", "active"
	mock.ExpectQuery(regexp.QuoteMeta("WHERE a.name = $1")).
		WithArgs("agent-a").
		WillReturnRows(pgxmock.NewRows([]string{"actor_id", "agent", "token_hash", "token_salt", "status"}).
			AddRow(11, &name, &hash, &salt, &status))
	rec := httptest.NewRecorder()
	if err := rt.login(rec, request(`{"agent":"agent-a","passphrase":"wrong"}`, "", 0)); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 403 {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// An admin web session has no agent: the answer and the log line leave the
// agent key out, as JSON.stringify dropped undefined.
func TestLogoutWithoutAgent(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	token := "web-token"
	mock.ExpectQuery(regexp.QuoteMeta("token_lookup_hash = $3")).
		WithArgs(3, "api", auth.TokenLookupHash(token)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "token_hash", "token_salt"}))
	mock.ExpectQuery(regexp.QuoteMeta("token_lookup_hash IS NULL")).
		WithArgs(3, "api").
		WillReturnRows(pgxmock.NewRows([]string{"id", "token_hash", "token_salt"}))
	r := request(`{}`, "", 3)
	r.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	if err := rt.logout(rec, r); err != nil {
		t.Fatal(err)
	}
	if rec.Body.String() != `{"message":"Logged out"}` {
		t.Errorf("%s", rec.Body)
	}
	if c := rt.SessionAuth.(*fakeCache); len(c.tokens) == 0 || c.tokens[0] != token {
		t.Errorf("token not forgotten: %v", c.tokens)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestRotateWrongPassphrase(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	salt := "00ff"
	hash, _ := auth.Hash("current", salt)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT token_hash, token_salt FROM actors WHERE id = $1")).
		WithArgs(11).
		WillReturnRows(pgxmock.NewRows([]string{"token_hash", "token_salt"}).AddRow(&hash, &salt))
	rec := httptest.NewRecorder()
	if err := rt.rotate(rec, request(`{"current_passphrase":"other"}`, "agent-a", 11)); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 403 || rec.Body.String() != errorBody("INVALID_PASSPHRASE", "Current passphrase does not match") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// An account with no passphrase salt (an admin user) made crypto.pbkdf2
// throw in Node.
func TestRotateNullSalt(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT token_hash, token_salt FROM actors WHERE id = $1")).
		WithArgs(3).
		WillReturnRows(pgxmock.NewRows([]string{"token_hash", "token_salt"}).AddRow((*string)(nil), (*string)(nil)))
	err := rt.rotate(httptest.NewRecorder(), request(`{"current_passphrase":"x"}`, "", 3))
	if err != auth.ErrNullSalt {
		t.Errorf("%v", err)
	}
}

func TestRotate(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	salt := "00ff"
	hash, _ := auth.Hash("current", salt)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT token_hash, token_salt FROM actors WHERE id = $1")).
		WithArgs(11).
		WillReturnRows(pgxmock.NewRows([]string{"token_hash", "token_salt"}).AddRow(&hash, &salt))
	expectRotateWrites(mock)
	rec := httptest.NewRecorder()
	if err := rt.rotate(rec, request(`{"current_passphrase":"current"}`, "agent-a", 11)); err != nil {
		t.Fatal(err)
	}
	if c := rt.SessionAuth.(*fakeCache); len(c.agents) == 0 || c.agents[0] != "agent-a" {
		t.Errorf("agent tokens not forgotten: %v", c.agents)
	}
	var got rotateResponse
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Agent != "agent-a" || strings.Count(got.Passphrase, "-") < 2 || !strings.HasPrefix(got.Message, "Passphrase rotated.") {
		t.Errorf("%s", rec.Body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestHeartbeatNotRegistered(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE actors SET last_seen = NOW() WHERE id = $1 RETURNING last_seen")).
		WithArgs(11).
		WillReturnRows(pgxmock.NewRows([]string{"last_seen"}))
	rec := httptest.NewRecorder()
	if err := rt.heartbeat(rec, request(``, "agent-a", 11)); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 404 || rec.Body.String() != errorBody("NOT_FOUND", `Agent "agent-a" is not registered`) {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

// Non-strings and blank strings are dropped; the rest are trimmed and
// lowercased, then sanitized (the zero-width space survives the trim but not
// sanitize).
func TestExpertiseCleaning(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE actors SET expertise = $1 WHERE id = $2")).
		WithArgs(`["go","postgres?"]`, 11).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	rec := httptest.NewRecorder()
	body := `{"expertise":[" Go ", 5, "", "  ", null, ["x"], "PostGres\u200b?"]}`
	if err := rt.expertise(rec, request(body, "agent-a", 11)); err != nil {
		t.Fatal(err)
	}
	if rec.Body.String() != `{"agent":"agent-a","expertise":["go","postgres?"],"message":"Expertise updated"}` {
		t.Errorf("%s", rec.Body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A field sent as null is set to NULL and echoed as null; a field not sent
// is left alone and left out of the answer.
func TestProfile(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_configuration SET provider = $1 WHERE actor_id = $2")).
		WithArgs((*string)(nil), 11).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	rec := httptest.NewRecorder()
	if err := rt.profile(rec, request(`{"provider":null}`, "agent-a", 11)); err != nil {
		t.Fatal(err)
	}
	if rec.Body.String() != `{"agent":"agent-a","provider":null,"message":"Profile updated"}` {
		t.Errorf("%s", rec.Body)
	}

	model := "claude-x.1"
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_configuration SET provider = $1, model = $2 WHERE actor_id = $3")).
		WithArgs(strPtr("anthropic"), &model, 11).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	rec = httptest.NewRecorder()
	if err := rt.profile(rec, request(`{"model":" claude-x.1! ","provider":"anthropic"}`, "agent-a", 11)); err != nil {
		t.Fatal(err)
	}
	if rec.Body.String() != `{"agent":"agent-a","provider":"anthropic","model":"claude-x.1","message":"Profile updated"}` {
		t.Errorf("%s", rec.Body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func strPtr(s string) *string { return &s }

func TestActivityBroadcasts(t *testing.T) {
	rt, mock, ev := newRoutes(t, nil)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE actors SET active_since = NOW() WHERE id = $1")).WithArgs(11).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE actors SET active_since = NULL WHERE id = $1")).WithArgs(11).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	rec := httptest.NewRecorder()
	rt.activityStart(rec, request(``, "agent-a", 11))
	if rec.Body.String() != `{"agent":"agent-a","active":true,"message":"Activity started"}` {
		t.Errorf("%s", rec.Body)
	}
	rec = httptest.NewRecorder()
	rt.activityStop(rec, request(``, "agent-a", 11))
	if rec.Body.String() != `{"agent":"agent-a","active":false,"message":"Activity stopped"}` {
		t.Errorf("%s", rec.Body)
	}
	if len(ev.names) != 2 || ev.names[0] != "agent_activity" {
		t.Fatalf("%v", ev.names)
	}
	if b, _ := json.Marshal(ev.data[1]); string(b) != `{"agent":"agent-a","active":false}` {
		t.Errorf("%s", b)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func instructionRows(startup, mode *string) *pgxmock.Rows {
	return pgxmock.NewRows([]string{"startup_instructions", "dream_mode"}).AddRow(startup, mode)
}

func expectSoul(mock pgxmock.PgxPoolIface, content *string) {
	q := mock.ExpectQuery(regexp.QuoteMeta("WHERE d.namespace = $1 AND LOWER(d.slug) = LOWER($2) AND d.deleted_at IS NULL")).
		WithArgs("agent-a", "context/soul")
	cols := []string{"id", "namespace", "slug", "title", "content", "created_by", "created_at", "updated_at", "metadata", "extension"}
	if content == nil {
		q.WillReturnRows(pgxmock.NewRows(cols))
		return
	}
	q.WillReturnRows(pgxmock.NewRows(cols).AddRow(1, "agent-a", "context/soul", (*string)(nil), *content, (*string)(nil), (*time.Time)(nil), (*time.Time)(nil), []byte(nil), (*string)(nil)))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE documents SET last_accessed = NOW()")).
		WithArgs("agent-a", "context/soul").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
}

func TestInstructionsRead(t *testing.T) {
	values := map[string]string{"dream_bootstrap": "DREAM"}
	cases := []struct {
		name          string
		startup, mode *string
		soul          *string
		want          string
	}{
		{"nothing", nil, strPtr("none"), nil, `{"agent":"agent-a","instructions":null}`},
		{"empty startup is null", strPtr(""), nil, nil, `{"agent":"agent-a","instructions":null}`},
		{"startup only", strPtr("S"), strPtr("none"), nil, `{"agent":"agent-a","instructions":"S"}`},
		{"dream bootstrap", strPtr("S"), strPtr("companion"), nil, `{"agent":"agent-a","instructions":"S\n\nDREAM"}`},
		{"sim-shared takes no bootstrap", strPtr("S"), strPtr("sim-shared"), nil, `{"agent":"agent-a","instructions":"S"}`},
		{"bootstrap without startup", nil, strPtr("technical"), strPtr("SOUL"), `{"agent":"agent-a","instructions":"\n\nDREAM\n\nSOUL"}`},
		{"soul without startup", nil, nil, strPtr("SOUL"), `{"agent":"agent-a","instructions":"\n\nSOUL"}`},
		{"empty soul skipped", strPtr("S"), nil, strPtr(""), `{"agent":"agent-a","instructions":"S"}`},
	}
	for _, c := range cases {
		rt, mock, _ := newRoutes(t, values)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT startup_instructions, dream_mode FROM agent_configuration WHERE actor_id = $1")).
			WithArgs(11).WillReturnRows(instructionRows(c.startup, c.mode))
		expectSoul(mock, c.soul)
		rec := httptest.NewRecorder()
		if err := rt.instructionsRead(rec, request(``, "agent-a", 11)); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if rec.Body.String() != c.want {
			t.Errorf("%s: %s", c.name, rec.Body)
		}
		testutil.WaitMet(t, mock)
	}
}

// A missing dream_bootstrap key threw in config.get: the route fails.
func TestInstructionsReadMissingBootstrapKey(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT startup_instructions, dream_mode")).
		WithArgs(11).WillReturnRows(instructionRows(nil, strPtr("companion")))
	err := rt.instructionsRead(httptest.NewRecorder(), request(``, "agent-a", 11))
	if err == nil || err.Error() != `Config key "dream_bootstrap" not found. Was config.init() called?` {
		t.Errorf("%v", err)
	}
}

func TestInstructionsSave(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	text := "héllo 😀"
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_configuration SET startup_instructions = $1 WHERE actor_id = $2")).
		WithArgs(&text, 11).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	rec := httptest.NewRecorder()
	if err := rt.instructionsSave(rec, request(`{"content":"héllo\u200b 😀"}`, "agent-a", 11)); err != nil {
		t.Fatal(err)
	}
	// length is UTF-16 units: the emoji counts 2.
	if rec.Body.String() != `{"agent":"agent-a","message":"Instructions saved","length":8}` {
		t.Errorf("%s", rec.Body)
	}
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_configuration SET startup_instructions = $1")).
		WithArgs((*string)(nil), 11).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	rec = httptest.NewRecorder()
	if err := rt.instructionsSave(rec, request(`{"content":null}`, "agent-a", 11)); err != nil {
		t.Fatal(err)
	}
	if rec.Body.String() != `{"agent":"agent-a","message":"Instructions saved","length":0}` {
		t.Errorf("%s", rec.Body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestSyncMappingsExcludeList(t *testing.T) {
	cases := []struct {
		values map[string]string
		want   string
	}{
		{nil, `[]`},
		{map[string]string{"sync_exclude_slugs": ""}, `[]`},
		{map[string]string{"sync_exclude_slugs": " work-log, ,active-work ,"}, `["work-log","active-work"]`},
	}
	for _, c := range cases {
		rt, mock, _ := newRoutes(t, c.values)
		created := time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC)
		mock.ExpectQuery(regexp.QuoteMeta("FROM note_synchronization ns")).WithArgs("agent-a").
			WillReturnRows(pgxmock.NewRows([]string{"id", "namespace", "slug", "local_path", "created_at"}).
				AddRow(1, "agent-a", "notes/", "C:/x", &created))
		rec := httptest.NewRecorder()
		if err := rt.syncMappings(rec, request(``, "agent-a", 11)); err != nil {
			t.Fatal(err)
		}
		want := `{"mappings":[{"id":1,"namespace":"agent-a","slug":"notes/","local_path":"C:/x","created_at":"2026-01-02T03:04:05.000Z"}],"exclude_slugs":` + c.want + `}`
		if rec.Body.String() != want {
			t.Errorf("%s", rec.Body)
		}
	}
}

func TestAgentConfig(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT key, value FROM config WHERE key = ANY($1)")).
		WithArgs([]string{"discussion_end_timeout_realtime", "discussion_maxrounds"}).
		WillReturnRows(pgxmock.NewRows([]string{"key", "value"}).
			AddRow("discussion_maxrounds", strPtr("20")).
			AddRow("discussion_end_timeout_realtime", (*string)(nil)))
	rec := httptest.NewRecorder()
	if err := rt.agentConfig(rec, request(``, "", 3)); err != nil {
		t.Fatal(err)
	}
	if rec.Body.String() != `{"config":{"discussion_maxrounds":"20","discussion_end_timeout_realtime":null}}` {
		t.Errorf("%s", rec.Body)
	}
}

func TestExpertiseOrEmpty(t *testing.T) {
	cases := map[string]string{
		``:              `[]`,
		`null`:          `[]`,
		`0`:             `[]`,
		`""`:            `[]`,
		`false`:         `[]`,
		`[]`:            `[]`,
		`["a", "b"]`:    `["a","b"]`,
		`{"b":1,"a":2}`: `{"b":1,"a":2}`,
		`"x"`:           `"x"`,
	}
	for in, want := range cases {
		var raw []byte
		if in != "" {
			raw = []byte(in)
		}
		got, err := expertiseOrEmpty(raw)
		if err != nil || string(got) != want {
			t.Errorf("%q: %s %v", in, got, err)
		}
	}
}

func expectRotateWrites(mock pgxmock.PgxPoolIface) {
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE actors SET token_hash = $1, token_salt = $2, passphrase_rotated_at = NOW() WHERE id = $3")).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), 11).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM sessions WHERE actor_id = $1 AND kind = $2")).
		WithArgs(11, "api").
		WillReturnResult(pgxmock.NewResult("DELETE", 2))
	mock.ExpectCommit()
}

// hangUpDB cancels the request (the client hangs up) right after the
// passphrase UPDATE, before the session DELETE.
type hangUpDB struct {
	pgxmock.PgxPoolIface
	cancel context.CancelFunc
}

func (h hangUpDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := h.PgxPoolIface.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return hangUpTx{Tx: tx, cancel: h.cancel}, nil
}

type hangUpTx struct {
	pgx.Tx
	cancel context.CancelFunc
}

func (t hangUpTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tag, err := t.Tx.Exec(ctx, sql, args...)
	if strings.HasPrefix(sql, "UPDATE actors SET token_hash") {
		t.cancel()
	}
	return tag, err
}

// Once the new passphrase is written, a client that hangs up cannot stop
// the old sessions from ending or the cached tokens from being dropped.
func TestRotateSurvivesHangUp(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	mock.MatchExpectationsInOrder(true)
	salt := "00ff"
	hash, _ := auth.Hash("current", salt)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT token_hash, token_salt FROM actors WHERE id = $1")).
		WithArgs(11).
		WillReturnRows(pgxmock.NewRows([]string{"token_hash", "token_salt"}).AddRow(&hash, &salt))
	expectRotateWrites(mock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt.DB = hangUpDB{PgxPoolIface: mock, cancel: cancel}
	r := httptest.NewRequest("POST", "/v1/agent/rotate", nil).WithContext(ctx)
	r = httpapi.WithStateValue(r, &httpapi.State{AuthenticatedAgent: "agent-a", ActorID: 11, Body: json.RawMessage(`{"current_passphrase":"current"}`)})
	if err := rt.rotate(httptest.NewRecorder(), r); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("the request was not cancelled")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
	if c := rt.SessionAuth.(*fakeCache); len(c.agents) == 0 {
		t.Error("agent tokens not forgotten")
	}
}

// A failed session DELETE still drops the cached tokens.
func TestRotateForgetsOnFailure(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	salt := "00ff"
	hash, _ := auth.Hash("current", salt)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT token_hash, token_salt FROM actors WHERE id = $1")).
		WithArgs(11).
		WillReturnRows(pgxmock.NewRows([]string{"token_hash", "token_salt"}).AddRow(&hash, &salt))
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE actors SET token_hash")).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), 11).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM sessions")).
		WithArgs(11, "api").
		WillReturnError(errors.New("connection lost"))
	mock.ExpectRollback()
	err := rt.rotate(httptest.NewRecorder(), request(`{"current_passphrase":"current"}`, "agent-a", 11))
	if err == nil || err.Error() != "connection lost" {
		t.Fatalf("%v", err)
	}
	if c := rt.SessionAuth.(*fakeCache); len(c.agents) == 0 {
		t.Error("agent tokens not forgotten after a failure")
	}
}
