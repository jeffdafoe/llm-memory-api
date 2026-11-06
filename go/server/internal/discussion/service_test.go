package discussion

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/system"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

var fixedNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

func newService(t *testing.T) (*Service, pgxmock.PgxPoolIface) {
	t.Helper()
	mock := testutil.NewMock(t)
	cache := actors.NewCache(mock)
	log := logger.New(mock, cache, "")
	handler := &system.Handler{Log: log}
	store := config.NewStore()
	store.Set("discussion_stale_vote_timeout", "30")
	return &Service{
		DB: mock, Config: store, Actors: cache, Log: log,
		Notifier: &system.Notifier{DB: mock, Actors: cache, Handler: handler},
		Now:      func() time.Time { return fixedNow },
	}, mock
}

func q(sql string) string { return regexp.QuoteMeta(sql) }

// num is a number id as pgbind binds it.
func num(s string) jsutil.Number { return jsutil.Number(s) }

const (
	readinessHead     = "SELECT status, timeout_at FROM discussions WHERE id = $1"
	readinessMembers  = "SELECT ac.name AS agent, dp.status, dp.role"
	voteHead          = "SELECT v.status, v.discussion_id, v.closes_at, v.type, v.threshold FROM discussion_votes v JOIN discussions d"
	joinedMembers     = "SELECT actor_id FROM discussion_participants WHERE discussion_id = $1 AND status = $2"
	choiceCounts      = "SELECT choice, COUNT(*) as count FROM discussion_ballots WHERE vote_id = $1 GROUP BY choice"
	closedGeneral     = "SELECT v.id, v.threshold FROM discussion_votes v WHERE v.discussion_id = $1 AND v.type = 'general' AND v.status = 'closed'"
	leaveAllSQL       = "UPDATE discussion_participants SET status = 'left'"
	actorByName       = "SELECT id, name FROM actors WHERE name = $1"
	insertTextSQL     = "INSERT INTO chat_message_texts"
	insertDeliverySQL = "INSERT INTO chat_messages"
)

func counts(pairs ...int64) *pgxmock.Rows {
	rows := pgxmock.NewRows([]string{"choice", "count"})
	for i := 0; i < len(pairs); i += 2 {
		rows.AddRow(int(pairs[i]), pairs[i+1])
	}
	return rows
}

func TestEvaluateThreshold(t *testing.T) {
	cases := []struct {
		counts    []choiceCount
		n         int
		threshold string
		want      bool
	}{
		{[]choiceCount{{1, 2}}, 2, "unanimous", true},
		{[]choiceCount{{2, 1}, {1, 1}}, 2, "unanimous", false},
		{nil, 0, "unanimous", true},
		{[]choiceCount{{1, 2}, {2, 1}}, 3, "majority", true},
		{[]choiceCount{{1, 1}, {2, 1}}, 2, "majority", false},
		{[]choiceCount{{1, 5}}, 5, "plurality", false},
	}
	for _, c := range cases {
		if got := evaluateThreshold(c.counts, c.n, c.threshold); got != c.want {
			t.Errorf("%v of %d %s: %v", c.counts, c.n, c.threshold, got)
		}
	}
}

