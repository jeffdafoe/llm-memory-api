package contract

import "strings"

// notesCases cover the /v1/documents routes (all but grep) against the
// fixture notes. A save or an edit indexes the note in the background, so
// the embeddings call and the memory_chunks rows are compared too.
var notesCases = concat(notesSaveCases, notesListCases, notesReadCases, notesDeleteRestoreCases, notesEditCases, notesMoveCases, notesRoutingCases)

const (
	savePath    = "/v1/documents/save"
	listPath    = "/v1/documents/list"
	readPath    = "/v1/documents/read"
	docDelPath  = "/v1/documents/delete"
	restorePath = "/v1/documents/restore"
	editPath    = "/v1/documents/edit"
	movePath    = "/v1/documents/move"
)

// oversizedNote is just over note_maximum_size (512000 bytes in seed.sql).
var oversizedNote = `{"namespace":"agent-a","title":"Big","content":"` + strings.Repeat("x", 512001) + `"}`

var notesSaveCases = []Scenario{
	postCase("notes save with a slug from the title", savePath, "agent-a-session",
		`{"namespace":"agent-a","title":"  New Note: Title!  ","content":"# New\n\nBody text."}`),
	postCase("notes save with a slug and created_by", savePath, "agent-a-session",
		`{"namespace":"shared","title":"T","content":"## Part\nShared content.","slug":"notes/codebase/new-shared","created_by":"agent-b"}`),
	postCase("notes save created_by unknown", savePath, "agent-a-session",
		`{"namespace":"agent-a","title":"T","content":"c","slug":"notes/by-nobody","created_by":"nobody-at-all"}`),
	postCase("notes save conversation", savePath, "agent-a-session",
		`{"namespace":"agent-a","title":"Conv","content":"Session\n---\n[10:00 work] one\n[10:01 jeff] two","slug":"conversations/2026-11-05-y"}`),
	postCase("notes save context is not indexed", savePath, "agent-a-session",
		`{"namespace":"agent-a","title":"Soul","content":"I am.","slug":"context/soul"}`),
	postCase("notes save duplicate slug", savePath, "agent-a-session",
		`{"namespace":"agent-a","title":"Dup","content":"x","slug":"notes/codebase/auth-middleware"}`),
	// The unique index is on the exact slug, so another case is a new note.
	postCase("notes save same slug in another case", savePath, "agent-a-session",
		`{"namespace":"agent-a","title":"Dup","content":"x","slug":"NOTES/CODEBASE/AUTH-MIDDLEWARE"}`),
	postCase("notes save upsert overwrites", savePath, "agent-a-session",
		`{"namespace":"agent-a","title":"Replaced","content":"## Replaced\nNew body.","slug":"Notes/Codebase/Auth-Middleware","upsert":true}`),
	postCase("notes save upsert of a new slug inserts", savePath, "agent-a-session",
		`{"namespace":"agent-a","title":"Fresh","content":"fresh","slug":"notes/fresh","upsert":true}`),
	postCase("notes save upsert must be true", savePath, "agent-a-session",
		`{"namespace":"agent-a","title":"Dup","content":"x","slug":"notes/codebase/auth-middleware","upsert":"true"}`),
	postCase("notes save slug starting with its namespace", savePath, "agent-a-session",
		`{"namespace":"shared","title":"T","content":"c","slug":"shared/ideas/x"}`),
	postCase("notes save slug with a double slash", savePath, "agent-a-session",
		`{"namespace":"agent-a","title":"T","content":"c","slug":"a//b"}`),
	postCase("notes save slug with a backslash", savePath, "agent-a-session",
		`{"namespace":"agent-a","title":"T","content":"c","slug":"a\\b"}`),
	postCase("notes save slug not a string", savePath, "agent-a-session",
		`{"namespace":"agent-a","title":"T","content":"c","slug":5}`),
	postCase("notes save title gives no slug", savePath, "agent-a-session",
		`{"namespace":"agent-a","title":"!!!","content":"c"}`),
	postCase("notes save over the size limit", savePath, "agent-a-session", oversizedNote),
	postCase("notes save over the default quota", savePath, "agent-a-session",
		`{"namespace":"quota-ns","title":"T","content":"twenty bytes of text","slug":"notes/q"}`),
	postCase("notes save over the agent quota", savePath, "agent-b-session",
		`{"namespace":"agent-b","title":"T","content":"`+strings.Repeat("y", 150)+`","slug":"notes/q"}`),
	postCase("notes save without write access", savePath, "agent-b-session",
		`{"namespace":"shared","title":"T","content":"c"}`),
	postCase("notes save missing content", savePath, "agent-a-session",
		`{"namespace":"agent-a","title":"T"}`),
	postCase("notes save reserved namespace", savePath, "agent-a-session",
		`{"namespace":"*","title":"T","content":"c"}`),
	{
		Name: "notes save then read and list",
		Steps: []Step{
			{Method: "POST", Path: savePath, Auth: "agent-b-session", Header: jsonType,
				Body: `{"namespace":"agent-a","title":"Round Trip","content":"line one\nline two"}`},
			{Method: "POST", Path: readPath, Auth: "agent-b-session", Header: jsonType,
				Body: `{"namespace":"agent-a","slug":"round-trip"}`},
			// The read stamps last_accessed in the background on both
			// servers; wait for it so the listing does not race it.
			{Settle: true},
			{Method: "POST", Path: listPath, Auth: "agent-b-session", Header: jsonType,
				Body: `{"namespace":"agent-a","limit":3}`},
		},
	},
}

