package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
)

// Fields are the top-level members of a JSON request body, as a route reads
// req.body.<name> in Express.
type Fields map[string]json.RawMessage

// BodyFields returns the request's body fields. ok is false when there is no
// parsed JSON body: in Express 5 req.body is then undefined, and a route that
// reads a field from it throws a TypeError (a 500). A JSON array or scalar
// body has no named fields, so every field reads as undefined.
func BodyFields(r *http.Request) (f Fields, ok bool) {
	body := StateOf(r).Body
	if body == nil {
		return nil, false
	}
	f = Fields{}
	// State.Body keeps any whitespace before the JSON value.
	body = bytes.TrimLeft(body, " \t\n\r")
	if len(body) > 0 && body[0] == '{' {
		// The body parser accepted this JSON, so it decodes; duplicate keys
		// keep the last value, as JSON.parse does.
		json.Unmarshal(body, &f)
	}
	return f, true
}

// Raw is the field's JSON text, nil when the field is absent.
func (f Fields) Raw(name string) json.RawMessage {
	return f[name]
}

// Truthy reports whether the field is truthy in JavaScript: absent, null,
// false, 0, NaN and "" are falsy; arrays and objects are truthy.
func (f Fields) Truthy(name string) bool {
	raw, ok := f[name]
	if !ok || len(raw) == 0 {
		return false
	}
	switch raw[0] {
	case 'n', 'f':
		return false
	case 't', '[', '{':
		return true
	case '"':
		return string(raw) != `""`
	}
	// A number. Past the float64 range ParseFloat gives ±Inf (truthy) with
	// an error, as JSON.parse gives ±Infinity; an underflow gives 0.
	n, _ := strconv.ParseFloat(string(raw), 64)
	return n != 0
}

// String returns the field when it is a JSON string. isString is false for
// an absent field and for any other JSON type.
func (f Fields) String(name string) (s string, isString bool) {
	raw, ok := f[name]
	if !ok || len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// Array returns the field's elements when it is a JSON array.
func (f Fields) Array(name string) ([]json.RawMessage, bool) {
	raw, ok := f[name]
	if !ok || len(raw) == 0 || raw[0] != '[' {
		return nil, false
	}
	var out []json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false
	}
	return out, true
}
