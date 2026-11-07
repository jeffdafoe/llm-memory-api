package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/pgbind"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/pgjson"
)

// maxInviteExpiryDays bounds invite-codes/generate's expires_days.
const maxInviteExpiryDays = 3650

// maxInviteBatch is the most codes one generate call makes.
const maxInviteBatch = 50

// inviteCode is 16 random bytes in hex.
func inviteCode() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// baseURL is BASE_URL, else the request's own http://host.
func (rt *Routes) baseURL(r *http.Request) string {
	if rt.BaseURL != "" {
		return rt.BaseURL
	}
	return "http://" + r.Host
}

// optionalText is `value || null` for a text column; a truthy value of
// another JSON type is refused.
func optionalText(f httpapi.Fields, name string) (any, error) {
	s, err := f.OptionalString(name)
	if err != nil || s == "" {
		return nil, err
	}
	return s, nil
}

// accessRequestsList is POST /admin/access-requests: the access requests,
// newest first, of one status when `status` is given, each with its invite
// code and registration link.
func (rt *Routes) accessRequestsList(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("status")
	}
	sql := `SELECT ar.id, ar.email, ar.usage_description, ar.status, ar.created_at, ar.reviewed_at, ar.reviewer_notes,
                       ic.code AS invite_code
                FROM access_requests ar
                LEFT JOIN invite_codes ic ON ic.access_request_id = ar.id`
	var args []any
	if f.Truthy("status") {
		sql += " WHERE ar.status = $1"
		args = append(args, pgbind.Value(f.Raw("status")))
	}
	sql += " ORDER BY ar.created_at DESC"
	rows, err := pgjson.Query(r.Context(), rt.DB, sql, args...)
	if err != nil {
		return err
	}
	base := rt.baseURL(r)
	for i := range rows {
		if code, ok := rows[i].String("invite_code"); ok && code != "" {
			rows[i].Set("register_url", jsString(base+"/register?code="+code))
		}
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string][]pgjson.Row{"requests": rows})
}

// pendingRequest reads an access request and checks it is pending. It
// writes the 404 or the 400 itself; ok is false then.
func (rt *Routes) pendingRequest(w http.ResponseWriter, r *http.Request, id any) (email *string, ok bool, err error) {
	var status string
	err = rt.DB.QueryRow(r.Context(), "SELECT status, email FROM access_requests WHERE id = $1", id).Scan(&status, &email)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, notFound(w, "Access request not found")
	}
	if err != nil {
		return nil, false, err
	}
	if status != "pending" {
		return nil, false, httpapi.BadRequest(w, "Request already "+status)
	}
	return email, true, nil
}

