package admin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/attempts"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/auth"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/pgbind"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/sanitize"
)

// dummySalt is hashed against when the account-link target is unknown, so a
// miss costs the same as a wrong password.
var dummySalt = auth.GenerateSalt()

// newSessionToken is 48 random bytes in base64url (64 characters).
func newSessionToken() string {
	b := make([]byte, 48)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// detached is a context for writes Node finished after the client hung up.
func detached(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), credentialWriteTimeout)
}

// readError is V8's TypeError for req.body.<field> on an undefined body.
func readError(field string) error {
	return errors.New("Cannot read properties of undefined (reading '" + field + "')")
}

// usernameField is sanitize.agentName(req.body.username): the sanitized
// string, and whether the field counts as given (a sanitized string that is
// not empty, or a truthy value of another type, which agentName returned as
// is). isString is false for that other type.
func usernameField(f httpapi.Fields) (username string, given, isString bool) {
	if s, ok := f.String("username"); ok {
		username = sanitize.AgentName(s)
		return username, username != "", true
	}
	return "", f.Truthy("username"), false
}

type loginResponse struct {
	SessionToken string                    `json:"session_token"`
	ExpiresAt    string                    `json:"expires_at"`
	User         httpapi.User              `json:"user"`
	Permissions  permissions.PermissionMap `json:"permissions"`
}

// login is POST /admin/login: a dashboard password login that creates a
// web session.
func (rt *Routes) login(w http.ResponseWriter, r *http.Request) {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		rt.unhandled(w, readError("username"))
		return
	}
	username, given, isString := usernameField(f)
	if !given || !f.Truthy("password") {
		httpapi.BadRequest(w, "Required fields: username, password")
		return
	}
	if !isString {
		httpapi.WriteError(w, http.StatusBadRequest, "BAD_REQUEST", "username must be a string")
		return
	}
	password, isString := f.String("password")
	if !isString {
		httpapi.WriteError(w, http.StatusBadRequest, "BAD_REQUEST", "password must be a string")
		return
	}

	account, err := auth.VerifyPasswordLogin(r.Context(), rt.DB, username, password)
	if err != nil {
		rt.fail(w, "Admin login error", "Login failed", err)
		return
	}
	if account == nil {
		rt.LogAdmin("login_failed", logger.Fields{{Key: "username", Value: username}})
		httpapi.WriteError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Invalid username or password")
		return
	}

	token := newSessionToken()
	salt := auth.GenerateSalt()
	tokenHash, err := auth.Hash(token, salt)
	if err != nil {
		rt.fail(w, "Admin login error", "Login failed", err)
		return
	}
	expiresAt := rt.now().Add(sessionTTL).Truncate(time.Millisecond)

	ctx, cancel := detached(r)
	defer cancel()
	if _, err := rt.DB.Exec(ctx,
		"INSERT INTO sessions (actor_id, token_hash, token_salt, token_lookup_hash, kind, expires_at) VALUES ($1, $2, $3, $4, $5, $6)",
		account.ID, tokenHash, salt, auth.TokenLookupHash(token), auth.SessionKindWeb, expiresAt); err != nil {
		rt.fail(w, "Admin login error", "Login failed", err)
		return
	}
	if _, err := rt.DB.Exec(ctx, "UPDATE actors SET last_seen = NOW() WHERE id = $1", account.ID); err != nil {
		rt.fail(w, "Admin login error", "Login failed", err)
		return
	}
	rt.LogAdmin("login", logger.Fields{{Key: "username", Value: username}, {Key: "user_id", Value: account.ID}})

	perms, err := rt.Admin.PermissionMap(r.Context(), account.ID)
	if err != nil {
		rt.fail(w, "Admin login error", "Login failed", err)
		return
	}
	if err := httpapi.WriteJSON(w, http.StatusOK, loginResponse{
		SessionToken: token,
		ExpiresAt:    jsutil.DateISO(expiresAt),
		User:         httpapi.User{ID: account.ID, Username: account.Name},
		Permissions:  perms,
	}); err != nil {
		rt.fail(w, "Admin login error", "Login failed", err)
	}
}

// logout is POST /admin/logout: deletes the web session of the token the
// request used, and only that one.
func (rt *Routes) logout(w http.ResponseWriter, r *http.Request) {
	user := User(r)
	token := strings.Replace(r.Header.Get("Authorization"), "Bearer ", "", 1)
	if token != "" {
		ctx, cancel := detached(r)
		defer cancel()
		// The cached entry goes even when the delete fails, so a session the
		// caller asked to end is looked up again on its next request.
		defer rt.SessionAuth.Forget(token)
		if _, err := rt.Sessions.DeleteByToken(ctx, token, auth.SessionKindWeb, user.ID); err != nil {
			rt.fail(w, "Admin logout error", "Logout failed", err)
			return
		}
	}
	rt.LogAdmin("logout", logger.Fields{{Key: "user_id", Value: user.ID}})
	httpapi.WriteJSON(w, http.StatusOK, map[string]string{"message": "Logged out"})
}

