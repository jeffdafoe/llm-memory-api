// Package oauth is Node's routes/oauth.js: OAuth 2.1 for MCP clients.
//
// Two grant types:
//
//   - client_credentials, for machine-to-machine clients (Claude Code,
//     scripts): client_id is the agent name, client_secret one of its API keys.
//   - authorization_code with PKCE, for claude.ai. Its client is either an
//     agent name with an API key as secret (the connector's Advanced
//     settings: /authorize shows a connect page, then approves), or a public
//     client (CIMD or DCR, publicclients.go) with no secret, where the user
//     logs in on /authorize with agent name and password and the code is
//     bound to that agent.
//
// The tokens are deterministic HMACs ("agent:hmac_hex", auth.HMACToken) that
// never expire server-side.
package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/attempts"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/auth"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/sanitize"
)

// tokenTTLSeconds is the advertised lifetime; the token never expires
// server-side.
const tokenTTLSeconds = 86400

// loginWindow: at most 5 password tries per agent name and 20 per client
// address in 15 minutes. Every try counts, right or wrong. The per-name cap
// lets someone lock a name out for 15 minutes; that is the accepted cost of
// not letting them guess at it.
const loginWindow = 15 * time.Minute

// KeyFinder resolves an API key (auth.APIKeys).
type KeyFinder interface {
	Find(ctx context.Context, token string, actorID int) (*auth.APIKeyMatch, error)
}

// Routes are the OAuth endpoints. Create them with New.
type Routes struct {
	DB      db.DB
	Config  *config.Store
	Actors  *actors.Cache
	APIKeys KeyFinder
	Log     *logger.Logger
	// BaseURL is BASE_URL; empty means the request's own http://host.
	BaseURL string

	public         *PublicClients
	codes          *codeStore
	verifyPassword func(ctx context.Context, conn db.DB, username, password string) (*auth.PasswordAccount, error)
	agentLimiter   *attempts.Limiter
	addrLimiter    *attempts.Limiter
}

// New returns the routes with their in-memory state (codes, login limits).
func New(conn db.DB, store *config.Store, actorCache *actors.Cache, keys KeyFinder, log *logger.Logger, baseURL string) *Routes {
	rt := &Routes{DB: conn, Config: store, Actors: actorCache, APIKeys: keys, Log: log, BaseURL: baseURL}
	rt.verifyPassword = auth.VerifyPasswordLogin
	rt.public = &PublicClients{SigningSecret: func() (string, error) { return store.Get("mcp_oauth_bearer_secret") }}
	rt.codes = newCodeStore(nil)
	rt.agentLimiter = attempts.New(attempts.Options{MaxAttempts: 5, Window: loginWindow})
	rt.addrLimiter = attempts.New(attempts.Options{MaxAttempts: 20, Window: loginWindow})
	return rt
}

// Register adds the routes to the top-level mux.
func (rt *Routes) Register(m *httpapi.RawMux) {
	m.Handle(http.MethodGet, "/.well-known/oauth-protected-resource", http.HandlerFunc(rt.protectedResource))
	m.Handle(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", http.HandlerFunc(rt.protectedResourceMCP))
	m.Handle(http.MethodGet, "/.well-known/oauth-authorization-server", http.HandlerFunc(rt.authorizationServer))
	m.Handle(http.MethodPost, "/oauth/register", http.HandlerFunc(rt.register))
	m.Handle(http.MethodGet, "/authorize", http.HandlerFunc(rt.authorizeGet))
	m.Handle(http.MethodPost, "/authorize", http.HandlerFunc(rt.authorizePost))
	m.Handle(http.MethodPost, "/oauth/token", http.HandlerFunc(rt.token))
}

func (rt *Routes) baseURL(r *http.Request) string {
	if rt.BaseURL != "" {
		return rt.BaseURL
	}
	return "http://" + r.Host
}

// unhandled is an error Node's async handler threw: Express's error handler
// answered it, and nothing went to error_log.
func (rt *Routes) unhandled(w http.ResponseWriter, err error) {
	rt.Log.Errorf("Unhandled error: %v", err)
	httpapi.WriteUnhandledError(w, http.StatusInternalServerError, err.Error())
}

type oauthError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	httpapi.WriteJSON(w, status, oauthError{code, description})
}

