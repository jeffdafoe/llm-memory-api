// Package jsonutil encodes JSON the way the Node server's JSON.stringify did.
// It is a leaf package so both httpapi and logger can use it.
package jsonutil

import (
	"bytes"
	"encoding/json"
)

// Marshal encodes v with no HTML escaping of <, > and &, and no trailing
// newline, matching JSON.stringify and Express's res.json. Go's encoder still
// escapes U+2028 and U+2029, which JSON.stringify leaves raw; both forms
// decode to the same string.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
