// Service layer for chat operations (send, receive, ack, status).
// Extracted from routes/chat.js so both REST routes and MCP handler can share the same logic.

const pool = require('../db');
const { log } = require('./logger');
const { broadcast } = require('./events');
const systemHandler = require('./system-handler');
const { resolveByName, resolveMultipleByName, requireByName, canAccessVirtualAgent } = require('./actors');
const { safeInt } = require('../util');

function logChat(action, details) {
    log('chat', action, details);
}

// Parse discussion_id from either an integer or a legacy 'discussion-{N}' channel string.
// Returns a positive integer or null. Discussion IDs are pg SERIAL so 0 and
// negatives are never valid identifiers; collapsing them to null here keeps
// callers from having to redo the same range check.
function resolveDiscussionId(discussionId, channel) {
    if (discussionId !== undefined && discussionId !== null && discussionId !== '') {
        const parsed = safeInt(discussionId);
        return parsed !== null && parsed > 0 ? parsed : null;
    }
    // Legacy backward compat: extract from channel string
    if (channel) {
        const match = channel.match(/^discussion-(\d+)$/);
        if (match) {
            const parsed = safeInt(match[1]);
            return parsed !== null && parsed > 0 ? parsed : null;
        }
    }
    return null;
}

// Tool-use fields (toolCalls / toolCallId / toolsOffered, all optional) are
// MEM-119 additions for the Salem engine ↔ NPC chat path. Existing callers
// pass nothing and behavior is unchanged. When the message carries
// toolsOffered, handleDirectChat takes the tool-use branch. Returns
// `pendingReplyPromise` (or null) so wait-mode HTTP routes can await the
// VA's reply inline; non-wait callers ignore it and the dispatch stays
// fire-and-forget.
//
// sceneId (MEM-121) is the engine's per-cascade UUID. When present, it
// rides through to the chat row, the call log, and the VA's reply chat
// row so the admin UI can group all rows from one tavern conversation
// (or whatever scene) together. NULL for companion-mode.
async function chatSend(fromAgent, toAgents, discussionId, message, opts) {
    // toolCallResults (parallel-tool-call path): an array of { id, content }
    // pairs, one per tool the model emitted in its prior assistant reply.
    // When non-empty, persists N tool-result rows in one request instead of
    // forcing the engine to re-emit dropped calls across multiple
    // round-trips. Mutually exclusive with the singular toolCallId/message
    // shape (route enforces).
    //
    // persistOnly skips the VA dispatch entirely. Used by the engine when
    // the prior reply included a terminal tool (done() / unknown) — the
    // tool results still need to be persisted so the assistant's
    // tool_calls aren't orphaned in history, but no follow-up LLM call
    // is needed.
    const toolCallResults = opts && Array.isArray(opts.toolCallResults) && opts.toolCallResults.length > 0
        ? opts.toolCallResults : null;
    const persistOnly = Boolean(opts && opts.persistOnly === true);
    if (!fromAgent) {
        throw Object.assign(new Error('Required field: from_agent'), { statusCode: 400 });
    }
    // toolCallResults substitutes for `message` — N tool-result rows in
    // lieu of one user-text row. The route enforces mutual exclusivity, so
    // require `message` only on the singular path.
    if (!toolCallResults && (message === undefined || message === null)) {
        throw Object.assign(new Error('Required field: message'), { statusCode: 400 });
    }
    if (!toAgents && !discussionId) {
        throw Object.assign(new Error('Required: to_agents (array) or discussion_id'), { statusCode: 400 });
    }
    const toolCalls = opts && opts.toolCalls !== undefined ? opts.toolCalls : null;
    const toolCallId = opts && opts.toolCallId !== undefined ? opts.toolCallId : null;
    const toolsOffered = opts && opts.toolsOffered !== undefined ? opts.toolsOffered : null;
    const sceneId = opts && opts.sceneId !== undefined ? opts.sceneId : null;
    // sceneStructure (MEM-127) — denormalized structure-name stamp from the
    // engine. Pre-resolved engine-side because the village_object/asset
    // tables that produce the name live in the engine's zbbs database,
    // not memory_api. Optional; null for companion-mode chat and for
    // engine messages that originated from a structure-less cascade
    // (chronicler dispatch, admin trigger, noticeboard generation).
    const sceneStructure = opts && opts.sceneStructure !== undefined ? opts.sceneStructure : null;
    // conversationId (MEM-133 / ZBBS-HOME-396): the engine's narrative-beat scene
    // id — the STABLE grouping key (across the ticks AND participants of one
    // conversation beat) the admin chat viewer collapses a whole exchange under,
    // distinct from the per-tick sceneId. NULL outside grouped sim ticks.
    const conversationId = opts && opts.conversationId !== undefined ? opts.conversationId : null;
    // ephemeralContext (lean sim-history): per-tick scratch context (current
    // affordances / world-state) forwarded to handleDirectChat to attach to
    // the model's current turn. NOT persisted — it never enters rowSpecs, so
    // only `message` (the durable narrative) hits chat_message_texts.
    const ephemeralContext = opts && opts.ephemeralContext !== undefined ? opts.ephemeralContext : null;
    // is_error flag (MEM-122): set on retry/error breadcrumb rows so
    // history readers (loadDirectChatHistory, loadChatHistory) filter
    // them out of next-call context. Without this, virtual agents read
    // their own error rows as if they were real conversation.
    // Coerce to a real boolean — `opts && opts.isError === true` returns
    // the falsy left operand (undefined/null) when opts is missing, and
    // pg sends those as NULL which violates is_error's NOT NULL constraint.
    const isError = Boolean(opts && opts.isError === true);
    // wait flag: the route is awaiting the VA reply inline via wait=true.
    // Used to forward ackReplyOnInsert into handleDirectChat below — the
    // reply row is consumed inline, so it should be acked at insert
    // rather than sitting unacked forever (no separate /chat/ack call).
    const wait = opts && opts.wait === true;
    // ackOnInsert: stamp acked_at = NOW() on the chat_messages delivery
    // row(s) at insert time. Set by handleDirectChat for the reply chat
    // it writes when the original /chat/send was wait=true — see comment
    // on `wait` above.
    const ackOnInsert = opts && opts.ackOnInsert === true;

    // Resolve sender
    const fromActor = await requireByName(fromAgent);

    const discussionParticipants = new Set();
    const recipientSet = new Set();

    if (discussionId) {
        const participants = await pool.query(
            `SELECT ac.name AS agent FROM discussion_participants dp
             JOIN actors ac ON ac.id = dp.actor_id
             WHERE dp.discussion_id = $1 AND dp.status = $2 AND dp.actor_id != $3`,
            [discussionId, 'joined', fromActor.id]
        );
        for (const row of participants.rows) {
            discussionParticipants.add(row.agent);
            recipientSet.add(row.agent);
        }
    }

    if (toAgents && toAgents.length === 1 && toAgents[0] === '*') {
        const known = await pool.query(
            `SELECT ac.name AS agent FROM agent_configuration agc
             JOIN actors ac ON ac.id = agc.actor_id
             WHERE ac.name != $1`,
            [fromAgent]
        );
        for (const row of known.rows) {
            recipientSet.add(row.agent);
        }
    } else if (toAgents) {
        if (!Array.isArray(toAgents) || toAgents.length === 0) {
            throw Object.assign(new Error('to_agents must be a non-empty array'), { statusCode: 400 });
        }
        for (const agent of toAgents) {
            recipientSet.add(agent);
        }
    }

    recipientSet.delete(fromAgent);
    const recipients = Array.from(recipientSet);
    if (recipients.length === 0) {
        throw Object.assign(new Error('No recipients resolved'), { statusCode: 400 });
    }

    // Resolve all recipients to actor IDs (also validates they exist)
    const recipientActors = await resolveMultipleByName(recipients);
    for (const name of recipients) {
        if (!recipientActors.has(name)) {
            throw Object.assign(new Error(`Agent "${name}" is not registered`), { statusCode: 404 });
        }
    }

    // Build the list of (message, toolCallId) pairs to insert. Default
    // path is one pair from the singular (message, toolCallId) opts.
    // toolCallResults path is N pairs — each entry becomes its own
    // chat_message_texts row, with the next assistant turn seeing all of
    // them at once instead of having to re-emit dropped tool calls
    // across multiple round-trips.
    const rowSpecs = toolCallResults
        ? toolCallResults.map(r => ({ message: r.content, toolCallId: r.id }))
        : [{ message, toolCallId }];

    // Insert text rows + delivery rows in one transaction. Atomicity
    // matters: a partial multi-row write would leave half-orphan tool
    // results that openai.js can paper over with orphan-drop, but this
    // path is explicitly trying to PRESERVE protocol completeness.
    // Delivery rows reference text ids, so they must commit together.
    //
    // Broadcast and VA dispatch happen AFTER commit — they read state
    // the world should already see, and rolling back a broadcast or a
    // half-fired VA call isn't possible anyway.
    //
    // scene_structure inheritance (MEM-128): when scene_id is set but
    // scene_structure isn't passed, inherit it from the FIRST row in
    // the same scene that DID carry a non-null scene_structure. The
    // engine populates scene_structure on the first message of a
    // scene (chronicler perception, NPC perception build) but
    // follow-up tool-call rows in the same scene typically don't
    // carry it. Without inheritance the admin chat UI groups by
    // scene_id and renders most scenes as `Scene · N messages · uuid`
    // without the structure label, since the grouper picks the first
    // row's value and that's often the bare tool-result row.
    //
    // COALESCE($8, ...) keeps the explicit-pass path zero-cost: when
    // the caller did pass a scene_structure, the subquery doesn't
    // run. The LIMIT-1 lookup is cheap given idx_cmt_scene.
    const insertedTexts = [];
    // Per-text delivery id map: when broadcasting one event per text
    // row, we need the matching delivery id (not just the LAST one
    // across all texts, which would alias N broadcasts onto one id and
    // confuse any admin reader keying by delivery).
    const deliveryIdByTextId = new Map();
    const results = [];
    const allDeliveryIds = [];

    const client = await pool.connect();
    try {
        await client.query('BEGIN');

        for (const [idx, spec] of rowSpecs.entries()) {
            const textResult = await client.query(
                `INSERT INTO chat_message_texts
                    (message, from_actor_id, discussion_id, sent_at, tool_calls, tool_call_id, tools_offered, scene_id, scene_structure, is_error, conversation_id)
                 VALUES (
                    $1, $2, $3, NOW(), $4, $5, $6, $7,
                    COALESCE($8, CASE
                        WHEN $7::uuid IS NULL THEN NULL
                        ELSE (
                            SELECT scene_structure FROM chat_message_texts
                             WHERE scene_id = $7::uuid AND scene_structure IS NOT NULL
                             ORDER BY id ASC LIMIT 1
                        )
                    END),
                    $9, $10
                 )
                 RETURNING id, sent_at`,
                [
                    spec.message,
                    fromActor.id,
                    discussionId || null,
                    // tool_calls (assistant-side tool_calls metadata) is only
                    // meaningful on the first row of the batch; subsequent
                    // rows are pure tool-result rows. Keep the existing field
                    // on row 0 for back-compat with single-row callers; null
                    // it on follow-up rows.
                    idx === 0 && toolCalls !== null ? JSON.stringify(toolCalls) : null,
                    spec.toolCallId,
                    // tools_offered likewise belongs to the first row only
                    // (it's a property of the assistant turn that elicited
                    // these tool results, not of each result individually).
                    idx === 0 && toolsOffered !== null ? JSON.stringify(toolsOffered) : null,
                    sceneId,
                    sceneStructure,
                    isError,
                    conversationId,
                ]
            );
            insertedTexts.push({
                id: textResult.rows[0].id,
                sent_at: textResult.rows[0].sent_at,
                message: spec.message,
            });
        }

        // Insert one delivery row per (text row × recipient). acked_at
        // gets stamped at insert time when ackOnInsert is set — used
        // for replies to wait=true callers, who consume the reply
        // inline and have no path to ack it afterwards. Bound parameter
        // for the boolean; CASE keeps it to one SQL string instead of
        // branching the JS.
        //
        // For the toolCallResults path, this produces N delivery rows
        // per recipient — one per tool result. The admin chat UI's
        // scene grouping collapses them under one scene heading.
        for (const recipient of recipients) {
            const toActor = recipientActors.get(recipient);
            let lastDeliveryId = null;
            for (const t of insertedTexts) {
                const result = await client.query(
                    `INSERT INTO chat_messages (message_text_id, to_actor_id, acked_at)
                     VALUES ($1, $2, CASE WHEN $3::boolean THEN NOW() ELSE NULL END)
                     RETURNING id`,
                    [t.id, toActor.id, ackOnInsert]
                );
                lastDeliveryId = result.rows[0].id;
                allDeliveryIds.push(lastDeliveryId);
                // Track first-recipient delivery per text for the
                // broadcast loop (the broadcast event is logically
                // per-text, with to_agents naming all recipients —
                // pick one delivery id to identify the row).
                if (!deliveryIdByTextId.has(t.id)) {
                    deliveryIdByTextId.set(t.id, lastDeliveryId);
                }
            }
            // Surface the LAST delivery id for this recipient — same
            // shape as the original single-row path (one entry per
            // recipient), and the most relevant id for VA dispatch /
            // wait-mode (the dispatch keys off the most-recent thing
            // the recipient saw).
            results.push({ id: lastDeliveryId, agent: recipient, sent_at: insertedTexts[insertedTexts.length - 1].sent_at });
        }

        await client.query('COMMIT');
    } catch (err) {
        try { await client.query('ROLLBACK'); } catch (_) { /* best-effort */ }
        throw err;
    } finally {
        client.release();
    }

    // The "primary" row for downstream broadcast/dispatch is the LAST
    // inserted — that's the most recent thing the recipient should see.
    // For the singular path this is the one and only row; for the
    // toolCallResults path it's the last tool result, which is what the
    // VA dispatch (if any) will most naturally key off.
    const lastText = insertedTexts[insertedTexts.length - 1];
    const messageTextId = lastText.id;
    const sentAt = lastText.sent_at;

    // Broadcast to admin WebSocket clients for live updates. One event
    // per inserted text row so the admin chat list shows each tool
    // result as its own row (matches the persistence model and how
    // single-result rows are broadcast today). Each broadcast uses the
    // FIRST-recipient delivery id for that text row, so admin readers
    // keying by delivery don't see the same id repeated across rows.
    if (results.length > 0) {
        for (const t of insertedTexts) {
            broadcast('chat_message', {
                id: deliveryIdByTextId.get(t.id),
                message_text_id: t.id,
                from_agent: fromAgent,
                to_agents: results.map(function(r) { return r.agent; }),
                message: t.message,
                discussion_id: discussionId || null,
                scene_id: sceneId,
                conversation_id: conversationId,
                sent_at: t.sent_at
            });
        }
    }

    logChat('send', { from_agent: fromAgent, to_agents: recipients, message_text_id: messageTextId, delivery_ids: allDeliveryIds, discussion_id: discussionId || null, tool_results_count: toolCallResults ? toolCallResults.length : 0, persist_only: persistOnly });

    // Fire-and-forget: if any recipient is 'system', dispatch to system handler
    for (const r of results) {
        if (r.agent === 'system') {
            systemHandler.handleMessage(r.id, fromAgent, message, null).catch(() => {});
            break;
        }
    }

    // Fire-and-forget: if this is a discussion message, trigger virtual agent processing
    if (discussionId && fromAgent !== 'system') {
        const { notifySystem } = require('./system-notify');
        notifySystem({ type: 'virtual-agent', discussionId, triggerType: 'message' }).catch(() => {});
    }

    // VA eligibility check is hoisted out of the prior fire-and-forget IIFE
    // so wait-mode callers can know whether a reply is coming. Fast — two
    // small queries — and the same work the IIFE was doing async anyway.
    //
    // persist_only short-circuits the dispatch entirely: the engine has
    // already seen the assistant's terminal tool call (done() / unknown)
    // and is just persisting the matching tool result rows so the
    // assistant's tool_calls aren't orphaned in conversation history.
    // No follow-up VA call is needed.
    let pendingReplyPromise = null;
    if (!persistOnly && !discussionId && fromAgent !== 'system') {
        try {
            const senderRow = await pool.query(
                'SELECT agc.virtual FROM agent_configuration agc WHERE agc.actor_id = $1',
                [fromActor.id]
            );
            const senderIsVirtual = senderRow.rows[0] && senderRow.rows[0].virtual;
            if (!senderIsVirtual) {
                const recipientIds = recipients.map(r => recipientActors.get(r).id);
                const vr = await pool.query(
                    `SELECT ac.name AS agent FROM agent_configuration agc
                     JOIN actors ac ON ac.id = agc.actor_id
                     WHERE agc.actor_id = ANY($1) AND agc.virtual = true`,
                    [recipientIds]
                );
                if (vr.rows.length > 0) {
                    const { handleDirectChat } = require('./virtual-agent');
                    const dispatches = [];
                    for (const row of vr.rows) {
                        const vrActor = recipientActors.get(row.agent);
                        if (vrActor) {
                            const hasAccess = await canAccessVirtualAgent(fromActor.id, vrActor.id);
                            if (!hasAccess) continue;
                        }
                        const msgRow = results.find(r => r.agent === row.agent);
                        dispatches.push({ agent: row.agent, msgId: msgRow ? msgRow.id : null });
                    }
                    // The discriminator for handleDirectChat's
                    // tool-result-follow-up branch is an explicit
                    // boolean — it triggers when the model is responding
                    // to tool results rather than a fresh perception.
                    // True for both the legacy singular toolCallId path
                    // and the new toolCallResults array path. Forwarding
                    // a specific id (e.g. "the last entry's id")
                    // pretends one tool result is the active one, which
                    // is misleading; the VA reads ALL N results from
                    // persisted history.
                    const isToolResultCall = Boolean(
                        (typeof toolCallId === 'string' && toolCallId.length > 0)
                        || (toolCallResults && toolCallResults.length > 0)
                    );

                    // Single-recipient: expose the promise so wait-mode can
                    // await it. Multi-recipient: no clean way to surface
                    // multiple replies in one HTTP response; stay
                    // fire-and-forget (and leave wait-mode to reject).
                    if (dispatches.length === 1) {
                        const d = dispatches[0];
                        pendingReplyPromise = handleDirectChat(d.agent, fromAgent, message, d.msgId, {
                            toolsOffered, isToolResultCall, sceneId, sceneStructure, conversationId, ackReplyOnInsert: wait, ephemeralContext,
                        });
                        // Swallow rejection for non-wait callers so unhandled-promise
                        // warnings don't appear; wait-mode awaiters get the error.
                        pendingReplyPromise.catch(() => {});
                    } else {
                        // Multi-recipient never coexists with wait=true (the
                        // route rejects that combination), so ackReplyOnInsert
                        // stays false here.
                        for (const d of dispatches) {
                            handleDirectChat(d.agent, fromAgent, message, d.msgId, {
                                toolsOffered, isToolResultCall, sceneId, sceneStructure, conversationId, ephemeralContext,
                            }).catch(() => {});
                        }
                    }
                }
            }
        } catch (e) { /* ignore — eligibility check is best-effort */ }
    }

    return { from_agent: fromAgent, to_agents: results, sent_at: sentAt, pendingReplyPromise };
}

