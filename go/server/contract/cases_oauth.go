package contract

import (
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strings"
)

// oauthCases cover the OAuth routes (metadata, dynamic registration,
// /authorize for both client kinds, the token endpoint). agent-a has a
// signup password in the fixture (fixture_oauth.go).

// AgentAPassword is agent-a's signup password.
const AgentAPassword = "agent-a-signup-password"

// The fixture agents, written out because this file has no build tag.
const (
	agentA = "agent-a"
	agentB = "agent-b"
)

const (
	callback    = "https://claude.ai/api/mcp/auth_callback"
	callbackCOM = "https://claude.com/api/mcp/auth_callback"
	cimdID      = "https://claude.ai/oauth/mcp-oauth-client-metadata"
	verifier    = "contract-verifier-0123456789-abcdefghijklmnopqrstuvwxyz"
)

// challenge is the S256 PKCE challenge of verifier.
var challenge = func() string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}()

var formType = [][2]string{{"Content-Type", "application/x-www-form-urlencoded"}}

// authorizeQuery is the authorize request for clientID with overrides
// (an empty value removes the parameter).
func authorizeQuery(clientID string, overrides ...string) string {
	v := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {callback},
		"state":                 {"state-123"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	for i := 0; i+1 < len(overrides); i += 2 {
		if overrides[i+1] == "" {
			v.Del(overrides[i])
			continue
		}
		v.Set(overrides[i], overrides[i+1])
	}
	// A {{capture}} placeholder must reach the runner unescaped; the values
	// it stands for (a DCR client_id) need no escaping.
	return strings.NewReplacer("%7B%7B", "{{", "%7D%7D", "}}").Replace(v.Encode())
}

func authorizeGet(clientID string, overrides ...string) Step {
	return Step{Method: "GET", Path: "/authorize?" + authorizeQuery(clientID, overrides...)}
}

// mintAgentCode is the confirmed authorize pass for agent-a, capturing the
// code.
func mintAgentCode(overrides ...string) Step {
	st := authorizeGet(agentA, append([]string{"confirmed", "1"}, overrides...)...)
	st.CaptureLocation = map[string]string{"code": "code"}
	return st
}

func tokenForm(fields string) Step {
	return Step{Method: "POST", Path: "/oauth/token", Header: formType, Body: fields}
}

func codeExchange(extra string) Step {
	return tokenForm("grant_type=authorization_code&code={{code}}&code_verifier=" + verifier +
		"&redirect_uri=" + url.QueryEscape(callback) + extra)
}

// loginForm posts the login form for a public client.
func loginForm(clientID, agent, password string, extraHeader ...[2]string) Step {
	body := authorizeQuery(clientID) + "&agent=" + url.QueryEscape(agent) + "&password=" + url.QueryEscape(password)
	return Step{Method: "POST", Path: "/authorize", Header: append([][2]string{formType[0]}, extraHeader...), Body: body}
}

var registerDCR = Step{Method: "POST", Path: "/oauth/register", Header: jsonType,
	Body:    `{"client_name":"Claude","redirect_uris":["` + callback + `"]}`,
	Capture: map[string]string{"client_id": "client_id"}, Random: []string{"client_id_issued_at"}}

var oauthCases = concat(oauthMetadataCases, oauthRegisterCases, oauthAgentClientCases, oauthPublicClientCases, oauthTokenCases)

var oauthMetadataCases = []Scenario{
	one("oauth protected resource", Step{Method: "GET", Path: "/.well-known/oauth-protected-resource"}),
	one("oauth protected resource mcp", Step{Method: "GET", Path: "/.well-known/oauth-protected-resource/mcp"}),
	one("oauth authorization server", Step{Method: "GET", Path: "/.well-known/oauth-authorization-server"}),
	one("oauth authorization server HEAD", Step{Method: "HEAD", Path: "/.well-known/oauth-authorization-server"}),
	one("oauth authorization server OPTIONS", Step{Method: "OPTIONS", Path: "/.well-known/oauth-authorization-server"}),
	one("oauth authorization server POST", Step{Method: "POST", Path: "/.well-known/oauth-authorization-server", Header: jsonType, Body: `{}`}),
	one("oauth metadata encoded path", Step{Method: "GET", Path: "/.well-known/oauth-%61uthorization-server"}),
	one("oauth authorize OPTIONS", Step{Method: "OPTIONS", Path: "/authorize"}),
	one("oauth token GET", Step{Method: "GET", Path: "/oauth/token"}),
	one("oauth token encoded path", Step{Method: "POST", Path: "/oauth/%74oken", Header: formType, Body: "grant_type=x"}),
}

var oauthRegisterCases = []Scenario{
	one("oauth register", Step{Method: "POST", Path: "/oauth/register", Header: jsonType,
		Body:   `{"client_name":"  My Claude  ","redirect_uris":["` + callback + `","` + callback + `"],"grant_types":["refresh_token"],"token_endpoint_auth_method":"client_secret_basic"}`,
		Random: []string{"client_id_issued_at"}}),
	one("oauth register default name", Step{Method: "POST", Path: "/oauth/register", Header: jsonType,
		Body: `{"client_name":"   ","redirect_uris":["` + callbackCOM + `"]}`, Random: []string{"client_id_issued_at"}}),
	one("oauth register long name", Step{Method: "POST", Path: "/oauth/register", Header: jsonType,
		Body: `{"client_name":"` + strings.Repeat("n", 99) + `😀tail","redirect_uris":["` + callback + `"]}`, Random: []string{"client_id_issued_at"}}),
	one("oauth register evil callback", Step{Method: "POST", Path: "/oauth/register", Header: jsonType,
		Body: `{"redirect_uris":["` + callback + `","https://evil.example/cb"]}`}),
	one("oauth register both callbacks", Step{Method: "POST", Path: "/oauth/register", Header: jsonType,
		Body: `{"redirect_uris":["` + callbackCOM + `","` + callback + `"]}`, Random: []string{"client_id_issued_at"}}),
	one("oauth register three callbacks", Step{Method: "POST", Path: "/oauth/register", Header: jsonType,
		Body: `{"redirect_uris":["` + callback + `","` + callback + `","` + callback + `"]}`}),
	one("oauth register empty list", Step{Method: "POST", Path: "/oauth/register", Header: jsonType, Body: `{"redirect_uris":[]}`}),
	one("oauth register not a list", Step{Method: "POST", Path: "/oauth/register", Header: jsonType, Body: `{"redirect_uris":"` + callback + `"}`}),
	one("oauth register array body", Step{Method: "POST", Path: "/oauth/register", Header: jsonType, Body: `[1]`}),
	one("oauth register form body", Step{Method: "POST", Path: "/oauth/register", Header: formType, Body: "redirect_uris=" + url.QueryEscape(callback)}),
	one("oauth register no body", Step{Method: "POST", Path: "/oauth/register"}),
}

var oauthAgentClientCases = []Scenario{
	one("authorize agent connect page", authorizeGet(agentA)),
	one("authorize agent connect page keeps parameters", Step{Method: "GET",
		Path: "/authorize?" + authorizeQuery(agentA, "state", "a b+c/d?e=f&g", "confirmed", "0") + "&resource=https%3A%2F%2Fx.example%2Fmcp&resource=%zz&empty&=v"}),
	one("authorize agent connect page HEAD", Step{Method: "HEAD", Path: "/authorize?" + authorizeQuery(agentA)}),
	one("authorize unknown agent", authorizeGet("nobody-here")),
	one("authorize unknown agent confirmed", authorizeGet("nobody-here", "confirmed", "1")),
	one("authorize evil redirect", authorizeGet(agentA, "redirect_uri", "https://evil.example/cb")),
	one("authorize plain method", authorizeGet(agentA, "code_challenge_method", "plain")),
	one("authorize no challenge", authorizeGet(agentA, "code_challenge", "")),
	one("authorize token response type", authorizeGet(agentA, "response_type", "token")),
	one("authorize repeated response type", Step{Method: "GET", Path: "/authorize?" + authorizeQuery(agentA) + "&response_type=code"}),
	one("authorize no query", Step{Method: "GET", Path: "/authorize"}),
	one("authorize encoded query", Step{Method: "GET", Path: "/authorize?response%5Ftype=code&client%5Fid=agent%2Da&redirect_uri=" +
		url.QueryEscape(callback) + "&code_challenge=" + challenge + "&code_challenge_method=S%32%356&state=%E2%9C%93%zz"}),
	{
		Name: "authorize agent code redirect and exchange",
		Steps: []Step{
			mintAgentCode("state", `st<"&> +`),
			codeExchange("&client_id=agent-a&client_secret={{agent-a-key}}"),
		},
	},
	{
		Name: "authorize agent code redirect with html accept",
		Steps: []Step{
			func() Step {
				st := mintAgentCode()
				st.Header = [][2]string{{"Accept", "text/html,application/xhtml+xml,*/*;q=0.8"}}
				return st
			}(),
		},
	},
	{
		Name:  "authorize agent code redirect with json accept",
		Steps: []Step{func() Step { st := mintAgentCode(); st.Header = [][2]string{{"Accept", "application/json"}}; return st }()},
	},
	{
		Name: "token agent code reused",
		Steps: []Step{
			mintAgentCode(),
			codeExchange("&client_id=agent-a&client_secret={{agent-a-key}}"),
			codeExchange("&client_id=agent-a&client_secret={{agent-a-key}}"),
		},
	},
	{
		Name:  "token agent code without secret",
		Steps: []Step{mintAgentCode(), codeExchange("&client_id=agent-a")},
	},
	{
		Name:  "token agent code wrong secret",
		Steps: []Step{mintAgentCode(), codeExchange("&client_id=agent-a&client_secret={{agent-b-key}}")},
	},
	{
		Name:  "token agent code wrong client",
		Steps: []Step{mintAgentCode(), codeExchange("&client_id=agent-b&client_secret={{agent-b-key}}")},
	},
	{
		Name: "token agent code wrong verifier",
		Steps: []Step{mintAgentCode(), tokenForm("grant_type=authorization_code&code={{code}}&code_verifier=wrong&client_id=agent-a&client_secret={{agent-a-key}}&redirect_uri=" +
			url.QueryEscape(callback))},
	},
	{
		Name: "token agent code wrong redirect",
		Steps: []Step{mintAgentCode(), tokenForm("grant_type=authorization_code&code={{code}}&code_verifier=" + verifier +
			"&client_id=agent-a&client_secret={{agent-a-key}}&redirect_uri=" + url.QueryEscape(callbackCOM))},
	},
	{
		Name: "token agent code verifier not a string",
		Steps: []Step{mintAgentCode(), {Method: "POST", Path: "/oauth/token", Header: jsonType,
			Body: `{"grant_type":"authorization_code","code":"{{code}}","code_verifier":5,"client_id":"agent-a","redirect_uri":"` + callback + `"}`}},
	},
	{
		Name: "token agent code as json",
		Steps: []Step{mintAgentCode(), {Method: "POST", Path: "/oauth/token", Header: jsonType,
			Body: `{"grant_type":"authorization_code","code":"{{code}}","code_verifier":"` + verifier + `","client_id":"agent-a","client_secret":"{{agent-a-key}}","redirect_uri":"` + callback + `"}`}},
	},
}

var oauthPublicClientCases = []Scenario{
	one("authorize cimd login page", authorizeGet(cimdID, "state", `"><script>alert(1)</script>`)),
	one("authorize cimd on claude.com", authorizeGet("https://claude.com/x", "redirect_uri", callbackCOM)),
	one("authorize cimd look-alike", authorizeGet("https://claude.ai.evil.example/x")),
	one("authorize cimd blob url", authorizeGet("blob:https://claude.ai/x")),
	one("authorize cimd no path", authorizeGet("https://claude.ai/")),
	{
		Name:  "authorize dcr login page",
		Steps: []Step{registerDCR, authorizeGet("{{client_id}}")},
	},
	{
		Name:  "authorize dcr unregistered callback",
		Steps: []Step{registerDCR, authorizeGet("{{client_id}}", "redirect_uri", callbackCOM)},
	},
	{
		Name:  "authorize dcr tampered",
		Steps: []Step{registerDCR, authorizeGet("{{client_id}}x")},
	},
	{
		Name: "login cimd and exchange",
		Steps: []Step{
			func() Step {
				st := loginForm(cimdID, " Agent-A ", AgentAPassword)
				st.CaptureLocation = map[string]string{"code": "code"}
				return st
			}(),
			codeExchange("&client_id=" + url.QueryEscape(cimdID)),
		},
	},
	{
		Name: "login dcr and exchange",
		Steps: []Step{
			registerDCR,
			func() Step {
				st := loginForm("{{client_id}}", agentA, AgentAPassword)
				st.CaptureLocation = map[string]string{"code": "code"}
				return st
			}(),
			tokenForm("grant_type=authorization_code&code={{code}}&code_verifier=" + verifier + "&client_id={{client_id}}&redirect_uri=" + url.QueryEscape(callback)),
		},
	},
	{
		Name: "login public code for another client",
		Steps: []Step{
			func() Step {
				st := loginForm(cimdID, agentA, AgentAPassword)
				st.CaptureLocation = map[string]string{"code": "code"}
				return st
			}(),
			codeExchange("&client_id=" + url.QueryEscape("https://claude.ai/oauth/other")),
		},
	},
	one("login wrong password", loginForm(cimdID, agentA, "not the password")),
	one("login unknown agent", loginForm(cimdID, "nobody-here", AgentAPassword)),
	one("login agent without password", loginForm(cimdID, agentB, AgentAPassword)),
	one("login empty agent", loginForm(cimdID, "!!!", AgentAPassword)),
	one("login long agent kept", loginForm(cimdID, strings.Repeat("x", 63)+"😀", "")),
	one("login agent-name client", loginForm(agentA, agentA, AgentAPassword)),
	{
		Name: "login five tries per name",
		Steps: []Step{
			loginForm(cimdID, agentA, "guess-1", [2]string{"X-Forwarded-For", "203.0.113.1"}),
			loginForm(cimdID, agentA, "guess-2", [2]string{"X-Forwarded-For", "203.0.113.2"}),
			loginForm(cimdID, agentA, "guess-3", [2]string{"X-Forwarded-For", "203.0.113.3"}),
			loginForm(cimdID, agentA, "guess-4", [2]string{"X-Forwarded-For", "203.0.113.4"}),
			loginForm(cimdID, agentA, "guess-5", [2]string{"X-Forwarded-For", "203.0.113.5"}),
			loginForm(cimdID, agentA, AgentAPassword, [2]string{"X-Forwarded-For", "203.0.113.6"}),
		},
	},
	one("login json body", Step{Method: "POST", Path: "/authorize", Header: jsonType,
		Body: `{"response_type":"code","client_id":"` + cimdID + `","redirect_uri":"` + callback + `","code_challenge":"` + challenge + `","code_challenge_method":"S256","agent":"agent-a","password":5}`}),
	one("login no body", Step{Method: "POST", Path: "/authorize"}),
	one("login bad charset", Step{Method: "POST", Path: "/authorize", Header: [][2]string{{"Content-Type", "application/x-www-form-urlencoded; charset=utf-16"}}, Body: "a=1"}),
	one("login latin1 charset", Step{Method: "POST", Path: "/authorize", Header: [][2]string{{"Content-Type", "application/x-www-form-urlencoded; charset=iso-8859-1"}},
		Body: authorizeQuery(cimdID, "state", "caf\xe9") + "&agent=%E9&password=x"}),
	one("login too many parameters", Step{Method: "POST", Path: "/authorize", Header: formType, Body: strings.Repeat("a=1&", 1000) + "a=1"}),
}

var oauthTokenCases = []Scenario{
	one("token no body", Step{Method: "POST", Path: "/oauth/token"}),
	one("token text body", Step{Method: "POST", Path: "/oauth/token", Header: [][2]string{{"Content-Type", "text/plain"}}, Body: "grant_type=client_credentials"}),
	one("token empty form", tokenForm("")),
	one("token unknown grant", tokenForm("grant_type=password")),
	one("token repeated grant", tokenForm("grant_type=client_credentials&grant_type=client_credentials")),
	one("token client credentials", tokenForm("grant_type=client_credentials&client_id=agent-a&client_secret={{agent-a-key}}")),
	one("token client credentials bracket keys", tokenForm("%5Bgrant_type%5D=client_credentials&[client_id]=agent-a&client_secret={{agent-a-key}}")),
	one("token client credentials json", Step{Method: "POST", Path: "/oauth/token", Header: jsonType,
		Body: `{"grant_type":"client_credentials","client_id":"agent-b","client_secret":"{{agent-b-key}}"}`}),
	one("token client credentials wrong key", tokenForm("grant_type=client_credentials&client_id=agent-a&client_secret={{agent-b-key}}")),
	one("token client credentials revoked key", tokenForm("grant_type=client_credentials&client_id=agent-b&client_secret={{agent-b-revoked-key}}")),
	one("token client credentials unknown agent", tokenForm("grant_type=client_credentials&client_id=nobody-here&client_secret={{agent-a-key}}")),
	one("token client credentials missing secret", tokenForm("grant_type=client_credentials&client_id=agent-a")),
	one("token client credentials secret not a key", tokenForm("grant_type=client_credentials&client_id=agent-a&client_secret=hunter2")),
	one("token code missing fields", tokenForm("grant_type=authorization_code&code=abc")),
	one("token unknown code", codeExchange("&client_id=agent-a").withPathVar("code", "0000")),
}

// withPathVar replaces a {{name}} placeholder in the step's body with a
// fixed value, for a step that needs no capture.
func (s Step) withPathVar(name, value string) Step {
	s.Body = strings.ReplaceAll(s.Body, "{{"+name+"}}", value)
	return s
}