// RFC 9728 protected resource metadata, root and /mcp path forms.
type resourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
}

func (rt *Routes) protectedResource(w http.ResponseWriter, r *http.Request) {
	base := rt.baseURL(r)
	httpapi.WriteJSON(w, http.StatusOK, resourceMetadata{base, []string{base}, []string{"header"}})
}

func (rt *Routes) protectedResourceMCP(w http.ResponseWriter, r *http.Request) {
	base := rt.baseURL(r)
	httpapi.WriteJSON(w, http.StatusOK, resourceMetadata{base + "/mcp", []string{base}, []string{"header"}})
}

// RFC 8414 authorization server metadata. claude.ai uses CIMD only when this
// advertises both client_id_metadata_document_supported and the "none" auth
// method; otherwise it registers at registration_endpoint.
type serverMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	ClientIDMetadataDocumentSupported bool     `json:"client_id_metadata_document_supported"`
}

func (rt *Routes) authorizationServer(w http.ResponseWriter, r *http.Request) {
	base := rt.baseURL(r)
	httpapi.WriteJSON(w, http.StatusOK, serverMetadata{
		Issuer:                            base,
		AuthorizationEndpoint:             base + "/authorize",
		TokenEndpoint:                     base + "/oauth/token",
		RegistrationEndpoint:              base + "/oauth/register",
		TokenEndpointAuthMethodsSupported: []string{"none", "client_secret_post"},
		GrantTypesSupported:               []string{"authorization_code", "client_credentials"},
		ResponseTypesSupported:            []string{"code"},
		CodeChallengeMethodsSupported:     []string{"S256"},
		ClientIDMetadataDocumentSupported: true,
	})
}

func jsonBody(r *http.Request) httpapi.Params {
	return httpapi.JSONParams(r)
}

// formOrJSONBody is req.body on a route with express.urlencoded: the JSON
// body when express.json took it, else the urlencoded one. defined is false
// when neither parser ran. A refused urlencoded body has been answered
// (answered true).
func (rt *Routes) formOrJSONBody(w http.ResponseWriter, r *http.Request) (p httpapi.Params, defined, answered bool) {
	if p := jsonBody(r); p != nil {
		return p, true, false
	}
	form, ok, err := httpapi.ReadForm(r)
	if err != nil {
		var fe *httpapi.FormError
		if errors.As(err, &fe) {
			rt.Log.Errorf("Unhandled error: %s", fe.Message)
			httpapi.WriteUnhandledError(w, fe.Status, fe.Message)
			return nil, false, true
		}
		rt.unhandled(w, err)
		return nil, false, true
	}
	if !ok {
		return nil, false, false
	}
	return form, true, false
}

func truthy(v any) bool {
	return jsutil.Truthy(v)
}

// register is RFC 7591 dynamic client registration, claude.ai's fallback
// when it does not use CIMD. Only public clients for the allowed callbacks
// are registered, and nothing is stored: the client_id carries its own
// signed registration. Not /register, which is the signup page.
func (rt *Routes) register(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	body := jsonBody(r)
	list, isArray := body["redirect_uris"].([]any)
	if !isArray || len(list) == 0 || len(list) > len(AllowedRedirectURIs) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris must list one or more allowed callback URLs")
		return
	}
	var uris []string
	for _, v := range list {
		s, ok := v.(string)
		if !ok || !allowedRedirect(s) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris may only contain: "+strings.Join(AllowedRedirectURIs, ", "))
			return
		}
		uris = append(uris, s)
	}
	clientName := json.RawMessage(`"Claude"`)
	if s, ok := body["client_name"].(string); ok && jsutil.Trim(s) != "" {
		clientName = sliceUTF16JSON(jsutil.Trim(s), 100)
	}
	issuedAt := time.Now().Unix()
	unique := dedupe(uris)
	clientID, err := rt.public.IssueDCRClientID(unique, issuedAt)
	if err != nil {
		rt.Log.LogError("oauth", "register", logger.ErrorOpts{Message: err.Error(), Detail: fmt.Sprintf("%+v", err), StatusCode: 500})
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "Registration failed")
		return
	}
	// Requested values not granted (a secret auth method, refresh_token) are
	// replaced in the response, which RFC 7591 section 3.2.1 allows.
	httpapi.WriteJSON(w, http.StatusCreated, struct {
		ClientID                string          `json:"client_id"`
		ClientIDIssuedAt        int64           `json:"client_id_issued_at"`
		ClientName              json.RawMessage `json:"client_name"`
		RedirectURIs            []string        `json:"redirect_uris"`
		GrantTypes              []string        `json:"grant_types"`
		ResponseTypes           []string        `json:"response_types"`
		TokenEndpointAuthMethod string          `json:"token_endpoint_auth_method"`
	}{clientID, issuedAt, clientName, unique, []string{"authorization_code"}, []string{"code"}, "none"})
}

