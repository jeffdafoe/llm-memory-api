package contract

import "strings"

// registrationCases cover the signup endpoints. The fixture leaves
// open_registration off (seed.sql), so codes are required; the invite codes
// are in fixture_oauth.go. The open-registration paths of /api/register are
// covered by the registration unit tests; the access request cases turn it
// on through the admin config route.

// Fixture invite codes.
const (
	InviteGood    = "contract-good-invite"
	InviteUsed    = "contract-used-invite"
	InviteExpired = "contract-expired-invite"
)

func registerPost(body string) Step {
	return Step{Method: "POST", Path: "/api/register", Header: jsonType, Body: body}
}

func checkName(body string) Step {
	return Step{Method: "POST", Path: "/api/check-name", Header: jsonType, Body: body}
}

var registrationCases = concat(registrationModeCases, checkNameCases, registerCases, accessRequestCases)

var registrationModeCases = []Scenario{
	one("registration mode", Step{Method: "GET", Path: "/api/registration-mode"}),
	one("registration mode OPTIONS", Step{Method: "OPTIONS", Path: "/api/registration-mode"}),
	one("register GET", Step{Method: "GET", Path: "/api/register"}),
}

var checkNameCases = []Scenario{
	one("check name available", checkName(`{"name":" NewName "}`)),
	one("check name taken", checkName(`{"name":"Agent-A"}`)),
	one("check name namespace", checkName(`{"name":"shared"}`)),
	one("check name bad shape", checkName(`{"name":"1abc"}`)),
	one("check name blank", checkName(`{"name":"   "}`)),
	one("check name missing", checkName(`{}`)),
	one("check name not a string", checkName(`{"name":["x"]}`)),
	one("check name no body", Step{Method: "POST", Path: "/api/check-name"}),
	one("check name array body", checkName(`["name"]`)),
}

var registerCases = []Scenario{
	{
		Name: "register with an invite",
		Steps: []Step{
			{Method: "POST", Path: "/api/register", Header: jsonType,
				Body:   `{"code":" ` + InviteGood + ` ","name":" Newcomer ","password":"long-enough-password","dream_mode":"companion"}`,
				Random: []string{"passphrase"}, VolatileLength: true},
			{Settle: true},
			// The name is now taken, and the code used.
			registerPost(`{"code":"` + InviteGood + `","name":"other-name","password":"long-enough-password"}`),
			checkName(`{"name":"newcomer"}`),
		},
	},
	one("register no body", Step{Method: "POST", Path: "/api/register"}),
	one("register missing name", registerPost(`{"code":"`+InviteGood+`","password":"long-enough-password"}`)),
	one("register no code", registerPost(`{"name":"newcomer","password":"long-enough-password"}`)),
	one("register short password", registerPost(`{"code":"`+InviteGood+`","name":"newcomer","password":"short"}`)),
	one("register password array", registerPost(`{"code":"`+InviteGood+`","name":"newcomer","password":[1,2]}`)),
	one("register password length object", registerPost(`{"code":"`+InviteGood+`","name":"newcomer","password":{"length":3}}`)),
	one("register password a number", registerPost(`{"code":"`+InviteGood+`","name":"newcomer","password":12345678901}`)),
	one("register bad name", registerPost(`{"code":"`+InviteGood+`","name":"x","password":"long-enough-password"}`)),
	one("register name not a string", registerPost(`{"code":"`+InviteGood+`","name":5,"password":"long-enough-password"}`)),
	one("register code not a string", registerPost(`{"code":5,"name":"newcomer","password":"long-enough-password"}`)),
	one("register used code", registerPost(`{"code":"`+InviteUsed+`","name":"newcomer","password":"long-enough-password"}`)),
	one("register expired code", registerPost(`{"code":"`+InviteExpired+`","name":"newcomer","password":"long-enough-password"}`)),
	one("register unknown code", registerPost(`{"code":"no-such-code","name":"newcomer","password":"long-enough-password"}`)),
	one("register taken name", registerPost(`{"code":"`+InviteGood+`","name":"agent-b","password":"long-enough-password"}`)),
	one("register namespace name", registerPost(`{"code":"`+InviteGood+`","name":"shared","password":"long-enough-password"}`)),
}

