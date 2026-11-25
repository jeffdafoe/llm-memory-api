package cleanup

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

// These tests run the LLM-1024 purges against a real Postgres: what they
// check is which rows the SQL picks and that the invite_codes foreign key
// holds, which a mock cannot show. testutil.ScratchDB says when they run.

// retentionSchema is the four tables the purges touch, cut to the columns
// they read, with production's foreign key from invites to requests (NO
// ACTION, so deleting a request an invite still points at fails).
const retentionSchema = `
CREATE TABLE access_requests (
    id serial PRIMARY KEY,
    email text NOT NULL,
    status text NOT NULL DEFAULT 'pending',
    created_at timestamptz DEFAULT now(),
    reviewed_at timestamptz
);
CREATE TABLE invite_codes (
    id serial PRIMARY KEY,
    code text NOT NULL,
    access_request_id integer REFERENCES access_requests(id),
    used_by text,
    used_at timestamptz,
    expires_at timestamptz
);
CREATE TABLE error_log (
    id serial PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now(),
    error_message text NOT NULL
);
CREATE TABLE system_errors (
    id serial PRIMARY KEY,
    error_code text NOT NULL,
    reported_at timestamptz DEFAULT now()
);`

func retentionKeys(access, pending, errors string) map[string]string {
	return map[string]string{
		"access_request_retention_days":         access,
		"pending_access_request_retention_days": pending,
		"error_log_retention_days":              errors,
	}
}

// request inserts an access request and returns its id. created and
// reviewed are SQL interval expressions back from now ("" = NULL for
// reviewed).
func request(t *testing.T, pool *pgxpool.Pool, email, status, createdAgo, reviewedAgo string) int {
	t.Helper()
	var id int
	sql := `INSERT INTO access_requests (email, status, created_at, reviewed_at)
            VALUES ($1, $2, now() - $3::interval,
                    CASE WHEN $4 = '' THEN NULL ELSE now() - $4::interval END)
            RETURNING id`
	if err := pool.QueryRow(context.Background(), sql, email, status, createdAgo, reviewedAgo).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// invite inserts an invite. requestID 0 = no access request; usedBy "" =
// unused; expiredAgo "" = never expires.
func invite(t *testing.T, pool *pgxpool.Pool, code string, requestID int, usedBy, expiredAgo string) {
	t.Helper()
	sql := `INSERT INTO invite_codes (code, access_request_id, used_by, used_at, expires_at)
            VALUES ($1, NULLIF($2, 0), NULLIF($3, ''),
                    CASE WHEN $3 = '' THEN NULL ELSE now() END,
                    CASE WHEN $4 = '' THEN NULL ELSE now() - $4::interval END)`
	if _, err := pool.Exec(context.Background(), sql, code, requestID, usedBy, expiredAgo); err != nil {
		t.Fatal(err)
	}
}

func remaining(t *testing.T, pool *pgxpool.Pool, sql string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), sql)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func sameSet(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]bool{}
	for _, g := range got {
		seen[g] = true
	}
	for _, w := range want {
		if !seen[w] {
			return false
		}
	}
	return true
}

func TestPurgeAccessRequestsRules(t *testing.T) {
	pool := testutil.ScratchDB(t, retentionSchema)
	ctx := context.Background()

	// Open signup: approved at once, invite valid 7 days, never used.
	stale := request(t, pool, "stale@x", "approved", "45 days", "45 days")
	invite(t, pool, "stale-code", stale, "", "38 days") // expired 38 days ago: past 30
	fresh := request(t, pool, "fresh@x", "approved", "12 days", "12 days")
	invite(t, pool, "fresh-code", fresh, "", "5 days") // expired 5 days ago: kept
	// Registered: the invite is used, so neither row goes, however old.
	member := request(t, pool, "member@x", "approved", "400 days", "400 days")
	invite(t, pool, "member-code", member, "member", "393 days")
	// An invite that never expires and was never used stays, and so does its
	// request.
	forever := request(t, pool, "forever@x", "approved", "200 days", "200 days")
	invite(t, pool, "forever-code", forever, "", "")
	// An admin invite with no access request holds no personal data.
	invite(t, pool, "admin-code", 0, "", "300 days")
	// Closed signup decisions with no invite.
	request(t, pool, "rejected-old@x", "rejected", "40 days", "31 days")
	request(t, pool, "rejected-new@x", "rejected", "40 days", "10 days")
	request(t, pool, "approved-noinvite@x", "approved", "40 days", "31 days")
	// Nobody reviewed these.
	request(t, pool, "pending-old@x", "pending", "91 days", "")
	request(t, pool, "pending-new@x", "pending", "30 days", "")
	// Status decides: an old pending row with reviewed_at set (only a hand
	// edit makes one) goes too.
	request(t, pool, "pending-odd@x", "pending", "91 days", "50 days")
	// An old pending request an invite still points at stays: deleting it
	// would break the foreign key.
	held := request(t, pool, "pending-held@x", "pending", "200 days", "")
	invite(t, pool, "held-code", held, "", "")
	// A request with two invites: the expired one goes, the live one keeps
	// the request.
	sibling := request(t, pool, "sibling@x", "approved", "60 days", "60 days")
	invite(t, pool, "sibling-old", sibling, "", "38 days")
	invite(t, pool, "sibling-live", sibling, "", "")
	// The request goes on its invite's clock, not its review's: reviewed 10
	// days ago, invite expired 38 days ago, both go.
	recent := request(t, pool, "recent-review@x", "approved", "60 days", "10 days")
	invite(t, pool, "recent-code", recent, "", "38 days")

	j := &Job{DB: pool, Config: storeOf(retentionKeys("30", "90", "0")), Log: &recorder{}}
	result, purged, err := j.PurgeAccessRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Invites: stale-code, sibling-old, recent-code. Requests: stale@x,
	// recent-review@x (with their invites), rejected-old@x,
	// approved-noinvite@x, pending-old@x, pending-odd@x.
	if purged != 9 {
		t.Errorf("purged %d, want 9 (result %v)", purged, result)
	}
	if got := remaining(t, pool, "SELECT email FROM access_requests"); !sameSet(got,
		"fresh@x", "member@x", "forever@x", "rejected-new@x", "pending-new@x", "pending-held@x", "sibling@x") {
		t.Errorf("requests left %v", got)
	}
	if got := remaining(t, pool, "SELECT code FROM invite_codes"); !sameSet(got,
		"fresh-code", "member-code", "forever-code", "admin-code", "held-code", "sibling-live") {
		t.Errorf("invites left %v", got)
	}

	// A second run finds nothing more.
	if _, purged, err := j.PurgeAccessRequests(ctx); err != nil || purged != 0 {
		t.Errorf("second run purged %d err %v", purged, err)
	}
}

