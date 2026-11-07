package registration

import (
	"context"
	"net/http"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/admin"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// Longest email and usage text an access request takes, in JavaScript
// .length units.
const (
	maxAccessEmail = 255
	maxAccessUsage = 5000
)

// accessWriteTimeout bounds the access request's writes. They run detached
// from the request: Node's queries ran on after a client hung up.
const accessWriteTimeout = 30 * time.Second

// accessRequest is server.js's POST /api/access-request, the landing page's
// request form. With open registration on, the request is approved at once
// and its invite code (7 days) is returned; otherwise it waits for an admin.
func (rt *Routes) accessRequest(w http.ResponseWriter, r *http.Request) {
	body := httpapi.JSONParams(r)
	if body == nil {
		rt.unhandled(w, httpapi.NoBody("email"))
		return
	}
	email, usage := body["email"], body["usage"]
	if !jsutil.Truthy(email) || !jsutil.Truthy(usage) {
		writeError(w, http.StatusBadRequest, "Email and usage description are required")
		return
	}
	if longerThan(email, maxAccessEmail) || longerThan(usage, maxAccessUsage) {
		writeError(w, http.StatusBadRequest, "Input too long")
		return
	}
	// From here Node is inside its try: anything thrown, a non-string field
	// on .trim() included, is the generic 500.
	open, err := rt.openRegistration()
	if err != nil {
		rt.accessFailed(w, err)
		return
	}
	emailText, ok := email.(string)
	if !ok {
		rt.accessFailed(w, notAFunction("email.trim"))
		return
	}
	usageText, ok := usage.(string)
	if !ok {
		rt.accessFailed(w, notAFunction("usage.trim"))
		return
	}
	emailText, usageText = jsutil.Trim(emailText), jsutil.Trim(usageText)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), accessWriteTimeout)
	defer cancel()

	if !open {
		if _, err := rt.DB.Exec(ctx,
			"INSERT INTO access_requests (email, usage_description) VALUES ($1, $2)",
			emailText, usageText); err != nil {
			rt.accessFailed(w, err)
			return
		}
		httpapi.WriteJSON(w, http.StatusOK, struct {
			OK bool `json:"ok"`
		}{true})
		return
	}

	code := admin.InviteCode()
	tx, err := rt.DB.Begin(ctx)
	if err != nil {
		rt.accessFailed(w, err)
		return
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var requestID int
	if err := tx.QueryRow(ctx,
		`INSERT INTO access_requests (email, usage_description, status, reviewed_at)
         VALUES ($1, $2, 'approved', NOW()) RETURNING id`,
		emailText, usageText).Scan(&requestID); err != nil {
		rt.accessFailed(w, err)
		return
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO invite_codes (code, created_by, access_request_id, expires_at)
         VALUES ($1, 'system', $2, NOW() + INTERVAL '7 days')`,
		code, requestID); err != nil {
		rt.accessFailed(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		rt.accessFailed(w, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		OK           bool   `json:"ok"`
		AutoApproved bool   `json:"auto_approved"`
		Code         string `json:"code"`
	}{true, true, code})
}

// longerThan is `value.length > limit` in JavaScript.
func longerThan(v any, limit float64) bool {
	length, comparable := jsLength(v)
	return comparable && length > limit
}

// accessFailed is the access request's catch block: the error goes to stderr
// and the client gets a generic 500.
func (rt *Routes) accessFailed(w http.ResponseWriter, err error) {
	rt.Log.Errorf("Access request error: %s", httpapi.ErrorMessage(err))
	writeError(w, http.StatusInternalServerError, "Something went wrong")
}