type linkResponse struct {
	Linked        string `json:"linked"`
	AlreadyLinked bool   `json:"already_linked"`
}

// linkAccount is POST /admin/link-account (services/account-link.js): the
// caller proves it knows another account's dashboard password, and the two
// get explicit visibility rows for each other.
//
// Every credential failure (unknown account, no dashboard password, wrong
// password) is the same 400 after the same hashing work, so the route does
// not tell which accounts exist. 400, not 401: the dashboard logs the user
// out on any 401.
func (rt *Routes) linkAccount(w http.ResponseWriter, r *http.Request) {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		rt.fail(w, "Admin link-account error", "Failed to link account", readError("username"))
		return
	}
	callerID := User(r).ID
	username, given, isString := usernameField(f)
	if !given || !f.Truthy("password") {
		httpapi.BadRequest(w, "Required fields: username, password")
		return
	}
	if !isString {
		httpapi.WriteError(w, http.StatusBadRequest, "BAD_REQUEST", "username must be a string")
		return
	}
	password, isString := f.String("password")
	if !isString {
		httpapi.WriteError(w, http.StatusBadRequest, "BAD_REQUEST", "password must be a string")
		return
	}

	// Every attempt counts for the window: a correct password refunds
	// nothing, or a caller could clear its count by linking an account it
	// controls.
	allowed, retryAfter := attempts.Acquire(
		attempts.Pair{Limiter: rt.linkCaller, Key: strconv.Itoa(callerID)},
		attempts.Pair{Limiter: rt.linkTarget, Key: username},
	)
	if !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		minutes := int(math.Ceil(float64(retryAfter) / 60))
		httpapi.WriteError(w, http.StatusTooManyRequests, "RATE_LIMITED", "Too many attempts. Try again in "+strconv.Itoa(minutes)+" minutes.")
		return
	}

	var (
		targetID   int
		targetName string
		hash, salt *string
	)
	err := rt.DB.QueryRow(r.Context(),
		"SELECT id, name, password_hash, password_salt FROM actors WHERE name = $1 AND password_hash IS NOT NULL",
		username).Scan(&targetID, &targetName, &hash, &salt)
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		rt.fail(w, "Admin link-account error", "Failed to link account", err)
		return
	}
	valid := false
	if found {
		if salt == nil {
			rt.fail(w, "Admin link-account error", "Failed to link account", auth.ErrNullSalt)
			return
		}
		valid = auth.VerifyLoose(password, *salt, *hash)
	} else {
		auth.Hash(password, dummySalt)
	}
	if !valid {
		rt.LogAdmin("account_link_failed", logger.Fields{{Key: "user_id", Value: callerID}, {Key: "username", Value: username}})
		httpapi.WriteError(w, http.StatusBadRequest, "INVALID_CREDENTIALS", "Username or password is incorrect")
		return
	}
	if targetID == callerID {
		httpapi.WriteError(w, http.StatusBadRequest, "BAD_REQUEST", "That is the account you are logged in as")
		return
	}

	// Visibility grants take effect only between actors that share a realm,
	// so a link across realms would save and show nothing.
	var one int
	err = rt.DB.QueryRow(r.Context(),
		"SELECT 1 FROM actors a1 JOIN actors a2 ON a1.realms && a2.realms WHERE a1.id = $1 AND a2.id = $2",
		callerID, targetID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		httpapi.WriteError(w, http.StatusConflict, "NO_SHARED_REALM", "These accounts are in different realms, so a link would not let them see each other. Ask an administrator to add them to a shared realm.")
		return
	}
	if err != nil {
		rt.fail(w, "Admin link-account error", "Failed to link account", err)
		return
	}

	// One statement, so both halves land or neither does. The unique
	// (actor_id, target_actor_id) makes an existing half a no-op; a wildcard
	// row is not touched.
	ctx, cancel := detached(r)
	defer cancel()
	tag, err := rt.DB.Exec(ctx,
		`INSERT INTO actor_visibility_configuration (actor_id, target_actor_id)
                     VALUES ($1, $2), ($2, $1)
                     ON CONFLICT (actor_id, target_actor_id) DO NOTHING`,
		callerID, targetID)
	if err != nil {
		rt.fail(w, "Admin link-account error", "Failed to link account", err)
		return
	}
	rowsAdded := tag.RowsAffected()
	rt.Visibility.ClearCache(callerID)
	rt.Visibility.ClearCache(targetID)
	rt.LogAdmin("account_link", logger.Fields{{Key: "user_id", Value: callerID}, {Key: "target_actor_id", Value: targetID}, {Key: "rows_added", Value: rowsAdded}})
	httpapi.WriteJSON(w, http.StatusOK, linkResponse{Linked: targetName, AlreadyLinked: rowsAdded == 0})
}

