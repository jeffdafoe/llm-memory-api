package admin

import (
	"errors"
	"reflect"
	"regexp"
	"testing"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

// overwriteSlugs is new Set(overwrite_slugs || []) of documents.js; the
// TypeError texts are what Node 24 prints for each value.
func TestOverwriteSlugs(t *testing.T) {
	cases := []struct {
		body string
		want []string
		err  string
	}{
		{`{}`, nil, ""},
		{`{"overwrite_slugs":null}`, nil, ""},
		{`{"overwrite_slugs":false}`, nil, ""},
		{`{"overwrite_slugs":0}`, nil, ""},
		{`{"overwrite_slugs":""}`, nil, ""},
		{`{"overwrite_slugs":[]}`, nil, ""},
		{`{"overwrite_slugs":["a/x",5,null,{"s":"b"},"b/y"]}`, []string{"a/x", "b/y"}, ""},
		{`{"overwrite_slugs":"ab😀"}`, []string{"a", "b", "😀"}, ""},
		{`{"overwrite_slugs":{"a":1}}`, nil, "object is not iterable (cannot read property Symbol(Symbol.iterator))"},
		{`{"overwrite_slugs":true}`, nil, "boolean true is not iterable (cannot read property Symbol(Symbol.iterator))"},
		{`{"overwrite_slugs":5}`, nil, "number 5 is not iterable (cannot read property Symbol(Symbol.iterator))"},
		{`{"overwrite_slugs":1e21}`, nil, "number 1e+21 is not iterable (cannot read property Symbol(Symbol.iterator))"},
		{`{"overwrite_slugs":-1.5}`, nil, "number -1.5 is not iterable (cannot read property Symbol(Symbol.iterator))"},
	}
	for _, c := range cases {
		r := asUser(1, "/v1/admin/notes/move-prefix", c.body)
		f, _ := httpapi.BodyFields(r)
		got, err := overwriteSlugs(f)
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		if !reflect.DeepEqual(got, c.want) || msg != c.err {
			t.Errorf("%s: got %q %q, want %q %q", c.body, got, msg, c.want, c.err)
		}
	}
}

// expectErrorMessage is an error_log row with this error_message.
func expectErrorMessage(mock pgxmock.PgxPoolIface, message string) {
	args := make([]any, 8)
	for i := range args {
		args[i] = pgxmock.AnyArg()
	}
	args[5] = message
	mock.ExpectExec("INSERT INTO error_log").WithArgs(args...).WillReturnResult(pgxmock.NewResult("INSERT", 1))
}

func writeAllGrant(mock pgxmock.PgxPoolIface) {
	grants(mock, 1, "notes", "write")
	mock.ExpectQuery(regexp.QuoteMeta("SELECT namespace, can_read, can_write, can_delete FROM namespace_permissions WHERE actor_id = $1")).
		WithArgs(1).WillReturnRows(pgxmock.NewRows([]string{"namespace", "can_read", "can_write", "can_delete"}).AddRow("/", true, true, true))
}

const setErrorBody = `{"namespace":"alpha","old_prefix":"a/","new_prefix":"b/","overwrite_slugs":7}`

// A value the Set cannot take fails after the prefix checks and the two scan
// queries, as in Node: error_log holds V8's TypeError, and nothing is
// written.
func TestMovePrefixSetErrorAfterScan(t *testing.T) {
	h, mock := newContent(t)
	writeAllGrant(mock)
	mock.ExpectExec("SELECT slug, title FROM documents").WithArgs("alpha", "a/").
		WillReturnResult(pgxmock.NewResult("SELECT", 1))
	mock.ExpectQuery("SELECT d2.slug, d2.title FROM documents d1").WithArgs("alpha", "a/", "b/", "a/").
		WillReturnRows(pgxmock.NewRows([]string{"slug", "title"}))
	expectErrorMessage(mock, "number 7 is not iterable (cannot read property Symbol(Symbol.iterator))")
	rec := serve(h, asUser(1, "/v1/admin/notes/move-prefix", setErrorBody))
	check(t, "set error", rec, 500, errorBody("INTERNAL", "Failed: notes-move-prefix"))
	testutil.WaitMet(t, mock)
}

// A failed scan query is the error, not the Set's TypeError behind it.
func TestMovePrefixScanErrorBeforeSetError(t *testing.T) {
	t.Run("count", func(t *testing.T) {
		h, mock := newContent(t)
		writeAllGrant(mock)
		mock.ExpectExec("SELECT slug, title FROM documents").WithArgs("alpha", "a/").
			WillReturnError(errors.New("count failed"))
		expectErrorMessage(mock, "count failed")
		rec := serve(h, asUser(1, "/v1/admin/notes/move-prefix", setErrorBody))
		check(t, "count", rec, 500, errorBody("INTERNAL", "Failed: notes-move-prefix"))
		testutil.WaitMet(t, mock)
	})
	t.Run("conflicts", func(t *testing.T) {
		h, mock := newContent(t)
		writeAllGrant(mock)
		mock.ExpectExec("SELECT slug, title FROM documents").WithArgs("alpha", "a/").
			WillReturnResult(pgxmock.NewResult("SELECT", 1))
		mock.ExpectQuery("SELECT d2.slug, d2.title FROM documents d1").WithArgs("alpha", "a/", "b/", "a/").
			WillReturnError(errors.New("conflicts failed"))
		expectErrorMessage(mock, "conflicts failed")
		rec := serve(h, asUser(1, "/v1/admin/notes/move-prefix", setErrorBody))
		check(t, "conflicts", rec, 500, errorBody("INTERNAL", "Failed: notes-move-prefix"))
		testutil.WaitMet(t, mock)
	})
}

// The prefix checks run in movePrefix's order: the old prefix, then the
// new one, a prefix of another JSON type being "Slug is required".
func TestMovePrefixPrefixTypes(t *testing.T) {
	cases := []struct {
		name, body, message string
	}{
		{"old not string", `{"namespace":"alpha","old_prefix":5,"new_prefix":"b/"}`, "Slug is required"},
		{"new not string", `{"namespace":"alpha","old_prefix":"a/","new_prefix":false}`, "Slug is required"},
		{"old checked first", `{"namespace":"alpha","old_prefix":"/a","new_prefix":0}`, "Slug must not start with /"},
	}
	for _, c := range cases {
		h, mock := newContent(t)
		writeAllGrant(mock)
		expectErrorMessage(mock, c.message)
		rec := serve(h, asUser(1, "/v1/admin/notes/move-prefix", c.body))
		check(t, c.name, rec, 400, errorBody("BAD_REQUEST", c.message))
		testutil.WaitMet(t, mock)
	}
}
