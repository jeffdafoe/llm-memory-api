package system

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

func str(s string) *string { return &s }

func TestHandleMessage(t *testing.T) {
	mock := testutil.NewMock(t)
	h := &Handler{Log: logger.New(mock, actors.NewCache(mock), "")}
	var got []string
	h.Register("virtual-agent", func(ctx context.Context, payload json.RawMessage, meta Meta) error {
		got = append(got, string(payload))
		if meta.MessageID != 9 || meta.FromAgent != "agent-a" {
			t.Errorf("meta %+v", meta)
		}
		return errors.New("handler failed")
	})
	for _, m := range []*string{nil, str("not json"), str("null"), str("5"), str(`{}`), str(`{"type":""}`), str(`{"type":"other"}`), str(`{"type":["virtual-agent"]}`)} {
		h.HandleMessage(context.Background(), 9, "agent-a", m, nil)
	}
	if len(got) != 0 {
		t.Fatalf("dispatched %v", got)
	}
	h.HandleMessage(context.Background(), 9, "agent-a", str(`{"type":"virtual-agent","discussionId":7}`), nil)
	if len(got) != 1 || got[0] != `{"type":"virtual-agent","discussionId":7}` {
		t.Errorf("got %v", got)
	}
}
