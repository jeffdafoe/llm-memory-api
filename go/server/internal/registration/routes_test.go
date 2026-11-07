package registration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

// Ported from node/api/src/routes/registration.test.js (invite-code handling,
// LLM-735), plus the parts of the route that test did not cover.

var now = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

type approve struct{}

func (approve) ModerateActorName(ctx context.Context, name string) (bool, string) { return true, "" }

type inviteRow struct {
	id              int
	usedBy          *string
	expiresAt       time.Time
	realm           string
	accessRequestID int
}

func str(s string) *string { return &s }

var invites = map[string]inviteRow{
	"good":  {1, nil, now.Add(24 * time.Hour), "invite-realm", 7},
	"used":  {2, str("someone"), now.Add(24 * time.Hour), "invite-realm", 8},
	"stale": {3, nil, now.Add(-24 * time.Hour), "invite-realm", 9},
	"raced": {4, nil, now.Add(24 * time.Hour), "invite-realm", 10},
}

type rig struct {
	t    *testing.T
	mock pgxmock.PgxPoolIface
	rt   *Routes
	h    http.Handler
}

func newRig(t *testing.T, open bool) *rig {
	mock := testutil.NewMock(t)
	store := config.NewStore()
	if open {
		store.Set("open_registration", "true")
	} else {
		store.Set("open_registration", "")
	}
	store.Set("minimum_password_length", "10")
	store.Set("realm_host_map", "{}")
	cache := actors.NewCache(mock)
	rt := &Routes{DB: mock, Config: store, Actors: cache, Log: logger.New(mock, cache, ""), Moderator: approve{}, Now: func() time.Time { return now }}
	mux := httpapi.NewRawMux("", http.HandlerFunc(httpapi.NotFound))
	rt.Register(mux)
	return &rig{t: t, mock: mock, rt: rt, h: httpapi.WithState(httpapi.ParseJSONBody(mux))}
}

func inviteRows(inv inviteRow, withRequest bool) *pgxmock.Rows {
	rows := pgxmock.NewRows([]string{"id", "used_by", "expires_at", "realm", "access_request_id"})
	var req *int
	if withRequest {
		req = &inv.accessRequestID
	}
	return rows.AddRow(inv.id, inv.usedBy, &inv.expiresAt, &inv.realm, req)
}

// expectPhase1 is the invite read, the name checks (moderation is stubbed).
func (rg *rig) expectPhase1(code string) {
	if code != "" {
		q := rg.mock.ExpectQuery("FROM invite_codes WHERE code = \\$1").WithArgs(code)
		if inv, ok := invites[code]; ok {
			q.WillReturnRows(inviteRows(inv, false))
		} else {
			q.WillReturnRows(pgxmock.NewRows([]string{"id", "used_by", "expires_at", "realm", "access_request_id"}))
		}
	}
	rg.mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs("newcomer").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}))
	rg.mock.ExpectQuery("SELECT 1 FROM documents WHERE namespace").WithArgs("newcomer").
		WillReturnRows(pgxmock.NewRows([]string{"?column?"}))
}

// captured records the arguments of the statements a test inspects.
type captured struct {
	actorArgs  []any
	inviteUsed *int
}

type capture struct {
	into *any
}

func (c capture) Match(v any) bool {
	*c.into = v
	return true
}