// accessRequestsApprove is POST /admin/access-requests/approve: marks a
// pending request approved and makes its invite code (30 days, in the
// approver's first realm).
func (rt *Routes) accessRequestsApprove(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("id")
	}
	if !f.Truthy("id") {
		return httpapi.BadRequest(w, "Required field: id")
	}
	id := pgbind.Value(f.Raw("id"))
	email, ok, err := rt.pendingRequest(w, r, id)
	if !ok {
		return err
	}
	notes, err := optionalText(f, "notes")
	if err != nil {
		return err
	}
	code := inviteCode()

	ctx, cancel := detached(r)
	defer cancel()
	tx, err := rt.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`UPDATE access_requests SET status = 'approved', reviewed_at = NOW(), reviewer_notes = $1 WHERE id = $2`, notes, id); err != nil {
		return err
	}
	// The invite is in the approver's first realm.
	realm, err := rt.firstRealm(ctx, tx, User(r).ID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO invite_codes (code, created_by, access_request_id, expires_at, realm)
             VALUES ($1, $2, $3, NOW() + INTERVAL '30 days', $4)`,
		code, User(r).Username, id, realm); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	registerURL := rt.baseURL(r) + "/register?code=" + code
	rt.LogAdmin("access_request_approved", logger.Fields{
		{Key: "request_id", Value: f.Raw("id")}, {Key: "email", Value: email}, {Key: "user_id", Value: User(r).ID},
	})
	return httpapi.WriteJSON(w, http.StatusOK, struct {
		OK          bool    `json:"ok"`
		InviteCode  string  `json:"invite_code"`
		RegisterURL string  `json:"register_url"`
		Email       *string `json:"email"`
	}{true, code, registerURL, email})
}

// rowQuerier is a pool or a transaction.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// firstRealm is the actor's first realm, else llm-memory.
func (rt *Routes) firstRealm(ctx context.Context, q rowQuerier, actorID int) (string, error) {
	var realms []string
	err := q.QueryRow(ctx, "SELECT realms FROM actors WHERE id = $1", actorID).Scan(&realms)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if len(realms) > 0 {
		return realms[0], nil
	}
	return defaultRealm, nil
}

// accessRequestsReject is POST /admin/access-requests/reject.
func (rt *Routes) accessRequestsReject(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("id")
	}
	if !f.Truthy("id") {
		return httpapi.BadRequest(w, "Required field: id")
	}
	id := pgbind.Value(f.Raw("id"))
	if _, ok, err := rt.pendingRequest(w, r, id); !ok {
		return err
	}
	notes, err := optionalText(f, "notes")
	if err != nil {
		return err
	}
	ctx, cancel := detached(r)
	defer cancel()
	if _, err := rt.DB.Exec(ctx,
		`UPDATE access_requests SET status = 'rejected', reviewed_at = NOW(), reviewer_notes = $1 WHERE id = $2`, notes, id); err != nil {
		return err
	}
	rt.LogAdmin("access_request_rejected", logger.Fields{{Key: "request_id", Value: f.Raw("id")}, {Key: "user_id", Value: User(r).ID}})
	return httpapi.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// inviteCodesList is POST /admin/invite-codes: every invite code, newest
// first, with its access request's email.
func (rt *Routes) inviteCodesList(w http.ResponseWriter, r *http.Request) error {
	rows, err := pgjson.Query(r.Context(), rt.DB,
		`SELECT ic.id, ic.code, ic.created_at, ic.created_by, ic.used_by, ic.used_at, ic.expires_at,
                ar.email AS request_email
         FROM invite_codes ic
         LEFT JOIN access_requests ar ON ar.id = ic.access_request_id
         ORDER BY ic.created_at DESC`)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string][]pgjson.Row{"codes": rows})
}

// inviteCodesGenerate is POST /admin/invite-codes/generate: `count` codes
// (1 to 50, default 1) that expire after expires_days (none when absent),
// in `realm` or the creator's first realm. Each is its own insert, as in
// Node.
func (rt *Routes) inviteCodesGenerate(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		// V8's text for a destructured property with a default.
		return readError("count")
	}
	// `count = 1` applies only when count is absent; safeInt(null) is null,
	// and ?? 1 covers it.
	count := int64(1)
	if raw, present := f["count"]; present {
		if n, ok := SafeInt(raw); ok {
			count = n
		}
	}
	count = min(max(count, 1), maxInviteBatch)

	var expiresDays any
	if raw := string(f.Raw("expires_days")); raw != "" && raw != "null" && raw != `""` {
		n, ok := SafeInt(f.Raw("expires_days"))
		if !ok || n <= 0 || n > maxInviteExpiryDays {
			return httpapi.BadRequest(w, "expires_days must be a positive integer (max "+strconv.Itoa(maxInviteExpiryDays)+")")
		}
		expiresDays = n
	}

	realm, err := optionalText(f, "realm")
	if err != nil {
		return err
	}
	ctx, cancel := detached(r)
	defer cancel()
	if realm == nil {
		first, err := rt.firstRealm(ctx, rt.DB, User(r).ID)
		if err != nil {
			return err
		}
		realm = first
	}
	codes := []string{}
	for i := int64(0); i < count; i++ {
		code := inviteCode()
		if _, err := rt.DB.Exec(ctx,
			`INSERT INTO invite_codes (code, created_by, expires_at, realm)
             VALUES ($1, $2,
                     CASE WHEN $3::int IS NULL THEN NULL
                          ELSE NOW() + ($3::int * INTERVAL '1 day') END,
                     $4)`,
			code, User(r).Username, expiresDays, realm); err != nil {
			return err
		}
		codes = append(codes, code)
	}
	rt.LogAdmin("invite_codes_generated", logger.Fields{{Key: "count", Value: count}, {Key: "user_id", Value: User(r).ID}})
	return httpapi.WriteJSON(w, http.StatusOK, struct {
		OK    bool     `json:"ok"`
		Codes []string `json:"codes"`
	}{true, codes})
}

// inviteCodesDelete is POST /admin/invite-codes/delete: an unused code.
func (rt *Routes) inviteCodesDelete(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("id")
	}
	if !f.Truthy("id") {
		return httpapi.BadRequest(w, "Required field: id")
	}
	id := pgbind.Value(f.Raw("id"))
	var usedBy *string
	err := rt.DB.QueryRow(r.Context(), "SELECT used_by FROM invite_codes WHERE id = $1", id).Scan(&usedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound(w, "Invite code not found")
	}
	if err != nil {
		return err
	}
	if usedBy != nil && *usedBy != "" {
		return httpapi.BadRequest(w, "Cannot delete a used invite code")
	}
	ctx, cancel := detached(r)
	defer cancel()
	if _, err := rt.DB.Exec(ctx, "DELETE FROM invite_codes WHERE id = $1", id); err != nil {
		return err
	}
	rt.LogAdmin("invite_code_deleted", logger.Fields{{Key: "code_id", Value: f.Raw("id")}, {Key: "user_id", Value: User(r).ID}})
	return httpapi.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
