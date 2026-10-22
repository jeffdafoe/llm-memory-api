// Which direct-chat history a virtual-agent turn replays (LLM-687).
//
// Two scopes:
//   - 'scene': only the rows of the turn's scene_id. With no scene_id the
//     history is empty (fail-closed). For agents whose one VA row backs many
//     personas or cases: the salem switchboard VAs (salem-visitor,
//     salem-vendor), the stateless cascade VA (salem-generic) and the court
//     (salem-magistrate). Unscoped history would replay one persona's or one
//     case's rows into another.
//   - default (no history_scope): the agent-pair history over
//     virtual_agent_chat_history_hours. This is the continuity channel of a
//     stateful NPC with its own VA, so it must stay the default.
//
// The caller states the scope with the optional history_scope field of
// POST /v1/chat/send. The salem engine owns the knowledge of which of its VAs
// are switchboards, so it sends 'scene' for them. The field can only narrow the
// history a turn replays, never widen it, so any authenticated caller may send it.
//
// Lives in its own pure module so the route validation and the scope decision
// can be unit-tested without loading the DB pool or the virtual-agent stack.

const HISTORY_SCOPE_SCENE = 'scene';

// Agents that get scene-scoped history even when the caller does not send
// history_scope. This keeps their scope for an engine build that does not send
// the field yet; it is removed once the engine sends it for all of them.
const LEGACY_SCENE_SCOPED_AGENTS = new Set(['salem-visitor', 'salem-vendor', 'salem-generic']);

// Validate the optional history_scope request field. Returns { value } with
// 'scene' or null, or { error } with the 400 message for any other value.
function parseHistoryScope(raw) {
    if (raw === undefined || raw === null) {
        return { value: null };
    }
    if (raw !== HISTORY_SCOPE_SCENE) {
        return { error: 'history_scope must be "scene" when present' };
    }
    return { value: HISTORY_SCOPE_SCENE };
}

// Whether this turn replays only its own scene's history. isCourtAgent is
// passed in because the court agent set lives with the court framing in
// virtual-agent.js.
function isSceneScopedHistory(agentName, historyScope, isCourtAgent) {
    if (historyScope === HISTORY_SCOPE_SCENE) {
        return true;
    }
    if (LEGACY_SCENE_SCOPED_AGENTS.has(agentName)) {
        return true;
    }
    return isCourtAgent === true;
}

// How to load the history of one turn. A scene-scoped turn with no scene_id
// gets { skip: true }: empty history, never the unscoped window (fail-closed).
// Otherwise { sceneId } is the filter for loadDirectChatHistory, where null
// means the unscoped agent-pair history.
function historyLoadFor(isSceneScoped, sceneId) {
    if (!isSceneScoped) {
        return { skip: false, sceneId: null };
    }
    if (!sceneId) {
        return { skip: true, sceneId: null };
    }
    return { skip: false, sceneId };
}

module.exports = { HISTORY_SCOPE_SCENE, parseHistoryScope, isSceneScopedHistory, historyLoadFor };
