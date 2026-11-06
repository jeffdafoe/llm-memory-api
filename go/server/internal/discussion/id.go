package discussion

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/pgbind"
)

// ID is a discussion or vote id as the caller sent it. Node passed the
// request-body value unchanged to the queries, the responses, the log lines
// and the notifySystem payload, so "101" stays a string there and Postgres
// refuses "abc" with its own message.
type ID struct {
	raw json.RawMessage
}

// RawID is an id from a JSON body; nil is absent.
func RawID(raw json.RawMessage) ID {
	return ID{raw: raw}
}

// IntID is an id read from the database.
func IntID(n int) ID {
	return ID{raw: json.RawMessage(strconv.Itoa(n))}
}

// Truthy is JavaScript truthiness of the value.
func (id ID) Truthy() bool {
	return pgbind.Truthy(id.raw)
}

// Param is the value as node-postgres bound it.
func (id ID) Param() any {
	return pgbind.Value(id.raw)
}

// String is the value as a template string printed it.
func (id ID) String() string {
	v, err := jsutil.Parse(id.raw)
	if err != nil {
		return ""
	}
	s, _ := jsutil.ValueString(v)
	return s
}

// MarshalJSON writes the value as JSON.stringify did.
func (id ID) MarshalJSON() ([]byte, error) {
	if len(id.raw) == 0 {
		return []byte("null"), nil
	}
	s, err := jsutil.Stringify(id.raw)
	if err != nil {
		return nil, err
	}
	return []byte(s), nil
}

// jsNumber is a JavaScript number in a response, as JSON.stringify writes it.
type jsNumber float64

func (n jsNumber) MarshalJSON() ([]byte, error) {
	return []byte(jsutil.NumberString(float64(n))), nil
}

// maxDateMS is the largest time value a JavaScript Date holds.
const maxDateMS = 8.64e15

// invalidDateParam is what node-postgres sends for an Invalid Date; Postgres
// refuses it with its own message.
const invalidDateParam = "0NaN-NaN-NaNTNaN:NaN:NaN.NaN+NaN:NaN"

// minutesFromNow is `new Date(Date.now() + parseInt(minutes) * 60 * 1000)`.
// valid is false for an Invalid Date (a minutes value parseInt cannot read,
// or a time outside the Date range); param is then node-postgres's text for
// it.
func minutesFromNow(now time.Time, minutes string) (t time.Time, param any, valid bool) {
	n, ok := jsutil.ParseInt(minutes)
	ms := float64(now.UnixMilli()) + n*60*1000
	if !ok || ms > maxDateMS || ms < -maxDateMS {
		return time.Time{}, invalidDateParam, false
	}
	t = time.UnixMilli(int64(ms))
	return t, t, true
}

// jsDate is a Date set from JavaScript (Date.now(), new Date()): milliseconds.
func jsDate(now time.Time) time.Time {
	return time.UnixMilli(now.UnixMilli())
}

// isoTime is a timestamptz in a response, as node-postgres and
// JSON.stringify wrote it; nil is null.
func isoTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := jsutil.DateISO(*t)
	return &s
}
