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
	"regexp"
	"strconv"
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
	case oidDate, oidTimestamp, oidTimestamptz:
		return dateJSON(text), nil
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

// postgres-date 1.0.7's patterns. DATE_TIME is not anchored at the start,
// and `.*?` before the BC suffix skips the zone.
var (
	pgDateTime = regexp.MustCompile(`(\d{1,})-(\d{2})-(\d{2}) (\d{2}):(\d{2}):(\d{2})(\.\d{1,})?.*?( BC)?$`)
	pgDate     = regexp.MustCompile(`^(\d{1,})-(\d{2})-(\d{2})( BC)?$`)
	pgZone     = regexp.MustCompile(`([Z+-])(\d{2})?:?(\d{2})?:?(\d{2})?`)
	pgInfinity = regexp.MustCompile(`^-?infinity$`)
)

// maxDateMS is the ECMAScript time value range (TimeClip).
const maxDateMS = 8.64e15

// dateJSON is postgres-date (pg-types' parser for date, timestamp and
// timestamptz) written by JSON.stringify: a timestamp with a zone is read
// as UTC with that offset, one without as local time, a date alone as the
// local midnight. A BC year y is year 1 - y; a year from 0 to 99 is set
// after the Date is built, as the multi-argument Date forms read it as
// 19xx. Infinity, a value outside the Date range and text that is no date
// are null (Number Infinity, Invalid Date, null).
func dateJSON(text string) json.RawMessage {
	ms, ok := postgresDate(text)
	if !ok || math.IsNaN(ms) || math.IsInf(ms, 0) || math.Abs(ms) > maxDateMS {
		return json.RawMessage("null")
	}
	out, _ := jsonutil.Marshal(jsutil.DateISO(time.UnixMilli(int64(ms))))
	return out
}

// postgresDate returns the Date's time value; ok false is a parse that gave
// no Date at all.
func postgresDate(text string) (float64, bool) {
	if pgInfinity.MatchString(text) {
		if text[0] == '-' {
			return math.Inf(-1), true
		}
		return math.Inf(1), true
	}
	m := pgDateTime.FindStringSubmatch(text)
	if m == nil {
		d := pgDate.FindStringSubmatch(text)
		if d == nil {
			return 0, false
		}
		year := atoi(d[1])
		if d[4] != "" {
			year = 1 - year
		}
		t := localDate(year, atoi(d[2]), atoi(d[3]), 0, 0, 0, 0)
		return float64(t.UnixMilli()), true
	}
	year := atoi(m[1])
	if m[8] != "" {
		year = 1 - year
	}
	month, day := atoi(m[2]), atoi(m[3])
	hour, minute, second := atoi(m[4]), atoi(m[5]), atoi(m[6])
	ms := 0
	if m[7] != "" {
		// 1000 * parseFloat(".086123"), which the Date truncates.
		f, _ := jsutil.ParseFloat(m[7])
		ms = int(math.Trunc(1000 * f))
	}
	offset, hasZone := zoneOffset(text)
	if !hasZone {
		return float64(localDate(year, month, day, hour, minute, second, ms).UnixMilli()), true
	}
	t := time.Date(year, time.Month(month), day, hour, minute, second, ms*int(time.Millisecond), time.UTC)
	if year >= 0 && year < 100 {
		// Date.UTC read the year as 19xx; setUTCFullYear(year) keeps the
		// month, day and time it rolled over to.
		t1 := time.Date(year+1900, time.Month(month), day, hour, minute, second, ms*int(time.Millisecond), time.UTC)
		t = time.Date(year, t1.Month(), t1.Day(), t1.Hour(), t1.Minute(), t1.Second(), t1.Nanosecond(), time.UTC)
	}
	// The Date.UTC value is a Date before the offset is applied, so one past
	// the range is already Invalid Date, whatever the offset brings it to.
	utc := float64(t.UnixMilli())
	if math.Abs(utc) > maxDateMS {
		return math.NaN(), true
	}
	return utc - offset, true
}

// localDate is new Date(year, month - 1, day, ...) in the process zone, with
// the 0-99 fix-up postgres-date applies (setFullYear).
func localDate(year, month, day, hour, minute, second, ms int) time.Time {
	if year >= 0 && year < 100 {
		t1 := jsutil.LocalDate(year+1900, time.Month(month), day, hour, minute, second, ms, time.Local).In(time.Local)
		return jsutil.LocalDate(year, t1.Month(), t1.Day(), t1.Hour(), t1.Minute(), t1.Second(), t1.Nanosecond()/int(time.Millisecond), time.Local)
	}
	return jsutil.LocalDate(year, time.Month(month), day, hour, minute, second, ms, time.Local)
}

// zoneOffset is timeZoneOffset: the offset in milliseconds, or hasZone
// false for a timestamp without one. A sign with no hours is NaN, as
// parseInt(undefined) was.
func zoneOffset(text string) (offset float64, hasZone bool) {
	if strings.HasSuffix(text, "+00") {
		return 0, true
	}
	parts := strings.Split(text, " ")
	if len(parts) < 2 {
		return 0, false
	}
	z := pgZone.FindStringSubmatch(parts[1])
	if z == nil {
		return 0, false
	}
	if z[1] == "Z" {
		return 0, true
	}
	if z[2] == "" {
		return math.NaN(), true
	}
	sign := 1.0
	if z[1] == "-" {
		sign = -1
	}
	seconds := float64(atoi(z[2])*3600 + atoi(z[3])*60 + atoi(z[4]))
	return seconds * sign * 1000, true
}

// atoi is parseInt(s, 10) of a digit run; "" is 0 (parseInt(x || 0)).
func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
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