async function chatReceive(agent, discussionId, afterId, fromAgent) {
    if (!agent) {
        throw Object.assign(new Error('Required field: agent'), { statusCode: 400 });
    }

    const actor = await requireByName(agent);

    let query = `SELECT cm.id, fa.name AS from_agent, ta.name AS to_agent, cmt.message, cmt.sent_at,
                        cmt.tool_calls, cmt.tool_call_id, cmt.tools_offered
                 FROM chat_messages cm
                 JOIN chat_message_texts cmt ON cmt.id = cm.message_text_id
                 JOIN actors fa ON fa.id = cmt.from_actor_id
                 JOIN actors ta ON ta.id = cm.to_actor_id
                 WHERE cm.to_actor_id = $1 AND cm.acked_at IS NULL AND cm.deleted_at IS NULL`;
    const params = [actor.id];

    if (discussionId) {
        params.push(discussionId);
        query += ` AND cmt.discussion_id = $${params.length}`;
    } else {
        query += ' AND cmt.discussion_id IS NULL';
    }

    if (fromAgent) {
        const fromActor = await requireByName(fromAgent);
        params.push(fromActor.id);
        query += ` AND cmt.from_actor_id = $${params.length}`;
    }

    if (afterId !== undefined) {
        params.push(afterId);
        query += ` AND cm.id > $${params.length}`;
    }

    query += ' ORDER BY cm.id ASC';

    const result = await pool.query(query, params);

    logChat('receive', { agent, discussion_id: discussionId || null, after_id: afterId || null, pending_count: result.rows.length, message_ids: result.rows.map(r => r.id) });

    return { messages: result.rows, pending_count: result.rows.length };
}

