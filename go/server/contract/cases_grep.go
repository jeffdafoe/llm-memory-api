package contract

import "strings"

// grepCases cover POST /v1/documents/grep against the fixture notes. Every
// fixture note's title is its slug (fixture_memory.go).
const grepPath = "/v1/documents/grep"

var grepCases = []Scenario{
	postCase("grep substring in one namespace", grepPath, "agent-a-session", `{"pattern":"middleware","namespace":"agent-a"}`),
	postCase("grep across namespaces as a reader", grepPath, "agent-b-session", `{"pattern":"dream lock"}`),
	postCase("grep across namespaces as a wildcard reader", grepPath, "agent-a-session", `{"pattern":"DREAM LOCK"}`),
	postCase("grep star namespace", grepPath, "agent-b-session", `{"pattern":"dream","namespace":"*"}`),
	// The title matches, no line does: the note is listed with no lines.
	postCase("grep title only", grepPath, "agent-a-session", `{"pattern":"codebase/dream","namespace":"shared"}`),
	// The substring is a LIKE pattern in the prefilter but a plain string
	// on the lines.
	postCase("grep substring with a LIKE wildcard", grepPath, "agent-a-session", `{"pattern":"auth%token","namespace":"agent-a"}`),
	postCase("grep context zero", grepPath, "agent-a-session", `{"pattern":"middleware","namespace":"agent-a","context":0}`),
	postCase("grep context before and after", grepPath, "agent-a-session",
		`{"pattern":"sessions","namespace":"agent-a","context_before":1,"context_after":0}`),
	postCase("grep context null falls back", grepPath, "agent-a-session",
		`{"pattern":"sessions","namespace":"agent-a","context_before":null,"context":1}`),
	postCase("grep context too large", grepPath, "agent-a-session", `{"pattern":"x","context":51}`),
	postCase("grep context as a string", grepPath, "agent-a-session", `{"pattern":"x","context_after":"2"}`),
	postCase("grep context not an integer", grepPath, "agent-a-session", `{"pattern":"x","context_before":1.5}`),
	postCase("grep limit", grepPath, "agent-a-session", `{"pattern":"dream","limit":1}`),
	postCase("grep limit as a string", grepPath, "agent-a-session", `{"pattern":"dream","limit":"2"}`),
	// Two matches with a gap: the second block starts with newBlock.
	postCase("grep regex with blocks", grepPath, "agent-a-session",
		`{"pattern":"^(## auth|sessions refresh)","namespace":"agent-a","regex":true,"context":0}`),
	postCase("grep regex lookbehind", grepPath, "agent-a-session", `{"pattern":"(?<=the )auth","namespace":"agent-a","regex":true}`),
	postCase("grep regex backreference", grepPath, "agent-a-session", `{"pattern":"(e)\\1","namespace":"agent-a","regex":true}`),
	postCase("grep regex word boundary", grepPath, "agent-a-session", `{"pattern":"\\bdream\\b","regex":true}`),
	// Valid in JavaScript, refused by Postgres's regex engine: both 500.
	postCase("grep regex Postgres refuses", grepPath, "agent-a-session", `{"pattern":"(?<n>auth)","namespace":"agent-a","regex":true}`),
	postCase("grep regex invalid", grepPath, "agent-a-session", `{"pattern":"(auth","regex":true}`),
	postCase("grep regex nothing to repeat", grepPath, "agent-a-session", `{"pattern":"*auth","regex":true}`),
	postCase("grep regex unsafe", grepPath, "agent-a-session", `{"pattern":"(a+)+","regex":true}`),
	postCase("grep regex too long", grepPath, "agent-a-session", `{"pattern":"`+strings.Repeat("a", 201)+`","regex":true}`),
	// Only a JSON true turns regex mode on.
	postCase("grep regex must be true", grepPath, "agent-a-session", `{"pattern":"(","regex":"true"}`),
	postCase("grep missing pattern", grepPath, "agent-a-session", `{"namespace":"agent-a"}`),
	postCase("grep without read access", grepPath, "agent-b-session", `{"pattern":"dream","namespace":"private-ns"}`),
	postCase("grep reserved namespace", grepPath, "agent-a-session", `{"pattern":"dream","namespace":"/"}`),
	one("grep without a JSON body", Step{Method: "POST", Path: grepPath, Auth: "agent-a-session",
		Header: [][2]string{{"Content-Type", "text/plain"}}, Body: "x"}),
	{
		Name: "grep after an edit",
		Steps: []Step{
			{Method: "POST", Path: editPath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"namespace":"agent-a","slug":"instructions/deploy","old_string":"pull main","new_string":"pull main\n\n\n\nthen grep for zebra"}`},
			{Method: "POST", Path: grepPath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"pattern":"zebra|deploy steps","namespace":"agent-a","regex":true,"context":1}`},
		},
	},
}
