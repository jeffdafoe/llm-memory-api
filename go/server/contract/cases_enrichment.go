package contract

import "context"

// enrichmentCases turn note enrichment on through the admin config route
// (both servers read config from their cache, which that route updates),
// add the memory-enrichment virtual agent (va-anthropic's provider, model,
// key and configuration), and save notes as agent-a. The classification
// runs in the background after the save, beside the note's embedding call,
// so the outbound requests are a multiset. The rows (the note's metadata,
// the usage and call rows) and the Messages request are compared.
var enrichmentCases = []Scenario{
	{
		// Classified, then saved again at once: the second save keeps the
		// metadata stamped a moment ago, so it is not classified again; a
		// conversation is never classified.
		Name: "enrichment classifies a saved note",
		Steps: []Step{
			enrichmentOn,
			{SQL: enrichmentAgent},
			{Method: "POST", Path: savePath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"namespace":"agent-a","title":"Deploy steps","content":"# Deploy\n\nRun the deploy script, then check the health route.","slug":"notes/deploy-steps"}`},
			// Each side must classify, so the case cannot pass on two
			// identical failures.
			classified("notes/deploy-steps", "procedural"),
			// The note's indexing runs apart from the classification; the
			// upsert's own indexing must not overlap it (concurrent ingests
			// of one file are not serialized, in either server).
			{Settle: true},
			{Method: "POST", Path: savePath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"namespace":"agent-a","title":"Deploy steps","content":"# Deploy\n\nRun the deploy script.","slug":"notes/deploy-steps","upsert":true}`},
			{Settle: true},
			{Method: "POST", Path: savePath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"namespace":"agent-a","title":"Conv","content":"[10:00 agent-a] hello","slug":"conversations/2026-11-06-enrich"}`},
		},
		Replies:          []QueuedReply{anthropicText("```json\n{\"cognitive_type\": \" Procedural \"}\n```")},
		OutboundMultiset: true,
		Known:            []KnownDifference{callDuration},
	},
	{
		// The fake's default Messages answer is not JSON: the call is made
		// and recorded, the metadata is not written.
		Name: "enrichment answer that does not parse",
		Steps: []Step{
			enrichmentOn,
			{SQL: enrichmentAgent},
			{Method: "POST", Path: savePath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"namespace":"agent-a","title":"Odd","content":"Something.","slug":"notes/odd"}`},
			// The model must have been called on each side.
			{Settle: true, Until: func(ctx context.Context, t *Target) (bool, error) {
				return t.Holds(ctx, `SELECT EXISTS (SELECT 1 FROM virtual_agent_calls c JOIN actors a ON a.id = c.actor_id
                    WHERE a.name = 'memory-enrichment' AND c.context = 'note-enrichment')
                    AND EXISTS (SELECT 1 FROM actors WHERE name = 'memory-enrichment' AND active_since IS NULL)`)
			}},
		},
		OutboundMultiset: true,
		Known:            []KnownDifference{callDuration},
	},
	{
		// On with no memory-enrichment agent: the save does nothing more.
		Name: "enrichment without its agent",
		Steps: []Step{
			enrichmentOn,
			{Method: "POST", Path: savePath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"namespace":"agent-a","title":"Plain","content":"Plain body.","slug":"notes/plain"}`},
		},
	},
	{
		// An upsert keeps the stored metadata; one that is a JSON string
		// holding no JSON fails the save after the row is written (Node
		// parsed it in saveNote, enrichment on or off).
		Name: "notes save upsert over metadata that does not parse",
		Steps: []Step{
			{SQL: `UPDATE documents SET metadata = '"not json"'::jsonb WHERE namespace = 'agent-a' AND slug = 'notes/codebase/auth-middleware';`},
			{Method: "POST", Path: savePath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"namespace":"agent-a","title":"Replaced","content":"## Replaced\nNew body.","slug":"notes/codebase/auth-middleware","upsert":true}`},
		},
		Known: []KnownDifference{
			{Where: "table error_log row *.error_message", Reason: "a stored metadata string that is not JSON fails with Go's JSON error text, not V8's"},
		},
	},
	{
		// A jsonb number past the float64 range is Infinity to
		// node-postgres, and JSON.parse("Infinity") throws: the same 500,
		// with V8's text in both.
		Name: "notes save upsert over metadata past the number range",
		Steps: []Step{
			{SQL: `UPDATE documents SET metadata = '1e400'::jsonb WHERE namespace = 'agent-a' AND slug = 'notes/codebase/auth-middleware';`},
			{Method: "POST", Path: savePath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"namespace":"agent-a","title":"Replaced","content":"## Replaced\nNew body.","slug":"notes/codebase/auth-middleware","upsert":true}`},
		},
	},
}

var enrichmentOn = Step{Method: "POST", Path: adminConfigUpdatePath, Auth: "admin-web", Header: jsonType,
	Body: `{"key":"note_enrichment_enabled","value":"true"}`}

// enrichmentAgent is the same text on both copies.
const enrichmentAgent = `
INSERT INTO actors (name, created_by) SELECT 'memory-enrichment', created_by FROM actors WHERE name = 'va-anthropic';
INSERT INTO agent_configuration (actor_id, virtual, provider, model, api_key, configuration, startup_instructions)
    SELECT (SELECT id FROM actors WHERE name = 'memory-enrichment'), true, provider, model, api_key, configuration,
           'You classify notes.'
    FROM agent_configuration WHERE actor_id = (SELECT id FROM actors WHERE name = 'va-anthropic');
`

// classified waits until agent-a's note has the cognitive type and the
// enrichment agent is no longer active.
func classified(slug, cognitiveType string) Step {
	return Step{Settle: true, Until: func(ctx context.Context, t *Target) (bool, error) {
		return t.Holds(ctx, `SELECT
                EXISTS (SELECT 1 FROM documents WHERE namespace = 'agent-a' AND slug = $1
                        AND metadata->>'cognitive_type' = $2 AND metadata ? '_enriched_at')
                AND EXISTS (SELECT 1 FROM actors WHERE name = 'memory-enrichment' AND active_since IS NULL)`,
			slug, cognitiveType)
	}}
}