func dedupe(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range list {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// sliceUTF16JSON is JSON.stringify(s.slice(0, n)): the first n UTF-16 code
// units, where a cut through a surrogate pair leaves the lone high surrogate,
// which JSON.stringify writes as a \u escape.
func sliceUTF16JSON(s string, n int) json.RawMessage {
	units := utf16.Encode([]rune(s))
	if len(units) <= n {
		out, _ := jsonutil.Marshal(s)
		return out
	}
	cut := units[:n]
	lone := ""
	if last := cut[n-1]; last >= 0xD800 && last <= 0xDBFF {
		cut = cut[:n-1]
		lone = fmt.Sprintf(`\u%04x`, last)
	}
	out, _ := jsonutil.Marshal(string(utf16.Decode(cut)))
	if lone != "" {
		out = append(append(out[:len(out)-1:len(out)-1], lone...), '"')
	}
	return out
}

// sliceUTF16 is s.slice(0, n) as it reaches an HTML page: a cut through a
// surrogate pair leaves a lone surrogate, which UTF-8 output writes as
// U+FFFD.
func sliceUTF16(s string, n int) string {
	units := utf16.Encode([]rune(s))
	if len(units) <= n {
		return s
	}
	cut := units[:n]
	tail := ""
	if last := cut[n-1]; last >= 0xD800 && last <= 0xDBFF {
		cut = cut[:n-1]
		tail = "�"
	}
	return string(utf16.Decode(cut)) + tail
}

// checkAuthorize holds the checks GET and POST /authorize share. It returns
// an OAuth error body for a 400, or the public client kind ("cimd", "dcr",
// or "" for an agent-name client_id). Errors are shown here rather than
// redirected to redirect_uri, which is not trusted until it has passed them.
func (rt *Routes) checkAuthorize(p httpapi.Params) (*oauthError, string, error) {
	if s, _ := p.String("response_type"); s != "code" || !isString(p["response_type"]) {
		return &oauthError{"unsupported_response_type", "Only response_type=code is supported"}, "", nil
	}
	clientID, ok1 := p.String("client_id")
	redirectURI, ok2 := p.String("redirect_uri")
	if !ok1 || clientID == "" || !ok2 || redirectURI == "" {
		return &oauthError{"invalid_request", "client_id and redirect_uri are required"}, "", nil
	}
	if !allowedRedirect(redirectURI) {
		return &oauthError{"invalid_request", "redirect_uri is not allowed"}, "", nil
	}
	challenge, ok := p.String("code_challenge")
	method, _ := p.String("code_challenge_method")
	if !ok || challenge == "" || method != "S256" {
		return &oauthError{"invalid_request", "code_challenge with S256 method is required"}, "", nil
	}
	kind, err := rt.public.Kind(clientID)
	if err != nil {
		return nil, "", err
	}
	// A DCR client may only use the callbacks it registered.
	if kind == "dcr" {
		uris, _, _ := rt.public.VerifyDCRClientID(clientID)
		registered := false
		for _, u := range uris {
			if u == redirectURI {
				registered = true
			}
		}
		if !registered {
			return &oauthError{"invalid_request", "redirect_uri was not registered for this client"}, "", nil
		}
	}
	return nil, kind, nil
}

func isString(v any) bool {
	_, ok := v.(string)
	return ok
}

// oauthFields are the authorize parameters the login form carries through.
func oauthFields(p httpapi.Params) []oauthField {
	names := []string{"response_type", "client_id", "redirect_uri", "state", "code_challenge", "code_challenge_method"}
	out := make([]oauthField, len(names))
	for i, n := range names {
		out[i] = oauthField{n, p[n]}
	}
	return out
}

// redirectHost is new URL(redirect_uri).host for an allowed redirect URI.
func redirectHost(uri string) string {
	rest := strings.TrimPrefix(uri, "https://")
	host, _, _ := strings.Cut(rest, "/")
	return host
}

// setPageHeaders: pages that take a password must not be framed
// (clickjacking) or cached.
func setPageHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
}

func writeHTML(w http.ResponseWriter, status int, page string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(page)))
	w.WriteHeader(status)
	w.Write([]byte(page))
}

