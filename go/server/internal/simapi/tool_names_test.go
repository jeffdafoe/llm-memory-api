package simapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
)

// raw-turns returns tool_names, the tools offered on the call in the order
// they were sent (LLM-991): the list when one was recorded, null otherwise.
// The Node goldens predate the field, so they carry null only.
func TestRawTurnsReturnToolNames(t *testing.T) {
	row := func(id string, toolNames any) []any {
		return []any{id, "salem-vendor", nil, "chat", nil, "openrouter", "m", "DeepInfra",
			nil, nil, "sys", "user", "resp", "success", nil, nil,
			float64(10), float64(5), float64(0), float64(0), "0.000035", float64(120), "2026-11-23 10:00:00.123456+00", toolNames}
	}
	db := &scriptDB{s: &stream{}, script: []scriptStep{{
		Match: "c.created_at, c.tool_names",
		Rows:  [][]any{row("2", []any{"speak", "move_to", "pay_with_item"}), row("1", nil)},
	}}}
	rt := &Routes{Service: &Service{DB: db}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sim/raw-turns", nil)
	httpapi.WithState(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpapi.StateOf(r).Body = json.RawMessage(`{}`)
		route(rt.rawTurns)(w, r)
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Turns []map[string]any `json:"turns"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Turns) != 2 {
		t.Fatalf("%d turns, want 2: %s", len(got.Turns), rec.Body.String())
	}
	want := []any{"speak", "move_to", "pay_with_item"}
	if v, ok := got.Turns[0]["tool_names"]; !ok || !reflect.DeepEqual(v, want) {
		t.Errorf("first turn tool_names = %v (present %v), want %v", v, ok, want)
	}
	if v, ok := got.Turns[1]["tool_names"]; !ok || v != nil {
		t.Errorf("second turn tool_names = %v (present %v), want null", v, ok)
	}
}