async function chatAck(agent, messageIds) {
    if (!agent || !messageIds || !Array.isArray(messageIds) || messageIds.length === 0) {
        throw Object.assign(new Error('Required fields: agent, message_ids (non-empty array)'), { statusCode: 400 });
    }

    const actor = await requireByName(agent);

    const result = await pool.query(
        'UPDATE chat_messages SET acked_at = NOW() WHERE id = ANY($1) AND to_actor_id = $2 AND acked_at IS NULL RETURNING id',
        [messageIds, actor.id]
    );

    logChat('ack', { agent, requested_ids: messageIds, acked_ids: result.rows.map(r => r.id) });

    return { agent, acked: result.rows.length, acked_ids: result.rows.map(r => r.id) };
}

async function chatStatus(agent, discussionId) {
    if (!agent) {
        throw Object.assign(new Error('Required field: agent'), { statusCode: 400 });
    }

    const actor = await requireByName(agent);

    let discussionFilter;
    let params;
    if (discussionId) {
        discussionFilter = 'AND cmt.discussion_id = $2';
        params = [actor.id, discussionId];
    } else {
        discussionFilter = 'AND cmt.discussion_id IS NULL';
        params = [actor.id];
    }

    const pending = await pool.query(
        `SELECT COUNT(*) as count FROM chat_messages cm
         JOIN chat_message_texts cmt ON cmt.id = cm.message_text_id
         WHERE cm.to_actor_id = $1 AND cm.acked_at IS NULL AND cm.deleted_at IS NULL ${discussionFilter}`,
        params
    );
    const latest = await pool.query(
        `SELECT MAX(cm.id) as max_id FROM chat_messages cm
         JOIN chat_message_texts cmt ON cmt.id = cm.message_text_id
         WHERE cm.to_actor_id = $1 AND cm.deleted_at IS NULL ${discussionFilter}`,
        params
    );
    const lastActivity = await pool.query(
        `SELECT MAX(cmt.sent_at) as last_sent FROM chat_messages cm
         JOIN chat_message_texts cmt ON cmt.id = cm.message_text_id
         WHERE cm.to_actor_id = $1 AND cm.deleted_at IS NULL ${discussionFilter}`,
        params
    );
    const lastAcked = await pool.query(
        `SELECT MAX(cm.acked_at) as last_acked FROM chat_messages cm
         JOIN chat_message_texts cmt ON cmt.id = cm.message_text_id
         WHERE cm.to_actor_id = $1 AND cm.acked_at IS NOT NULL AND cm.deleted_at IS NULL ${discussionFilter}`,
        params
    );

    logChat('status', { agent, discussion_id: discussionId || null });

    return {
        agent,
        pending_count: parseInt(pending.rows[0].count),
        max_message_id: latest.rows[0].max_id,
        last_message_at: lastActivity.rows[0].last_sent,
        last_ack_at: lastAcked.rows[0].last_acked
    };
}

module.exports = { chatSend, chatReceive, chatAck, chatStatus, resolveDiscussionId };