func TestPurgeAccessRequestsKeysOff(t *testing.T) {
	pool := testutil.ScratchDB(t, retentionSchema)
	ctx := context.Background()
	stale := request(t, pool, "stale@x", "approved", "45 days", "45 days")
	invite(t, pool, "stale-code", stale, "", "38 days")
	request(t, pool, "pending-old@x", "pending", "91 days", "")

	// Both keys off (0 and blank): nothing is touched.
	j := &Job{DB: pool, Config: storeOf(retentionKeys("0", " ", "0")), Log: &recorder{}}
	if _, purged, err := j.PurgeAccessRequests(ctx); err != nil || purged != 0 {
		t.Fatalf("purged %d err %v", purged, err)
	}

	// Only the pending rule on: the expired invite and its request stay.
	j.Config = storeOf(retentionKeys("0", "90", "0"))
	if _, purged, err := j.PurgeAccessRequests(ctx); err != nil || purged != 1 {
		t.Fatalf("purged %d err %v", purged, err)
	}
	if got := remaining(t, pool, "SELECT email FROM access_requests"); !sameSet(got, "stale@x") {
		t.Errorf("requests left %v", got)
	}
}

func TestPurgeErrorLogs(t *testing.T) {
	pool := testutil.ScratchDB(t, retentionSchema)
	ctx := context.Background()
	// One more old error_log row than a batch, so the batch loop runs twice.
	seed := []string{
		`INSERT INTO error_log (created_at, error_message)
         SELECT now() - interval '91 days', 'old' FROM generate_series(1, 5001)`,
		`INSERT INTO error_log (created_at, error_message) VALUES (now() - interval '10 days', 'new')`,
		`INSERT INTO system_errors (error_code, reported_at) VALUES
             ('old', now() - interval '120 days'),
             ('new', now() - interval '1 day'),
             ('undated', NULL)`,
	}
	for _, s := range seed {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	j := &Job{DB: pool, Config: storeOf(retentionKeys("0", "0", "90")), Log: &recorder{}}
	result, purged, err := j.PurgeErrorLogs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if purged != 5002 {
		t.Errorf("purged %d, want 5002 (result %v)", purged, result)
	}
	if got := remaining(t, pool, "SELECT error_message FROM error_log"); !sameSet(got, "new") {
		t.Errorf("error_log left %v", got)
	}
	if got := remaining(t, pool, "SELECT error_code FROM system_errors"); !sameSet(got, "new", "undated") {
		t.Errorf("system_errors left %v", got)
	}

	// Off: an eligible row is there and stays.
	if _, err := pool.Exec(ctx, `INSERT INTO error_log (created_at, error_message) VALUES (now() - interval '200 days', 'old again')`); err != nil {
		t.Fatal(err)
	}
	j.Config = storeOf(retentionKeys("0", "0", "0"))
	if _, purged, err := j.PurgeErrorLogs(ctx); err != nil || purged != 0 {
		t.Errorf("off: purged %d err %v", purged, err)
	}
	if got := remaining(t, pool, "SELECT error_message FROM error_log"); !sameSet(got, "new", "old again") {
		t.Errorf("off: error_log left %v", got)
	}
}
