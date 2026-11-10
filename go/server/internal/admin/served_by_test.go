package admin

import (
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/pashagolub/pgxmock/v6"
)

// The usage list and call detail return served_by, the OpenRouter upstream
// that served the call (LLM-935): the host when one was recorded, null for
// direct providers, failed calls and older rows. A usage row with no call
// row keeps both call_id and served_by null.
func TestUsageAndCallDetailReturnServedBy(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, name FROM actors WHERE name")).WithArgs("agent-a").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(9, "agent-a"))
	grants(mock, 2, "*", "*")
	// The mock has no column types, so pgjson writes every value as text;
	// served_by is what this test reads.
	cols := []string{"id", "provider", "model", "call_id", "served_by"}
	mock.ExpectQuery(`(?s)c\.id AS call_id, c\.served_by\s+FROM virtual_agent_usage u\s+LEFT JOIN LATERAL \(SELECT id, served_by FROM virtual_agent_calls WHERE usage_id = u\.id LIMIT 1\) c ON true`).
		WithArgs(9, int64(50)).
		WillReturnRows(pgxmock.NewRows(cols).
			AddRow("3", "openrouter", "m", "30", "DeepInfra").
			AddRow("2", "anthropic", "claude", "20", nil).
			AddRow("1", "openrouter", "m", nil, nil))
	rec := httptest.NewRecorder()
	if err := rt.agentsUsage(rec, request(`{"agent":"agent-a"}`)); err != nil {
		t.Fatal(err)
	}
	check(t, "usage", rec, 200, `{"usage":[`+
		`{"id":"3","provider":"openrouter","model":"m","call_id":"30","served_by":"DeepInfra"},`+
		`{"id":"2","provider":"anthropic","model":"claude","call_id":"20","served_by":null},`+
		`{"id":"1","provider":"openrouter","model":"m","call_id":null,"served_by":null}]}`)

	detailCols := []string{"id", "provider", "model", "served_by"}
	for _, c := range []struct {
		name      string
		servedBy  any
		wantField string
	}{
		{"with host", "DeepInfra", `"DeepInfra"`},
		{"null", nil, `null`},
	} {
		// The caller's visibility is cached from the usage call above.
		mock.ExpectQuery(regexp.QuoteMeta("c.provider, c.model, c.served_by,")).WithArgs(int64(30)).
			WillReturnRows(pgxmock.NewRows(detailCols).AddRow("30", "openrouter", "m", c.servedBy))
		rec := httptest.NewRecorder()
		if err := rt.agentsCallDetail(rec, request(`{"call_id":30}`)); err != nil {
			t.Fatal(err)
		}
		check(t, "call detail "+c.name, rec, 200, `{"call":{"id":"30","provider":"openrouter","model":"m","served_by":`+c.wantField+`}}`)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