// mintAndRedirect stores a code and sends the browser back to redirectURI
// with it (and the state, when it is a non-empty string).
func (rt *Routes) mintAndRedirect(w http.ResponseWriter, r *http.Request, status int, c authCode, state any) {
	// The Location is built from the allowlist's own string, never the
	// request's, so it cannot point anywhere else even if a caller skipped
	// checkAuthorize.
	target, ok := allowedRedirectTarget(c.redirectURI)
	if !ok {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "redirect_uri is not allowed")
		return
	}
	code := rt.codes.mint(c)
	pairs := setSearch(nil, "code", code)
	if s, ok := state.(string); ok && s != "" {
		pairs = setSearch(pairs, "state", s)
	}
	redirect(w, r, status, target+"?"+serializeSearch(pairs))
}

// authorizeGet starts the authorization_code flow. A public client gets the
// login page. An agent-name client first gets the connect page, whose
// Continue link is this URL plus confirmed=1; only that second pass mints
// the code, because a code lives 60 seconds, less than a user may spend on
// the page.
func (rt *Routes) authorizeGet(w http.ResponseWriter, r *http.Request) {
	q := httpapi.Query(r)
	bad, kind, err := rt.checkAuthorize(q)
	if err != nil {
		rt.unhandled(w, err)
		return
	}
	if bad != nil {
		httpapi.WriteJSON(w, http.StatusBadRequest, bad)
		return
	}
	redirectURI, _ := q.String("redirect_uri")
	if kind != "" {
		setPageHeaders(w)
		writeHTML(w, http.StatusOK, renderLoginPage(redirectHost(redirectURI), oauthFields(q), "", ""))
		return
	}

	clientID, _ := q.String("client_id")
	agentName, found, err := rt.agentExists(r.Context(), clientID)
	if err != nil {
		rt.unhandled(w, err)
		return
	}
	if !found {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "Unknown or inactive agent")
		return
	}

	if confirmed, ok := q.String("confirmed"); !ok || confirmed != "1" {
		pairs := setSearch(parseSearch(httpapi.RawQuery(r)), "confirmed", "1")
		setPageHeaders(w)
		writeHTML(w, http.StatusOK, renderConnectPage(agentName, "/authorize?"+serializeSearch(pairs)))
		return
	}

	challenge, _ := q.String("code_challenge")
	method, _ := q.String("code_challenge_method")
	rt.mintAndRedirect(w, r, http.StatusFound, authCode{
		clientID: clientID, redirectURI: redirectURI, codeChallenge: challenge, codeChallengeMethod: method,
	}, q["state"])
}

