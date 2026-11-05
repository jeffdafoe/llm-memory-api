// Package pgbind turns request-body JSON values into query parameters the
// way node-postgres 8 prepared them (prepareValue in pg/lib/utils.js), so a
// value Postgres refuses is refused with the same message on both servers.
//
// Every parameter here is a Go string or a jsutil.Number. pgx sends a string
// argument in text format whatever the parameter's type, as node-postgres
// sent every value, so Postgres does the parsing and the error text is its
// own.
package pgbind

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// Truthy is JavaScript truthiness for a decoded JSON value: absent, null,
// false, 0 and "" are falsy; arrays and objects are truthy.
func Truthy(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	switch {
	case len(raw) == 0, string(raw) == "null", string(raw) == "false", string(raw) == `""`:
		return false
	case raw[0] == '-' || raw[0] >= '0' && raw[0] <= '9':
		return jsutil.ToNumber(raw) != 0
	}
	return true
}

// Value is the parameter node-postgres bound for a JSON value: nil for
// null or absent, a number as its JS text, a string as is, true / false, an
// array as a Postgres array literal, an object as JSON.stringify writes it.
func Value(raw json.RawMessage) any {
	raw = bytes.TrimSpace(raw)
	switch {
	case len(raw) == 0, string(raw) == "null":
		return nil
	case raw[0] == '"':
		var s string
		json.Unmarshal(raw, &s)
		return s
	case raw[0] == '-' || raw[0] >= '0' && raw[0] <= '9':
		return jsutil.Num(jsutil.ToNumber(raw))
	case raw[0] == '[':
		var elems []json.RawMessage
		json.Unmarshal(raw, &elems)
		return ArrayLiteral(elems)
	}
	str, _ := jsutil.Stringify(raw)
	return str
}

// OrDefault is `value || fallback` bound as node-postgres bound it.
func OrDefault(raw json.RawMessage, fallback int) any {
	if Truthy(raw) {
		return Value(raw)
	}
	return fallback
}

// ArrayLiteral is node-postgres's arrayString: `{` elements `,` `}`, where
// null is NULL, a nested array is a nested literal, and anything else is
// its prepared text in double quotes with `\` and `"` escaped.
func ArrayLiteral(elems []json.RawMessage) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, e := range elems {
		if i > 0 {
			b.WriteByte(',')
		}
		e = bytes.TrimSpace(e)
		switch {
		case len(e) == 0, string(e) == "null":
			b.WriteString("NULL")
		case e[0] == '[':
			var inner []json.RawMessage
			json.Unmarshal(e, &inner)
			b.WriteString(ArrayLiteral(inner))
		default:
			var text string
			switch v := Value(e).(type) {
			case string:
				text = v
			case jsutil.Number:
				text = string(v)
			}
			b.WriteByte('"')
			b.WriteString(strings.ReplaceAll(strings.ReplaceAll(text, `\`, `\\`), `"`, `\"`))
			b.WriteByte('"')
		}
	}
	b.WriteByte('}')
	return b.String()
}