var notesListCases = []Scenario{
	postCase("notes list", listPath, "agent-a-session", `{"namespace":"agent-a"}`),
	postCase("notes list with prefix, limit and offset", listPath, "agent-a-session",
		`{"namespace":"agent-a","prefix":"notes/","limit":2,"offset":1}`),
	// The prefix is a LIKE pattern: "_" matches any character.
	postCase("notes list prefix underscore is a wildcard", listPath, "agent-a-session",
		`{"namespace":"shared","prefix":"ANNE_"}`),
	postCase("notes list with deleted", listPath, "agent-a-session", `{"namespace":"agent-a","include_deleted":1}`),
	postCase("notes list limit as a string", listPath, "agent-a-session", `{"namespace":"agent-a","limit":"2"}`),
	postCase("notes list limit zero is the default", listPath, "agent-a-session", `{"namespace":"shared","limit":0}`),
	postCase("notes list missing namespace", listPath, "agent-a-session", `{}`),
	postCase("notes list without read access", listPath, "agent-b-session", `{"namespace":"private-ns"}`),
	postCase("notes list as a reader", listPath, "agent-b-session", `{"namespace":"shared"}`),
}

var notesReadCases = []Scenario{
	postCase("notes read", readPath, "agent-a-session", `{"namespace":"agent-a","slug":"notes/codebase/auth-middleware"}`),
	postCase("notes read slug in another case", readPath, "agent-a-session", `{"namespace":"agent-a","slug":"NOTES/codebase/AUTH-middleware"}`),
	postCase("notes read with metadata", readPath, "agent-a-session", `{"namespace":"agent-a","slug":"notes/mixed-case"}`),
	postCase("notes read with created_by", readPath, "agent-b-session", `{"namespace":"shared","slug":"notes/codebase/dream-lock"}`),
	postCase("notes read a page", readPath, "agent-a-session", `{"namespace":"agent-a","slug":"notes/codebase/auth-middleware","offset":2,"limit":2}`),
	postCase("notes read past the end", readPath, "agent-a-session", `{"namespace":"agent-a","slug":"notes/codebase/auth-middleware","offset":50}`),
	postCase("notes read limit only", readPath, "agent-a-session", `{"namespace":"agent-a","slug":"notes/codebase/auth-middleware","limit":1}`),
	postCase("notes read offset as a string", readPath, "agent-a-session", `{"namespace":"agent-a","slug":"notes/codebase/auth-middleware","offset":"2"}`),
	postCase("notes read limit too large", readPath, "agent-a-session", `{"namespace":"agent-a","slug":"notes/codebase/auth-middleware","limit":10001}`),
	postCase("notes read null offset and limit", readPath, "agent-a-session", `{"namespace":"agent-a","slug":"notes/codebase/auth-middleware","offset":null,"limit":null}`),
	postCase("notes read missing", readPath, "agent-a-session", `{"namespace":"agent-a","slug":"notes/none"}`),
	postCase("notes read a deleted note", readPath, "agent-a-session", `{"namespace":"agent-a","slug":"notes/codebase/deleted-doc"}`),
	postCase("notes read missing slug", readPath, "agent-a-session", `{"namespace":"agent-a"}`),
	postCase("notes read without access", readPath, "agent-b-session", `{"namespace":"private-ns","slug":"notes/codebase/not-shared"}`),
}

var notesDeleteRestoreCases = []Scenario{
	postCase("notes delete", docDelPath, "agent-a-session", `{"namespace":"agent-a","slug":"instructions/deploy"}`),
	postCase("notes delete missing", docDelPath, "agent-a-session", `{"namespace":"agent-a","slug":"notes/none"}`),
	postCase("notes delete without delete access", docDelPath, "agent-b-session", `{"namespace":"agent-a","slug":"instructions/deploy"}`),
	postCase("notes restore", restorePath, "agent-a-session", `{"namespace":"agent-a","slug":"notes/codebase/deleted-doc"}`),
	postCase("notes restore a purged tombstone", restorePath, "agent-a-session", `{"namespace":"agent-a","slug":"conversations/purged-old"}`),
	postCase("notes restore a live note", restorePath, "agent-a-session", `{"namespace":"agent-a","slug":"instructions/deploy"}`),
	{
		Name: "notes delete then restore",
		Steps: []Step{
			{Method: "POST", Path: docDelPath, Auth: "agent-b-session", Header: jsonType, Body: `{"namespace":"shared","slug":"imports/raw-file"}`},
			{Method: "POST", Path: docDelPath, Auth: "agent-a-session", Header: jsonType, Body: `{"namespace":"shared","slug":"notes/codebase/dream-lock"}`},
			{Method: "POST", Path: restorePath, Auth: "agent-a-session", Header: jsonType, Body: `{"namespace":"shared","slug":"notes/codebase/dream-lock"}`},
			{Method: "POST", Path: listPath, Auth: "agent-a-session", Header: jsonType, Body: `{"namespace":"shared"}`},
		},
	},
}

