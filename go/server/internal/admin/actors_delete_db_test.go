package admin

import (
	"context"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/visibility"
)

// The GDPR delete check (LLM-1022) runs against the real schema: what it
// proves is that after an account is deleted, no row in ANY table still
// holds its id, name or email. A mock can only show the statements a test
// author thought of; walking information_schema also catches a table added
// later that the delete does not know about. testutil.ScratchDB says when
// it runs.

// fullSchema is migrations/schema.sql made loadable on any Postgres 17: the
// psql-only \restrict lines go, the search_path reset goes (it would stick
// to the pooled connection and break unqualified queries), and pgvector is
// replaced by a plain array, since no test here reads an embedding.
func fullSchema(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../../../migrations/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	var keep []string
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, `\`),
			strings.Contains(line, "set_config('search_path'"),
			strings.Contains(line, "EXTENSION IF NOT EXISTS vector"),
			strings.Contains(line, "COMMENT ON EXTENSION vector"),
			strings.Contains(line, "USING ivfflat"):
			continue
		}
		keep = append(keep, line)
	}
	return regexp.MustCompile(`public\.vector\(\d+\)`).ReplaceAllString(strings.Join(keep, "\n"), "real[]")
}

// dbRoutes is newRoutes on a real pool, for the handlers a test runs end to
// end. The request user is actor 2, "admin".
func dbRoutes(pool *pgxpool.Pool) *Routes {
	cache := actors.NewCache(pool)
	log := logger.New(pool, cache, "")
	adm := &permissions.Admin{DB: pool}
	return &Routes{
		Common: &Common{
			DB: pool, Log: log, Router: &httpapi.Router{Log: log}, Admin: adm, Actors: cache,
			Visibility: &visibility.Visibility{DB: pool, Admin: adm},
		},
		SessionAuth: &fakeCache{},
	}
}

func TestDeleteLeavesNoTraceDB(t *testing.T) {
	pool := testutil.ScratchDB(t, fullSchema(t))
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	// 2 admin (superadmin, does the delete); 5 victim, the account to delete,
	// registered by invite; 6 its virtual agent; 7 friend, linked to the
	// victim; 8 twin, another account on the same email address, whose
	// signup record must survive.
	exec(`INSERT INTO actors (id, name, email, created_by) VALUES
	        (2, 'admin', NULL, NULL), (5, 'victim', 'Victim@Example.com', NULL),
	        (6, 'victim-va', NULL, 5), (7, 'friend', NULL, NULL), (8, 'twin', 'victim@example.com', NULL)`)
	exec(`INSERT INTO admin_permissions (actor_id, resource, action) VALUES (2, '*', '*')`)
	exec(`INSERT INTO agent_configuration (actor_id, virtual) VALUES (5, false), (6, true), (7, false), (8, false)`)

	// Signup: the victim's used invite and its request, a second request
	// from the same address that was never reviewed, and the twin's.
	exec(`INSERT INTO access_requests (id, email, usage_description, status) VALUES
	        (11, 'victim@example.com', 'notes', 'approved'),
	        (12, 'VICTIM@example.com', 'again', 'pending'),
	        (13, 'victim@example.com', 'twin', 'approved')`)
	exec(`INSERT INTO invite_codes (code, created_by, access_request_id, used_by, used_at) VALUES
	        ('c-victim', 'admin', 11, 'victim', now()),
	        ('c-twin', 'admin', 13, 'twin', now())`)

	// Notes in the victim's namespace, grants and shares on it held by
	// others, another account's sync state for it, and its usage row.
	exec(`INSERT INTO documents (namespace, slug, content, created_by_actor_id) VALUES
	        ('victim', 'notes/a', 'x', 5), ('victim-va', 'notes/b', 'y', 6), ('friend', 'notes/c', 'z', 5)`)
	exec(`INSERT INTO namespace_permissions (actor_id, namespace, can_read) VALUES (7, 'victim', true), (7, 'victim-va', true)`)
	exec(`INSERT INTO note_permissions (owner_namespace, slug_pattern, grantee_actor_id, can_read, granted_by) VALUES
	        ('victim', 'notes/*', 7, true, 2)`)
	exec(`INSERT INTO note_synchronization (actor_id, namespace, slug, local_path) VALUES (7, 'victim', 'notes/a', '/x')`)
	exec(`INSERT INTO namespace_usage (namespace, note_count, total_bytes) VALUES ('victim', 1, 1), ('victim-va', 1, 1)`)

	// The account link (LLM-670): one visibility row each way.
	exec(`INSERT INTO actor_visibility_configuration (actor_id, target_actor_id) VALUES (5, 7), (7, 5)`)

	rt := dbRoutes(pool)
	rec := httptest.NewRecorder()
	if err := rt.actorsDelete(rec, request(`{"actor_id":5}`)); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}

	// Walk every column of every table. Text columns must not hold either
	// name or the email (case-insensitive); id-like integer columns must
	// not hold either id.
	rows, err := pool.Query(ctx, `
	    SELECT table_name, column_name, data_type FROM information_schema.columns
	     WHERE table_schema = 'public' AND table_name IN
	           (SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE')`)
	if err != nil {
		t.Fatal(err)
	}
	type column struct{ table, name, typ string }
	var cols []column
	for rows.Next() {
		var c column
		if err := rows.Scan(&c.table, &c.name, &c.typ); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	rows.Close()
	idColumn := regexp.MustCompile(`actor_id$|_by$|^virtual_agent_id$`)
	for _, c := range cols {
		var sql string
		var args []any
		switch {
		case c.typ == "text" || c.typ == "character varying":
			sql = `SELECT count(*) FROM public.` + c.table + ` WHERE "` + c.name + `" IN ($1, $2) OR LOWER("` + c.name + `") = LOWER($3)`
			args = []any{"victim", "victim-va", "victim@example.com"}
		case c.typ == "integer" && idColumn.MatchString(c.name):
			sql = `SELECT count(*) FROM public.` + c.table + ` WHERE "` + c.name + `" IN (5, 6)`
		case c.typ == "integer" && c.name == "id" && c.table == "actors":
			sql = `SELECT count(*) FROM public.actors WHERE id IN (5, 6)`
		default:
			continue
		}
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%s.%s: %v", c.table, c.name, err)
		}
		// The twin's own email is the one deliberate survivor: it is another
		// account's data.
		if c.table == "actors" && c.name == "email" && n == 1 {
			continue
		}
		if c.table == "access_requests" && c.name == "email" && n == 1 {
			continue
		}
		if n != 0 {
			t.Errorf("%s.%s still holds the deleted account (%d rows)", c.table, c.name, n)
		}
	}

	// What must survive: the twin's signup record and account, the linked
	// account, and the note the victim wrote in friend's namespace (without
	// its creator).
	var survivors int
	if err := pool.QueryRow(ctx, `SELECT
	        (SELECT count(*) FROM access_requests WHERE id = 13) +
	        (SELECT count(*) FROM invite_codes WHERE used_by = 'twin') +
	        (SELECT count(*) FROM actors WHERE id IN (7, 8)) +
	        (SELECT count(*) FROM documents WHERE namespace = 'friend' AND created_by_actor_id IS NULL)`).Scan(&survivors); err != nil {
		t.Fatal(err)
	}
	if survivors != 5 {
		t.Fatalf("survivors: %d, want 5", survivors)
	}
}
