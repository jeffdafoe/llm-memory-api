package contract

// simCases are the /v1/sim routes: the engine's daily conversation push and
// shared-NPC soul synthesis behind the salem-engine gate, and the
// operator-gated raw-turns read. The setup adds a dedicated sim NPC, a
// shared-VA pool, a companion agent (a mode the distiller refuses), makes
// va-anthropic the system-owned dream-sim-soul agent, and puts two call-log
// rows in place for raw-turns.

const simEngineKey = engineName + "-key"

const simSetup = `
INSERT INTO actors (name, created_by) SELECT n, (SELECT id FROM actors WHERE name = 'system')
    FROM (VALUES ('zbbs-john-ellis'), ('salem-vendor'), ('npc-companion')) AS v(n);
INSERT INTO agent_configuration (actor_id, dream_mode)
    SELECT id, CASE name WHEN 'zbbs-john-ellis' THEN 'sim' WHEN 'salem-vendor' THEN 'sim-shared' ELSE 'companion' END
    FROM actors WHERE name IN ('zbbs-john-ellis', 'salem-vendor', 'npc-companion');
UPDATE actors SET expertise = '["dream-sim-soul"]', created_by = (SELECT id FROM actors WHERE name = 'system')
    WHERE name = 'va-anthropic';
INSERT INTO virtual_agent_calls (actor_id, provider, model, context, scene_id, conversation_id, sim_actor_id, sim_actor_name,
        system_prompt, user_message, response, status, status_code, error_message, input_tokens, output_tokens, cost, duration_ms, created_at)
    SELECT id, 'openrouter', 'deepseek/deepseek-v4-flash', 'chat', '6d2a0c1e-0000-4000-8000-0000000000a1', 'hud-abc', 'a1', 'John Ellis',
        'system prompt', 'You see the tavern.', 'Another round?', 'success', NULL, NULL, 10, 5, 0.000035, 120, '2026-11-05T10:00:00.123456Z'
    FROM actors WHERE name = 'va-openrouter';
INSERT INTO virtual_agent_calls (actor_id, provider, model, context, scene_id, conversation_id, sim_actor_id, sim_actor_name,
        system_prompt, user_message, response, status, status_code, error_message, input_tokens, output_tokens, cost, duration_ms, created_at)
    SELECT id, 'openrouter', 'deepseek/deepseek-v4-flash', 'chat', NULL, 'hud-def', 'b2', 'Anne Hale',
        'system prompt', 'You see the market.', NULL, 'error', 502, 'Provider error', 0, 0, 0, 30, '2026-11-05T11:00:00Z'
    FROM actors WHERE name = 'va-openrouter';
`

func simPost(path, auth, body string) Step {
	return Step{Method: "POST", Path: "/v1/sim/" + path, Auth: auth, Header: jsonType, Body: body}
}

const simDayEvents = `[
  {"at":"2026-11-05T19:00:00.000Z","kind":"spoke","payload":{"text":"Another round?"},"speaker":"John Ellis"},
  {"at":"2026-11-05T19:00:00.000Z","kind":"spoke","payload":{"text":"Aye"},"speaker":"Jefferey"},
  {"at":"2026-11-05T08:30:00Z","kind":"paid","payload":{"recipient":"Josiah Thorne","amount":2,"for":"ale","rate_settled":0}},
  {"at":"2026-11-05T09:00:00Z","kind":"look_around","payload":{}},
  {"at":"2026-11-06T00:00:00Z","kind":"walked","payload":{"destination":"Mill"}},
  {"at":"2026-11-05T12:00:00Z","kind":"labored","payload":{"employer":"Ezekiel Crane","amount":4,"reward_items":[{"item":"porridge","qty":1}]}},
  {"at":"2026-11-05T13:00:00Z","kind":"gathered","payload":{}}
]`

