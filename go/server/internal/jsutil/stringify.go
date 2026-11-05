package jsutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Stringify is JSON.stringify(JSON.parse(raw)): compact; object keys in
// JavaScript's property order (array-index keys "0" to "4294967294" first,
// ascending, then the rest in first-seen order), with the last value of a
// repeated key; numbers as JavaScript writes them (a number past the float64
// range is Infinity, which JSON.stringify writes as null); strings escaped as
// JSON.stringify escapes them (only ", \ and control characters; not <, >,
// & or U+2028). Anything after the one JSON value is an error, as in
// JSON.parse.
//
// The input is JSON Go's decoder reads: a lone surrogate escape becomes
// U+FFFD, where JSON.parse would keep it. The callers pass jsonb text, which
// cannot hold one, and request values, which the body parser has already
// decoded the same way.
func Stringify(raw []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := parseValue(dec)
	if err != nil {
		return "", err
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", fmt.Errorf("unexpected data after the JSON value")
	}
	return Marshal(v), nil
}

func parseValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := NewObject()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("object key is not a string")
				}
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				o.Set(k, v)
			}
			_, err := dec.Token()
			return o, err
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			_, err := dec.Token()
			return arr, err
		}
		return nil, fmt.Errorf("unexpected %v", t)
	default:
		return t, nil
	}
}

func writeValue(b *strings.Builder, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case json.Number:
		writeNumber(b, jsonNumber(string(x)))
	case float64:
		writeNumber(b, x)
	case int:
		writeNumber(b, float64(x))
	case string:
		writeString(b, x)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			// JSON.stringify writes undefined in an array as null.
			if e == Undefined {
				e = nil
			}
			writeValue(b, e)
		}
		b.WriteByte(']')
	case *Object:
		b.WriteByte('{')
		n := 0
		for _, k := range x.Keys() {
			v := x.values[k]
			if v == Undefined {
				continue
			}
			if n > 0 {
				b.WriteByte(',')
			}
			n++
			writeString(b, k)
			b.WriteByte(':')
			writeValue(b, v)
		}
		b.WriteByte('}')
	}
}

// writeNumber writes a number as JSON.stringify does; NaN and the
// infinities are null.
func writeNumber(b *strings.Builder, x float64) {
	s := NumberString(x)
	if s == "Infinity" || s == "-Infinity" || s == "NaN" {
		s = "null"
	}
	b.WriteString(s)
}

func writeString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// propertyOrder is the order JavaScript enumerates an object's own string
// keys: array indices ascending, then the others as inserted.
func propertyOrder(keys []string) []string {
	var indices, others []string
	for _, k := range keys {
		if isArrayIndex(k) {
			indices = append(indices, k)
		} else {
			others = append(others, k)
		}
	}
	sort.SliceStable(indices, func(i, j int) bool {
		a, _ := strconv.ParseUint(indices[i], 10, 64)
		b, _ := strconv.ParseUint(indices[j], 10, 64)
		return a < b
	})
	return append(indices, others...)
}

// isArrayIndex reports whether k is the canonical decimal form of an
// integer from 0 to 2^32 - 2 ("01" and "4294967295" are not).
func isArrayIndex(k string) bool {
	if k == "" || len(k) > 10 || (len(k) > 1 && k[0] == '0') {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < '0' || k[i] > '9' {
			return false
		}
	}
	n, err := strconv.ParseUint(k, 10, 64)
	return err == nil && n <= 4294967294
}

// HasLoneSurrogateEscape reports whether a JSON text holds a \u escape for
// a UTF-16 surrogate that is not part of a pair. JSON.parse keeps such a
// value and JSON.stringify writes it back as the escape, which Postgres
// refuses for jsonb; Go's decoder would turn it into U+FFFD instead. A
// caller that stores the result in jsonb checks this first, so it fails as
// Node did rather than storing a different value.
func HasLoneSurrogateEscape(raw []byte) bool {
	inString := false
	pendingHigh := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			continue
		}
		if c == '"' {
			if pendingHigh {
				return true
			}
			inString = false
			continue
		}
		if c != '\\' {
			if pendingHigh {
				return true
			}
			continue
		}
		if i+1 >= len(raw) {
			return false
		}
		i++
		if raw[i] != 'u' {
			if pendingHigh {
				return true
			}
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		unit, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		i += 4
		if err != nil {
			return false
		}
		switch {
		case unit >= 0xD800 && unit <= 0xDBFF:
			if pendingHigh {
				return true
			}
			pendingHigh = true
		case unit >= 0xDC00 && unit <= 0xDFFF:
			if !pendingHigh {
				return true
			}
			pendingHigh = false
		default:
			if pendingHigh {
				return true
			}
		}
	}
	return pendingHigh
}
