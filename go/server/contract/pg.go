//go:build contract

package contract

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// appPassword is the password of the run's app role. The role is created for
// one run and dropped after it, and only listens on the test cluster.
const appPassword = "contract"

// multisetTables are written from background goroutines (the request log
// after the response, error_log from a fire-and-forget insert), so their row
// order and serial IDs depend on timing. They are compared as multisets
// without the dropped columns.
var multisetTables = map[string][]string{
	"request_log": {"id", "duration_ms"},
	// error_detail is the stack trace in Node and Go's %+v of the error:
	// implementation text, never the same.
	"error_log": {"id", "error_detail"},
	// A note save indexes the note in the background; when one request
	// saves several (memory-sync), their chunks are inserted in whatever
	// order the embedding calls return.
	"memory_chunks": {"id"},
}

// tableOrder replaces primary-key order for tables whose key is random
// (gen_random_uuid), which would order new rows differently on the two
// copies. The servers write them in request order, so the creation time
// orders them the same on both sides.
var tableOrder = map[string]string{
	"mail": "sent_at, subject, body",
	// The fixture's sessions share one created_at; an actor's two API
	// sessions differ in expiry.
	"sessions": "created_at, actor_id, kind, expires_at",
	// Chat texts by content, then id: when a route starts two message
	// chains without awaiting either (a discussion conclude), which one
	// Node inserts first is a race between commits, so those cases name the
	// ids volatile and the rows still pair by text. The deliveries are
	// ordered by recipient and linked text after the snapshot
	// (linkChatDeliveries).
	"chat_message_texts": "message, id",
}

// Cluster is the Postgres server the harness runs on, reached as a superuser
// through CONTRACT_PG_URL. All databases of one test run, and its app role,
// share a random prefix, so two runs on one cluster do not collide.
type Cluster struct {
	adminURL *url.URL
	prefix   string
	template string
	// role is the login the servers connect as (<prefix>_app). Like
	// memory_api on the VPS it is not a superuser; it gets the same grants a
	// fresh install gives.
	role string

	createMu sync.Mutex // CREATE DATABASE ... TEMPLATE needs the template idle
	mu       sync.Mutex
	next     int
	tables   []tableInfo
}

type tableInfo struct {
	name    string
	columns []string // compared columns, in column order
	orderBy string
}

// NewCluster checks the URL and picks this run's prefix.
func NewCluster(adminURL string) (*Cluster, error) {
	u, err := url.Parse(adminURL)
	if err != nil {
		return nil, errors.New("CONTRACT_PG_URL is not a URL")
	}
	var b [3]byte
	rand.Read(b[:])
	prefix := "contract_" + hex.EncodeToString(b[:])
	return &Cluster{adminURL: u, prefix: prefix, role: prefix + "_app"}, nil
}

// URL returns the connection URL for one database, as the superuser or as
// the app role.
func (c *Cluster) URL(db string, asApp bool) string {
	u := *c.adminURL
	u.Path = "/" + db
	if asApp {
		u.User = url.UserPassword(c.role, appPassword)
	}
	return u.String()
}

func (c *Cluster) admin(ctx context.Context, db string) (*pgx.Conn, error) {
	return pgx.Connect(ctx, c.URL(db, false))
}

func maintenanceDB(u *url.URL) string {
	db := strings.TrimPrefix(u.Path, "/")
	if db == "" {
		return "postgres"
	}
	return db
}

// execFile runs one SQL text on its own connection to db, as the migration
// runner gives each file its own psql: a file that changes session settings
// (schema.sql empties search_path) cannot affect the next one.
func (c *Cluster) execFile(ctx context.Context, db, name, sql string) error {
	conn, err := c.admin(ctx, db)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, sql); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// BuildTemplate makes the template database the way a fresh install builds
