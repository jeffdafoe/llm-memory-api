package notes

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

// A direct caller (an MCP tool) gets Node's `limit || 50` and `offset || 0`
// from the service, not only from the route.
func TestListDefaultsInTheService(t *testing.T) {
	cases := []struct {
		limit, offset json.RawMessage
		wantLimit     any
		wantOffset    any
	}{
		{nil, nil, 50, 0},
		{json.RawMessage(`0`), json.RawMessage(`""`), 50, 0},
		{json.RawMessage(` null `), json.RawMessage(`false`), 50, 0},
		{json.RawMessage(`7`), json.RawMessage(`2`), jsutil.Number("7"), jsutil.Number("2")},
		{json.RawMessage(`"3"`), nil, "3", 0},
	}
	for _, c := range cases {
		mock := testutil.NewMock(t)
		mock.ExpectQuery("FROM documents d").
			WithArgs("ns", c.wantLimit, c.wantOffset).
			WillReturnRows(pgxmock.NewRows([]string{"id"}))
		s := &Service{DB: mock}
		if _, err := s.List(context.Background(), ListInput{Namespace: "ns", Limit: c.limit, Offset: c.offset}); err != nil {
			t.Errorf("limit %s offset %s: %v", c.limit, c.offset, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("limit %s offset %s: %v", c.limit, c.offset, err)
		}
	}
}

func TestRawJSONHelpersAcceptWhitespace(t *testing.T) {
	meta, err := metadataJSON(json.RawMessage(" \n{\"a\": 1} "))
	if err != nil || meta == nil || *meta != `{"a":1}` {
		t.Errorf("whitespace-wrapped metadata: %v %v", meta, err)
	}
	if meta, err := metadataJSON(json.RawMessage(" null ")); err != nil || meta != nil {
		t.Errorf("whitespace-wrapped null metadata: %v %v", meta, err)
	}
	if _, err := metadataJSON(json.RawMessage(`[1]`)); err == nil {
		t.Error("an array is not a plain object")
	}
	page, err := PaginateContent("a\nb\nc", json.RawMessage(" 2 "), json.RawMessage(" null "))
	if err != nil || page == nil || page.Text != "[lines 2-3 of 3]\n\nb\nc" {
		t.Errorf("whitespace-wrapped offset: %+v %v", page, err)
	}
}

func TestStringifyRejectsTrailingData(t *testing.T) {
	for _, in := range []string{`{} {}`, `{} garbage`, `[1]]`} {
		if out, err := jsutil.Stringify([]byte(in)); err == nil {
			t.Errorf("Stringify(%q) = %q, want an error as JSON.parse throws", in, out)
		}
	}
	if out, err := jsutil.Stringify([]byte(" {} \n")); err != nil || out != "{}" {
		t.Errorf("trailing whitespace: %q %v", out, err)
	}
}

func TestMetadataWithALoneSurrogateFailsAsInNode(t *testing.T) {
	bad := []string{`{"x":"\ud800"}`, `{"x":"\udc00"}`, `{"x":"\ud800\u0041"}`, `{"x":"\ud800"}  `, `{"\ud83d":1}`}
	for _, in := range bad {
		if _, err := metadataJSON(json.RawMessage(in)); err == nil {
			t.Errorf("metadata %s was accepted", in)
		}
	}
	good := []string{`{"x":"\ud83d\ude00"}`, `{"x":"\\ud800"}`, `{"x":"\u00e9"}`}
	for _, in := range good {
		if _, err := metadataJSON(json.RawMessage(in)); err != nil {
			t.Errorf("metadata %s: %v", in, err)
		}
	}
}
