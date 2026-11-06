package contract

import (
	"strings"
	"testing"
)

func jsonBody(t *testing.T, s string) Body {
	t.Helper()
	v, err := ParseJSON([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return Body{IsJSON: true, JSON: v}
}

func oneStep(status int, headers []Header, body Body) Result {
	return Result{Steps: []StepResult{{
		Label:    "POST /v1/agent/login",
		Response: Response{Status: status, Headers: headers, Body: body},
	}}}
}

func wheres(rep Report) []string {
	var out []string
	for _, d := range rep.Differences {
		out = append(out, d.Where)
	}
	return out
}

func TestCompareIdentical(t *testing.T) {
	r := oneStep(400, []Header{{Name: "content-type", Values: []string{"application/json; charset=utf-8"}}},
		jsonBody(t, `{"error":{"code":"BAD_REQUEST","message":"x"}}`))
	rep, err := Compare(r, r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("identical results differ:\n%s", rep)
	}
}

func TestCompareFindsEachKind(t *testing.T) {
	node := oneStep(400, []Header{
		{Name: "content-type", Values: []string{"application/json; charset=utf-8"}},
		{Name: "www-authenticate", Values: []string{"Bearer x"}},
	}, jsonBody(t, `{"error":{"code":"BAD_REQUEST","message":"x"},"list":[1,2]}`))
	goRes := oneStep(500, []Header{
		{Name: "content-type", Values: []string{"application/json"}},
	}, jsonBody(t, `{"list":[1],"error":{"message":"x","code":"INTERNAL"}}`))

	rep, err := Compare(node, goRes, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(wheres(rep), "\n")
	for _, want := range []string{
		"step 1 POST /v1/agent/login: status",
		"step 1 POST /v1/agent/login: header content-type",
		"step 1 POST /v1/agent/login: header www-authenticate",
		"step 1 POST /v1/agent/login: body: keys",
		"step 1 POST /v1/agent/login: body.error: keys",
		"step 1 POST /v1/agent/login: body.error.code",
		"step 1 POST /v1/agent/login: body.list: length",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing difference %q in:\n%s", want, got)
		}
	}
	if s := rep.String(); !strings.Contains(s, "node: 400") || !strings.Contains(s, "go:   500") {
		t.Errorf("report does not show both sides:\n%s", s)
	}
}

func TestCompareTextAndKindMismatch(t *testing.T) {
	node := oneStep(404, nil, Body{Text: "Cannot GET /x"})
	goRes := oneStep(404, nil, jsonBody(t, `{"error":"nope"}`))
	rep, _ := Compare(node, goRes, nil)
	if len(rep.Differences) != 1 || !strings.Contains(rep.Differences[0].Node, "text") {
		t.Fatalf("want one text-vs-JSON difference, got:\n%s", rep)
	}
}

func TestKnownDifferences(t *testing.T) {
	node := oneStep(400, nil, jsonBody(t, `{"error":{"code":"BAD_REQUEST","message":"Unexpected token"}}`))
	goRes := oneStep(400, nil, jsonBody(t, `{"error":{"code":"BAD_REQUEST","message":"invalid character"}}`))

	rep, err := Compare(node, goRes, []KnownDifference{{
		Where:  "step 1 POST /v1/agent/login: body.error.message",
		Reason: "malformed-JSON text is Go's, not V8's",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() || len(rep.Allowed) != 1 {
		t.Fatalf("want the message allowed, got:\n%s", rep)
	}

	rep, _ = Compare(node, goRes, []KnownDifference{{Where: "step 1 POST /v1/agent/login: body.error.*", Reason: "prefix"}})
	if !rep.OK() {
		t.Fatalf("prefix entry did not match:\n%s", rep)
	}

	rep, _ = Compare(node, node, []KnownDifference{{Where: "step 1 POST /v1/agent/login: body.error.message", Reason: "stale"}})
	if rep.OK() || !strings.Contains(rep.Differences[0].Where, "known difference") {
		t.Fatalf("a known difference that matches nothing must fail:\n%s", rep)
	}

	if _, err := Compare(node, goRes, []KnownDifference{{Where: "x"}}); err == nil {
		t.Fatal("a known difference without a reason was accepted")
	}
	if _, err := Compare(node, goRes, []KnownDifference{{Where: "*", Reason: "everything"}}); err == nil {
		t.Fatal("a known difference that allows everything was accepted")
	}
}

func TestCompareOutboundAndTables(t *testing.T) {
	ob := func(path, body string) Outbound {
		return Outbound{Upstream: "openai", Method: "POST", Path: path, Auth: []string{"authorization"}, Body: jsonBody(t, body)}
	}
	row := func(s string) Value {
		v, _ := ParseJSON([]byte(s))
		return v
	}
	node := Result{
		Outbound: []Outbound{ob("/v1/embeddings", `{"input":"a"}`)},
		Tables: []Table{
			{Name: "actors", Rows: []Value{row(`{"id":1,"name":"system"}`), row(`{"id":2,"name":"agent-a"}`)}},
			{Name: "mail", Rows: nil},
		},
	}
	goRes := Result{
		Outbound: []Outbound{{Upstream: "openai", Method: "POST", Path: "/v1/embeddings", Body: jsonBody(t, `{"input":"b"}`)}},
		Tables: []Table{
			{Name: "actors", Rows: []Value{row(`{"id":1,"name":"system"}`)}},
			{Name: "chat", Rows: nil},
		},
	}
	rep, _ := Compare(node, goRes, nil)
	got := strings.Join(wheres(rep), "\n")
	for _, want := range []string{
		"outbound 1 openai POST /v1/embeddings: auth headers",
		"outbound 1 openai POST /v1/embeddings: body.input",
		"table actors: row count",
		"table actors row 2",
		"table mail",
		"table chat",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing difference %q in:\n%s", want, got)
		}
	}
}

// A duplicate key's earlier value is compared, and an empty key counts.
func TestCompareDuplicateAndEmptyKeys(t *testing.T) {
	cases := []struct{ node, goBody, where string }{
		{`{"x":1,"x":2}`, `{"x":9,"x":2}`, "step 1 POST /v1/agent/login: body.x#1"},
		{`{}`, `{"":1}`, "step 1 POST /v1/agent/login: body: keys"},
		{`{"a,b":1}`, `{"a":1,"b":1}`, "step 1 POST /v1/agent/login: body: keys"},
	}
	for _, c := range cases {
		rep, _ := Compare(oneStep(200, nil, jsonBody(t, c.node)), oneStep(200, nil, jsonBody(t, c.goBody)), nil)
		if !strings.Contains(strings.Join(wheres(rep), "\n"), c.where) {
			t.Errorf("%s vs %s: want %q, got:\n%s", c.node, c.goBody, c.where, rep)
		}
	}
}

func TestVolatileKnownDifferenceMayMatchNothing(t *testing.T) {
	res := Result{Steps: []StepResult{{Label: "GET /x", Response: Response{Status: 200}}}}
	rep, err := Compare(res, res, []KnownDifference{{Where: "table request_log row *", Reason: "random length", Volatile: true}})
	if err != nil || !rep.OK() {
		t.Fatalf("%v %s", err, rep)
	}
	rep, _ = Compare(res, res, []KnownDifference{{Where: "table request_log row *", Reason: "stale"}})
	if rep.OK() {
		t.Fatal("a stale non-volatile entry passed")
	}
}

// A "*" inside a known difference matches any run of characters, so an
// entry can allow one field of every row without allowing the rows.
func TestKnownDifferenceGlob(t *testing.T) {
	k := KnownDifference{Where: "table request_log row *.response_length"}
	for where, want := range map[string]bool{
		"table request_log row 4.response_length":  true,
		"table request_log row 12.response_length": true,
		"table request_log row 4.status":           false,
		"table request_log: row count":             false,
		"table request_log row 4.response_length2": false,
	} {
		if got := k.matches(where); got != want {
			t.Errorf("%s: %v", where, got)
		}
	}
	prefix := KnownDifference{Where: "table chat_message*"}
	if !prefix.matches("table chat_message_texts row 1.body") || prefix.matches("table mail row 1.body") {
		t.Error("trailing star is no longer a prefix")
	}
	if (KnownDifference{Where: "a*a"}).matches("a") {
		t.Error("overlapping parts matched")
	}
}
