package admin

import (
	"context"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/dream"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

func newDreamRoutes(t *testing.T) (*DreamRoutes, pgxmock.PgxPoolIface) {
	mock := testutil.NewMock(t)
	cache := actors.NewCache(mock)
	log := logger.New(mock, cache, "")
	adm := &permissions.Admin{DB: mock}
	return &DreamRoutes{
		Common: &Common{DB: mock, Log: log, Router: &httpapi.Router{Log: log}, Admin: adm, Actors: cache},
		Dream:  &dream.Job{DB: mock, Log: log, Trust: adm},
		Notes:  &notes.Service{DB: mock},
		Now:    func() time.Time { return testNow },
	}, mock
}

const dreamAgentsByName = `SELECT a.name, agc.dream_mode FROM actors a
              JOIN agent_configuration agc ON agc.actor_id = a.id
              WHERE a.name = $1 AND agc.dream_mode IN ('sim', 'companion')`

// The refusals before any work: superadmin only, then the body, the two
// filters' types, the person slug's canonical form and the named agent.
func TestConsolidatePeopleRefusals(t *testing.T) {
	cases := []struct {
		name, body string
		grants     []string
		agentQuery bool
		status     int
		want       string
		err        string
	}{
		{"not superadmin", `{}`, []string{"agents", "write"}, false, 403, errorBody("FORBIDDEN", "Dream consolidation requires superadmin"), ""},
		{"no body", ``, []string{"*", "*"}, false, 0, "", "Cannot read properties of undefined (reading 'agent')"},
		{"agent number", `{"agent":5}`, []string{"*", "*"}, false, 400, errorBody("BAD_REQUEST", "agent must be a string"), ""},
		{"agent false", `{"agent":false}`, []string{"*", "*"}, false, 400, errorBody("BAD_REQUEST", "agent must be a string"), ""},
		{"person number", `{"agent":"a","person_slug":0}`, []string{"*", "*"}, false, 400, errorBody("BAD_REQUEST", "person_slug must be a string"), ""},
		{"person not canonical", `{"person_slug":"Jane Doe"}`, []string{"*", "*"}, false, 400,
			errorBody("BAD_REQUEST", "invalid person_slug — must match [a-z0-9-]+ with no leading/trailing hyphen"), ""},
		{"person traversal", `{"person_slug":"../x"}`, []string{"*", "*"}, false, 400,
			errorBody("BAD_REQUEST", "invalid person_slug — must match [a-z0-9-]+ with no leading/trailing hyphen"), ""},
		{"agent not found", `{"agent":"nobody","person_slug":null}`, []string{"*", "*"}, true, 404,
			errorBody("NOT_FOUND", `agent "nobody" not found or not in sim/companion mode`), ""},
	}
	for _, c := range cases {
		rt, mock := newDreamRoutes(t)
		grants(mock, 2, c.grants...)
		if c.agentQuery {
			mock.ExpectQuery(regexp.QuoteMeta(dreamAgentsByName)).WithArgs("nobody").
				WillReturnRows(pgxmock.NewRows([]string{"name", "dream_mode"}))
		}
		rec := httptest.NewRecorder()
		err := rt.consolidatePeople(rec, request(c.body))
		if c.err != "" {
			if err == nil || err.Error() != c.err {
				t.Errorf("%s: error %v, want %q", c.name, err, c.err)
			}
		} else {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			check(t, c.name, rec, c.status, c.want)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// An agent whose people writer is missing is a result entry, and the
// route goes on; "" is no agent filter.
func TestConsolidatePeopleNoPeopleAgent(t *testing.T) {
	rt, mock := newDreamRoutes(t)
	grants(mock, 2, "*", "*")
	mock.ExpectQuery(regexp.QuoteMeta(`WHERE agc.dream_mode IN ('sim', 'companion')
              ORDER BY a.name`)).
		WillReturnRows(pgxmock.NewRows([]string{"name", "dream_mode"}).AddRow("a", "companion").AddRow("b", "sim"))
	for range 2 {
		mock.ExpectQuery(regexp.QuoteMeta("WHERE ac.expertise @> jsonb_build_array($1::text)")).WithArgs(pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows([]string{"id", "name", "created_by", "provider", "model", "api_key"}))
	}
	rec := httptest.NewRecorder()
	if err := rt.consolidatePeople(rec, request(`{"agent":"","dry_run":1}`)); err != nil {
		t.Fatal(err)
	}
	check(t, "no people agent", rec, 200,
		`{"dry_run":true,"count":2,"results":[{"agent":"a","error":"dream-companion-people not available"},{"agent":"b","error":"dream-sim-people not available"}]}`)
}

// The listing pages 1000 notes at a time until a short page.
func TestAllPeopleNotesPages(t *testing.T) {
	rt, mock := newDreamRoutes(t)
	cols := []string{"id", "slug", "title", "snippet", "content_hash", "created_by", "created_at", "updated_at", "updated_at_exact", "last_accessed"}
	page := func(n, start int) *pgxmock.Rows {
		rows := pgxmock.NewRows(cols)
		for i := range n {
			rows.AddRow(start+i, "context/people/p"+strconv.Itoa(start+i), nil, "", "", nil, nil, nil, nil, nil)
		}
		return rows
	}
	listing := regexp.QuoteMeta("LOWER(d.slug) LIKE LOWER($4)")
	mock.ExpectQuery(listing).WithArgs("ns", jsutil.Number("1000"), 0, "context/people/%").WillReturnRows(page(1000, 0))
	mock.ExpectQuery(listing).WithArgs("ns", jsutil.Number("1000"), jsutil.Number("1000"), "context/people/%").WillReturnRows(page(1, 1000))
	got, err := rt.allPeopleNotes(context.Background(), "ns")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1001 || got[1000].Slug != "context/people/p1000" {
		t.Errorf("got %d notes", len(got))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestPersonDisplay(t *testing.T) {
	s := func(v string) *string { return &v }
	cases := []struct {
		title *string
		want  string
	}{
		{nil, "jane"},
		{s(""), "jane"},
		{s("People — "), "jane"},
		{s("People — Jane Doe"), "Jane Doe"},
		{s("  Jane\r\n## Injected\t x "), "Jane ## Injected x"},
		{s("People — People — Jane"), "People — Jane"},
		{s("Jane  Doe"), "Jane Doe"},
	}
	for _, c := range cases {
		if got := personDisplay(c.title, "jane"); got != c.want {
			t.Errorf("personDisplay(%v) = %q, want %q", c.title, got, c.want)
		}
	}
}
