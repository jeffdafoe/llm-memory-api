package simapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
)

// raw-turns returns served_by, the OpenRouter upstream that served the call
// (LLM-935): the host when one was recorded, null otherwise. The Node
// goldens predate the field, so they carry null only.
func TestRawTurnsReturnServedBy(t *testing.T) {
	row := func(id string, servedBy any) []any {
		return []any{id, "zbbs-john-ellis", nil, "chat", nil, "openrouter", "m", servedBy,
			nil, nil, "sys", "user", "resp", "success", nil, nil,
			float64(10), float64(5), float64(0), float64(0), "0.000035", float64(120), "2026-11-05 10:00:00.123456+00"}
	}
	db := &scriptDB{s: &stream{}, script: []scriptStep{{
		Match: "c.provider, c.model, c.served_by,",
		Rows:  [][]any{row("2", "DeepInfra"), row("1", nil)},
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
	if v, ok := got.Turns[0]["served_by"]; !ok || v != "DeepInfra" {
		t.Errorf("first turn served_by = %v (present %v), want DeepInfra", v, ok)
	}
	if v, ok := got.Turns[1]["served_by"]; !ok || v != nil {
		t.Errorf("second turn served_by = %v (present %v), want null", v, ok)
	}
}