// changePassword is POST /admin/change-password: the caller's own
// dashboard password.
func (rt *Routes) changePassword(w http.ResponseWriter, r *http.Request) {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		rt.unhandled(w, httpapi.NoBody("current_password"))
		return
	}
	if !f.Truthy("current_password") || !f.Truthy("new_password") {
		httpapi.BadRequest(w, "Required fields: current_password, new_password")
		return
	}
	current, isString := f.String("current_password")
	if !isString {
		httpapi.WriteError(w, http.StatusBadRequest, "BAD_REQUEST", "current_password must be a string")
		return
	}
	next, isString := f.String("new_password")
	if !isString {
		httpapi.WriteError(w, http.StatusBadRequest, "BAD_REQUEST", "new_password must be a string")
		return
	}
	minText, err := rt.Config.Get("minimum_password_length")
	if err != nil {
		rt.unhandled(w, err)
		return
	}
	minLen, ok := jsutil.ParseInt(minText)
	if !ok || minLen == 0 {
		minLen = 10
	}
	if float64(jsutil.UTF16Len(next)) < minLen {
		httpapi.BadRequest(w, "Password must be at least "+jsutil.NumberString(minLen)+" characters")
		return
	}

	userID := User(r).ID
	var hash, salt *string
	err = rt.DB.QueryRow(r.Context(), "SELECT password_hash, password_salt FROM actors WHERE id = $1", userID).Scan(&hash, &salt)
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		rt.fail(w, "Admin change-password error", "Failed to change password", err)
		return
	}
	valid := false
	if found && hash != nil && *hash != "" {
		if salt == nil {
			rt.fail(w, "Admin change-password error", "Failed to change password", auth.ErrNullSalt)
			return
		}
		valid = auth.VerifyLoose(current, *salt, *hash)
	}
	// 400, not 401: the dashboard logs the user out on any 401.
	if !valid {
		httpapi.WriteError(w, http.StatusBadRequest, "INVALID_CREDENTIALS", "Current password is incorrect")
		return
	}

	newSalt := auth.GenerateSalt()
	newHash, err := auth.Hash(next, newSalt)
	if err != nil {
		rt.fail(w, "Admin change-password error", "Failed to change password", err)
		return
	}
	ctx, cancel := detached(r)
	defer cancel()
	if _, err := rt.DB.Exec(ctx, "UPDATE actors SET password_hash = $1, password_salt = $2 WHERE id = $3", newHash, newSalt, userID); err != nil {
		rt.fail(w, "Admin change-password error", "Failed to change password", err)
		return
	}
	rt.LogAdmin("password_changed", logger.Fields{{Key: "user_id", Value: userID}})
	httpapi.WriteJSON(w, http.StatusOK, map[string]string{"message": "Password changed"})
}

// profileVisibility is POST /admin/profile/visibility: reads or sets
// visible_to_others for actor_id, or for the caller. Node checks no
// permission and no ownership here; the port keeps that.
func (rt *Routes) profileVisibility(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return readError("actor_id")
	}
	// req.body.actor_id || req.actorId, bound as node-postgres bound it.
	var target any = httpapi.StateOf(r).ActorID
	var logTarget any = target
	if f.Truthy("actor_id") {
		target = pgbind.Value(f.Raw("actor_id"))
		logTarget = json.RawMessage(f.Raw("actor_id"))
	}
	if raw, present := f["visible_to_others"]; present {
		visible := pgbind.Truthy(raw)
		ctx, cancel := detached(r)
		defer cancel()
		if _, err := rt.DB.Exec(ctx, "UPDATE actors SET visible_to_others = $1 WHERE id = $2", visible, target); err != nil {
			return err
		}
		rt.LogAdmin("visibility_update", logger.Fields{
			{Key: "actor_id", Value: logTarget},
			{Key: "visible_to_others", Value: visible},
			{Key: "user_id", Value: User(r).ID},
		})
		return httpapi.WriteJSON(w, http.StatusOK, map[string]bool{"visible_to_others": visible})
	}
	var visible *bool
	err := rt.DB.QueryRow(r.Context(), "SELECT visible_to_others FROM actors WHERE id = $1", target).Scan(&visible)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string]bool{"visible_to_others": visible != nil && *visible})
}
