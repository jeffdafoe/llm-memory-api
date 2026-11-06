package discussion

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

func actorRows(pairs ...any) *pgxmock.Rows {
	rows := pgxmock.NewRows([]string{"id", "name"})
	for i := 0; i < len(pairs); i += 2 {
		rows.AddRow(pairs[i], pairs[i+1])
	}
	return rows
}

func expectActorByName(mock pgxmock.PgxPoolIface, name string, id int) {
	mock.ExpectQuery(q(actorByName)).WithArgs(name).WillReturnRows(actorRows(id, name))
}

func discussionRow(id int, status string, creator int) *pgxmock.Rows {
	return pgxmock.NewRows([]string{"id", "topic", "status", "channel", "created_at", "concluded_at", "mode", "context", "timeout_at", "outcome", "created_by_actor_id"}).
		AddRow(id, "Topic", status, nil, fixedNow, nil, "realtime", nil, nil, nil, creator)
}

func TestCreateValidation(t *testing.T) {
	ctx := context.Background()
	s, mock := newService(t)
	cases := []struct {
		in   CreateInput
		want string
	}{
		{CreateInput{CreatedBy: "a", Participants: []string{"a", "b"}}, "Required fields: topic, created_by, participants (array)"},
		{CreateInput{Topic: "t", CreatedBy: "a"}, "Required fields: topic, created_by, participants (array)"},
		{CreateInput{Topic: "t", CreatedBy: "a", Participants: []string{"a"}}, "A discussion requires at least 2 participants"},
		{CreateInput{Topic: "t", CreatedBy: "a", Participants: []string{"b"}, Optional: []string{"a"}}, "Creator must be in the participants list"},
		{CreateInput{Topic: "t", CreatedBy: "a", Participants: []string{"a", "b"}, Optional: []string{"b"}}, "Agents cannot be both required and optional: b"},
	}
	for _, c := range cases {
		if _, err := s.Create(ctx, c.in); err == nil || err.Error() != c.want || status(err) != 400 {
			t.Errorf("%+v: %v", c.in, err)
		}
	}
	// Every name is resolved in one query; the first missing one is the 404.
	mock.ExpectQuery(q("SELECT id, name FROM actors WHERE name = ANY($1)")).WithArgs([]string{"a", "x", "y"}).
		WillReturnRows(actorRows(1, "a"))
	_, err := s.Create(ctx, CreateInput{Topic: "t", CreatedBy: "a", Participants: []string{"a", "x"}, Optional: []string{"y"}})
	if err == nil || err.Error() != `Agent "x" is not registered` || status(err) != 404 {
		t.Errorf("unknown participant: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

const conflictSQL = "SELECT d.id, d.topic, d.status FROM discussions d"

func TestCreateConflict(t *testing.T) {
	ctx := context.Background()
	s, mock := newService(t)
	mock.ExpectQuery(q("SELECT id, name FROM actors WHERE name = ANY($1)")).WithArgs([]string{"a", "b"}).
		WillReturnRows(actorRows(1, "a", 2, "b"))
	mock.ExpectQuery(q(conflictSQL)).WithArgs(1).
		WillReturnRows(pgxmock.NewRows([]string{"id", "topic", "status"}).AddRow(9, "Old", "active"))
	_, err := s.Create(ctx, CreateInput{Topic: "t", CreatedBy: "a", Participants: []string{"a", "b"}})
	var c *ConflictError
	if !errors.As(err, &c) || c.ExistingDiscussionID != 9 || err.Error() != `Agent "a" is already in discussion #9 (active): "Old"` {
		t.Fatalf("conflict: %v", err)
	}
	if st, code := apierr.StatusAndCode(err); st != http.StatusConflict || code != "DISCUSSION_CONFLICT" {
		t.Errorf("status %d %s", st, code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateAutoJoinsVirtualAgents(t *testing.T) {
	ctx := context.Background()
	s, mock := newService(t)
	s.Config.Set("discussion_wait_timeout_async", "60")
	mock.ExpectQuery(q("SELECT id, name FROM actors WHERE name = ANY($1)")).WithArgs([]string{"a", "va", "c"}).
		WillReturnRows(actorRows(1, "a", 5, "va", 3, "c"))
	mock.ExpectQuery(q(conflictSQL)).WithArgs(1).WillReturnRows(pgxmock.NewRows([]string{"id", "topic", "status"}))
	mock.ExpectBegin()
	timeoutAt := fixedNow.Add(time.Hour)
	mock.ExpectQuery(q("INSERT INTO discussions")).WithArgs("t", 1, "waiting", pgxmock.AnyArg(), "async", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).AddRow(201, fixedNow))
	mock.ExpectQuery(q("SELECT actor_id FROM agent_configuration WHERE actor_id = ANY($1) AND virtual = TRUE")).WithArgs([]int{1, 5, 3}).
		WillReturnRows(pgxmock.NewRows([]string{"actor_id"}).AddRow(5))
	insertParticipant := q("INSERT INTO discussion_participants (discussion_id, actor_id, status, role, joined_at) VALUES ($1, $2, $3, $4, $5)")
	mock.ExpectExec(insertParticipant).WithArgs(201, 1, "joined", "required", pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(insertParticipant).WithArgs(201, 5, "joined", "required", pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(insertParticipant).WithArgs(201, 3, "invited", "optional", (*time.Time)(nil)).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()
	mock.ExpectRollback()
	// The invites (virtual agent included), after the answer. The names were
	// cached by the one lookup of all of them.
	invite := `You've been invited to discussion #201: "t" (created by a)`
	expectActorByName(mock, "system", 10)
	mock.ExpectQuery(q(insertTextSQL)).WithArgs(invite, 10, nil).WillReturnRows(pgxmock.NewRows([]string{"id", "sent_at"}).AddRow(50, fixedNow))
	mock.ExpectQuery(q(insertDeliverySQL)).WithArgs(50, 5).WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(60))
	mock.ExpectQuery(q(insertTextSQL)).WithArgs(invite, 10, nil).WillReturnRows(pgxmock.NewRows([]string{"id", "sent_at"}).AddRow(51, fixedNow))
	mock.ExpectQuery(q(insertDeliverySQL)).WithArgs(51, 3).WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(61))
	// Readiness: c is still invited.
	mock.ExpectQuery(q(readinessHead)).WithArgs(num("201")).WillReturnRows(pgxmock.NewRows([]string{"status", "timeout_at"}).AddRow("waiting", &timeoutAt))
	mock.ExpectQuery(q(readinessMembers)).WithArgs(num("201")).WillReturnRows(members(
		[3]string{"a", "joined", "required"}, [3]string{"va", "joined", "required"}, [3]string{"c", "invited", "optional"}))
	mock.ExpectQuery(q("SELECT status FROM discussions WHERE id = $1")).WithArgs(201).WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("waiting"))

	res, err := s.Create(ctx, CreateInput{Topic: "t", CreatedBy: "a", Participants: []string{"a", "va"}, Optional: []string{"c"}, Mode: "async"})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(res)
	want := `{"discussion":{"id":201,"topic":"t","created_by":"a","status":"waiting","channel":null,"mode":"async","timeout_at":"2026-11-06T13:00:00.000Z","created_at":"2026-11-06T12:00:00.000Z"},` +
		`"participants":[{"agent":"a","role":"required","status":"joined"},{"agent":"va","role":"required","status":"joined"},{"agent":"c","role":"optional","status":"invited"}]}`
	if string(body) != want {
		t.Errorf("answer %s", body)
	}
	testutil.WaitMet(t, mock)
}

func TestList(t *testing.T) {
	ctx := context.Background()
	s, mock := newService(t)
	// An unknown agent lists nothing, and nothing else is read.
	mock.ExpectQuery(q(actorByName)).WithArgs("nobody").WillReturnRows(pgxmock.NewRows([]string{"id", "name"}))
	got, err := s.List(ctx, RawID(nil), "nobody")
	if err != nil || len(got) != 0 {
		t.Fatalf("unknown agent: %v %v", got, err)
	}

	expectActorByName(mock, "a", 1)
	mock.ExpectQuery(q("SELECT "+discussionColumns+" FROM discussions d JOIN discussion_participants dp ON d.id = dp.discussion_id WHERE dp.actor_id = $1 AND d.status = $2 ORDER BY d.created_at DESC")).
		WithArgs(1, "active").WillReturnRows(discussionRow(7, "active", 4))
	// The creator is resolved by id; a gone actor is null.
	mock.ExpectQuery(q("SELECT id, name FROM actors WHERE id = $1")).WithArgs(4).WillReturnRows(pgxmock.NewRows([]string{"id", "name"}))
	got, err = s.List(ctx, RawID(json.RawMessage(`"active"`)), "a")
	if err != nil || len(got) != 1 || got[0].CreatedBy != nil {
		t.Fatalf("list: %+v %v", got, err)
	}
	body, _ := json.Marshal(got[0])
	if string(body) != `{"id":7,"topic":"Topic","status":"active","channel":null,"created_at":"2026-11-06T12:00:00.000Z","concluded_at":null,"mode":"realtime","context":null,"timeout_at":null,"outcome":null,"created_by_actor_id":4,"created_by":null}` {
		t.Errorf("row %s", body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestStatusAndVoteStatusRefusals(t *testing.T) {
	ctx := context.Background()
	s, mock := newService(t)
	if _, err := s.Status(ctx, RawID(nil)); err == nil || err.Error() != "Required field: discussion_id" {
		t.Errorf("status without id: %v", err)
	}
	mock.ExpectQuery(q("SELECT status FROM discussions WHERE id = $1")).WithArgs(num("9")).WillReturnRows(pgxmock.NewRows([]string{"status"}))
	if _, err := s.Status(ctx, IntID(9)); err == nil || err.Error() != "Discussion not found" || status(err) != 404 {
		t.Errorf("status unknown: %v", err)
	}
	if _, err := s.VoteStatus(ctx, RawID(nil)); err == nil || err.Error() != "Required field: vote_id" {
		t.Errorf("vote status without id: %v", err)
	}
	mock.ExpectQuery(q("SELECT status FROM discussion_votes WHERE id = $1")).WithArgs(num("9")).WillReturnRows(pgxmock.NewRows([]string{"status"}))
	if _, err := s.VoteStatus(ctx, IntID(9)); err == nil || err.Error() != "Vote not found" || status(err) != 404 {
		t.Errorf("vote status unknown: %v", err)
	}
	if _, err := s.Pending(ctx, "", RawID(nil)); err == nil || err.Error() != "Required field: agent" {
		t.Errorf("pending without agent: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestJoinAndDeferRefusals(t *testing.T) {
	ctx := context.Background()
	s, mock := newService(t)
	s.Config.Set("max_defer_count", "3")
	for _, fn := range []func() error{
		func() error { _, err := s.Join(ctx, RawID(nil), "a"); return err },
		func() error { _, err := s.Defer(ctx, IntID(1), ""); return err },
		func() error { _, err := s.Leave(ctx, RawID(json.RawMessage(`0`)), "a"); return err },
	} {
		if err := fn(); err == nil || err.Error() != "Required fields: discussion_id, agent" || status(err) != 400 {
			t.Errorf("required: %v", err)
		}
	}
	statusOf := q("SELECT status FROM discussions WHERE id = $1")
	mock.ExpectQuery(statusOf).WithArgs(num("4")).WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("concluded"))
	mock.ExpectQuery(statusOf).WithArgs(num("4")).WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("concluded"))
	if _, err := s.Join(ctx, IntID(4), "a"); err == nil || err.Error() != "Discussion is concluded and not accepting joins" {
		t.Errorf("join concluded: %v", err)
	}
	mock.ExpectQuery(statusOf).WithArgs(num("5")).WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("waiting"))
	expectActorByName(mock, "b", 2)
	mock.ExpectQuery(q("SELECT status, defer_count FROM discussion_participants")).WithArgs(num("5"), 2).
		WillReturnRows(pgxmock.NewRows([]string{"status", "defer_count"}).AddRow("invited", ptrInt(3)))
	_, err := s.Defer(ctx, IntID(5), "b")
	if st, code := apierr.StatusAndCode(err); st != 400 || code != "MAX_DEFERRALS_REACHED" || err.Error() != "Maximum deferrals reached (3). Must join or let timeout expire." {
		t.Errorf("defer at the limit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func ptrInt(n int) *int { return &n }

func TestLeaveReevaluatesOpenVotes(t *testing.T) {
	ctx := InOrder(context.Background())
	s, mock := newService(t)
	expectActorByName(mock, "b", 2)
	mock.ExpectQuery(q("SELECT status FROM discussion_participants WHERE discussion_id = $1 AND actor_id = $2")).WithArgs(num("7"), 2).
		WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("joined"))
	mock.ExpectExec(q("UPDATE discussion_participants SET status = $1 WHERE discussion_id = $2 AND actor_id = $3")).WithArgs("left", num("7"), 2).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	// The event, then the open votes evaluated again: the one left joined
	// voter has voted, so the vote closes.
	expectActorByName(mock, "system", 10)
	mock.ExpectQuery(q(insertTextSQL)).WithArgs("b left the discussion", 10, num("7")).WillReturnRows(pgxmock.NewRows([]string{"id", "sent_at"}).AddRow(50, fixedNow))
	mock.ExpectQuery(q(insertDeliverySQL)).WithArgs(50, 10).WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(60))
	mock.ExpectQuery(q("SELECT id FROM discussion_votes WHERE discussion_id = $1 AND status = $2")).WithArgs(num("7"), "open").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(3))
	mock.ExpectQuery(q(voteHead)).WithArgs(num("3")).WillReturnRows(voteRow("open", 7, nil, "general", "unanimous"))
	mock.ExpectQuery(q(joinedMembers)).WithArgs(7, "joined").WillReturnRows(joined(1))
	mock.ExpectQuery(q(choiceCounts)).WithArgs(num("3")).WillReturnRows(counts(1, 1))
	mock.ExpectExec(q("UPDATE discussion_votes SET status = $1, closed_at = NOW() WHERE id = $2")).WithArgs("closed", num("3")).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	res, err := s.Leave(ctx, IntID(7), "b")
	if err != nil {
		t.Fatal(err)
	}
	if body, _ := json.Marshal(res); string(body) != `{"discussion_id":7,"agent":"b","status":"left"}` {
		t.Errorf("answer %s", body)
	}
	testutil.WaitMet(t, mock)
}

func TestProposeNotifiesVirtualAgents(t *testing.T) {
	ctx := InOrder(context.Background())
	s, mock := newService(t)
	in := ProposeInput{DiscussionID: RawID(json.RawMessage(`"7"`)), ProposedBy: "a", Question: "Ship?", Type: RawID(json.RawMessage(`"conclude"`))}

	expectActorByName(mock, "a", 1)
	mock.ExpectQuery(q("SELECT status FROM discussion_participants WHERE discussion_id = $1 AND actor_id = $2")).WithArgs("7", 1).
		WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("joined"))
	mock.ExpectQuery(q("SELECT status FROM discussions WHERE id = $1")).WithArgs("7").WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("active"))
	mock.ExpectQuery(q("INSERT INTO discussion_votes")).WithArgs("7", 1, "Ship?", "conclude", "unanimous", nil).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).AddRow(301, fixedNow))
	expectActorByName(mock, "system", 10)
	mock.ExpectQuery(q(insertTextSQL)).WithArgs("a proposed conclude vote #301: Ship?", 10, "7").WillReturnRows(pgxmock.NewRows([]string{"id", "sent_at"}).AddRow(50, fixedNow))
	mock.ExpectQuery(q(insertDeliverySQL)).WithArgs(50, 10).WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(60))
	mock.ExpectQuery(q(insertTextSQL)).WithArgs(`{"type":"virtual-agent","discussionId":"7","triggerType":"vote-proposed","voteId":301}`, 10, nil).
		WillReturnRows(pgxmock.NewRows([]string{"id", "sent_at"}).AddRow(51, fixedNow))
	mock.ExpectQuery(q(insertDeliverySQL)).WithArgs(51, 10).WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(61))

	res, err := s.Propose(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(res)
	if string(body) != `{"vote":{"id":301,"discussion_id":"7","proposed_by":"a","question":"Ship?","type":"conclude","threshold":"unanimous","closes_at":null,"created_at":"2026-11-06T12:00:00.000Z"}}` {
		t.Errorf("answer %s", body)
	}
	testutil.WaitMet(t, mock)

	// An unknown proposer is not a participant (the route's own copy says
	// 404), and a type of another JSON type fails the check.
	s2, mock2 := newService(t)
	mock2.ExpectQuery(q(actorByName)).WithArgs("x").WillReturnRows(pgxmock.NewRows([]string{"id", "name"}))
	if _, err := s2.Propose(ctx, ProposeInput{DiscussionID: IntID(7), ProposedBy: "x", Question: "q"}); err == nil || err.Error() != "Agent is not a participant in this discussion" {
		t.Errorf("unknown proposer: %v", err)
	}
	if _, ok := kind(RawID(json.RawMessage(`7`)), "general", "general"); ok {
		t.Error("number type accepted")
	}
	if v, ok := kind(RawID(json.RawMessage(`""`)), "general", "general"); !ok || v != "general" {
		t.Error("falsy type not defaulted")
	}
	if err := mock2.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