// one (infrastructure/tasks/run-migrations.yml, CI's fresh-install job):
// the vector extension and migrations_applied, schema.sql plus seed.sql in
// one transaction, every *_up.sql in file-name order, the grants, and then
// the harness fixture.
func (c *Cluster) BuildTemplate(ctx context.Context, repoRoot string) error {
	c.template = c.prefix + "_template"
	maint := maintenanceDB(c.adminURL)
	role := pgx.Identifier{c.role}.Sanitize()

	conn, err := c.admin(ctx, maint)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", maint, err)
	}
	defer conn.Close(ctx)
	// A role name cannot be a bind parameter; the password is a constant.
	if _, err := conn.Exec(ctx, "CREATE ROLE "+role+" LOGIN NOSUPERUSER PASSWORD '"+appPassword+"'"); err != nil {
		return fmt.Errorf("create role: %w", err)
	}
	// Owned by the app role as on a fresh install; the tables are still the
	// superuser's, since the superuser creates them.
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{c.template}.Sanitize()+" OWNER "+role); err != nil {
		return fmt.Errorf("create template: %w", err)
	}

	if err := c.execFile(ctx, c.template, "extension", `CREATE EXTENSION IF NOT EXISTS vector;
		CREATE TABLE IF NOT EXISTS migrations_applied (migration_id VARCHAR(50) PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`); err != nil {
		return err
	}

	var baseline strings.Builder
	for _, name := range []string{"schema.sql", "seed.sql"} {
		sql, err := loadSQL(filepath.Join(repoRoot, "migrations", name))
		if err != nil {
			return err
		}
		baseline.WriteString(sql)
		baseline.WriteString("\n")
	}
	// One simple-protocol query with many statements runs as one implicit
	// transaction, as psql --single-transaction does. The files hold no
	// transaction statements of their own.
	if err := c.execFile(ctx, c.template, "baseline", baseline.String()); err != nil {
		return err
	}

	ups, _ := filepath.Glob(filepath.Join(repoRoot, "migrations", "*_up.sql"))
	sort.Strings(ups)
	for _, f := range ups {
		sql, err := loadSQL(f)
		if err != nil {
			return err
		}
		if err := c.execFile(ctx, c.template, filepath.Base(f), sql); err != nil {
			return err
		}
	}

	tpl, err := c.admin(ctx, c.template)
	if err != nil {
		return err
	}
	defer tpl.Close(ctx)
	if _, err := tpl.Exec(ctx, `GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA public TO `+role+`;
		GRANT ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public TO `+role); err != nil {
		return fmt.Errorf("grants: %w", err)
	}
	if err := applyFixture(ctx, tpl); err != nil {
		return fmt.Errorf("fixture: %w", err)
	}
	tables, err := readTables(ctx, tpl)
	if err != nil {
		return err
	}
	c.tables = tables
	return nil
}

// loadSQL reads a migration file for a plain SQL connection. pg_dump writes
// \restrict and \unrestrict lines (psql commands, not SQL); they are
// dropped. Any other psql command is an error rather than skipped.
func loadSQL(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, `\`) {
			continue
		}
		if strings.HasPrefix(line, `\restrict `) || strings.HasPrefix(line, `\unrestrict `) {
			lines[i] = ""
			continue
		}
		return "", fmt.Errorf("%s:%d: psql command %q is not supported by the contract harness", filepath.Base(path), i+1, strings.Fields(line)[0])
	}
	return strings.Join(lines, "\n"), nil
}

// readTables lists the public tables with their compared columns and the
// order rows are read in: primary key, else the whole row.
func readTables(ctx context.Context, conn *pgx.Conn) ([]tableInfo, error) {
	rows, err := conn.Query(ctx, `
		SELECT c.relname,
		       (SELECT array_agg(a.attname::text ORDER BY a.attnum) FROM pg_attribute a
		         WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped),
		       coalesce((SELECT array_agg(a.attname::text ORDER BY array_position(i.indkey, a.attnum))
		                   FROM pg_index i JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
		                  WHERE i.indrelid = c.oid AND i.indisprimary), '{}')
		  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p')
		 ORDER BY c.relname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tableInfo
	for rows.Next() {
		var name string
		var cols, pk []string
		if err := rows.Scan(&name, &cols, &pk); err != nil {
			return nil, err
		}
		drop := map[string]bool{}
		for _, c := range multisetTables[name] {
			drop[c] = true
		}
		t := tableInfo{name: name}
		for _, col := range cols {
			if !drop[col] {
				t.columns = append(t.columns, col)
			}
		}
		if order, ok := tableOrder[name]; ok {
			t.orderBy = order
		} else if len(pk) > 0 && len(drop) == 0 {
			quoted := make([]string, len(pk))
			for i, col := range pk {
				quoted[i] = pgx.Identifier{col}.Sanitize()
			}
			t.orderBy = strings.Join(quoted, ", ")
		} else {
			t.orderBy = "x::text"
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ScenarioDB is one copy of the template for one server.
type ScenarioDB struct {
	Name    string
	cluster *Cluster
	pool    *pgxpool.Pool
}

// Copy makes a fresh copy of the template.
func (c *Cluster) Copy(ctx context.Context, label string) (*ScenarioDB, error) {
	c.mu.Lock()
	c.next++
	name := fmt.Sprintf("%s_%d_%s", c.prefix, c.next, label)
	c.mu.Unlock()

	c.createMu.Lock()
	err := func() error {
		conn, err := c.admin(ctx, maintenanceDB(c.adminURL))
		if err != nil {
			return err
		}
		defer conn.Close(ctx)
		// A copy is owned by whoever creates it unless OWNER says otherwise.
		_, err = conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+
			" TEMPLATE "+pgx.Identifier{c.template}.Sanitize()+" OWNER "+pgx.Identifier{c.role}.Sanitize())
		return err
	}()
	c.createMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("copy template: %w", err)
	}
	pool, err := pgxpool.New(ctx, c.URL(name, false))
	if err != nil {
		// The database exists but the caller gets no handle to drop it.
		c.dropDatabase(context.Background(), name)
		return nil, err
	}
	return &ScenarioDB{Name: name, cluster: c, pool: pool}, nil
}

// dropDatabase drops one database of this run. FORCE ends any connection a
// stopped server left.
func (c *Cluster) dropDatabase(ctx context.Context, name string) error {
	conn, err := c.admin(ctx, maintenanceDB(c.adminURL))
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	return err
}

// Exec runs sql on the copy as the superuser. With no arguments pgx sends
// it as one simple query, so it may hold several statements.
func (d *ScenarioDB) Exec(ctx context.Context, sql string) error {
	_, err := d.pool.Exec(ctx, sql)
	return err
}

// Drop removes the copy.
func (d *ScenarioDB) Drop(ctx context.Context) error {
	d.pool.Close()
	return d.cluster.dropDatabase(ctx, d.Name)
}

// Cleanup drops the template and then the run's role, which can only go once
// no database it owns is left. Call it after every copy is dropped.
func (c *Cluster) Cleanup(ctx context.Context) error {
	if c.template != "" {
		if err := c.dropDatabase(ctx, c.template); err != nil {
			return err
		}
	}
	conn, err := c.admin(ctx, maintenanceDB(c.adminURL))
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, "DROP ROLE IF EXISTS "+pgx.Identifier{c.role}.Sanitize())
	return err
}