var simCases = []Scenario{
	{
		Name: "sim conversation-day for a dedicated NPC",
		Steps: []Step{
			{SQL: simSetup},
			simPost("conversation-day", simEngineKey, `{"agent":" ZBBS-John-Ellis ","day":"2026-11-05","events":`+simDayEvents+`}`),
			{Settle: true},
			// A re-push of the day overwrites the note.
			simPost("conversation-day", simEngineKey, `{"agent":"zbbs-john-ellis","day":"2026-11-05","events":[{"at":"2026-11-05T20:00:00Z","kind":"spoke","payload":{"text":"Last call."}}]}`),
			{Settle: true},
			simPost("conversation-day", simEngineKey, `{"agent":"zbbs-john-ellis","day":"2026-11-04","events":[{"at":"2026-11-04T09:00:00Z","kind":"done","payload":{}}]}`),
		},
	},
	{
		Name: "sim conversation-day for a shared-VA villager",
		Steps: []Step{
			{SQL: simSetup},
			simPost("conversation-day", simEngineKey, `{"agent":"salem-vendor","slug_prefix":" constance-scott// ","actor":" Constance Scott ","day":"2026-11-05","events":`+simDayEvents+`}`),
			{Settle: true},
			// The roster row's display name and day follow the latest push.
			simPost("conversation-day", simEngineKey, `{"agent":"salem-vendor","slug_prefix":"constance-scott/","actor":"Constance [Scott]","day":"2026-11-06","events":[{"at":"2026-11-06T07:00:00Z","kind":"spoke","payload":{"text":"Morning."}}]}`),
		},
	},
	{
		Name: "sim conversation-day refusals",
		Steps: []Step{
			{SQL: simSetup},
			simPost("conversation-day", simEngineKey, `{"day":"2026-11-05","events":[]}`),
			simPost("conversation-day", simEngineKey, `{"agent":5,"day":"2026-11-05","events":[]}`),
			simPost("conversation-day", simEngineKey, `{"agent":"zbbs-john-ellis","day":"2026-11-5","events":[]}`),
			simPost("conversation-day", simEngineKey, `{"agent":"zbbs-john-ellis","day":"2026-11-05","events":{}}`),
			simPost("conversation-day", simEngineKey, `{"agent":"nobody","day":"2026-11-05","events":[]}`),
			simPost("conversation-day", simEngineKey, `{"agent":"npc-companion","day":"2026-11-05","events":[]}`),
			simPost("conversation-day", simEngineKey, `{"agent":"salem-vendor","day":"2026-11-05","events":[],"actor":"A"}`),
			simPost("conversation-day", simEngineKey, `{"agent":"salem-vendor","day":"2026-11-05","events":[],"slug_prefix":"../x/","actor":"A"}`),
			simPost("conversation-day", simEngineKey, `{"agent":"salem-vendor","day":"2026-11-05","events":[],"slug_prefix":"a/","actor":"[]"}`),
			simPost("conversation-day", simEngineKey, `{"agent":"zbbs-john-ellis","day":"2026-02-31","events":[]}`),
			simPost("conversation-day", simEngineKey, `{"agent":"zbbs-john-ellis","day":"2026-11-05","events":[{"at":"2026-11-05T10:00:00Z","kind":"object_refresh","payload":{"refreshes":[null]}}]}`),
			// No JSON body at all is req.body || {}.
			{Method: "POST", Path: "/v1/sim/conversation-day", Auth: simEngineKey},
		},
	},
	{
		Name: "sim soul",
		Steps: []Step{
			{SQL: simSetup},
			simPost("soul", simEngineKey, `{"character_description":"  Moses James, a farmer at the James Farm.  ","current_soul":"Steady.","day_snapshot":"- planted corn","day":"2026-11-05"}`),
			{Settle: true},
			simPost("soul", simEngineKey, `{"character_description":"Moses James","day_snapshot":"- planted corn"}`),
			{Settle: true},
			simPost("soul", simEngineKey, `{"character_description":"Moses James","day_snapshot":"- planted corn"}`),
			{Settle: true},
			simPost("soul", simEngineKey, `{"character_description":"   ","day_snapshot":"x"}`),
			simPost("soul", simEngineKey, `{"character_description":"M","day_snapshot":"x","current_soul":5}`),
			simPost("soul", simEngineKey, `{"character_description":"M","day_snapshot":"x","day":"2026-11-05\n## Inject"}`),
		},
		Replies: []QueuedReply{
			anthropicText("  A farmer of few words, steady with the land.  "),
			anthropicText("Thinking process: first I consider the farm."),
			anthropicText("   "),
		},
		Known: []KnownDifference{callDuration},
	},
	{
		// No system-owned dream-sim-soul agent: Node's 503, answered as 500.
		Name: "sim soul without the soul agent",
		Steps: []Step{
			simPost("soul", simEngineKey, `{"character_description":"M","day_snapshot":"x"}`),
		},
	},
	{
		Name: "sim raw-turns",
		Steps: []Step{
			{SQL: simSetup},
			simPost("raw-turns", "admin-web", `{}`),
			simPost("raw-turns", "admin-web", `{"scene_id":"6D2A0C1E-0000-4000-8000-0000000000A1"}`),
			simPost("raw-turns", "admin-web", `{"agent":"va-openrouter","status":"error","limit":"1"}`),
			simPost("raw-turns", "admin-web", `{"conversation":"hud-abc","sim_actor":"  a1  "}`),
			simPost("raw-turns", "admin-web", `{"since":"2026-11-05T10:30:00Z","until":"2026-11-06"}`),
			simPost("raw-turns", "admin-web", `{"limit":1}`),
			simPost("raw-turns", "admin-web", `{"scene_id":"nope"}`),
			simPost("raw-turns", "admin-web", `{"agent":5}`),
			simPost("raw-turns", "admin-web", `{"since":"not a date"}`),
			simPost("raw-turns", "admin-web", `{"status":"ok"}`),
			// The operator gate: plugins/administer.
			simPost("raw-turns", "agent-b-key", `{}`),
			simPost("raw-turns", simEngineKey, `{}`),
		},
	},
	{
		Name: "sim engine gate",
		Steps: []Step{
			{SQL: simSetup},
			simPost("soul", "agent-a-key", `{"character_description":"M","day_snapshot":"x"}`),
			simPost("conversation-day", "admin-web", `{"agent":"zbbs-john-ellis","day":"2026-11-05","events":[]}`),
			simPost("conversation-day", "agent-a-session", `{}`),
			{Method: "OPTIONS", Path: "/v1/sim/conversation-day", Auth: "agent-a-key"},
			{Method: "OPTIONS", Path: "/v1/sim/conversation-day", Auth: simEngineKey},
			{Method: "OPTIONS", Path: "/v1/sim/raw-turns", Auth: simEngineKey},
			{Method: "OPTIONS", Path: "/v1/sim/raw-turns", Auth: "agent-a-key"},
			{Method: "GET", Path: "/v1/sim/raw-turns", Auth: "agent-a-key"},
			{Method: "GET", Path: "/v1/sim/soul", Auth: simEngineKey},
			simPost("unknown", simEngineKey, `{}`),
			simPost("unknown", "agent-a-key", `{}`),
			{Method: "POST", Path: "/v1/sim", Auth: "agent-a-key", Header: jsonType, Body: `{}`},
			// A percent-encoded spelling is not under the /sim mount.
			{Method: "POST", Path: "/v1/%73im/soul", Auth: simEngineKey, Header: jsonType, Body: `{}`},
			{Method: "POST", Path: "/v1/sim/%73oul", Auth: simEngineKey, Header: jsonType, Body: `{}`},
			// No credentials: the /v1 auth answers first.
			{Method: "POST", Path: "/v1/sim/soul", Header: jsonType, Body: `{}`},
		},
	},
}
