package contract

// registrationCases cover the signup endpoints. The fixture leaves
// open_registration off (seed.sql), so codes are required; the invite codes
// are in fixture_oauth.go. The open-registration paths are covered by the
// registration unit tests.

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

var registrationCases = concat(registrationModeCases, checkNameCases, registerCases)

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
				Random: []string{"passphrase"}},
			{Settle: true},
			// The name is now taken, and the code used.
			registerPost(`{"code":"` + InviteGood + `","name":"other-name","password":"long-enough-password"}`),
			checkName(`{"name":"newcomer"}`),
		},
		Known: []KnownDifference{{Where: "table request_log row *.response_length", Volatile: true,
			Reason: "the register response carries a random passphrase, so its response_length differs between correct runs"}},
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