// agentExists resolves an agent-name client_id and reads its stored name.
// The status column is read but, as in Node, not checked.
func (rt *Routes) agentExists(ctx context.Context, clientID string) (string, bool, error) {
	actor, found, err := rt.Actors.ResolveByName(ctx, clientID)
	if err != nil || !found {
		return "", false, err
	}
	var name string
	var status *string
	err = rt.DB.QueryRow(ctx, "SELECT ac.name AS agent, ac.status FROM actors ac WHERE ac.id = $1", actor.ID).Scan(&name, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return name, true, nil
}

// authorizePost is the login form's POST (public clients only). The whole
// OAuth request is checked again (the hidden fields came back from the
// browser), then the rate limits, then the password. A failure shows the
// form again with the agent name kept; the password is never echoed.
func (rt *Routes) authorizePost(w http.ResponseWriter, r *http.Request) {
	body, _, answered := rt.formOrJSONBody(w, r)
	if answered {
		return
	}
	if body == nil {
		body = httpapi.Params{}
	}
	bad, kind, err := rt.checkAuthorize(body)
	if err != nil {
		rt.unhandled(w, err)
		return
	}
	if bad != nil {
		httpapi.WriteJSON(w, http.StatusBadRequest, bad)
		return
	}
	if kind == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "Login is only for connectors added without a client secret")
		return
	}

	agentInput, _ := body.String("agent")
	agent := sanitize.AgentName(agentInput)
	password, _ := body.String("password")
	redirectURI, _ := body.String("redirect_uri")
	showForm := func(status int, message string) {
		setPageHeaders(w)
		writeHTML(w, status, renderLoginPage(redirectHost(redirectURI), oauthFields(body), sliceUTF16(agentInput, 64), message))
	}

	if agent == "" || password == "" {
		showForm(http.StatusBadRequest, "Enter your agent name and password.")
		return
	}

	address := httpapi.ClientAddress(r)
	allowed, wait := attempts.Acquire(attempts.Pair{Limiter: rt.agentLimiter, Key: agent}, attempts.Pair{Limiter: rt.addrLimiter, Key: address})
	if !allowed {
		minutes := int(math.Max(1, math.Ceil(float64(wait)/60)))
		plural := "s"
		if minutes == 1 {
			plural = ""
		}
		w.Header().Set("Retry-After", strconv.Itoa(wait))
		rt.Log.Log("oauth", "connector_login_limited", logger.Fields{{Key: "agent", Value: agent}, {Key: "address", Value: address}})
		showForm(http.StatusTooManyRequests, fmt.Sprintf("Too many tries. Wait %d minute%s and try again.", minutes, plural))
		return
	}

	login, err := rt.verifyPassword(r.Context(), rt.DB, agent, password)
	if err != nil {
		rt.Log.LogError("oauth", "connector-login", logger.ErrorOpts{Agent: agent, Message: err.Error(), Detail: fmt.Sprintf("%+v", err), StatusCode: 500})
		showForm(http.StatusInternalServerError, "Something went wrong. Try again.")
		return
	}
	if login == nil {
		rt.Log.Log("oauth", "connector_login_failed", logger.Fields{{Key: "agent", Value: agent}, {Key: "address", Value: address}})
		showForm(http.StatusUnauthorized, "Wrong agent name or password.")
		return
	}

	rt.Log.Log("oauth", "connector_login", logger.Fields{{Key: "agent", Value: login.Name}, {Key: "client_kind", Value: kind}})
	clientID, _ := body.String("client_id")
	challenge, _ := body.String("code_challenge")
	method, _ := body.String("code_challenge_method")
	// 303: the browser follows a POST's redirect with a GET.
	rt.mintAndRedirect(w, r, http.StatusSeeOther, authCode{
		clientID: clientID, redirectURI: redirectURI, codeChallenge: challenge, codeChallengeMethod: method, agent: login.Name,
	}, body["state"])
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

// issueToken is the deterministic bearer token for an agent.
func (rt *Routes) issueToken(agent string) (tokenResponse, error) {
	secret, err := rt.Config.Get("mcp_oauth_bearer_secret")
	if err != nil {
		return tokenResponse{}, err
	}
	return tokenResponse{auth.HMACToken(secret, agent), "Bearer", tokenTTLSeconds}, nil
}

// validateClient checks a client_id + client_secret pair: the agent's
// unrevoked API keys, by the indexed lookup. Returns the agent name, or ""
// on a mismatch. A client_id that is not a string never names an actor here.
func (rt *Routes) validateClient(ctx context.Context, clientID, secret any) (string, error) {
	name, ok := clientID.(string)
	if !ok {
		return "", nil
	}
	actor, found, err := rt.Actors.ResolveByName(ctx, name)
	if err != nil || !found {
		return "", err
	}
	token, ok := secret.(string)
	if !ok {
		return "", nil
	}
	match, err := rt.APIKeys.Find(ctx, token, actor.ID)
	if err != nil || match == nil {
		return "", err
	}
	return name, nil
}