func accessRequest(body string) Step {
	return Step{Method: "POST", Path: "/api/access-request", Header: jsonType, Body: body}
}

// openRegistrationOn turns open registration on through the admin config
// route, which updates both servers' in-process config.
var openRegistrationOn = Step{Method: "POST", Path: adminConfigUpdatePath, Auth: "admin-web", Header: jsonType,
	Body: `{"key":"open_registration","value":"true"}`}

// accessRequestCases cover server.js's landing-page access request form.
// With open registration off the request is stored as pending; with it on
// it is approved at once and its invite code returned (a 32-hex token).
var accessRequestCases = []Scenario{
	one("access request pending", accessRequest(`{"email":"  visitor@example.com ","usage":"\tTrying the memory API\n"}`)),
	{
		Name: "access request auto-approved",
		Steps: []Step{
			openRegistrationOn,
			accessRequest(`{"email":" visitor@example.com ","usage":" Trying the memory API "}`),
		},
	},
	{
		Name: "access request auto-approved non-string usage",
		Steps: []Step{
			openRegistrationOn,
			accessRequest(`{"email":"visitor@example.com","usage":["a","b"]}`),
		},
	},
	{
		Name: "access request auto-approved database error",
		Steps: []Step{
			openRegistrationOn,
			accessRequest(`{"email":"visitor@example.com","usage":"nul \u0000 byte"}`),
		},
	},
	one("access request database error", accessRequest(`{"email":"nul \u0000 byte","usage":"x"}`)),
	one("access request missing email", accessRequest(`{"usage":"x"}`)),
	one("access request missing usage", accessRequest(`{"email":"visitor@example.com"}`)),
	one("access request empty email", accessRequest(`{"email":"","usage":"x"}`)),
	one("access request zero usage", accessRequest(`{"email":"visitor@example.com","usage":0}`)),
	one("access request email at the limit", accessRequest(`{"email":"`+strings.Repeat("e", 255)+`","usage":"x"}`)),
	one("access request email too long", accessRequest(`{"email":"`+strings.Repeat("e", 256)+`","usage":"x"}`)),
	// 128 characters outside the BMP: 256 UTF-16 units, so too long for
	// JavaScript though only 128 runes.
	one("access request email too long in UTF-16", accessRequest(`{"email":"`+strings.Repeat("\U0001F600", 128)+`","usage":"x"}`)),
	one("access request usage too long", accessRequest(`{"email":"visitor@example.com","usage":"`+strings.Repeat("u", 5001)+`"}`)),
	one("access request email length object", accessRequest(`{"email":{"length":"300"},"usage":"x"}`)),
	one("access request email array", accessRequest(`{"email":["visitor@example.com"],"usage":"x"}`)),
	one("access request usage a number", accessRequest(`{"email":"visitor@example.com","usage":5}`)),
	one("access request no body", Step{Method: "POST", Path: "/api/access-request"}),
	one("access request text body", Step{Method: "POST", Path: "/api/access-request",
		Header: [][2]string{{"Content-Type", "text/plain"}}, Body: `{"email":"visitor@example.com","usage":"x"}`}),
	one("access request form body", Step{Method: "POST", Path: "/api/access-request",
		Header: [][2]string{{"Content-Type", "application/x-www-form-urlencoded"}}, Body: "email=visitor%40example.com&usage=x"}),
	one("access request empty JSON body", Step{Method: "POST", Path: "/api/access-request", Header: jsonType}),
	one("access request malformed JSON", accessRequest(`{"email":`), malformedText("POST /api/access-request")),
	one("access request array body", accessRequest(`["visitor@example.com","x"]`)),
	one("access request GET", Step{Method: "GET", Path: "/api/access-request"}),
	one("access request OPTIONS", Step{Method: "OPTIONS", Path: "/api/access-request"}),
}
