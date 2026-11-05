package contract

import "testing"

func TestParseJSONKeepsOrderNumbersAndDuplicates(t *testing.T) {
	v, err := ParseJSON([]byte(` {"b":1.50,"a":[true,null,"x"],"b":2e3} `))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.Compact(), `{"b":1.50,"a":[true,null,"x"],"b":2e3}`; got != want {
		t.Fatalf("Compact = %s, want %s", got, want)
	}
	if got, _ := v.Get("b"); got.Num != "2e3" {
		t.Fatalf("Get keeps the last duplicate as JSON.parse does, got %q", got.Num)
	}
}

func TestParseJSONRejectsTrailingData(t *testing.T) {
	for _, in := range []string{`{} {}`, `[1] x`, ``, `{"a":}`} {
		if _, err := ParseJSON([]byte(in)); err == nil {
			t.Errorf("ParseJSON(%q) accepted", in)
		}
	}
}

func TestPathAndText(t *testing.T) {
	v, err := ParseJSON([]byte(`{"data":[{"id":7,"token":"abc"}],"n":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := v.Path("data.0.token"); !ok || got.Text() != "abc" {
		t.Fatalf("data.0.token = %v %v", got, ok)
	}
	if got, ok := v.Path("data.0.id"); !ok || got.Text() != "7" {
		t.Fatalf("data.0.id = %v %v", got, ok)
	}
	for _, p := range []string{"data.1", "data.x", "missing", "data.0.id.deeper"} {
		if _, ok := v.Path(p); ok {
			t.Errorf("Path(%q) found something", p)
		}
	}
}

func TestEqualIsOrderSensitive(t *testing.T) {
	a, _ := ParseJSON([]byte(`{"a":1,"b":2}`))
	b, _ := ParseJSON([]byte(`{"b":2,"a":1}`))
	c, _ := ParseJSON([]byte(`{"a":1.0,"b":2}`))
	if Equal(a, b) {
		t.Error("different key order compared equal")
	}
	if Equal(a, c) {
		t.Error("1 and 1.0 compared equal; number text is part of the contract")
	}
	if !Equal(a, a) {
		t.Error("value not equal to itself")
	}
}

func TestIndent(t *testing.T) {
	v, _ := ParseJSON([]byte(`{"a":[1,{}],"b":[]}`))
	want := "{\n  \"a\": [\n    1,\n    {}\n  ],\n  \"b\": []\n}"
	if got := v.Indent(); got != want {
		t.Fatalf("Indent =\n%s\nwant\n%s", got, want)
	}
}
