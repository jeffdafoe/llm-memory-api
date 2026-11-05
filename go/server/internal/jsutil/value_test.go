package jsutil

import (
	"math"
	"testing"
)

func TestObjectOrderAndMarshal(t *testing.T) {
	o := NewObject()
	o.Set("b", 1.0)
	o.Set("a", "x")
	o.Set("2", true)
	o.Set("gone", Undefined)
	o.Set("b", 2.0)
	o.Set("arr", []any{Undefined, nil, math.NaN(), -0.0, 1e21})
	if got := Marshal(o); got != `{"2":true,"b":2,"a":"x","arr":[null,null,null,0,1e+21]}` {
		t.Errorf("marshal: %s", got)
	}
	if !o.Has("gone") || o.Value("missing") != Undefined {
		t.Error("Has / Value")
	}
	o.Delete("b")
	o.Set("b", 3.0)
	c := o.Clone()
	c.Set("a", "y")
	if Marshal(o) != `{"2":true,"a":"x","arr":[null,null,null,0,1e+21],"b":3}` || Marshal(c) != `{"2":true,"a":"y","arr":[null,null,null,0,1e+21],"b":3}` {
		t.Errorf("delete / clone: %s %s", Marshal(o), Marshal(c))
	}
	if Marshal(Undefined) != "" {
		t.Error("JSON.stringify(undefined) has no text")
	}
}

func TestParseKeepsJavaScriptRules(t *testing.T) {
	v, err := Parse([]byte(`{"x":1,"10":2,"1":3,"x":4,"s":"a b"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := Marshal(v); got != "{\"1\":3,\"10\":2,\"x\":4,\"s\":\"a b\"}" {
		t.Errorf("round trip: %s", got)
	}
	if _, err := Parse([]byte(`{} x`)); err == nil {
		t.Error("trailing data must fail")
	}
}

func TestValueConversions(t *testing.T) {
	withToString, _ := Parse([]byte(`{"toString":1}`))
	if _, err := ValueString(withToString); err != ErrNoPrimitive {
		t.Errorf("String({toString:1}): %v", err)
	}
	if _, err := ValueNumber([]any{withToString}); err != ErrNoPrimitive {
		t.Errorf("Number([{toString:1}]): %v", err)
	}
	cases := []struct {
		in  any
		str string
		num float64
	}{
		{nil, "null", 0},
		{Undefined, "undefined", math.NaN()},
		{true, "true", 1},
		{" 12 ", " 12 ", 12},
		{[]any{1.0, nil, "a"}, "1,,a", math.NaN()},
		{[]any{"7"}, "7", 7},
		{NewObject(), "[object Object]", math.NaN()},
		{-0.0, "0", 0},
	}
	for _, c := range cases {
		s, _ := ValueString(c.in)
		n, _ := ValueNumber(c.in)
		if s != c.str || !(n == c.num || math.IsNaN(n) && math.IsNaN(c.num)) {
			t.Errorf("%v: String %q Number %v", c.in, s, n)
		}
	}
	for v, want := range map[any]bool{nil: false, Undefined: false, "": false, "0": true, 0.0: false, math.NaN(): false, false: false} {
		if Truthy(v) != want {
			t.Errorf("Truthy(%v)", v)
		}
	}
	if !Truthy([]any{}) || !Truthy(NewObject()) {
		t.Error("objects and arrays are truthy")
	}
}
