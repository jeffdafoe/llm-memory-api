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

// AppRole is the login the servers connect as. Like memory_api on the VPS it
// is not a superuser; it gets the same grants a fresh install gives.
const (
	AppRole     = "contract_app"
	appPassword = "contract"
)

// multisetTables are written from background goroutines (the request log
// after the response, error_log from a fire-and-forget insert), so their row
// order and serial IDs depend on timing. They are compared as multisets
// without the dropped columns.
var multisetTables = map[string][]string{
	"request_log": {"id", "duration_ms"},
	"error_log":   {"id"},
}

// Cluster is the Postgres server the harness runs on, reached as a superuser
// through CONTRACT_PG_URL. All databases of one test run share a random
// prefix, so two runs on one cluster do not collide.
type Cluster struct {
	adminURL *url.URL
	prefix   string
	template string

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
	return &Cluster{adminURL: u, prefix: "contract_" + hex.EncodeToString(b[:])}, nil
}

// URL returns the connection URL for one database, as the superuser or as
// the app role.
func (c *Cluster) URL(db string, asApp bool) string {
	u := *c.adminURL
	u.Path = "/" + db
	if asApp {
		u.User = url.UserPassword(AppRole, appPassword)
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

// BuildTemplate makes the template database the way a fresh install builds
// one (infrastructure/tasks/run-migrations.yml, CI's fresh-install job):
// the vector extension and migrations_applied, schema.sql plus seed.sql in
// one transaction, every *_up.sql in file-name order, the secret config keys
// filled, the grants, and then the harness fixture.
func (c *Cluster) BuildTemplate(ctx context.Context, repoRoot string) error {
	c.template = c.prefix + "_template"
	maint := maintenanceDB(c.adminURL)

	conn, err := c.admin(ctx, maint)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", maint, err)
	}
	defer conn.Close(ctx)
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", AppRole).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		// Role names cannot be bind parameters; both are constants.
		if _, err := conn.Exec(ctx, "CREATE ROLE "+AppRole+" LOGIN PASSWORD '"+appPassword+"'"); err != nil {
			return fmt.Errorf("create role: %w", err)
		}
	}
	// Owned by the app role as on a fresh install; the tables are still the
	// superuser's, since the superuser creates them.
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{c.template}.Sanitize()+" OWNER "+AppRole); err != nil {
		return fmt.Errorf("create template: %w", err)
	}

	tpl, err := c.admin(ctx, c.template)
	if err != nil {
		return err
	}
	defer tpl.Close(ctx)
	if _, err := tpl.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector;
		CREATE TABLE IF NOT EXISTS migrations_applied (migration_id VARCHAR(50) PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`); err != nil {
		return fmt.Errorf("extension: %w", err)
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
	// transaction, as psql --single-transaction does.
	if _, err := tpl.Exec(ctx, baseline.String()); err != nil {
		return fmt.Errorf("baseline: %w", err)
	}

	ups, _ := filepath.Glob(filepath.Join(repoRoot, "migrations", "*_up.sql"))
	sort.Strings(ups)
	for _, f := range ups {
		sql, err := loadSQL(f)
		if err != nil {
			return err
		}
		if _, err := tpl.Exec(ctx, sql); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
	}
	// schema.sql empties search_path for its session; start clean.
	tpl.Close(ctx)

	tpl, err = c.admin(ctx, c.template)
	if err != nil {
		return err
	}
	defer tpl.Close(ctx)
	if _, err := tpl.Exec(ctx, `GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA public TO `+AppRole+`;
		GRANT ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public TO `+AppRole); err != nil {
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
		if len(pk) > 0 && len(drop) == 0 {
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
		_, err = conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" TEMPLATE "+pgx.Identifier{c.template}.Sanitize())
		return err
	}()
	c.createMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("copy template: %w", err)
	}
	pool, err := pgxpool.New(ctx, c.URL(name, false))
	if err != nil {
		return nil, err
	}
	return &ScenarioDB{Name: name, cluster: c, pool: pool}, nil
}

// Drop removes the copy. FORCE ends any connection a stopped server left.
func (d *ScenarioDB) Drop(ctx context.Context) error {
	d.pool.Close()
	conn, err := d.cluster.admin(ctx, maintenanceDB(d.cluster.adminURL))
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{d.Name}.Sanitize()+" WITH (FORCE)")
	return err
}

// DropTemplate removes the template at the end of the run.
func (c *Cluster) DropTemplate(ctx context.Context) error {
	if c.template == "" {
		return nil
	}
	conn, err := c.admin(ctx, maintenanceDB(c.adminURL))
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{c.template}.Sanitize()+" WITH (FORCE)")
	return err
}

// Fingerprint is the row count and newest row version (xmin) of every table.
// An insert, update or delete in any table changes it, which is what the
// settle step waits on.
func (d *ScenarioDB) Fingerprint(ctx context.Context) (string, error) {
	parts := make([]string, len(d.cluster.tables))
	for i, t := range d.cluster.tables {
		parts[i] = fmt.Sprintf("SELECT %s || '=' || count(*) || '/' || coalesce(max(xmin::text::bigint), 0) FROM %s",
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