var notesEditCases = []Scenario{
	postCase("notes edit", editPath, "agent-a-session",
		`{"namespace":"agent-a","slug":"notes/codebase/auth-middleware","old_string":"bearer token","new_string":"API key"}`),
	postCase("notes edit ambiguous", editPath, "agent-a-session",
		`{"namespace":"agent-a","slug":"notes/codebase/auth-middleware","old_string":"middleware","new_string":"layer"}`),
	postCase("notes edit replace all", editPath, "agent-a-session",
		`{"namespace":"agent-a","slug":"notes/codebase/auth-middleware","old_string":"middleware","new_string":"$& layer","replace_all":"yes"}`),
	postCase("notes edit to empty", editPath, "agent-a-session",
		`{"namespace":"agent-a","slug":"notes/codebase/auth-middleware","old_string":" on every request","new_string":""}`),
	postCase("notes edit old string not found", editPath, "agent-a-session",
		`{"namespace":"agent-a","slug":"notes/codebase/auth-middleware","old_string":"absent text","new_string":"x"}`),
	postCase("notes edit null new string", editPath, "agent-a-session",
		`{"namespace":"agent-a","slug":"notes/codebase/auth-middleware","old_string":"bearer","new_string":null}`),
	postCase("notes edit missing new string", editPath, "agent-a-session",
		`{"namespace":"agent-a","slug":"notes/codebase/auth-middleware","old_string":"bearer"}`),
	postCase("notes edit missing note", editPath, "agent-a-session",
		`{"namespace":"agent-a","slug":"notes/none","old_string":"a","new_string":"b"}`),
	postCase("notes edit without write access", editPath, "agent-b-session",
		`{"namespace":"shared","slug":"notes/codebase/dream-lock","old_string":"dream","new_string":"x"}`),
}

var notesMoveCases = []Scenario{
	postCase("notes move rename", movePath, "agent-a-session",
		`{"namespace":"agent-a","slug":"notes/codebase/auth-middleware","new_slug":"instructions/auth"}`),
	postCase("notes move to another namespace", movePath, "agent-a-session",
		`{"namespace":"agent-a","slug":"instructions/deploy","new_slug":"notes/deploy","new_namespace":"shared"}`),
	postCase("notes move onto a live note", movePath, "agent-a-session",
		`{"namespace":"agent-a","slug":"instructions/deploy","new_slug":"Notes/Codebase/Auth-Middleware"}`),
	postCase("notes move without write on the target", movePath, "agent-b-session",
		`{"namespace":"agent-a","slug":"instructions/deploy","new_slug":"notes/deploy","new_namespace":"shared"}`),
	postCase("notes move to an invalid slug", movePath, "agent-a-session",
		`{"namespace":"agent-a","slug":"instructions/deploy","new_slug":"/x"}`),
	postCase("notes move new slug not a string", movePath, "agent-a-session",
		`{"namespace":"agent-a","slug":"instructions/deploy","new_slug":["x"]}`),
	postCase("notes move missing note", movePath, "agent-a-session",
		`{"namespace":"agent-a","slug":"notes/none","new_slug":"notes/other"}`),
	postCase("notes move reserved target namespace", movePath, "agent-a-session",
		`{"namespace":"agent-a","slug":"instructions/deploy","new_slug":"x","new_namespace":"/"}`),
	postCase("notes move missing new slug", movePath, "agent-a-session",
		`{"namespace":"agent-a","slug":"instructions/deploy"}`),
}

var notesRoutingCases = []Scenario{
	one("notes save without a JSON body", Step{Method: "POST", Path: savePath, Auth: "agent-a-session",
		Header: [][2]string{{"Content-Type", "text/plain"}}, Body: "x"}),
	one("notes read OPTIONS", Step{Method: "OPTIONS", Path: readPath, Auth: "agent-a-session"}),
	one("notes list GET falls through", Step{Method: "GET", Path: listPath, Auth: "agent-a-session"}),
	one("notes read encoded path", Step{Method: "POST", Path: "/v1/documents/%72ead", Auth: "agent-a-session",
		Header: jsonType, Body: `{"namespace":"agent-a","slug":"instructions/deploy"}`}),
}
