package contract

// memoryCases cover the /v1/memory routes against the notes and chunks in
// fixture_memory.go. agent-a reads and writes every namespace; agent-b reads
// "shared", reads and writes "agent-a", and has one private-ns note shared
// with it. A case that returns fixture notes searches once (see the fixture
// for why).
var memoryCases = concat(memorySearchCases, memoryIngestCases, memoryDeleteCases, memoryCleanupCases, memoryStatusCases)

// postCase is one authenticated JSON POST.
func postCase(name, path, auth, body string, known ...KnownDifference) Scenario {
	return one(name, Step{Method: "POST", Path: path, Auth: auth, Header: jsonType, Body: body}, known...)
}

const (
	searchPath  = "/v1/memory/search"
	ingestPath  = "/v1/memory/ingest"
	statusPath  = "/v1/memory/ingest/status"
	deletePath  = "/v1/memory/delete"
	cleanupPath = "/v1/memory/cleanup"
)

var memorySearchCases = []Scenario{
	// The deleted note has the same vector as the best hit and must not
	// appear.
	postCase("memory search one namespace", searchPath, "agent-a-session",
		`{"query":"how does the auth middleware work","namespace":"agent-a"}`),
	postCase("memory search all namespaces as a wildcard reader", searchPath, "agent-a-session",
		`{"query":"dream lock","limit":20}`),
	// agent-b reads shared and agent-a, plus the one private-ns note shared
	// with it, but not the other private-ns note.
	postCase("memory search star with a shared note", searchPath, "agent-b-session",
		`{"query":"dream lock","namespace":"*","limit":20}`),
	postCase("memory search slug prefix underscore is literal", searchPath, "agent-a-session",
		`{"query":"dream lock","namespace":"shared","slug_prefix":"Anne_Walker/"}`),
	postCase("memory search slug prefix sanitized", searchPath, "agent-a-session",
		`{"query":"dream lock","namespace":"shared","slug_prefix":"  anne<_>walker/  "}`),
	postCase("memory search slug prefix not a string is ignored", searchPath, "agent-a-session",
		`{"query":"dream lock","namespace":"shared","slug_prefix":5}`),
	postCase("memory search limit as a string", searchPath, "agent-a-session",
		`{"query":"dream lock","limit":"2"}`),
	postCase("memory search limit below one falls back", searchPath, "agent-a-session",
		`{"query":"dream lock","limit":0.5}`),
	postCase("memory search limit as an array", searchPath, "agent-a-session",
		`{"query":"dream lock","limit":[1]}`),
	postCase("memory search filler-only query", searchPath, "agent-a-session",
		`{"query":"what is the","namespace":"shared"}`),
	postCase("memory search with an API key", searchPath, "agent-b-key",
		`{"query":"dream lock","namespace":"agent-a"}`),
	// An admin user has no namespace grants: only its own (empty) one.
	postCase("memory search as an admin user", searchPath, "admin-web",
		`{"query":"dream lock"}`),
	postCase("memory search without read access", searchPath, "agent-b-session",
		`{"query":"dream lock","namespace":"private-ns"}`),
	postCase("memory search reserved namespace", searchPath, "agent-a-session",
		`{"query":"dream lock","namespace":"/"}`),
	postCase("memory search missing query", searchPath, "agent-a-session", `{"namespace":"shared"}`),
	postCase("memory search null query", searchPath, "agent-a-session", `{"query":null}`),
	postCase("memory search query of stripped characters only", searchPath, "agent-a-session",
		`{"query":"\u200b\u0007\ufeff"}`),
	postCase("memory search array body", searchPath, "agent-a-session", `[{"query":"dream lock"}]`),
	postCase("memory search body with leading whitespace", searchPath, "agent-a-session",
		" \n\t "+`{"query":"dream lock","namespace":"shared","limit":1}`),
	one("memory search without a JSON body", Step{Method: "POST", Path: searchPath, Auth: "agent-a-session",
		Header: [][2]string{{"Content-Type", "text/plain"}}, Body: "query=dream"}),
	one("memory search without auth", Step{Method: "POST", Path: searchPath, Header: jsonType, Body: `{"query":"x"}`}),
	// Express answers OPTIONS for a path with routes; other methods fall
	// through to the admin router.
	one("memory search OPTIONS", Step{Method: "OPTIONS", Path: searchPath, Auth: "agent-a-session"}),
	one("memory search GET as an agent", Step{Method: "GET", Path: searchPath, Auth: "agent-a-session"}),
	one("memory search GET as an admin", Step{Method: "GET", Path: searchPath, Auth: "admin-web"}),
	// Express matches the raw path: an encoded letter is not the route.
	one("memory search encoded path", Step{Method: "POST", Path: "/v1/memory/%73earch", Auth: "agent-a-session",
		Header: jsonType, Body: `{"query":"dream lock"}`}),
	{
		Name: "memory ingest then search",
		Steps: []Step{
			{Method: "POST", Path: ingestPath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"namespace":"agent-a","source_file":"notes/zebra","content":"zebra crossing rules"}`},
			{Method: "POST", Path: searchPath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"query":"zebra crossing rules","namespace":"agent-a","limit":1}`},
		},
	},
}