// token is the token endpoint for both grant types.
func (rt *Routes) token(w http.ResponseWriter, r *http.Request) {
	body, defined, answered := rt.formOrJSONBody(w, r)
	if answered {
		return
	}
	if !defined {
		rt.unhandled(w, httpapi.NoBody("grant_type"))
		return
	}
	grant, _ := body.String("grant_type")
	switch {
	case isString(body["grant_type"]) && grant == "client_credentials":
		rt.clientCredentials(w, r, body)
	case isString(body["grant_type"]) && grant == "authorization_code":
		rt.authorizationCode(w, r, body)
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "Supported grant types: authorization_code, client_credentials")
	}
}

func agentText(v any) string {
	s, _ := v.(string)
	return s
}

// clientCredentials: machine-to-machine auth for Claude Code.
func (rt *Routes) clientCredentials(w http.ResponseWriter, r *http.Request, body httpapi.Params) {
	if !truthy(body["client_id"]) || !truthy(body["client_secret"]) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "client_id and client_secret are required")
		return
	}
	fail := func(err error) {
		rt.Log.LogError("oauth", "client-credentials", logger.ErrorOpts{Agent: agentText(body["client_id"]), Message: err.Error(), Detail: fmt.Sprintf("%+v", err), StatusCode: 500})
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "Token generation failed")
	}
	agent, err := rt.validateClient(r.Context(), body["client_id"], body["client_secret"])
	if err != nil {
		fail(err)
		return
	}
	if agent == "" {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "Invalid client_id or client_secret")
		return
	}
	tok, err := rt.issueToken(agent)
	if err != nil {
		fail(err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, tok)
}

// authorizationCode: the browser flow for claude.ai, with PKCE always.
func (rt *Routes) authorizationCode(w http.ResponseWriter, r *http.Request, body httpapi.Params) {
	// client_secret is checked below, once the code says which kind of
	// client this is: a public client has none.
	if !truthy(body["code"]) || !truthy(body["code_verifier"]) || !truthy(body["client_id"]) || !truthy(body["redirect_uri"]) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "code, code_verifier, client_id, and redirect_uri are required")
		return
	}

	// Look up and spend the code (one use).
	var data authCode
	found := false
	if code, ok := body["code"].(string); ok {
		data, found = rt.codes.take(code)
	}
	if !found {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "Invalid or expired authorization code")
		return
	}
	if uri, ok := body["redirect_uri"].(string); !ok || uri != data.redirectURI {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri mismatch")
		return
	}
	if id, ok := body["client_id"].(string); !ok || id != data.clientID {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "client_id mismatch")
		return
	}

	// S256: BASE64URL(SHA256(code_verifier)) must equal code_challenge.
	verifier, ok := body["code_verifier"].(string)
	if !ok {
		// Node's hash.update threw on a value that is not a string.
		rt.unhandled(w, errors.New(`The "data" argument must be of type string or an instance of Buffer, TypedArray, or DataView. `+jsutil.Received(body["code_verifier"])))
		return
	}
	sum := sha256.Sum256([]byte(verifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != data.codeChallenge {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "PKCE code_verifier validation failed")
		return
	}

	// Public client: the user proved who they are by logging in on
	// /authorize, and PKCE proves this request comes from whoever started
	// that flow. The token is for the agent bound into the code, never one
	// the client names.
	if data.agent != "" {
		tok, err := rt.issueToken(data.agent)
		if err != nil {
			rt.Log.LogError("oauth", "authorization-code", logger.ErrorOpts{Agent: data.agent, Message: err.Error(), Detail: fmt.Sprintf("%+v", err), StatusCode: 500})
			writeOAuthError(w, http.StatusInternalServerError, "server_error", "Token generation failed")
			return
		}
		httpapi.WriteJSON(w, http.StatusOK, tok)
		return
	}

	if !truthy(body["client_secret"]) {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "client_secret is required")
		return
	}
	fail := func(err error) {
		rt.Log.LogError("oauth", "authorization-code", logger.ErrorOpts{Agent: agentText(body["client_id"]), Message: err.Error(), Detail: fmt.Sprintf("%+v", err), StatusCode: 500})
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "Token generation failed")
	}
	agent, err := rt.validateClient(r.Context(), data.clientID, body["client_secret"])
	if err != nil {
		fail(err)
		return
	}
	if agent == "" {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "Invalid client_secret")
		return
	}
	tok, err := rt.issueToken(data.clientID)
	if err != nil {
		fail(err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, tok)
}
