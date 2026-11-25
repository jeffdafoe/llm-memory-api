package admin

import (
	"context"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/visibility"
)

// The GDPR delete check (LLM-1022) runs against the real schema. It seeds an
// account into every place it is known to leave a trace, deletes it, and
// then checks two things: each seeded row is gone or survives as intended
// (asserted row by row), and, as a backstop, no text column of any table
// still equals the account's name and no actor-id column holds its id. The
// backstop only sees what the fixture put in; a table this test does not
// seed is not covered. testutil.ScratchDB says when it runs.

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

// A registration redeeming a spare invite on the account's own request while
// the delete runs: the delete waits on the invite lock registration holds,
// then sees the invite as another account's, so that invite and its request
// survive. Only the account's own invite goes.
func TestDeleteWaitsForRedemptionDB(t *testing.T) {
	pool := testutil.ScratchDB(t, fullSchema(t))
	ctx := context.Background()
	for _, sql := range []string{
		`INSERT INTO actors (id, name) VALUES (2, 'admin'), (5, 'victim')`,
		`INSERT INTO admin_permissions (actor_id, resource, action) VALUES (2, '*', '*')`,
		`INSERT INTO agent_configuration (actor_id, virtual) VALUES (5, false)`,
		`INSERT INTO access_requests (id, email, usage_description, status) VALUES (11, 'v@example.com', 'notes', 'approved')`,
		`INSERT INTO invite_codes (id, code, access_request_id, used_by, used_at) VALUES
		    (21, 'c-victim', 11, 'victim', now()), (22, 'c-spare', 11, NULL, NULL)`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	// Registration's first step: lock the invite it is about to redeem.
	reg, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Rollback(ctx)
	if _, err := reg.Exec(ctx, `SELECT id FROM invite_codes WHERE id = 22 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}

	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		rec := httptest.NewRecorder()
		err := dbRoutes(pool).actorsDelete(rec, request(`{"actor_id":5}`))
		done <- result{rec.Code, err}
	}()

	// Wait until the delete is blocked on that lock, then let the
	// redemption commit.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
		        WHERE wait_event_type = 'Lock' AND query LIKE '%FROM invite_codes%FOR UPDATE%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case r := <-done:
			t.Fatalf("delete finished without waiting for the invite lock: %d %v", r.code, r.err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the delete never waited on the invite lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := reg.Exec(ctx, `UPDATE invite_codes SET used_by = 'newbie', used_at = now() WHERE id = 22`); err != nil {
		t.Fatal(err)
	}
	if err := reg.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	r := <-done
	if r.err != nil || r.code != 200 {
		t.Fatalf("delete: %d %v", r.code, r.err)
	}
	var own, redeemed, request int
	if err := pool.QueryRow(ctx, `SELECT
	        (SELECT count(*) FROM invite_codes WHERE id = 21),
	        (SELECT count(*) FROM invite_codes WHERE id = 22 AND used_by = 'newbie' AND access_request_id = 11),
	        (SELECT count(*) FROM access_requests WHERE id = 11)`).Scan(&own, &redeemed, &request); err != nil {
		t.Fatal(err)
	}
	if own != 0 || redeemed != 1 || request != 1 {
		t.Fatalf("own invite %d (want 0), redeemed invite %d (want 1), request %d (want 1)", own, redeemed, request)
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
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}

	// 2 admin (superadmin, does the delete); 5 victim, the account to delete,
	// registered by invite; 6 its virtual agent; 7 friend, linked to the
	// victim; 8 twin, another account on the same email address.
	exec(`INSERT INTO actors (id, name, email, created_by) VALUES
	        (2, 'admin', NULL, NULL), (5, 'victim', 'Victim@Example.com', NULL),
	        (6, 'victim-va', NULL, 5), (7, 'friend', NULL, NULL), (8, 'twin', 'victim@example.com', NULL)`)
	exec(`INSERT INTO admin_permissions (actor_id, resource, action) VALUES (2, '*', '*')`)
	exec(`INSERT INTO agent_configuration (actor_id, virtual) VALUES (5, false), (6, true), (7, false), (8, false)`)

	// Signup records:
	//   11  the victim's request: its used invite and a spare unused one (both go, then the request)
	//   12  a request from the victim's address with no invite (not provably the victim's: stays)
	//   13  the twin's request, same address (stays, with its invite)
	//   14  one request whose two invites the victim and friend each used (the request and
	//       friend's invite stay; the victim's invite goes)
	//   15  an unrelated request with an unused invite the victim issued (stays, creator cleared)
	exec(`INSERT INTO access_requests (id, email, usage_description, status) VALUES
	        (11, 'victim@example.com', 'notes', 'approved'),
	        (12, 'VICTIM@example.com', 'again', 'pending'),
	        (13, 'victim@example.com', 'twin', 'approved'),
	        (14, 'shared@example.com', 'shared', 'approved'),
	        (15, 'someone@example.com', 'other', 'approved')`)
	exec(`INSERT INTO invite_codes (id, code, created_by, access_request_id, used_by, used_at) VALUES
	        (21, 'c-victim', 'admin', 11, 'victim', now()),
	        (22, 'c-spare', 'admin', 11, NULL, NULL),
	        (23, 'c-twin', 'admin', 13, 'twin', now()),
	        (24, 'c-victim-2', 'admin', 14, 'victim', now()),
	        (25, 'c-friend', 'admin', 14, 'friend', now()),
	        (26, 'c-issued', 'victim', 15, NULL, NULL)`)

	// Notes in the victim's namespaces, grants and shares on them held by
	// others, another account's sync state, and the usage rows.
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

	// Row by row.
	checks := []struct {
		what string
		sql  string
		want int
	}{
		{"victim and its virtual agent", `SELECT count(*) FROM actors WHERE id IN (5, 6)`, 0},
		{"friend and twin", `SELECT count(*) FROM actors WHERE id IN (7, 8)`, 2},
		{"victim's request 11", `SELECT count(*) FROM access_requests WHERE id = 11`, 0},
		{"victim's invite and the spare", `SELECT count(*) FROM invite_codes WHERE id IN (21, 22)`, 0},
		{"address-only request 12", `SELECT count(*) FROM access_requests WHERE id = 12`, 1},
		{"twin's request 13", `SELECT count(*) FROM access_requests WHERE id = 13`, 1},
		{"twin's invite on 13", `SELECT count(*) FROM invite_codes WHERE id = 23 AND access_request_id = 13 AND used_by = 'twin'`, 1},
		{"shared request 14", `SELECT count(*) FROM access_requests WHERE id = 14`, 1},
		{"victim's invite on 14", `SELECT count(*) FROM invite_codes WHERE id = 24`, 0},
		{"friend's invite on 14", `SELECT count(*) FROM invite_codes WHERE id = 25 AND access_request_id = 14 AND used_by = 'friend'`, 1},
		{"issued invite 26, creator cleared", `SELECT count(*) FROM invite_codes WHERE id = 26 AND access_request_id = 15 AND created_by IS NULL`, 1},
		{"request 15", `SELECT count(*) FROM access_requests WHERE id = 15`, 1},
		{"note in friend's namespace, creator cleared", `SELECT count(*) FROM documents WHERE namespace = 'friend' AND created_by_actor_id IS NULL`, 1},
		{"victim's documents", `SELECT count(*) FROM documents WHERE namespace IN ('victim', 'victim-va')`, 0},
		{"grants on the namespaces", `SELECT count(*) FROM namespace_permissions WHERE namespace IN ('victim', 'victim-va')`, 0},
		{"shares out of the namespace", `SELECT count(*) FROM note_permissions WHERE owner_namespace = 'victim'`, 0},
		{"sync state", `SELECT count(*) FROM note_synchronization WHERE namespace = 'victim'`, 0},
		{"usage rows", `SELECT count(*) FROM namespace_usage WHERE namespace IN ('victim', 'victim-va')`, 0},
		{"link rows", `SELECT count(*) FROM actor_visibility_configuration WHERE actor_id = 5 OR target_actor_id = 5`, 0},
	}
	for _, c := range checks {
		if got := count(c.sql); got != c.want {
			t.Errorf("%s: %d rows, want %d", c.what, got, c.want)
		}
	}

	// Backstop over every table: no text column equal to either name, no
	// actor-id-like integer column holding either id.
	rows, err := pool.Query(ctx, `
	    SELECT c.table_name, c.column_name, c.data_type FROM information_schema.columns c
	      JOIN information_schema.tables t ON t.table_schema = c.table_schema AND t.table_name = c.table_name
	     WHERE c.table_schema = 'public' AND t.table_type = 'BASE TABLE'`)
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
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(cols) == 0 {
		t.Fatal("schema scan found no columns")
	}
	idColumn := regexp.MustCompile(`actor_id$|_by$|^virtual_agent_id$`)
	for _, c := range cols {
		var sql string
		switch {
		case c.typ == "text" || c.typ == "character varying":
			sql = `SELECT count(*) FROM public.` + c.table + ` WHERE "` + c.name + `" IN ('victim', 'victim-va')`
		case c.typ == "integer" && idColumn.MatchString(c.name):
			sql = `SELECT count(*) FROM public.` + c.table + ` WHERE "` + c.name + `" IN (5, 6)`
		default:
			continue
		}
		if n := count(sql); n != 0 {
			t.Errorf("%s.%s still holds the deleted account (%d rows)", c.table, c.name, n)
		}
	}
}