func TestComputeOutcome(t *testing.T) {
	ctx := context.Background()
	for _, status := range []string{"cancelled", "timed_out"} {
		s, _ := newService(t)
		if got, err := s.computeOutcome(ctx, 1, status); got != "abandoned" || err != nil {
			t.Errorf("%s: %q %v", status, got, err)
		}
	}
	cases := []struct {
		name  string
		votes [][2]any
		tally map[int]*pgxmock.Rows
		want  string
	}{
		{"no votes", nil, nil, "consensus"},
		{"all passed", [][2]any{{1, "unanimous"}}, map[int]*pgxmock.Rows{1: counts(1, 2)}, "consensus"},
		{"none passed", [][2]any{{1, "unanimous"}, {2, "majority"}}, map[int]*pgxmock.Rows{1: counts(1, 1, 2, 1), 2: counts(2, 2)}, "deadlock"},
		{"some passed", [][2]any{{1, "majority"}, {2, "unanimous"}}, map[int]*pgxmock.Rows{1: counts(1, 2, 2, 1), 2: counts(1, 1, 2, 1)}, "partial"},
	}
	for _, c := range cases {
		s, mock := newService(t)
		rows := pgxmock.NewRows([]string{"id", "threshold"})
		for _, v := range c.votes {
			rows.AddRow(v[0], v[1])
		}
		mock.ExpectQuery(q(closedGeneral)).WithArgs(7).WillReturnRows(rows)
		for id, tally := range c.tally {
			mock.ExpectQuery(q(choiceCounts)).WithArgs(id).WillReturnRows(tally)
		}
		got, err := s.computeOutcome(ctx, 7, "concluded")
		if got != c.want || err != nil {
			t.Errorf("%s: %q %v", c.name, got, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func members(rows ...[3]string) *pgxmock.Rows {
	r := pgxmock.NewRows([]string{"agent", "status", "role"})
	for _, row := range rows {
		r.AddRow(row[0], row[1], row[2])
	}
	return r
}

// expectNotify is the virtual-agent notification: a system-to-system text
// with the payload, then its delivery. The handler finds no type handler
// and only logs.
func expectNotify(mock pgxmock.PgxPoolIface, payload string) {
	mock.ExpectQuery(q(actorByName)).WithArgs("system").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(1, "system"))
	mock.ExpectQuery(q(insertTextSQL)).WithArgs(payload, 1, nil).
		WillReturnRows(pgxmock.NewRows([]string{"id", "sent_at"}).AddRow(50, fixedNow))
	mock.ExpectQuery(q(insertDeliverySQL)).WithArgs(50, 1).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(60))
}

func TestEvaluateReadiness(t *testing.T) {
	ctx := context.Background()
	past := fixedNow.Add(-time.Minute)
	future := fixedNow.Add(time.Minute)

	t.Run("not found or not waiting", func(t *testing.T) {
		s, mock := newService(t)
		mock.ExpectQuery(q(readinessHead)).WithArgs("9").WillReturnRows(pgxmock.NewRows([]string{"status", "timeout_at"}))
		mock.ExpectQuery(q(readinessHead)).WithArgs("8").WillReturnRows(pgxmock.NewRows([]string{"status", "timeout_at"}).AddRow("active", &past))
		if err := s.EvaluateReadiness(ctx, RawID(json.RawMessage(`"9"`))); err != nil {
			t.Fatal(err)
		}
		if err := s.EvaluateReadiness(ctx, RawID(json.RawMessage(`"8"`))); err != nil {
			t.Fatal(err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("all joined", func(t *testing.T) {
		s, mock := newService(t)
		mock.ExpectQuery(q(readinessHead)).WithArgs("7").WillReturnRows(pgxmock.NewRows([]string{"status", "timeout_at"}).AddRow("waiting", &future))
		mock.ExpectQuery(q(readinessMembers)).WithArgs("7").WillReturnRows(members(
			[3]string{"agent-a", "joined", "required"}, [3]string{"agent-b", "joined", "optional"}))
		mock.ExpectExec(q("UPDATE discussions SET status = $1 WHERE id = $2")).WithArgs("active", "7").WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		// The id goes into the payload as the caller sent it: a string here.
		expectNotify(mock, `{"type":"virtual-agent","discussionId":"7","triggerType":"discussion-active"}`)
		if err := s.EvaluateReadiness(ctx, RawID(json.RawMessage(`"7"`))); err != nil {
			t.Fatal(err)
		}
		testutil.WaitMet(t, mock)
	})

	t.Run("before the timeout", func(t *testing.T) {
		s, mock := newService(t)
		mock.ExpectQuery(q(readinessHead)).WithArgs(num("7")).WillReturnRows(pgxmock.NewRows([]string{"status", "timeout_at"}).AddRow("waiting", &future))
		mock.ExpectQuery(q(readinessMembers)).WithArgs(num("7")).WillReturnRows(members([3]string{"agent-b", "invited", "required"}))
		if err := s.EvaluateReadiness(ctx, IntID(7)); err != nil {
			t.Fatal(err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("timeout with the optional members out", func(t *testing.T) {
		s, mock := newService(t)
		mock.ExpectQuery(q(readinessHead)).WithArgs(num("7")).WillReturnRows(pgxmock.NewRows([]string{"status", "timeout_at"}).AddRow("waiting", &past))
		mock.ExpectQuery(q(readinessMembers)).WithArgs(num("7")).WillReturnRows(members(
			[3]string{"agent-a", "joined", "required"}, [3]string{"helper", "invited", "optional"}, [3]string{"agent-c", "joined", "optional"}))
		mock.ExpectQuery(q(actorByName)).WithArgs("helper").WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(4, "helper"))
		mock.ExpectExec(q("UPDATE discussion_participants SET status = $1 WHERE discussion_id = $2 AND actor_id = $3")).
			WithArgs("timed_out", num("7"), 4).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		mock.ExpectExec(q("UPDATE discussions SET status = $1 WHERE id = $2")).WithArgs("active", num("7")).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		expectNotify(mock, `{"type":"virtual-agent","discussionId":7,"triggerType":"discussion-active"}`)
		if err := s.EvaluateReadiness(ctx, IntID(7)); err != nil {
			t.Fatal(err)
		}
		testutil.WaitMet(t, mock)
	})

	t.Run("timeout with a required member out", func(t *testing.T) {
		s, mock := newService(t)
		mock.ExpectQuery(q(readinessHead)).WithArgs(num("7")).WillReturnRows(pgxmock.NewRows([]string{"status", "timeout_at"}).AddRow("waiting", &fixedNow))
		mock.ExpectQuery(q(readinessMembers)).WithArgs(num("7")).WillReturnRows(members(
			[3]string{"agent-a", "joined", "required"}, [3]string{"agent-b", "invited", "required"}))
		mock.ExpectExec(q("UPDATE discussions SET status = $1, outcome = $2 WHERE id = $3")).
			WithArgs("timed_out", "abandoned", num("7")).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		mock.ExpectExec(q(leaveAllSQL)).WithArgs(num("7")).WillReturnResult(pgxmock.NewResult("UPDATE", 2))
		if err := s.EvaluateReadiness(ctx, IntID(7)); err != nil {
			t.Fatal(err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}

func voteRow(status string, discussionID int, closesAt *time.Time, typ, threshold string) *pgxmock.Rows {
	return pgxmock.NewRows([]string{"status", "discussion_id", "closes_at", "type", "threshold"}).
		AddRow(status, discussionID, closesAt, typ, threshold)
}

func joined(n int) *pgxmock.Rows {
	rows := pgxmock.NewRows([]string{"actor_id"})
	for i := 0; i < n; i++ {
		rows.AddRow(i + 1)
	}
	return rows
}

func TestEvaluateVote(t *testing.T) {
	ctx := context.Background()
	past := fixedNow.Add(-time.Second)
	closeSQL := "UPDATE discussion_votes SET status = $1, closed_at = NOW() WHERE id = $2"

	t.Run("not open", func(t *testing.T) {
		s, mock := newService(t)
		mock.ExpectQuery(q(voteHead)).WithArgs(num("3")).WillReturnRows(voteRow("closed", 7, nil, "general", "unanimous"))
		mock.ExpectQuery(q(voteHead)).WithArgs(num("4")).WillReturnRows(pgxmock.NewRows([]string{"status"}))
		if err := s.EvaluateVote(ctx, IntID(3)); err != nil {
			t.Fatal(err)
		}
		if err := s.EvaluateVote(ctx, IntID(4)); err != nil {
			t.Fatal(err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("still open", func(t *testing.T) {
		s, mock := newService(t)
		future := fixedNow.Add(time.Second)
		mock.ExpectQuery(q(voteHead)).WithArgs(num("3")).WillReturnRows(voteRow("open", 7, &future, "general", "unanimous"))
		mock.ExpectQuery(q(joinedMembers)).WithArgs(7, "joined").WillReturnRows(joined(3))
		mock.ExpectQuery(q(choiceCounts)).WithArgs(num("3")).WillReturnRows(counts(1, 2))
		if err := s.EvaluateVote(ctx, IntID(3)); err != nil {
			t.Fatal(err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("expired general vote closes", func(t *testing.T) {
		s, mock := newService(t)
		mock.ExpectQuery(q(voteHead)).WithArgs(num("3")).WillReturnRows(voteRow("open", 7, &past, "general", "unanimous"))
		mock.ExpectQuery(q(joinedMembers)).WithArgs(7, "joined").WillReturnRows(joined(3))
		mock.ExpectQuery(q(choiceCounts)).WithArgs(num("3")).WillReturnRows(counts())
		mock.ExpectExec(q(closeSQL)).WithArgs("closed", num("3")).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		if err := s.EvaluateVote(ctx, IntID(3)); err != nil {
			t.Fatal(err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("conclude vote that fails closes only", func(t *testing.T) {
		s, mock := newService(t)
		mock.ExpectQuery(q(voteHead)).WithArgs(num("3")).WillReturnRows(voteRow("open", 7, nil, "conclude", "majority"))
		mock.ExpectQuery(q(joinedMembers)).WithArgs(7, "joined").WillReturnRows(joined(2))
		mock.ExpectQuery(q(choiceCounts)).WithArgs(num("3")).WillReturnRows(counts(1, 1, 2, 1))
		mock.ExpectExec(q(closeSQL)).WithArgs("closed", num("3")).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		if err := s.EvaluateVote(ctx, IntID(3)); err != nil {
			t.Fatal(err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("conclude vote that passes concludes", func(t *testing.T) {
		s, mock := newService(t)
		mock.ExpectQuery(q(voteHead)).WithArgs(num("3")).WillReturnRows(voteRow("open", 7, nil, "conclude", "majority"))
		mock.ExpectQuery(q(joinedMembers)).WithArgs(7, "joined").WillReturnRows(joined(2))
		mock.ExpectQuery(q(choiceCounts)).WithArgs(num("3")).WillReturnRows(counts(1, 2))
		mock.ExpectExec(q(closeSQL)).WithArgs("closed", num("3")).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		mock.ExpectQuery(q(closedGeneral)).WithArgs(7).WillReturnRows(pgxmock.NewRows([]string{"id", "threshold"}))
		mock.ExpectExec(q("UPDATE discussions SET status = $1, concluded_at = NOW(), outcome = $2 WHERE id = $3")).
			WithArgs("concluded", "consensus", 7).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		mock.ExpectExec(q(leaveAllSQL)).WithArgs(7).WillReturnResult(pgxmock.NewResult("UPDATE", 2))
		if err := s.EvaluateVote(ctx, IntID(3)); err != nil {
			t.Fatal(err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}

func status(err error) int {
	var apiErr *apierr.Error
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return 0
}

func TestVoteCastValidation(t *testing.T) {
	ctx := context.Background()
	s, mock := newService(t)
	vote := RawID(json.RawMessage(`5`))
	cases := []struct {
		id             ID
		agent          string
		choice, reason string
		want           string
	}{
		{RawID(nil), "agent-a", `1`, "", "Required fields: vote_id, agent, choice"},
		{vote, "", `1`, "", "Required fields: vote_id, agent, choice"},
		{vote, "agent-a", ``, "", "Required fields: vote_id, agent, choice"},
		{vote, "agent-a", `null`, "", "Required fields: vote_id, agent, choice"},
		{vote, "agent-a", `"1"`, "", "Choice must be an integer"},
		{vote, "agent-a", `1.5`, "", "Choice must be an integer"},
		{vote, "agent-a", `1e400`, "", "Choice must be an integer"},
		{vote, "agent-a", `true`, "", "Choice must be an integer"},
		{vote, "agent-a", `[1]`, "", "Choice must be an integer"},
		{vote, "agent-a", `1`, `5`, "reason must be a string"},
	}
	for _, c := range cases {
		var reason json.RawMessage
		if c.reason != "" {
			reason = json.RawMessage(c.reason)
		}
		_, err := s.VoteCast(ctx, c.id, c.agent, json.RawMessage(c.choice), reason)
		if err == nil || err.Error() != c.want || status(err) != 400 {
			t.Errorf("%s %q %s: %v", c.id, c.agent, c.choice, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestVoteCast(t *testing.T) {
	ctx := context.Background()
	s, mock := newService(t)
	mock.ExpectQuery(q("SELECT status, discussion_id, type FROM discussion_votes WHERE id = $1")).WithArgs("5").
		WillReturnRows(pgxmock.NewRows([]string{"status", "discussion_id", "type"}).AddRow("open", 7, "general"))
	mock.ExpectQuery(q(actorByName)).WithArgs("agent-a").WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(2, "agent-a"))
	mock.ExpectQuery(q("SELECT status FROM discussion_participants WHERE discussion_id = $1 AND actor_id = $2")).WithArgs(7, 2).
		WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("joined"))
	mock.ExpectQuery(q("SELECT 1 FROM discussion_ballots WHERE vote_id = $1 AND actor_id = $2")).WithArgs("5", 2).
		WillReturnRows(pgxmock.NewRows([]string{"?column?"}))
	mock.ExpectExec(q("INSERT INTO discussion_ballots (vote_id, actor_id, choice, reason) VALUES ($1, $2, $3, $4)")).
		WithArgs("5", 2, pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// EvaluateVote: one of two joined voted, nothing to close.
	mock.ExpectQuery(q(voteHead)).WithArgs("5").WillReturnRows(voteRow("open", 7, nil, "general", "unanimous"))
	mock.ExpectQuery(q(joinedMembers)).WithArgs(7, "joined").WillReturnRows(joined(2))
	mock.ExpectQuery(q(choiceCounts)).WithArgs("5").WillReturnRows(counts(0, 1))
	mock.ExpectQuery(q("SELECT status FROM discussion_votes WHERE id = $1")).WithArgs("5").
		WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("open"))
	// The event, after the answer.
	mock.ExpectQuery(q(joinedMembers)).WithArgs(7, "joined").WillReturnRows(joined(2))
	mock.ExpectQuery(q("SELECT COUNT(*) as count FROM discussion_ballots WHERE vote_id = $1")).WithArgs("5").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(1)))
	mock.ExpectQuery(q(actorByName)).WithArgs("system").WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(1, "system"))
	mock.ExpectQuery(q(insertTextSQL)).WithArgs("agent-a voted 0 on general vote #5. 1/2 votes cast.", 1, 7).
		WillReturnRows(pgxmock.NewRows([]string{"id", "sent_at"}).AddRow(50, fixedNow))
	mock.ExpectQuery(q(insertDeliverySQL)).WithArgs(50, 1).WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(60))

	res, err := s.VoteCast(ctx, RawID(json.RawMessage(`"5"`)), "agent-a", json.RawMessage(`-0`), json.RawMessage(`"Fine."`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(res)
	if string(body) != `{"vote_id":"5","agent":"agent-a","choice":0,"reason":"Fine.","vote_status":"open"}` {
		t.Errorf("answer %s", body)
	}
	testutil.WaitMet(t, mock)
}

func TestIDValues(t *testing.T) {
	cases := []struct {
		raw          string
		truthy       bool
		param        any
		text, encode string
	}{
		{`101`, true, nil, "101", `101`},
		{`101.0`, true, nil, "101", `101`},
		{`"101"`, true, "101", "101", `"101"`},
		{`0`, false, nil, "0", `0`},
		{`""`, false, "", "", `""`},
		{`[1,2]`, true, "{\"1\",\"2\"}", "1,2", `[1,2]`},
		{`{"a":1}`, true, `{"a":1}`, "[object Object]", `{"a":1}`},
		{`true`, true, "true", "true", `true`},
	}
	for _, c := range cases {
		id := RawID(json.RawMessage(c.raw))
		if id.Truthy() != c.truthy || id.String() != c.text {
			t.Errorf("%s: truthy %v, text %q", c.raw, id.Truthy(), id.String())
		}
		if c.param != nil && id.Param() != c.param {
			t.Errorf("%s: param %#v", c.raw, id.Param())
		}
		if b, err := id.MarshalJSON(); err != nil || string(b) != c.encode {
			t.Errorf("%s: json %s %v", c.raw, b, err)
		}
	}
	if b, _ := RawID(nil).MarshalJSON(); string(b) != "null" || RawID(nil).Truthy() {
		t.Errorf("absent id: %s", b)
	}
	if IntID(7).String() != "7" {
		t.Error("IntID")
	}
}

func TestMinutesFromNow(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 123456789, time.UTC)
	got, param, ok := minutesFromNow(now, "5")
	if !ok || !got.Equal(time.Date(2026, 11, 6, 12, 5, 0, 123000000, time.UTC)) || param != got {
		t.Errorf("5 minutes: %v %v", got, ok)
	}
	// parseInt reads the leading digits.
	if got, _, ok := minutesFromNow(now, " 2.9 min"); !ok || got.Sub(time.UnixMilli(now.UnixMilli())) != 2*time.Minute {
		t.Errorf("parseInt prefix: %v", got)
	}
	for _, bad := range []string{"", "soon", "99999999999999"} {
		if _, param, ok := minutesFromNow(now, bad); ok || param != invalidDateParam {
			t.Errorf("%q: %v %v", bad, param, ok)
		}
	}
}

func TestTruncateUTF16Replacing(t *testing.T) {
	if got := truncateUTF16Replacing("abc", 2); got != "ab" {
		t.Errorf("ascii: %q", got)
	}
	// "a😀" is 3 units; a cut at 2 splits the pair, which reached Postgres
	// as U+FFFD.
	if got := truncateUTF16Replacing("a😀b", 2); got != "a�" {
		t.Errorf("split pair: %q", got)
	}
	if got := truncateUTF16Replacing("a😀b", 3); got != "a😀" {
		t.Errorf("whole pair: %q", got)
	}
	if got := truncateUTF16Replacing(strings.Repeat("x", 5), 10); got != "xxxxx" {
		t.Errorf("short: %q", got)
	}
}

func TestScanStaleVotes(t *testing.T) {
	ctx := context.Background()
	s, mock := newService(t)
	s.Config.Set("discussion_stale_vote_timeout", "0")
	mock.ExpectQuery(q("SELECT id, discussion_id FROM discussion_votes")).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "discussion_id"}).AddRow(3, 7))
	mock.ExpectExec(q("UPDATE discussion_votes SET closes_at = NOW() WHERE id = $1 AND status = $2")).WithArgs(3, "open").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(q(voteHead)).WithArgs(num("3")).WillReturnRows(voteRow("open", 7, &fixedNow, "general", "unanimous"))
	mock.ExpectQuery(q(joinedMembers)).WithArgs(7, "joined").WillReturnRows(joined(2))
	mock.ExpectQuery(q(choiceCounts)).WithArgs(num("3")).WillReturnRows(counts())
	mock.ExpectExec(q("UPDATE discussion_votes SET status = $1, closed_at = NOW() WHERE id = $2")).WithArgs("closed", num("3")).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	s.ScanStaleVotes(ctx)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	// A missing config key is logged and ends the scan.
	s2, mock2 := newService(t)
	s2.Config = config.NewStore()
	s2.ScanStaleVotes(ctx)
	if err := mock2.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Node compared Dates: the deadline and the clock in the same millisecond
// count as reached, whatever their microseconds.
func TestDeadlinesInMilliseconds(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 6, 12, 0, 0, 123500000, time.UTC)
	deadline := time.Date(2026, 11, 6, 12, 0, 0, 123900000, time.UTC)
	nextMS := time.Date(2026, 11, 6, 12, 0, 0, 124000000, time.UTC)
	if dateMS(deadline) != now.UnixMilli() || dateMS(nextMS) != now.UnixMilli()+1 {
		t.Fatalf("dateMS %d %d, now %d", dateMS(deadline), dateMS(nextMS), now.UnixMilli())
	}

	s, mock := newService(t)
	s.Now = func() time.Time { return now }
	mock.ExpectQuery(q(voteHead)).WithArgs(num("3")).WillReturnRows(voteRow("open", 7, &deadline, "general", "unanimous"))
	mock.ExpectQuery(q(joinedMembers)).WithArgs(7, "joined").WillReturnRows(joined(2))
	mock.ExpectQuery(q(choiceCounts)).WithArgs(num("3")).WillReturnRows(counts())
	mock.ExpectExec(q("UPDATE discussion_votes SET status = $1, closed_at = NOW() WHERE id = $2")).WithArgs("closed", num("3")).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	if err := s.EvaluateVote(ctx, IntID(3)); err != nil {
		t.Fatal(err)
	}
	// One millisecond later is still open.
	mock.ExpectQuery(q(voteHead)).WithArgs(num("4")).WillReturnRows(voteRow("open", 7, &nextMS, "general", "unanimous"))
	mock.ExpectQuery(q(joinedMembers)).WithArgs(7, "joined").WillReturnRows(joined(2))
	mock.ExpectQuery(q(choiceCounts)).WithArgs(num("4")).WillReturnRows(counts())
	if err := s.EvaluateVote(ctx, IntID(4)); err != nil {
		t.Fatal(err)
	}
	// The readiness timeout the same way.
	mock.ExpectQuery(q(readinessHead)).WithArgs(num("7")).WillReturnRows(pgxmock.NewRows([]string{"status", "timeout_at"}).AddRow("waiting", &deadline))
	mock.ExpectQuery(q(readinessMembers)).WithArgs(num("7")).WillReturnRows(members([3]string{"b", "invited", "required"}))
	mock.ExpectExec(q("UPDATE discussions SET status = $1, outcome = $2 WHERE id = $3")).WithArgs("timed_out", "abandoned", num("7")).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(q(leaveAllSQL)).WithArgs(num("7")).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	if err := s.EvaluateReadiness(ctx, IntID(7)); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A job under InOrder starts after the one before it ends, with a deadline
// of its own: queued behind a job held for 300 ms, its deadline is that much
// later than the first job's.
func TestLaterOrderAndDeadline(t *testing.T) {
	ctx := InOrder(context.Background())
	var got []string
	var mu sync.Mutex
	add := func(s string) {
		mu.Lock()
		got = append(got, s)
		mu.Unlock()
	}
	firstDeadline := make(chan time.Time, 1)
	release := make(chan struct{})
	secondDeadline := make(chan time.Time, 1)
	finished := make(chan struct{})
	later(ctx, func(ctx context.Context) {
		d, _ := ctx.Deadline()
		firstDeadline <- d
		<-release
		add("first")
	})
	later(ctx, func(ctx context.Context) {
		add("second")
		d, _ := ctx.Deadline()
		secondDeadline <- d
	})
	later(ctx, func(ctx context.Context) {
		add("third")
		close(finished)
	})
	d1 := <-firstDeadline
	time.Sleep(300 * time.Millisecond)
	close(release)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("jobs did not finish")
	}
	if strings.Join(got, ",") != "first,second,third" {
		t.Errorf("order %v", got)
	}
	if gap := (<-secondDeadline).Sub(d1); gap < 250*time.Millisecond {
		t.Errorf("second job's deadline only %v after the first's: it started with the first", gap)
	}
}

// blockingDB holds the queries whose first argument is block until release
// is closed, and reports every other query's SQL on seen.
type blockingDB struct {
	db.DB
	block   any
	release chan struct{}
	seen    chan string
}

func (b *blockingDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if len(args) > 0 && args[0] == b.block {
		<-b.release
	} else {
		select {
		case b.seen <- sql:
		default:
		}
	}
	return b.DB.QueryRow(ctx, sql, args...)
}

func (b *blockingDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	select {
	case b.seen <- sql:
	default:
	}
	return b.DB.Exec(ctx, sql, args...)
}

// waitSeen waits for a query whose SQL starts with prefix; it releases the
// held query and fails the test after two seconds.
func (b *blockingDB) waitSeen(t *testing.T, prefix, what string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case sql := <-b.seen:
			if strings.HasPrefix(sql, prefix) {
				return
			}
		case <-deadline:
			close(b.release)
			t.Fatal(what + " waited for the held event")
		}
	}
}

// Conclude's participant messages do not wait on the event: with the event
// insert held, the participant's delivery is still written.
func TestConcludeChainsAreIndependent(t *testing.T) {
	ctx := InOrder(context.Background())
	s, mock := newService(t)
	expectActor := func(name string, id int) {
		mock.ExpectQuery(q(actorByName)).WithArgs(name).WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(id, name))
	}
	// Warm the cache, so the two jobs do not race to look the system actor up.
	expectActor("system", 1)
	expectActor("agent-b", 3)
	for _, name := range []string{"system", "agent-b"} {
		if _, _, err := s.Actors.ResolveByName(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	expectActor("agent-a", 2)
	mock.ExpectQuery(q("SELECT status FROM discussion_participants WHERE discussion_id = $1 AND actor_id = $2")).WithArgs(num("7"), 2).
		WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("joined"))
	mock.ExpectQuery(q("SELECT status, created_by_actor_id, topic FROM discussions WHERE id = $1")).WithArgs(num("7")).
		WillReturnRows(pgxmock.NewRows([]string{"status", "created_by_actor_id", "topic"}).AddRow("active", 2, "Plan"))
	mock.ExpectQuery(q(closedGeneral)).WithArgs(num("7")).WillReturnRows(pgxmock.NewRows([]string{"id", "threshold"}))
	mock.ExpectExec(q("UPDATE discussions SET status = $1, concluded_at = NOW(), outcome = $2 WHERE id = $3")).
		WithArgs("concluded", "consensus", num("7")).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(q(leaveAllSQL)).WithArgs(num("7")).WillReturnResult(pgxmock.NewResult("UPDATE", 2))
	mock.ExpectQuery(q("SELECT ac.name AS agent FROM discussion_participants dp")).WithArgs(num("7"), 2).
		WillReturnRows(pgxmock.NewRows([]string{"agent"}).AddRow("agent-b"))
	mock.ExpectQuery(q(insertTextSQL)).WithArgs("Discussion concluded by agent-a", 1, num("7")).WillReturnError(errors.New("event failed"))
	held := &blockingDB{DB: mock, block: "Discussion concluded by agent-a", release: make(chan struct{}), seen: make(chan string, 64)}
	s.Notifier.DB = held
	mock.ExpectQuery(q(insertTextSQL)).WithArgs(`Discussion #7 ("Plan") was concluded by agent-a`, 1, nil).
		WillReturnRows(pgxmock.NewRows([]string{"id", "sent_at"}).AddRow(50, fixedNow))
	mock.ExpectQuery(q(insertDeliverySQL)).WithArgs(50, 3).WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(60))

	res, err := s.Conclude(ctx, IntID(7), "agent-a", false)
	if err != nil || res.Status != "concluded" || res.Outcome != "consensus" {
		t.Fatalf("%+v %v", res, err)
	}
	deadline := time.After(2 * time.Second)
	for delivered := false; !delivered; {
		select {
		case sql := <-held.seen:
			delivered = strings.HasPrefix(sql, insertDeliverySQL)
		case <-deadline:
			close(held.release)
			t.Fatal("the participant delivery waited for the held event")
		}
	}
	close(held.release)
	testutil.WaitMet(t, mock)
}