var memoryIngestCases = []Scenario{
	postCase("memory ingest markdown into a granted namespace", ingestPath, "agent-b-session",
		`{"namespace":"agent-a","source_file":"notes/new-note","content":"# New\n\n## Part one\nFirst part.\n\n## Part two\nSecond part.\n\n## Empty"}`),
	// The old rows are matched case-insensitively and replaced.
	postCase("memory ingest replaces case-insensitively", ingestPath, "agent-a-session",
		`{"namespace":"agent-a","source_file":"MEMORY-ONLY/FILE-1","content":"Replacement text."}`),
	postCase("memory ingest conversation windows", ingestPath, "agent-a-session",
		`{"namespace":"agent-a","source_file":"conversations/2026-11-05-x","content":"Session: x\n---\n[10:00 work] one\n[10:01 jeff] two\ncontinued\n[10:02 work] three\n[10:03 jeff] four\n[10:04 work] five\n[10:05 jeff] six\n[10:06 work] seven"}`),
	postCase("memory ingest context is not indexed", ingestPath, "agent-a-session",
		`{"namespace":"agent-a","source_file":"context/soul","content":"I am the agent."}`),
	postCase("memory ingest whitespace content has no chunks", ingestPath, "agent-a-session",
		`{"namespace":"agent-a","source_file":"notes/blank","content":"   \n  "}`),
	postCase("memory ingest without write access", ingestPath, "agent-b-session",
		`{"namespace":"shared","source_file":"notes/x","content":"text"}`),
	postCase("memory ingest source file sanitized", ingestPath, "agent-a-session",
		`{"namespace":"agent-a","source_file":"  notes/<script>x.md  ","content":"Sanitized name."}`),
	postCase("memory ingest source file sanitizes to empty", ingestPath, "agent-a-session",
		`{"namespace":"agent-a","source_file":"<<>>","content":"text"}`),
	postCase("memory ingest missing content", ingestPath, "agent-a-session",
		`{"namespace":"agent-a","source_file":"notes/x"}`),
	postCase("memory ingest strips control characters", ingestPath, "agent-a-session",
		`{"namespace":"agent-a","source_file":"notes/controls","content":"Line\u0000 one\u200b and\u202e two"}`),
	postCase("memory ingest reserved namespace", ingestPath, "agent-a-session",
		`{"namespace":"*","source_file":"notes/x","content":"text"}`),
	{
		Name: "memory ingest embeddings upstream fails",
		Replies: []QueuedReply{{Upstream: "openai", Path: "/v1/embeddings",
			Reply: JSONReply(500, `{"error":{"message":"upstream down"}}`)}},
		Steps: []Step{{Method: "POST", Path: ingestPath, Auth: "agent-a-session", Header: jsonType,
			Body: `{"namespace":"agent-a","source_file":"notes/fails","content":"Will not be stored."}`}},
	},
}

var memoryDeleteCases = []Scenario{
	postCase("memory delete", deletePath, "agent-a-session", `{"namespace":"agent-a","source_file":"memory-only/file-2"}`),
	postCase("memory delete matches case exactly", deletePath, "agent-a-session", `{"namespace":"agent-a","source_file":"MEMORY-ONLY/FILE-2"}`),
	postCase("memory delete without delete access", deletePath, "agent-b-session", `{"namespace":"agent-a","source_file":"memory-only/file-2"}`),
	postCase("memory delete missing source file", deletePath, "agent-a-session", `{"namespace":"agent-a"}`),
}

var memoryCleanupCases = []Scenario{
	// A number in the list can never match a source file.
	postCase("memory cleanup", cleanupPath, "agent-a-session",
		`{"namespace":"agent-a","valid_source_files":["notes/codebase/auth-middleware","instructions/deploy",5,"Notes/Mixed-Case"]}`),
	postCase("memory cleanup with nothing to remove", cleanupPath, "agent-a-session",
		`{"namespace":"shared","valid_source_files":["notes/codebase/dream-lock","imports/raw-file","anne_walker/memory/day-1","annexwalker/memory/day-2"]}`),
	postCase("memory cleanup list not an array", cleanupPath, "agent-a-session", `{"namespace":"agent-a","valid_source_files":"x"}`),
	postCase("memory cleanup without delete access", cleanupPath, "agent-b-session", `{"namespace":"agent-a","valid_source_files":[]}`),
}

var memoryStatusCases = []Scenario{
	postCase("memory status one namespace", statusPath, "agent-b-session", `{"namespace":"shared"}`),
	postCase("memory status without read access", statusPath, "agent-b-session", `{"namespace":"private-ns"}`),
	// Without a namespace Node filters by the readable namespaces, but with
	// the actor type in place of its name (LLM-918): agent-b sees shared and
	// agent-a, and a wildcard reader gets a 500.
	postCase("memory status all namespaces as agent-b", statusPath, "agent-b-session", `{}`),
	postCase("memory status all namespaces as a wildcard reader", statusPath, "agent-a-session", `{}`),
	// The throw is inside the filter callback, so with no files at all a
	// wildcard reader gets an empty list.
	{
		Name: "memory status all namespaces as a wildcard reader with no files",
		Steps: []Step{
			{Method: "POST", Path: cleanupPath, Auth: "agent-a-session", Header: jsonType, Body: `{"namespace":"agent-a","valid_source_files":[]}`},
			{Method: "POST", Path: cleanupPath, Auth: "agent-a-session", Header: jsonType, Body: `{"namespace":"shared","valid_source_files":[]}`},
			{Method: "POST", Path: cleanupPath, Auth: "agent-a-session", Header: jsonType, Body: `{"namespace":"private-ns","valid_source_files":[]}`},
			{Method: "POST", Path: statusPath, Auth: "agent-a-session", Header: jsonType, Body: `{}`},
		},
	},
}
