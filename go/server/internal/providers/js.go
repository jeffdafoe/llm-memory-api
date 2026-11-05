package providers

import (
	"errors"
	"fmt"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// Helpers that read caller and provider JSON with JavaScript's rules, so a
// value of an unexpected type takes the same path it took in Node.

var undefined = jsutil.Undefined

type object = jsutil.Object

// readErr is V8's TypeError for reading a property of null or undefined.
func readErr(base any, key string) error {
	what := "undefined"
	if base == nil {
		what = "null"
	}
	return fmt.Errorf("Cannot read properties of %s (reading '%s')", what, key)
}

// get is base[key]. Primitives other than strings have no members this
// package reads; a string's only one is length.
func get(base any, key string) (any, error) {
	switch x := base.(type) {
	case nil:
		return nil, readErr(nil, key)
	case *object:
		return x.Value(key), nil
	case []any:
		if key == "length" {
			return float64(len(x)), nil
		}
	case string:
		if key == "length" {
			return float64(jsutil.UTF16Len(x)), nil
		}
	}
	if base == undefined {
		return nil, readErr(undefined, key)
	}
	return undefined, nil
}

// optional is base?.[key].
func optional(base any, key string) any {
	if base == nil || base == undefined {
		return undefined
	}
	v, _ := get(base, key)
	return v
}

// member is base[key] when base is known to be an object or undefined.
func member(base any, key string) any {
	if o, ok := base.(*object); ok {
		return o.Value(key)
	}
	return undefined
}

func truthy(v any) bool {
	return jsutil.Truthy(v)
}

// isString is typeof v === 'string'.
func isString(v any) bool {
	_, ok := v.(string)
	return ok
}

// isStr is v === s.
func isStr(v any, s string) bool {
	x, ok := v.(string)
	return ok && x == s
}

// isNullish is v == null.
func isNullish(v any) bool {
	return v == nil || v == undefined
}

// typeofObject is typeof v === 'object' && v: an object or an array.
func typeofObject(v any) bool {
	switch v.(type) {
	case *object, []any:
		return true
	}
	return false
}

// or is a || b.
func or(a, b any) any {
	if truthy(a) {
		return a
	}
	return b
}

// coalesce is a ?? b.
func coalesce(a, b any) any {
	if isNullish(a) {
		return b
	}
	return a
}

// stringifyValue is JSON.stringify(v); undefined for undefined.
func stringifyValue(v any) any {
	if v == undefined {
		return undefined
	}
	return jsutil.Marshal(v)
}

// toNumber is Number(v) where the value came from a provider response and
// is used in arithmetic.
func toNumber(v any) float64 {
	f, _ := jsutil.ValueNumber(v)
	return f
}

// errJSONParse marks a JSON.parse failure, which callers catch.
var errJSONParse = errors.New("JSON.parse failed")

// parseJSON is JSON.parse(String(s)) for a value Node passed to JSON.parse.
func parseJSON(v any) (any, error) {
	s, err := jsutil.ValueString(v)
	if err != nil {
		return nil, err
	}
	out, err := jsutil.Parse([]byte(s))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errJSONParse, err)
	}
	return out, nil
}

// arr returns v as an array when Array.isArray(v).
func arr(v any) ([]any, bool) {
	a, ok := v.([]any)
	return a, ok
}

// obj builds an object from key/value pairs in order.
func obj(kv ...any) *object {
	o := jsutil.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

// minNum and maxNum are Math.min and Math.max of two numbers (NaN wins).
func minNum(a, b float64) float64 {
	if a != a || b != b {
		return a + b
	}
	if a < b {
		return a
	}
	return b
}

func maxNum(a, b float64) float64 {
	if a != a || b != b {
		return a + b
	}
	if a > b {
		return a
	}
	return b
}

// notFunction is V8's TypeError for calling a value that is not a function;
// expr is the callee as V8 prints it.
func notFunction(expr string) error {
	return fmt.Errorf("%s is not a function", expr)
}

// join is a.join(sep): null and undefined are "", the rest String(v).
func join(a []any, sep string) (string, error) {
	out := ""
	for i, e := range a {
		if i > 0 {
			out += sep
		}
		if e == nil || e == undefined {
			continue
		}
		s, err := jsutil.ValueString(e)
		if err != nil {
			return "", err
		}
		out += s
	}
	return out, nil
}

// index0 is v[0].
func index0(v any) any {
	switch x := v.(type) {
	case []any:
		if len(x) > 0 {
			return x[0]
		}
	case *object:
		return x.Value("0")
	case string:
		if units := utf16Units(x); len(units) > 0 {
			return units[0]
		}
	}
	return undefined
}

// sameValueKey is a comparable key under SameValueZero for the primitive
// values JSON can hold; ok is false for objects and arrays, which never
// equal a value from another part of a parsed document.
func sameValueKey(v any) (any, bool) {
	switch x := v.(type) {
	case nil:
		return "null", true
	case bool:
		return x, true
	case string:
		return "s" + x, true
	}
	if f, ok := jsutil.AsFloat(v); ok {
		if f == 0 {
			f = 0
		}
		return f, true
	}
	return nil, false
}

// valueSet is a JavaScript Set of primitive values.
type valueSet map[any]bool

func (s valueSet) add(v any) {
	if k, ok := sameValueKey(v); ok {
		s[k] = true
	}
}

func (s valueSet) has(v any) bool {
	k, ok := sameValueKey(v)
	return ok && s[k]
}
