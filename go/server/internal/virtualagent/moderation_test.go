package virtualagent

import (
	"context"
	"errors"
	"testing"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

func moderator(t *testing.T) (Moderator, pgxmock.PgxPoolIface) {
	mock := testutil.NewMock(t)
	return Moderator{DB: mock, Log: logger.New(mock, actors.NewCache(mock), "")}, mock
}

func strp(s string) *string { return &s }
func boolp(b bool) *bool    { return &b }

// Every outcome approves until the providers are ported: no moderation
// agent, an unconfigured one, a configured one, and a query error.
func TestModerationApproves(t *testing.T) {
	rows := []*pgxmock.Rows{
		pgxmock.NewRows([]string{"virtual", "provider", "model", "api_key"}),
		pgxmock.NewRows([]string{"virtual", "provider", "model", "api_key"}).AddRow(boolp(false), strp("anthropic"), strp("m"), strp("k")),
		pgxmock.NewRows([]string{"virtual", "provider", "model", "api_key"}).AddRow(boolp(true), nil, strp("m"), strp("k")),
		pgxmock.NewRows([]string{"virtual", "provider", "model", "api_key"}).AddRow(boolp(true), strp("anthropic"), strp("m"), strp("k")),
	}
	for i, r := range rows {
		m, mock := moderator(t)
		mock.ExpectQuery("FROM agent_configuration agc").WithArgs("actor-name-check").WillReturnRows(r)
		if ok, reason := m.ModerateActorName(context.Background(), "newcomer"); !ok || reason != "" {
			t.Errorf("case %d: %v %q", i, ok, reason)
		}
	}
	m, mock := moderator(t)
	mock.ExpectQuery("FROM agent_configuration agc").WithArgs("actor-name-check").WillReturnError(errors.New("boom"))
	if ok, _ := m.ModerateActorName(context.Background(), "newcomer"); !ok {
		t.Error("an error refused the name")
	}
}