// expectPhase3 sets up the transaction. lockedID is the invite locked under
// FOR UPDATE (0 for none); lockedUsed makes it lost to a concurrent signup.
func (rg *rig) expectPhase3(lockedID int, lockedUsed, wantEmail bool, realm *any, email *any, rollback bool) {
	rg.mock.ExpectBegin()
	if lockedID != 0 {
		var inv inviteRow
		for _, v := range invites {
			if v.id == lockedID {
				inv = v
			}
		}
		if lockedUsed {
			inv.usedBy = str("someone-else")
		}
		rg.mock.ExpectQuery("FROM invite_codes WHERE id = \\$1 FOR UPDATE").WithArgs(lockedID).WillReturnRows(inviteRows(inv, true))
		if wantEmail {
			rg.mock.ExpectQuery("SELECT email FROM access_requests").WithArgs(inv.accessRequestID).
				WillReturnRows(pgxmock.NewRows([]string{"email"}).AddRow(str("request-" + string(rune('0'+inv.accessRequestID)) + "@example.com")))
		}
	}
	if rollback {
		rg.mock.ExpectRollback()
		return
	}
	rg.mock.ExpectExec("INSERT INTO actors").WithArgs("newcomer", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), capture{realm}, capture{email}).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	rg.mock.ExpectExec("UPDATE actors SET created_by = id").WithArgs("newcomer").WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	rg.mock.ExpectExec("INSERT INTO agent_configuration").WithArgs("newcomer", "none").WillReturnResult(pgxmock.NewResult("INSERT", 1))
	if lockedID != 0 && !lockedUsed {
		rg.mock.ExpectExec("UPDATE invite_codes SET used_by").WithArgs("newcomer", lockedID).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	}
	rg.mock.ExpectQuery("SELECT id FROM actors WHERE name").WithArgs("newcomer").WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(99))
	rg.mock.ExpectExec("INSERT INTO agent_permissions").WithArgs(99).WillReturnResult(pgxmock.NewResult("INSERT", 30))
	rg.mock.ExpectExec("INSERT INTO admin_permissions").WithArgs(99).WillReturnResult(pgxmock.NewResult("INSERT", 6))
	rg.mock.ExpectExec("INSERT INTO agent_api_keys").WithArgs(99, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	rg.mock.ExpectCommit()
	rg.mock.ExpectRollback()
	// No welcome templates.
	for _, kind := range []string{"welcome", "welcome-note"} {
		rg.mock.ExpectQuery("SELECT content FROM templates WHERE kind").WithArgs(kind).WillReturnRows(pgxmock.NewRows([]string{"content"}))
	}
}

