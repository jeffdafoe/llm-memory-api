package jsutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// JavaScript values for code that passes JSON through as Node did: Parse
// returns nil (null), bool, json.Number, string, []any or *Object, and
// Marshal writes any of those, plus float64, int and Undefined, as
// JSON.stringify does.

type undefinedType struct{}

// Undefined is JavaScript's undefined. An object member set to it is left
// out by Marshal; an array element set to it is written as null.
var Undefined any = undefinedType{}

// Object is a JavaScript object with string keys. It keeps insertion order;
// Keys gives JavaScript's enumeration order (array-index keys first).
type Object struct {
	keys   []string
	values map[string]any
}

func NewObject() *Object {
	return &Object{values: map[string]any{}}
}

// Get returns the member and whether the object has it.
func (o *Object) Get(k string) (any, bool) {
	v, ok := o.values[k]
	return v, ok
}

// Value is o[k]: Undefined when the object has no such member.
func (o *Object) Value(k string) any {
	v, ok := o.values[k]
	if !ok {
		return Undefined
	}
	return v
}

// Has is `k in o` for own members.
func (o *Object) Has(k string) bool {
	_, ok := o.values[k]
	return ok
}

// Set is o[k] = v: a new key goes last, an existing one keeps its place.
func (o *Object) Set(k string, v any) {
	if _, seen := o.values[k]; !seen {
		o.keys = append(o.keys, k)
	}
	o.values[k] = v
}

// Delete is `delete o[k]`.
func (o *Object) Delete(k string) {
	if _, ok := o.values[k]; !ok {
		return
	}
	delete(o.values, k)
	for i, key := range o.keys {
		if key == k {
			o.keys = append(o.keys[:i:i], o.keys[i+1:]...)
			break
		}
	}
}

// Keys is Object.keys(o).
func (o *Object) Keys() []string {
	return propertyOrder(o.keys)
}

func (o *Object) Len() int {
	return len(o.keys)
}

// Clone is {...o}: a shallow copy.
func (o *Object) Clone() *Object {
	c := NewObject()
	for _, k := range o.Keys() {
		c.Set(k, o.values[k])
	}
	return c
}

// Parse is JSON.parse(raw). It reads what Go's decoder reads; a lone
// surrogate escape becomes U+FFFD (see Stringify).
func Parse(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected data after the JSON value")
	}
	return v, nil
}

// Marshal is JSON.stringify(v) for a value from Parse or built from the
// types listed above. JSON.stringify(undefined) has no text; Marshal gives "".
func Marshal(v any) string {
	if v == Undefined {
		return ""
	}
	var b strings.Builder
	writeValue(&b, v)
	return b.String()
}

// Truthy is JavaScript truthiness.
func Truthy(v any) bool {
	switch x := v.(type) {
	case nil, undefinedType:
		return false
	case bool:
		return x
	case json.Number:
		f := jsonNumber(string(x))
		return f != 0 && !math.IsNaN(f)
	case float64:
		return x != 0 && !math.IsNaN(x)
	case int:
		return x != 0
	case string:
		return x != ""
	}
	return true
}

// AsFloat returns a number value as a float64.
func AsFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		return jsonNumber(string(x)), true
	case float64:
		return x, true
	case int:
		return float64(x), true
	}
	return 0, false
}

// primitiveOK reports whether v can become a primitive. A JSON object with
// its own "toString" member has no callable toString, and its valueOf
// returns the object itself, so V8 throws; an array becomes a primitive by
// joining its elements.
func primitiveOK(v any) bool {
	switch x := v.(type) {
	case *Object:
		return !x.Has("toString")
	case []any:
		for _, e := range x {
			if !primitiveOK(e) {
				return false
			}
		}
	}
	return true
}

// ValueNumber is Number(v), with V8's TypeError for a value that cannot
// become a primitive.
func ValueNumber(v any) (float64, error) {
	if !primitiveOK(v) {
		return math.NaN(), ErrNoPrimitive
	}
	switch x := v.(type) {
	case nil:
		return 0, nil
	case undefinedType:
		return math.NaN(), nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case json.Number, float64, int:
		f, _ := AsFloat(x)
		return f, nil
	case string:
		return StringToNumber(x), nil
	case []any:
		return StringToNumber(valueJoin(x)), nil
	}
	return math.NaN(), nil
}

// ValueString is String(v), with V8's TypeError for a value that cannot
// become a primitive.
func ValueString(v any) (string, error) {
	if !primitiveOK(v) {
		return "", ErrNoPrimitive
	}
	return valueString(v), nil
}

func valueString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case undefinedType:
		return "undefined"
	case bool:
		return strconv.FormatBool(x)
	case json.Number, float64, int:
		f, _ := AsFloat(x)
		return NumberString(f)
	case string:
		return x
	case []any:
		return valueJoin(x)
	}
	return "[object Object]"
}

// valueJoin is String(array): elements joined by ",", null and undefined
// as "".
func valueJoin(a []any) string {
	sep := ","
	parts := make([]string, len(a))
	for i, e := range a {
		if e == nil || e == Undefined {
			continue
		}
		parts[i] = valueString(e)
	}
	return strings.Join(parts, sep)
}

// ParseFloat is parseFloat(s): leading whitespace, then the longest prefix
// that is a decimal literal or Infinity. ok is false where it gives NaN.
func ParseFloat(s string) (float64, bool) {
	s = TrimStart(s)
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	if strings.HasPrefix(s[i:], "Infinity") {
		if s[0] == '-' {
			return math.Inf(-1), true
		}
		return math.Inf(1), true
	}
	start := i
	digits := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
		digits++
	}
	if i < len(s) && s[i] == '.' {
		j := i + 1
		frac := 0
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
			frac++
		}
		if digits+frac > 0 {
			i = j
			digits += frac
		}
	}
	if digits == 0 {
		return math.NaN(), false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		exp := 0
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
			exp++
		}
		if exp > 0 {
			i = j
		}
	}
	text := s[start:i]
	if strings.HasPrefix(text, ".") {
		text = "0" + text
	}
	text = strings.Replace(text, ".e", ".0e", 1)
	text = strings.Replace(text, ".E", ".0E", 1)
	if strings.HasSuffix(text, ".") {
		text += "0"
	}
	f := jsonNumber(text)
	if s[0] == '-' {
		f = -f
	}
	return f, true
}
