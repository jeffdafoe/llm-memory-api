// Package contract is the LLM-909 contract harness: it sends the same
// requests to the Node server and the Go server, each against its own copy of
// one database, and compares the responses, the rows written and the requests
// each server sent to the LLM providers. Node's result is the expected value.
//
// The files without a build tag (values, normalizer, diff, fake upstream,
// runner) need no database and no Node, so their unit tests run in the
// go-server CI job. The files that start Postgres databases and the two
// servers carry the "contract" build tag and run only in the contract job.
package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
)

// Kind is the JSON type of a Value.
type Kind int

const (
	Null Kind = iota
	Bool
	Number
	String
	Array
	Object
)

// Value is a decoded JSON value that keeps what encoding/json drops: object
// key order (JSON.stringify order is part of the contract), duplicate keys,
// and the exact text of each number.
type Value struct {
	Kind    Kind
	Bool    bool
	Num     string // the number exactly as written
	Str     string
	Items   []Value
	Members []Member
}

// Member is one key of an Object, in document order.
type Member struct {
	Key   string
	Value Value
}

// ParseJSON decodes exactly one JSON value. Trailing data other than
// whitespace is an error.
func ParseJSON(data []byte) (Value, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return Value{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return Value{}, errors.New("trailing data after JSON value")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (Value, error) {
	tok, err := dec.Token()
	if err != nil {
		return Value{}, err
	}
	switch t := tok.(type) {
	case nil:
		return Value{Kind: Null}, nil
	case bool:
		return Value{Kind: Bool, Bool: t}, nil
	case json.Number:
		return Value{Kind: Number, Num: t.String()}, nil
	case string:
		return Value{Kind: String, Str: t}, nil
	case json.Delim:
		switch t {
		case '[':
			v := Value{Kind: Array, Items: []Value{}}
			for dec.More() {
				item, err := decodeValue(dec)
				if err != nil {
					return Value{}, err
				}
				v.Items = append(v.Items, item)
			}
			if _, err := dec.Token(); err != nil {
				return Value{}, err
			}
			return v, nil
		case '{':
			v := Value{Kind: Object, Members: []Member{}}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return Value{}, err
				}
				key, ok := keyTok.(string)
				if !ok {
					return Value{}, fmt.Errorf("object key is %T", keyTok)
				}
				item, err := decodeValue(dec)
				if err != nil {
					return Value{}, err
				}
				v.Members = append(v.Members, Member{Key: key, Value: item})
			}
			if _, err := dec.Token(); err != nil {
				return Value{}, err
			}
			return v, nil
		}
	}
	return Value{}, fmt.Errorf("unexpected JSON token %v", tok)
}

// Str returns a String value.
func Str(s string) Value {
	return Value{Kind: String, Str: s}
}

// Get returns the member named key of an Object (the last one, as JSON.parse
// keeps it) and whether it was found.
func (v Value) Get(key string) (Value, bool) {
	if v.Kind != Object {
		return Value{}, false
	}
	for i := len(v.Members) - 1; i >= 0; i-- {
		if v.Members[i].Key == key {
			return v.Members[i].Value, true
		}
	}
	return Value{}, false
}

// Path follows a dot-separated path of object keys and array indexes
// ("session.token", "data.0.embedding").
func (v Value) Path(path string) (Value, bool) {
	cur := v
	if path == "" {
		return cur, true
	}
	for _, part := range strings.Split(path, ".") {
		switch cur.Kind {
		case Object:
			next, ok := cur.Get(part)
			if !ok {
				return Value{}, false
			}
			cur = next
		case Array:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(cur.Items) {
				return Value{}, false
			}
			cur = cur.Items[i]
		default:
			return Value{}, false
		}
	}
	return cur, true
}

// Text is the plain string form used when a captured value goes into a later
// request: a string's contents, any other value's compact JSON.
func (v Value) Text() string {
	if v.Kind == String {
		return v.Str
	}
	return v.Compact()
}

// Compact is the value as compact JSON, keys in document order.
func (v Value) Compact() string {
	var b strings.Builder
	v.write(&b, "", "")
	return b.String()
}

// Indent is the value as indented JSON, for diff output.
func (v Value) Indent() string {
	var b strings.Builder
	v.write(&b, "", "  ")
	return b.String()
}

func (v Value) write(b *strings.Builder, prefix, indent string) {
	switch v.Kind {
	case Null:
		b.WriteString("null")
	case Bool:
		b.WriteString(strconv.FormatBool(v.Bool))
	case Number:
		b.WriteString(v.Num)
	case String:
		writeString(b, v.Str)
	case Array:
		if len(v.Items) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteByte('[')
		for i, item := range v.Items {
			if i > 0 {
				b.WriteByte(',')
			}
			newline(b, prefix+indent, indent)
			item.write(b, prefix+indent, indent)
		}
		newline(b, prefix, indent)
		b.WriteByte(']')
	case Object:
		if len(v.Members) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteByte('{')
		for i, m := range v.Members {
			if i > 0 {
				b.WriteByte(',')
			}
			newline(b, prefix+indent, indent)
			writeString(b, m.Key)
			b.WriteByte(':')
			if indent != "" {
				b.WriteByte(' ')
			}
			m.Value.write(b, prefix+indent, indent)
		}
		newline(b, prefix, indent)
		b.WriteByte('}')
	}
}

func newline(b *strings.Builder, prefix, indent string) {
	if indent == "" {
		return
	}
	b.WriteByte('\n')
	b.WriteString(prefix)
}

func writeString(b *strings.Builder, s string) {
	enc, _ := jsonutil.Marshal(s)
	b.Write(enc)
}

// Equal reports whether two values are the same, key order and number text
// included.
func Equal(a, b Value) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case Null:
		return true
	case Bool:
		return a.Bool == b.Bool
	case Number:
		return a.Num == b.Num
	case String:
		return a.Str == b.Str
	case Array:
		if len(a.Items) != len(b.Items) {
			return false
		}
		for i := range a.Items {
			if !Equal(a.Items[i], b.Items[i]) {
				return false
			}
		}
		return true
	case Object:
		if len(a.Members) != len(b.Members) {
			return false
		}
		for i := range a.Members {
			if a.Members[i].Key != b.Members[i].Key || !Equal(a.Members[i].Value, b.Members[i].Value) {
				return false
			}
		}
		return true
	}
	return false
}