func (rg *rig) post(path, body string) (int, map[string]any) {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	rg.h.ServeHTTP(rec, req)
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

func registerBody(code string) string {
	b := `{"name":"newcomer","password":"long-enough-password","dream_mode":"none"`
	if code != "" {
		b += `,"code":"` + code + `"`
	}
	return b + "}"
}

func realmOf(v any) string {
	list, _ := v.([]string)
	if len(list) != 1 {
		return "?"
	}
	return list[0]
}

func emailOf(v any) string {
	p, _ := v.(*string)
	if p == nil {
		return ""
	}
	return *p
}

func TestOpenRegistrationWithGoodCodeLinksTheRequest(t *testing.T) {
	rg := newRig(t, true)
	var realm, email any
	rg.expectPhase1("good")
	rg.expectPhase3(1, false, true, &realm, &email, false)
	status, body := rg.post("/api/register", registerBody("good"))
	if status != 200 || body["agent"] != "newcomer" {
		t.Fatalf("%d %v", status, body)
	}
	if emailOf(email) != "request-7@example.com" || realmOf(realm) != "invite-realm" {
		t.Fatalf("email %v realm %v", email, realm)
	}
	pass, _ := body["passphrase"].(string)
	if len(strings.Split(pass, "-")) < 3 || len(body["api_key"].(string)) != 64 {
		t.Fatalf("credentials %v", body)
	}
	testutil.WaitMet(t, rg.mock)
}

func TestOpenRegistrationDropsBadCodes(t *testing.T) {
	for _, code := range []string{"used", "stale", "no-such-code"} {
		rg := newRig(t, true)
		var realm, email any
		rg.expectPhase1(code)
		rg.expectPhase3(0, false, false, &realm, &email, false)
		status, body := rg.post("/api/register", registerBody(code))
		if status != 200 || emailOf(email) != "" || realmOf(realm) != "llm-memory" {
			t.Fatalf("%s: %d %v %v %v", code, status, body, email, realm)
		}
		testutil.WaitMet(t, rg.mock)
	}
}

func TestOpenRegistrationLosingTheCodeRegistersCodeLess(t *testing.T) {
	rg := newRig(t, true)
	var realm, email any
	rg.expectPhase1("raced")
	rg.expectPhase3(4, true, false, &realm, &email, false)
	status, _ := rg.post("/api/register", registerBody("raced"))
	if status != 200 || emailOf(email) != "" || realmOf(realm) != "llm-memory" {
		t.Fatalf("%d %v %v", status, email, realm)
	}
	testutil.WaitMet(t, rg.mock)
}

func TestOpenRegistrationWithoutCode(t *testing.T) {
	rg := newRig(t, true)
	var realm, email any
	rg.expectPhase1("")
	rg.expectPhase3(0, false, false, &realm, &email, false)
	if status, _ := rg.post("/api/register", registerBody("")); status != 200 || emailOf(email) != "" {
		t.Fatalf("%d %v", status, email)
	}
	testutil.WaitMet(t, rg.mock)
}

func TestInviteOnlyRefusesBadCodes(t *testing.T) {
	for code, want := range map[string]string{
		"used":         "This invite code has already been used",
		"stale":        "This invite code has expired",
		"no-such-code": "Invalid invite code",
	} {
		rg := newRig(t, false)
		rg.mock.ExpectQuery("FROM invite_codes WHERE code = \\$1").WithArgs(code).WillReturnRows(func() *pgxmock.Rows {
			if inv, ok := invites[code]; ok {
				return inviteRows(inv, false)
			}
			return pgxmock.NewRows([]string{"id", "used_by", "expires_at", "realm", "access_request_id"})
		}())
		status, body := rg.post("/api/register", registerBody(code))
		if status != 400 || body["error"] != want {
			t.Fatalf("%s: %d %v", code, status, body)
		}
		if err := rg.mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInviteOnlyRefusesCodeLostUnderLock(t *testing.T) {
	rg := newRig(t, false)
	rg.expectPhase1("raced")
	rg.expectPhase3(4, true, false, nil, nil, true)
	status, body := rg.post("/api/register", registerBody("raced"))
	if status != 400 || body["error"] != "This invite code has already been used" {
		t.Fatalf("%d %v", status, body)
	}
	if err := rg.mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterValidation(t *testing.T) {
	cases := []struct {
		open   bool
		body   string
		status int
		want   string
	}{
		{true, `{}`, 400, "Agent name is required"},
		{false, `{"name":"newcomer","password":"long-enough-password"}`, 400, "Invite code is required (open registration is disabled)"},
		{true, `{"name":"newcomer","password":"short"}`, 400, "Password must be at least 10 characters"},
		{true, `{"name":"newcomer","password":["a","b"]}`, 400, "Password must be at least 10 characters"},
		{true, `{"name":"newcomer","password":{"length":3}}`, 400, "Password must be at least 10 characters"},
		{true, `{"name":"1bad","password":"long-enough-password"}`, 400, nameRule},
		{true, `{"name":5,"password":"long-enough-password"}`, 500, "name.trim is not a function"},
	}
	for _, c := range cases {
		rg := newRig(t, c.open)
		status, body := rg.post("/api/register", c.body)
		msg := body["error"]
		if m, ok := msg.(map[string]any); ok {
			msg = m["message"]
		}
		if status != c.status || msg != c.want {
			t.Errorf("%s: %d %v", c.body, status, body)
		}
	}
}

func TestRegisterWithoutJSONBody(t *testing.T) {
	rg := newRig(t, true)
	req := httptest.NewRequest(http.MethodPost, "/api/register", strings.NewReader("name=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	rg.h.ServeHTTP(rec, req)
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "Cannot destructure property 'code' of 'req.body' as it is undefined.") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestCheckName(t *testing.T) {
	rg := newRig(t, true)
	if status, body := rg.post("/api/check-name", `{"name":"  "}`); status != 400 || body["error"] != "Name is required" {
		t.Fatalf("%d %v", status, body)
	}
	if status, body := rg.post("/api/check-name", `{"name":"x"}`); status != 200 || body["available"] != false || body["reason"] != nameRule {
		t.Fatalf("%d %v", status, body)
	}
	rg.mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs("taken").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(3, "taken"))
	if _, body := rg.post("/api/check-name", `{"name":" Taken "}`); body["available"] != false || body["reason"] != "Name already taken." {
		t.Fatalf("%v", body)
	}
	rg.mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs("shared").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}))
	rg.mock.ExpectQuery("SELECT 1 FROM documents").WithArgs("shared").WillReturnRows(pgxmock.NewRows([]string{"?column?"}).AddRow(1))
	if _, body := rg.post("/api/check-name", `{"name":"shared"}`); body["reason"] != "This name conflicts with an existing namespace." {
		t.Fatalf("%v", body)
	}
	rg.mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs("fresh").WillReturnRows(pgxmock.NewRows([]string{"id", "name"}))
	rg.mock.ExpectQuery("SELECT 1 FROM documents").WithArgs("fresh").WillReturnRows(pgxmock.NewRows([]string{"?column?"}))
	status, body := rg.post("/api/check-name", `{"name":"fresh"}`)
	if status != 200 || !reflect.DeepEqual(body, map[string]any{"available": true}) {
		t.Fatalf("%d %v", status, body)
	}
}