// Fingerprint is an MD5 of every table's full contents, all columns. Any
// committed insert, update or delete changes it, including one that commits
// after a later transaction (row counts and xmin would miss that). The
// settle step waits until it stops changing.
func (d *ScenarioDB) Fingerprint(ctx context.Context) (string, error) {
	parts := make([]string, len(d.cluster.tables))
	for i, t := range d.cluster.tables {
		parts[i] = fmt.Sprintf("SELECT %s || '=' || md5(coalesce(string_agg(x::text, E'\\n' ORDER BY x::text), '')) FROM %s x",
			quoteLiteral(t.name), pgx.Identifier{"public", t.name}.Sanitize())
	}
	rows, err := d.pool.Query(ctx, strings.Join(parts, " UNION ALL "))
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return "", err
		}
		out = append(out, s)
	}
	return strings.Join(out, " "), rows.Err()
}

// Busy counts the other client connections to this database that are
// running a statement (a commit waiting on the disk included) or sit inside
// a transaction. The pool connects as the superuser, so it sees the state of
// the servers' connections.
func (d *ScenarioDB) Busy(ctx context.Context) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
        WHERE datname = current_database() AND pid <> pg_backend_pid()
          AND backend_type = 'client backend' AND state IS DISTINCT FROM 'idle'`).Scan(&n)
	return n, err
}

// Snapshot reads every table as JSON rows in column order. row_to_json
// keeps column order (jsonb would sort the keys); timestamps come out as ISO
// strings and vectors as their text form.
func (d *ScenarioDB) Snapshot(ctx context.Context) ([]Table, error) {
	var out []Table
	for _, t := range d.cluster.tables {
		cols := make([]string, len(t.columns))
		for i, c := range t.columns {
			cols[i] = pgx.Identifier{c}.Sanitize()
		}
		q := fmt.Sprintf("SELECT row_to_json(x)::text FROM (SELECT %s FROM %s) x ORDER BY %s",
			strings.Join(cols, ", "), pgx.Identifier{"public", t.name}.Sanitize(), t.orderBy)
		rows, err := d.pool.Query(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.name, err)
		}
		tbl := Table{Name: t.name, Multiset: multisetTables[t.name] != nil}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				return nil, err
			}
			v, err := ParseJSON([]byte(s))
			if err != nil {
				rows.Close()
				return nil, fmt.Errorf("%s: %w", t.name, err)
			}
			tbl.Rows = append(tbl.Rows, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		out = append(out, tbl)
	}
	return out, nil
}

func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
