package contract

import (
	"bytes"
	"compress/gzip"
	"strings"
	"unicode/utf16"
)

// Scenarios are the contract cases, run by TestContract (contract build
// tag). Each port step adds its cases in the same PR as the Go code.
//
// Paths with no route in either server are used to test the layers in
// front of the routes:
//   - a body the parser accepts goes to bodyPath, which neither server
//     routes, so both answer with the 404 page;
//   - a body the parser rejects is answered before any route, so it can
//     also go to /v1 with no credentials (parsing comes before auth);
//   - a valid /v1 credential goes to noRoutePath: auth and the heartbeat
//     run, then both 404.
//
// /v1/admin/login is not used with POST: Go lets it through auth but has not
// ported it, so an accepted request there gets Node's login route and Go's
// 404.
var Scenarios = concat(healthCases, notFoundCases, authCases, bodyCases, memoryCases, notesCases, grepCases, mailCases, vaMailCases, chatCases, discussionCases, agentCases, oauthCases, registrationCases, cleanupCases, eventsCases)

const (
	bodyPath    = "/contract/body"
	noRoutePath = "/v1/contract-no-route"
)

func concat(lists ...[]Scenario) []Scenario {
	var out []Scenario
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

func one(name string, st Step, known ...KnownDifference) Scenario {
	return Scenario{Name: name, Steps: []Step{st}, Known: known}
}

// routingCase is the known difference for Express's case-insensitive,
// trailing-slash-tolerant matching, which Go does not copy.
func routingCase(label string) KnownDifference {
	return KnownDifference{
		Where:  "step 1 " + label + ": *",
		Reason: "Go matches paths case-sensitively and exactly; Express ignores case and a trailing slash (go-server note, Known differences)",
	}
}

var healthCases = []Scenario{
	one("health GET", Step{Method: "GET", Path: "/health"}),
	one("health HEAD", Step{Method: "HEAD", Path: "/health"}),
	one("health with a query", Step{Method: "GET", Path: "/health?x=1"}),
	// Express answers OPTIONS itself for a path with routes.
	one("health OPTIONS", Step{Method: "OPTIONS", Path: "/health"}),
	one("health trailing slash", Step{Method: "GET", Path: "/health/"}, routingCase("GET /health/")),
	one("health upper case", Step{Method: "GET", Path: "/HEALTH"}, routingCase("GET /HEALTH"),
		KnownDifference{Where: "table request_log*", Reason: "Go's 404 is logged; Express's /health is not"}),
	one("v1 upper case without auth", Step{Method: "GET", Path: "/V1/missing"}, routingCase("GET /V1/missing"),
		KnownDifference{Where: "table request_log*", Reason: "same request, different status in the log row"}),
}

var notFoundCases = []Scenario{
	one("404 GET", Step{Method: "GET", Path: "/no/such/route"}),
	one("404 HEAD", Step{Method: "HEAD", Path: "/no/such/route"}),
	one("404 POST no body", Step{Method: "POST", Path: "/no/such/route"}),
	one("404 PUT", Step{Method: "PUT", Path: "/no/such/route"}),
	one("404 DELETE", Step{Method: "DELETE", Path: "/no/such/route"}),
	one("404 PATCH", Step{Method: "PATCH", Path: "/no/such/route"}),
	one("404 OPTIONS", Step{Method: "OPTIONS", Path: "/no/such/route"}),
	one("404 query not in page", Step{Method: "GET", Path: "/no/such/route?q=%3Cb%3E&x=1"}),
	one("404 escaped markup", Step{Method: "GET", Path: "/%3Cscript%3Ealert(1)%3C/script%3E"}),
	one("404 encoded slash and space", Step{Method: "GET", Path: "/a%2Fb/c%20d"}),
	one("404 utf-8 path", Step{Method: "GET", Path: "/caf%C3%A9"}),
	one("404 double slash", Step{Method: "GET", Path: "//double//slash"}),
	one("404 dot segment", Step{Method: "GET", Path: "/./health"}),
	one("404 encoded v1 slash is not under v1", Step{Method: "GET", Path: "/v1%2Fcontract"}),
	// Express matches mounts and routes on the raw path; these decode to
	// /v1/... and /health but match neither.
	one("404 encoded v1 mount", Step{Method: "GET", Path: "/%76%31/missing"}),
	one("404 encoded v1 mount with a session", Step{Method: "GET", Path: "/%76%31/missing", Auth: "agent-a-session"}),
	one("404 partly encoded v1 mount", Step{Method: "GET", Path: "/v%31/missing", Auth: "admin-web"}),
	one("404 encoded health", Step{Method: "GET", Path: "/%68ealth"}),
	one("404 POST health", Step{Method: "POST", Path: "/health"}),
	one("404 bad percent escape", Step{Method: "GET", Path: "/bad%ZZescape"},
		KnownDifference{Where: "step 1 GET /bad%ZZescape: *", Reason: "Go's net/http refuses an invalid percent escape with a plain 400 before any handler (go-server note, Known differences)"},
		KnownDifference{Where: "table request_log*", Reason: "that 400 never reaches the request log; Node logs its 404"},
	),
	one("404 ampersand and quote", Step{Method: "GET", Path: "/a&b'c"}),
}

func bearer(token string) [][2]string {
	return [][2]string{{"Authorization", "Bearer " + token}}
}

var authCases = []Scenario{
	one("v1 no Authorization header", Step{Method: "GET", Path: noRoutePath}),
	one("v1 unknown token", Step{Method: "GET", Path: noRoutePath, Header: bearer("not-a-real-token")}),
	one("v1 empty bearer", Step{Method: "GET", Path: noRoutePath, Header: [][2]string{{"Authorization", "Bearer "}}}),
	one("v1 lower-case bearer prefix", Step{Method: "GET", Path: noRoutePath, Header: [][2]string{{"Authorization", "bearer {{agent-a-session}}"}}}),
	one("v1 token without Bearer prefix", Step{Method: "GET", Path: noRoutePath, Header: [][2]string{{"Authorization", "{{agent-a-session}}"}}}),
	one("v1 agent API session", Step{Method: "GET", Path: noRoutePath, Auth: "agent-a-session"}),
	one("v1 admin web session", Step{Method: "GET", Path: noRoutePath, Auth: "admin-web"}),
	one("v1 agent API key", Step{Method: "GET", Path: noRoutePath, Auth: "agent-a-key"}),
	one("v1 expired session", Step{Method: "GET", Path: noRoutePath, Auth: "agent-b-expired-session"}),
	one("v1 revoked API key", Step{Method: "GET", Path: noRoutePath, Auth: "agent-b-revoked-key"}),
	one("v1 passphrase is not a token", Step{Method: "GET", Path: noRoutePath, Auth: "agent-a-passphrase"}),
	one("v1 bare path without auth", Step{Method: "GET", Path: "/v1"}),
	one("v1 bare path admin", Step{Method: "GET", Path: "/v1", Auth: "admin-web"}),
	one("v1 double slash agent", Step{Method: "GET", Path: "/v1//contract", Auth: "agent-a-session"}),
	one("v1 dot-dot without auth", Step{Method: "GET", Path: "/v1/../health"}),
	one("v1 encoded dot-dot with a session", Step{Method: "GET", Path: "/v1/%2e%2e/x", Auth: "agent-a-session"}),
	one("v1 GET agent login without auth", Step{Method: "GET", Path: "/v1/agent/login"}),
	one("v1 GET admin login without auth", Step{Method: "GET", Path: "/v1/admin/login"}),
	one("v1 encoded login path needs auth", Step{Method: "POST", Path: "/v1/agent%2Flogin", Header: jsonType, Body: `{}`}),
	{
		Name: "v1 same session twice",
		Steps: []Step{
			{Method: "GET", Path: noRoutePath, Auth: "agent-b-session"},
			{Method: "POST", Path: noRoutePath, Auth: "agent-b-session", Header: jsonType, Body: `{"x":1}`},
		},
	},
}

var jsonType = [][2]string{{"Content-Type", "application/json"}}

// malformedText: the parser's message for malformed JSON is V8's in Node
// and Go's in Go; status and code match ("Known differences" in the
// go-server codebase note).
func malformedText(label string) KnownDifference {
	return KnownDifference{
		Where:  "step 1 " + label + ": body.error.message",
		Reason: "malformed-JSON message text is Go's, not V8's (go-server note, Known differences)",
	}
}

var bodyCases = []Scenario{
	one("body malformed JSON", Step{Method: "POST", Path: bodyPath, Header: jsonType, Body: `{"a":`},
		malformedText("POST "+bodyPath)),
	one("body malformed JSON on v1 without auth", Step{Method: "POST", Path: noRoutePath, Header: jsonType, Body: `{"a" 1}`},
		malformedText("POST "+noRoutePath)),
	one("body malformed JSON chunked", Step{Method: "POST", Path: bodyPath, Header: jsonType, Body: `[1,`, Chunked: true},
		malformedText("POST "+bodyPath)),
	one("body empty with JSON type", Step{Method: "POST", Path: bodyPath, Header: jsonType}),
	one("body whitespace only", Step{Method: "POST", Path: bodyPath, Header: jsonType, Body: " \n\t "}),
	one("body object", Step{Method: "POST", Path: bodyPath, Header: jsonType, Body: `{"a":[1,2,{"b":null}]}`}),
	one("body top-level array", Step{Method: "POST", Path: bodyPath, Header: jsonType, Body: `[1,"two"]`}),
	// body-parser's strict-mode refusal is V8's JSON.parse message with the
	// input quoted; Go writes its own. Same class as malformed JSON.
	one("body top-level string", Step{Method: "POST", Path: bodyPath, Header: jsonType, Body: `"just a string"`},
		malformedText("POST "+bodyPath)),
	one("body top-level number", Step{Method: "POST", Path: bodyPath, Header: jsonType, Body: `42`},
		malformedText("POST "+bodyPath)),
	one("body over 5 MiB", Step{Method: "POST", Path: bodyPath, Header: jsonType, Body: oversized}),
	one("body exactly 5 MiB", Step{Method: "POST", Path: bodyPath, Header: jsonType, Body: exactLimit}),
	one("body utf-8 with BOM", Step{Method: "POST", Path: bodyPath,
		Header: [][2]string{{"Content-Type", "application/json; charset=utf-8"}}, Body: "\xEF\xBB\xBF" + `{"a":"é"}`}),
	one("body utf-16le with BOM", Step{Method: "POST", Path: bodyPath,
		Header: [][2]string{{"Content-Type", "application/json; charset=utf-16le"}}, Body: utf16le(`{"a":"é"}`, true)}),
	one("body utf-16 without BOM", Step{Method: "POST", Path: bodyPath,
		Header: [][2]string{{"Content-Type", "application/json; charset=utf-16"}}, Body: utf16le(`{"a":"é"}`, false)}),
	one("body invalid utf-8 bytes", Step{Method: "POST", Path: bodyPath, Header: jsonType, Body: "{\"a\":\"\xff\xc3\"}"}),
	one("body unknown charset", Step{Method: "POST", Path: bodyPath,
		Header: [][2]string{{"Content-Type", "application/json; charset=latin-9"}}, Body: `{"a":1}`}),
	one("body JSON with +json type", Step{Method: "POST", Path: bodyPath,
		Header: [][2]string{{"Content-Type", "application/merge-patch+json"}}, Body: `{"a":`}),
	one("body text/plain is not parsed", Step{Method: "POST", Path: bodyPath,
		Header: [][2]string{{"Content-Type", "text/plain"}}, Body: `{"a":`}),
	one("body gzip", Step{Method: "POST", Path: bodyPath,
		Header: [][2]string{{"Content-Type", "application/json"}, {"Content-Encoding", "gzip"}}, Body: gzipped(`{"a":1}`)},
		KnownDifference{Where: "step 1 POST " + bodyPath + ": *", Reason: "Go refuses gzip bodies (415); Node inflates them (go-server note, Known differences)"},
		KnownDifference{Where: "table request_log*", Reason: "the refused gzip body is not logged by Go; Node logs its 404"},
	),
}

// fiveMiB is express.json's limit ('5mb' is 5 * 1024 * 1024 bytes).
const fiveMiB = 5 << 20

var (
	exactLimit = sizedJSON(fiveMiB)
	oversized  = sizedJSON(fiveMiB + 1)
)

// sizedJSON is a valid JSON object of exactly n bytes.
func sizedJSON(n int) string {
	const head, tail = `{"pad":"`, `"}`
	return head + strings.Repeat("x", n-len(head)-len(tail)) + tail
}

func utf16le(s string, bom bool) string {
	var b bytes.Buffer
	if bom {
		b.Write([]byte{0xFF, 0xFE})
	}
	for _, u := range utf16.Encode([]rune(s)) {
		b.WriteByte(byte(u))
		b.WriteByte(byte(u >> 8))
	}
	return b.String()
}

func gzipped(s string) string {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.String()
}
