// Package pgjson turns query results into the JSON the Node server sent for
// them: node-postgres 8 rows (text-format results parsed by pg-types) written
// by JSON.stringify. It is for routes that answered with whole rows
// (SELECT np.*, RETURNING *), whose columns come from the table definition.
package pgjson

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// Field is one column of a row: its name and its value as JSON.
type Field struct {
	Name  string
	Value json.RawMessage
}

// Row is one result row as node-postgres built it: a plain object with the
// columns in result order. A repeated column name keeps its first place and
// the last value, as assigning row[name] did.
type Row []Field

// MarshalJSON writes the row as JSON.stringify wrote the object.
func (r Row) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range r {
		if i > 0 {
			b.WriteByte(',')
		}
		name, err := jsonutil.Marshal(f.Name)
		if err != nil {
			return nil, err
		}
		b.Write(name)
		b.WriteByte(':')
		b.Write(f.Value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// Get returns the value of column name.
func (r Row) Get(name string) (json.RawMessage, bool) {
	for _, f := range r {
		if f.Name == name {
			return f.Value, true
		}
	}
	return nil, false
}

// String returns column name when it is a JSON string.
func (r Row) String(name string) (string, bool) {
	v, ok := r.Get(name)
	if !ok {
		return "", false
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return "", false
	}
	return s, true
}

func (r *Row) set(name string, value json.RawMessage) {
	for i := range *r {
		if (*r)[i].Name == name {
			(*r)[i].Value = value
			return
		}
	}
	*r = append(*r, Field{Name: name, Value: value})
}

// Query runs sql and returns every row. The results are asked for in text
// format, which is what node-postgres reads, so each value is parsed from
// the same text pg-types parsed.
func Query(ctx context.Context, conn db.DB, sql string, args ...any) ([]Row, error) {
	rows, err := conn.Query(ctx, sql, append([]any{pgx.QueryResultFormats{pgx.TextFormatCode}}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	fields := rows.FieldDescriptions()
	out := []Row{}
	for rows.Next() {
		raw := rows.RawValues()
		row := make(Row, 0, len(fields))
		for i, fd := range fields {
			var v json.RawMessage
			if raw[i] == nil {
				v = json.RawMessage("null")
			} else {
				v, err = Parse(fd.DataTypeOID, string(raw[i]))
				if err != nil {
					return nil, fmt.Errorf("column %s: %w", fd.Name, err)
				}
			}
			row.set(fd.Name, v)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// QueryOne is Query for a statement that returns at most one row; ok is
// false when it returned none.
func QueryOne(ctx context.Context, conn db.DB, sql string, args ...any) (Row, bool, error) {
	rows, err := Query(ctx, conn, sql, args...)
	if err != nil || len(rows) == 0 {
		return nil, false, err
	}
	return rows[0], true, nil
}

// Type OIDs pg-types parses (the rest stay strings).
const (
	oidBool        = 16
	oidInt8        = 20
	oidInt2        = 21
	oidInt4        = 23
	oidOID         = 26
	oidJSON        = 114
	oidFloat4      = 700
	oidFloat8      = 701
	oidDate        = 1082
	oidTimestamp   = 1114
	oidTimestamptz = 1184
	oidJSONB       = 3802

	oidBoolArray        = 1000
	oidInt2Array        = 1005
	oidInt4Array        = 1007
	oidTextArray        = 1009
	oidVarcharArray     = 1015
	oidInt8Array        = 1016
	oidFloat4Array      = 1021
	oidFloat8Array      = 1022
	oidOIDArray         = 1028
	oidTimestampArray   = 1115
	oidDateArray        = 1182
	oidTimestamptzArray = 1185
	oidNumericArray     = 1231
	oidUUIDArray        = 2951
	oidJSONArray        = 199
	oidJSONBArray       = 3807
	oidBpcharArray      = 1014
)

// Parse is pg-types' text parser for one value of type oid, written as JSON.
func Parse(oid uint32, text string) (json.RawMessage, error) {
	switch oid {
	case oidBool:
		return boolJSON(text), nil
	case oidInt2, oidInt4, oidOID:
		return intJSON(text)
	case oidFloat4, oidFloat8:
		return floatJSON(text)
	case oidJSON, oidJSONB:
		s, err := jsutil.Stringify([]byte(text))
		if err != nil {
			return nil, err
		}
		return json.RawMessage(s), nil
	case oidDate:
		return dateJSON(text)
	case oidTimestamp:
		return timestampJSON(text, time.Local)
	case oidTimestamptz:
		return timestampJSON(text, nil)
	}
	if elem, ok := arrayElem(oid); ok {
		return arrayJSON(text, elem)
	}
	// int8, numeric, text, varchar, uuid, ... stay strings.
	return jsonutil.Marshal(text)
}

func arrayElem(oid uint32) (uint32, bool) {
	switch oid {
	case oidBoolArray:
		return oidBool, true
	case oidInt2Array:
		return oidInt2, true
	case oidInt4Array:
		return oidInt4, true
	case oidOIDArray:
		return oidOID, true
	case oidFloat4Array:
		return oidFloat4, true
	case oidFloat8Array:
		return oidFloat8, true
	case oidTimestampArray:
		return oidTimestamp, true
	case oidTimestamptzArray:
		return oidTimestamptz, true
	case oidDateArray:
		return oidDate, true
	case oidJSONArray:
		return oidJSON, true
	case oidJSONBArray:
		return oidJSONB, true
	case oidNumericArray:
		// pg-types parses numeric[] with parseFloatArray, though a plain
		// numeric stays a string.
		return oidFloat8, true
	case oidTextArray, oidVarcharArray, oidInt8Array, oidUUIDArray, oidBpcharArray:
		// Elements stay strings.
		return 0, true
	}
	return 0, false
}

func boolJSON(text string) json.RawMessage {
	// pg-types: value === 'TRUE' || 't' || 'true' || 'y' || 'yes' || 'on' || '1'.
	switch text {
	case "TRUE", "t", "true", "y", "yes", "on", "1":
		return json.RawMessage("true")
	}
	return json.RawMessage("false")
}

func intJSON(text string) (json.RawMessage, error) {
	n, ok := jsutil.ParseInt(text)
	if !ok {
		return json.RawMessage("null"), nil
	}
	return json.RawMessage(jsutil.NumberString(n)), nil
}

func floatJSON(text string) (json.RawMessage, error) {
	f, ok := jsutil.ParseFloat(text)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return json.RawMessage("null"), nil
	}
	return json.RawMessage(jsutil.NumberString(f)), nil
}

// dateJSON is pg-types' date parser: the local midnight of that day.
func dateJSON(text string) (json.RawMessage, error) {
	if text == "infinity" || text == "-infinity" {
		return json.RawMessage("null"), nil
	}
	t, err := time.Parse("2006-01-02", text)
	if err != nil {
		return jsonutil.Marshal(text)
	}
	return jsonutil.Marshal(jsutil.DateISO(jsutil.LocalDate(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)))
}

// timestampJSON is postgres-date: a timestamptz with its offset, or a
// timestamp read in loc. Infinity is a Date of Infinity, which
// JSON.stringify writes as null.
func timestampJSON(text string, loc *time.Location) (json.RawMessage, error) {
	if text == "infinity" || text == "-infinity" {
		return json.RawMessage("null"), nil
	}
	var t time.Time
	var err error
	if loc != nil {
		// The wall-clock fields, then the local Date V8 makes of them.
		t, err = time.Parse("2006-01-02 15:04:05.999999999", text)
		if err == nil {
			ms := int(math.Trunc(1000 * (float64(t.Nanosecond()/1000) / 1e6)))
			t = jsutil.LocalDate(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), ms, loc)
		}
	} else {
		t, err = parseTimestamptz(text)
	}
	if err != nil {
		return nil, err
	}
	return jsonutil.Marshal(jsutil.DateISO(t))
}

func parseTimestamptz(text string) (time.Time, error) {
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999Z07",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999Z07:00:00",
	} {
		if t, err := time.Parse(layout, text); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("timestamptz %q", text)
}

// arrayJSON is postgres-array's parser with each element parsed as elem
// (0: kept as a string). NULL elements are null.
func arrayJSON(text string, elem uint32) (json.RawMessage, error) {
	p := &arrayParser{s: text}
	v, err := p.parse(elem)
	if err != nil {
		return nil, err
	}
	return v, nil
}

type arrayParser struct {
	s string
	i int
}

var errArray = errors.New("malformed array literal")

func (p *arrayParser) parse(elem uint32) (json.RawMessage, error) {
	// A leading dimension decoration ("[1:2]={...}") is skipped.
	if strings.HasPrefix(p.s[p.i:], "[") {
		eq := strings.IndexByte(p.s[p.i:], '=')
		if eq < 0 {
			return nil, errArray
		}
		p.i += eq + 1
	}
	return p.array(elem)
}

func (p *arrayParser) array(elem uint32) (json.RawMessage, error) {
	if p.i >= len(p.s) || p.s[p.i] != '{' {
		return nil, errArray
	}
	p.i++
	var b bytes.Buffer
	b.WriteByte('[')
	first := true
	for {
		if p.i >= len(p.s) {
			return nil, errArray
		}
		if p.s[p.i] == '}' {
			p.i++
			break
		}
		if !first {
			if p.s[p.i] != ',' {
				return nil, errArray
			}
			p.i++
			b.WriteByte(',')
		}
		first = false
		if p.i < len(p.s) && p.s[p.i] == '{' {
			v, err := p.array(elem)
			if err != nil {
				return nil, err
			}
			b.Write(v)
			continue
		}
		text, quoted, err := p.element()
		if err != nil {
			return nil, err
		}
		if !quoted && text == "NULL" {
			b.WriteString("null")
			continue
		}
		var v json.RawMessage
		if elem == 0 {
			v, err = jsonutil.Marshal(text)
		} else {
			v, err = Parse(elem, text)
		}
		if err != nil {
			return nil, err
		}
		b.Write(v)
	}
	b.WriteByte(']')
	return b.Bytes(), nil
}

func (p *arrayParser) element() (string, bool, error) {
	var b strings.Builder
	if p.s[p.i] == '"' {
		p.i++
		for p.i < len(p.s) {
			c := p.s[p.i]
			switch c {
			case '\\':
				p.i++
				if p.i >= len(p.s) {
					return "", false, errArray
				}
				b.WriteByte(p.s[p.i])
				p.i++
			case '"':
				p.i++
				return b.String(), true, nil
			default:
				b.WriteByte(c)
				p.i++
			}
		}
		return "", false, errArray
	}
	for p.i < len(p.s) && p.s[p.i] != ',' && p.s[p.i] != '}' {
		b.WriteByte(p.s[p.i])
		p.i++
	}
	return b.String(), false, nil
}
