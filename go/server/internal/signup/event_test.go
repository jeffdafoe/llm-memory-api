package signup

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

func TestRecordEventValidation(t *testing.T) {
	cases := []struct{ event, value, want string }{
		{``, `"x"`, "event must be one of: guide_shown, client, copy"},
		{`"other"`, `"x"`, "event must be one of: guide_shown, client, copy"},
		{`["copy"]`, `"x"`, "event must be one of: guide_shown, client, copy"},
		{`"copy"`, ``, "value must be 1-32 lowercase letters, digits, hyphens or underscores, starting with a letter"},
		{`"copy"`, `5`, "value must be 1-32 lowercase letters, digits, hyphens or underscores, starting with a letter"},
		{`"copy"`, `"1abc"`, "value must be 1-32 lowercase letters, digits, hyphens or underscores, starting with a letter"},
		{`"copy"`, `"` + "a234567890123456789012345678901233" + `"`, "value must be 1-32 lowercase letters, digits, hyphens or underscores, starting with a letter"},
	}
	for _, c := range cases {
		var ev, val json.RawMessage
		if c.event != "" {
			ev = json.RawMessage(c.event)
		}
		if c.value != "" {
			val = json.RawMessage(c.value)
		}
		_, err := RecordEvent(context.Background(), nil, 1, ev, val)
		status, _ := apierr.StatusAndCode(err)
		if err == nil || status != 400 || err.Error() != c.want {
			t.Errorf("%s %s: %v", c.event, c.value, err)
		}
	}
}

func TestRecordEvent(t *testing.T) {
	for _, inserted := range []bool{true, false} {
		mock := testutil.NewMock(t)
		mock.MatchExpectationsInOrder(true)
		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock($1, $2)")).WithArgs(734001, 7).
			WillReturnResult(pgxmock.NewResult("SELECT", 1))
		rows := pgxmock.NewRows([]string{"id"})
		if inserted {
			rows.AddRow(1)
		}
		mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO signup_events (actor_id, event, value)")).
			WithArgs(7, "client", "claude-code", 24, 100).WillReturnRows(rows)
		mock.ExpectCommit()
		mock.ExpectRollback()
		got, err := RecordEvent(context.Background(), mock, 7, json.RawMessage(`"client"`), json.RawMessage(`"claude-code"`))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(got)
		want := `{"recorded":true}`
		if !inserted {
			want = `{"recorded":false,"reason":"outside the signup window or over the event cap"}`
		}
		if string(b) != want {
			t.Errorf("%s", b)
		}
	}
}
