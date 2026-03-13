// Service layer for discussion operations.
// Extracted from routes/discussion.js so both REST routes and MCP handler can share the same logic.

const pool = require('../db');
const { log } = require('./logger');
const config = require('./config');
const { notifyDiscussionInvite, sendSystemMessageToMany, sendDiscussionEvent, notifySystem } = require('./system-notify');
const { resolveByName, resolveMultipleByName, requireByName } = require('./actors');

function logDiscussion(action, details) {
    log('discussion', action, details);
}

async function getDiscussionChannel(discussionId) {
    const result = await pool.query('SELECT channel FROM discussions WHERE id = $1', [discussionId]);
    if (result.rows.length === 0) {
        return `discussion-${discussionId}`;
    }
    return result.rows[0].channel || `discussion-${discussionId}`;
}

async function evaluateReadiness(discussionId) {
    const discussion = await pool.query('SELECT * FROM discussions WHERE id = $1', [discussionId]);
    if (discussion.rows.length === 0 || discussion.rows[0].status !== 'waiting') {
        return;
    }

    const d = discussion.rows[0];
    const participants = await pool.query(
        `SELECT ac.name AS agent, dp.status, dp.role
         FROM discussion_participants dp
         JOIN actors ac ON ac.id = dp.actor_id
         WHERE dp.discussion_id = $1`,
        [discussionId]
    );

    const required = participants.rows.filter(p => p.role === 'required');
    const optional = participants.rows.filter(p => p.role === 'optional');

    const allRequiredJoined = required.every(p => p.status === 'joined');
    const allOptionalJoined = optional.every(p => p.status === 'joined');

    if (allRequiredJoined && allOptionalJoined) {
        await pool.query('UPDATE discussions SET status = $1 WHERE id = $2', ['active', discussionId]);
        logDiscussion('ready', { discussion_id: discussionId, reason: 'all_joined' });
        notifySystem({ type: 'virtual-agent', discussionId, triggerType: 'discussion-active' }).catch(() => {});
        return;
    }

    const now = new Date();
    if (d.timeout_at && now >= new Date(d.timeout_at)) {
        if (allRequiredJoined) {
            for (const p of optional) {
                if (p.status !== 'joined') {
                    const pActor = await resolveByName(p.agent);
                    await pool.query(
                        'UPDATE discussion_participants SET status = $1 WHERE discussion_id = $2 AND actor_id = $3',
                        ['timed_out', discussionId, pActor.id]
                    );
                }
            }
            await pool.query('UPDATE discussions SET status = $1 WHERE id = $2', ['active', discussionId]);
            logDiscussion('ready', { discussion_id: discussionId, reason: 'timeout_required_present' });
            notifySystem({ type: 'virtual-agent', discussionId, triggerType: 'discussion-active' }).catch(() => {});
        } else {
            const timeoutOutcome = await computeOutcome(discussionId, 'timed_out');
            await pool.query('UPDATE discussions SET status = $1, outcome = $2 WHERE id = $3', ['timed_out', timeoutOutcome, discussionId]);
            await pool.query(
                `UPDATE discussion_participants SET status = 'left'
                 WHERE discussion_id = $1 AND status IN ('invited', 'joined', 'deferred')`,
                [discussionId]
            );
            logDiscussion('timed_out', { discussion_id: discussionId, reason: 'missing_required', outcome: timeoutOutcome });
        }
    }
}

async function requireJoined(discussionId, agent) {
    const actor = await resolveByName(agent);
    if (!actor) {
        return { ok: false, code: 'NOT_PARTICIPANT', message: 'Agent is not a participant in this discussion' };
    }
    const result = await pool.query(
        'SELECT status FROM discussion_participants WHERE discussion_id = $1 AND actor_id = $2',
        [discussionId, actor.id]
    );
    if (result.rows.length === 0) {
        return { ok: false, code: 'NOT_PARTICIPANT', message: 'Agent is not a participant in this discussion' };
    }
    if (result.rows[0].status !== 'joined') {
        return { ok: false, code: 'NOT_JOINED', message: 'Agent has not joined this discussion' };
    }
    return { ok: true };
}

function evaluateThreshold(choiceRows, participantCount, threshold) {
    const yesVotes = choiceRows.find(r => r.choice === 1);
    const yesCount = yesVotes ? parseInt(yesVotes.count) : 0;

    if (threshold === 'unanimous') {
        return yesCount === participantCount;
    }
    if (threshold === 'majority') {
        return yesCount > participantCount / 2;
    }
    return false;
}

async function evaluateVote(voteId) {
    const vote = await pool.query(
        'SELECT v.*, d.id as disc_id FROM discussion_votes v JOIN discussions d ON v.discussion_id = d.id WHERE v.id = $1',
        [voteId]
    );
    if (vote.rows.length === 0) {
        return;
    }
    const v = vote.rows[0];
    if (v.status !== 'open') {
        return;
    }

    const participants = await pool.query(
        'SELECT actor_id FROM discussion_participants WHERE discussion_id = $1 AND status = $2',
        [v.discussion_id, 'joined']
    );
    const participantCount = participants.rows.length;

    const ballots = await pool.query(
        'SELECT choice, COUNT(*) as count FROM discussion_ballots WHERE vote_id = $1 GROUP BY choice',
        [voteId]
    );
    const totalVotes = ballots.rows.reduce((sum, r) => sum + parseInt(r.count), 0);

    const now = new Date();
    const expired = v.closes_at && now >= new Date(v.closes_at);
    const allVoted = totalVotes >= participantCount;

    if (!allVoted && !expired) {
        return;
    }

    await pool.query('UPDATE discussion_votes SET status = $1, closed_at = NOW() WHERE id = $2', ['closed', voteId]);

    if (v.type === 'conclude') {
        const passed = evaluateThreshold(ballots.rows, participantCount, v.threshold);
        if (passed) {
            const voteOutcome = await computeOutcome(v.discussion_id, 'concluded');
            await pool.query(
                'UPDATE discussions SET status = $1, concluded_at = NOW(), outcome = $2 WHERE id = $3',
                ['concluded', voteOutcome, v.discussion_id]
            );
            await pool.query(
                `UPDATE discussion_participants SET status = 'left'
                 WHERE discussion_id = $1 AND status IN ('invited', 'joined', 'deferred')`,
                [v.discussion_id]
            );
            logDiscussion('auto_conclude', { discussion_id: v.discussion_id, vote_id: voteId, outcome: voteOutcome });
        }
    }
}

// Derive the outcome of a finished discussion from its general vote history.
async function computeOutcome(discussionId, newStatus) {
    if (newStatus === 'cancelled' || newStatus === 'timed_out') {
        return 'abandoned';
    }

    const votes = await pool.query(
        "SELECT v.id, v.threshold FROM discussion_votes v WHERE v.discussion_id = $1 AND v.type = 'general' AND v.status = 'closed'",
        [discussionId]
    );

    if (votes.rows.length === 0) {
        return 'consensus';
    }

    let passedCount = 0;
    let failedCount = 0;

    for (const vote of votes.rows) {
        const ballots = await pool.query(
            'SELECT choice, COUNT(*) as count FROM discussion_ballots WHERE vote_id = $1 GROUP BY choice',
            [vote.id]
        );
        const totalVoters = ballots.rows.reduce((sum, r) => sum + parseInt(r.count), 0);

        if (evaluateThreshold(ballots.rows, totalVoters, vote.threshold)) {
            passedCount++;
        } else {
            failedCount++;
        }
    }

    if (failedCount === 0) {
        return 'consensus';
    }
    if (passedCount === 0) {
        return 'deadlock';
    }
    return 'partial';
}

// --- Public service functions ---

async function discussionCreate(topic, createdBy, participants, optionalParticipants, channel, mode, context) {
    if (!topic || !createdBy || !participants || !Array.isArray(participants)) {
        throw Object.assign(new Error('Required fields: topic, created_by, participants (array)'), { statusCode: 400 });
    }

    const optionalList = Array.isArray(optionalParticipants) ? optionalParticipants : [];
    const allParticipants = [...participants, ...optionalList];

    if (allParticipants.length < 2) {
        throw Object.assign(new Error('A discussion requires at least 2 participants'), { statusCode: 400 });
    }

    if (!participants.includes(createdBy)) {
        throw Object.assign(new Error('Creator must be in the participants list'), { statusCode: 400 });
    }

    const overlap = optionalList.filter(a => participants.includes(a));
    if (overlap.length > 0) {
        throw Object.assign(new Error('Agents cannot be both required and optional: ' + overlap.join(', ')), { statusCode: 400 });
    }

    // Resolve all participant names to actor IDs
    const actorMap = await resolveMultipleByName(allParticipants);
    for (const name of allParticipants) {
        if (!actorMap.has(name)) {
            throw Object.assign(new Error(`Agent "${name}" is not registered`), { statusCode: 404 });
        }
    }
    const creatorActor = actorMap.get(createdBy);

    // Reject if creator is already in an active or waiting discussion
    const existing = await pool.query(
        `SELECT d.id, d.topic, d.status FROM discussions d
         JOIN discussion_participants dp ON dp.discussion_id = d.id
         WHERE dp.actor_id = $1 AND dp.status IN ('invited', 'joined', 'deferred')
         AND d.status IN ('waiting', 'active')
         ORDER BY d.id DESC LIMIT 1`,
        [creatorActor.id]
    );
    if (existing.rows.length > 0) {
        const d = existing.rows[0];
        if (d.status === 'waiting') {
            await evaluateReadiness(d.id);
            const recheck = await pool.query('SELECT status FROM discussions WHERE id = $1', [d.id]);
            if (recheck.rows.length > 0 &&
                recheck.rows[0].status !== 'waiting' &&
                recheck.rows[0].status !== 'active') {
                logDiscussion('conflict_cleared', { discussion_id: d.id, new_status: recheck.rows[0].status, agent: createdBy });
            } else {
                throw Object.assign(
                    new Error(`Agent "${createdBy}" is already in discussion #${d.id} (${d.status}): "${d.topic}"`),
                    { statusCode: 409, code: 'DISCUSSION_CONFLICT', existing_discussion_id: d.id }
                );
            }
        } else {
            throw Object.assign(
                new Error(`Agent "${createdBy}" is already in discussion #${d.id} (${d.status}): "${d.topic}"`),
                { statusCode: 409, code: 'DISCUSSION_CONFLICT', existing_discussion_id: d.id }
            );
        }
    }

    const client = await pool.connect();
    try {
        await client.query('BEGIN');

        const validModes = ['realtime', 'async'];
        const discussionMode = validModes.includes(mode) ? mode : 'realtime';
        const contextText = context ? String(context).slice(0, 10000) : null;

        const timeoutKey = discussionMode === 'async'
            ? 'discussion_wait_timeout_async'
            : 'discussion_wait_timeout_realtime';
        const timeoutMinutes = parseInt(config.get(timeoutKey));
        const timeoutAt = new Date(Date.now() + timeoutMinutes * 60 * 1000);

        const result = await client.query(
            'INSERT INTO discussions (topic, created_by_actor_id, status, channel, mode, context, timeout_at) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id, created_at',
            [topic, creatorActor.id, 'waiting', channel || null, discussionMode, contextText, timeoutAt]
        );
        const discussionId = result.rows[0].id;

        // Look up which agents are virtual (auto-join them)
        const actorIds = allParticipants.map(n => actorMap.get(n).id);
        const virtualCheck = await client.query(
            'SELECT actor_id FROM agents WHERE actor_id = ANY($1) AND virtual = TRUE',
            [actorIds]
        );
        const virtualActorIds = new Set(virtualCheck.rows.map(r => r.actor_id));

        for (const agent of participants) {
            const actorId = actorMap.get(agent).id;
            const isCreator = agent === createdBy;
            const isVirtual = virtualActorIds.has(actorId);
            const shouldJoin = isCreator || isVirtual;
            await client.query(
                'INSERT INTO discussion_participants (discussion_id, actor_id, status, role, joined_at) VALUES ($1, $2, $3, $4, $5)',
                [discussionId, actorId, shouldJoin ? 'joined' : 'invited', 'required', shouldJoin ? new Date() : null]
            );
        }

        for (const agent of optionalList) {
            const actorId = actorMap.get(agent).id;
            const isVirtual = virtualActorIds.has(actorId);
            await client.query(
                'INSERT INTO discussion_participants (discussion_id, actor_id, status, role, joined_at) VALUES ($1, $2, $3, $4, $5)',
                [discussionId, actorId, isVirtual ? 'joined' : 'invited', 'optional', isVirtual ? new Date() : null]
            );
        }

        await client.query('COMMIT');

        logDiscussion('create', {
            discussion_id: discussionId, topic, created_by: createdBy,
            required: participants, optional: optionalList, mode: discussionMode
        });

        // Notify invited agents (fire-and-forget)
        const invitedAgents = allParticipants.filter(a => a !== createdBy);
        if (invitedAgents.length > 0) {
            notifyDiscussionInvite(discussionId, topic, createdBy, invitedAgents)
                .catch(err => console.error('System notify error:', err.message));
        }

        await evaluateReadiness(discussionId);

        const current = await pool.query('SELECT status FROM discussions WHERE id = $1', [discussionId]);

        const discussion = {
            id: discussionId,
            topic,
            created_by: createdBy,
            status: current.rows[0].status,
            channel: channel || null,
            mode: discussionMode,
            timeout_at: timeoutAt,
            created_at: result.rows[0].created_at
        };
        if (contextText) {
            discussion.context = contextText;
        }

        const participantList = participants.map(a => ({
            agent: a,
            role: 'required',
            status: (a === createdBy || virtualActorIds.has(actorMap.get(a).id)) ? 'joined' : 'invited'
        })).concat(optionalList.map(a => ({
            agent: a,
            role: 'optional',
            status: virtualActorIds.has(actorMap.get(a).id) ? 'joined' : 'invited'
        })));

        return { discussion, participants: participantList };
    } catch (err) {
        await client.query('ROLLBACK');
        throw err;
    } finally {
        client.release();
    }
}

async function discussionList(status, agent) {
    let query = 'SELECT d.* FROM discussions d';
    const params = [];
    const conditions = [];

    if (agent) {
        const actor = await resolveByName(agent);
        if (!actor) {
            return { discussions: [] };
        }
        query += ' JOIN discussion_participants dp ON d.id = dp.discussion_id';
        params.push(actor.id);
        conditions.push(`dp.actor_id = $${params.length}`);
    }

    if (status) {
        params.push(status);
        conditions.push(`d.status = $${params.length}`);
    }

    if (conditions.length > 0) {
        query += ' WHERE ' + conditions.join(' AND ');
    }

    query += ' ORDER BY d.created_at DESC';

    const result = await pool.query(query, params);

    // Resolve created_by_actor_id to name for each discussion
    for (const row of result.rows) {
        const { resolveById } = require('./actors');
        const actor = await resolveById(row.created_by_actor_id);
        row.created_by = actor ? actor.name : null;
    }

    logDiscussion('list', { status, agent, count: result.rows.length });
    return { discussions: result.rows };
}

async function discussionStatus(discussionId) {
    if (!discussionId) {
        throw Object.assign(new Error('Required field: discussion_id'), { statusCode: 400 });
    }

    const discussion = await pool.query('SELECT * FROM discussions WHERE id = $1', [discussionId]);
    if (discussion.rows.length === 0) {
        throw Object.assign(new Error('Discussion not found'), { statusCode: 404 });
    }

    if (discussion.rows[0].status === 'waiting') {
        await evaluateReadiness(discussionId);
    }

    const participants = await pool.query(
        `SELECT ac.name AS agent, dp.status, dp.role, dp.invited_at, dp.joined_at, dp.deferred_at, dp.defer_count
         FROM discussion_participants dp
         JOIN actors ac ON ac.id = dp.actor_id
         WHERE dp.discussion_id = $1`,
        [discussionId]
    );

    const votes = await pool.query(
        'SELECT * FROM discussion_votes WHERE discussion_id = $1 ORDER BY created_at DESC',
        [discussionId]
    );

    for (const vote of votes.rows) {
        if (vote.status === 'open') {
            await evaluateVote(vote.id);
        }
    }

    const updatedDiscussion = await pool.query('SELECT * FROM discussions WHERE id = $1', [discussionId]);

    // Resolve created_by_actor_id to name
    const { resolveById } = require('./actors');
    const creatorActor = await resolveById(updatedDiscussion.rows[0].created_by_actor_id);
    updatedDiscussion.rows[0].created_by = creatorActor ? creatorActor.name : null;

    // Resolve proposed_by_actor_id to name on votes
    const updatedVotes = await pool.query(
        `SELECT v.*, ac.name AS proposed_by
         FROM discussion_votes v
         JOIN actors ac ON ac.id = v.proposed_by_actor_id
         WHERE v.discussion_id = $1 ORDER BY v.created_at DESC`,
        [discussionId]
    );

    logDiscussion('status', { discussion_id: discussionId });

    return {
        discussion: updatedDiscussion.rows[0],
        participants: participants.rows,
        votes: updatedVotes.rows
    };
}

async function discussionPending(agent, discussionId) {
    if (!agent) {
        throw Object.assign(new Error('Required field: agent'), { statusCode: 400 });
    }

    const actor = await requireByName(agent);

    const invited = await pool.query(
        `SELECT d.* FROM discussions d
         JOIN discussion_participants dp ON d.id = dp.discussion_id
         WHERE dp.actor_id = $1 AND dp.status = $2 AND d.status IN ($3, $4)`,
        [actor.id, 'invited', 'waiting', 'active']
    );

    const deferred = await pool.query(
        `SELECT d.* FROM discussions d
         JOIN discussion_participants dp ON d.id = dp.discussion_id
         WHERE dp.actor_id = $1 AND dp.status = $2 AND d.status IN ($3, $4)`,
        [actor.id, 'deferred', 'waiting', 'active']
    );

    const voteParams = [actor.id, 'joined', 'open'];
    let modeFilter = `AND d.mode = 'async'`;
    if (discussionId) {
        modeFilter = `AND (d.mode = 'async' OR d.id = $4)`;
        voteParams.push(discussionId);
    }

    const openVotes = await pool.query(
        `SELECT v.*, d.topic as discussion_topic, d.mode as discussion_mode
         FROM discussion_votes v
         JOIN discussions d ON v.discussion_id = d.id
         JOIN discussion_participants dp ON d.id = dp.discussion_id
         WHERE dp.actor_id = $1
         AND dp.status = $2
         AND v.status = $3
         AND d.status = 'active'
         ${modeFilter}
         AND NOT EXISTS (
             SELECT 1 FROM discussion_ballots b WHERE b.vote_id = v.id AND b.actor_id = $1
         )
         ORDER BY v.created_at DESC`,
        voteParams
    );

    logDiscussion('pending', { agent, invited: invited.rows.length, deferred: deferred.rows.length, open_votes: openVotes.rows.length });

    return {
        invited_discussions: invited.rows,
        deferred_discussions: deferred.rows,
        open_votes: openVotes.rows
    };
}

async function discussionConclude(discussionId, agent, { cancel = false } = {}) {
    if (!discussionId || !agent) {
        throw Object.assign(new Error('Required fields: discussion_id, agent'), { statusCode: 400 });
    }

    const check = await requireJoined(discussionId, agent);
    if (!check.ok) {
        throw Object.assign(new Error(check.message), { statusCode: 403, code: check.code });
    }

    const discussion = await pool.query(
        'SELECT status, created_by_actor_id, topic FROM discussions WHERE id = $1',
        [discussionId]
    );
    if (discussion.rows.length === 0) {
        throw Object.assign(new Error('Discussion not found'), { statusCode: 404 });
    }

    const dStatus = discussion.rows[0].status;
    if (dStatus === 'waiting') {
        // Check if agent is the creator
        const agentActor = await requireByName(agent);
        if (discussion.rows[0].created_by_actor_id !== agentActor.id) {
            throw Object.assign(new Error('Only the creator can cancel a waiting discussion'), { statusCode: 403 });
        }
    } else if (dStatus !== 'active') {
        throw Object.assign(new Error('Discussion is not active'), { statusCode: 400 });
    }

    const newStatus = (dStatus === 'waiting' || cancel) ? 'cancelled' : 'concluded';
    const outcome = await computeOutcome(discussionId, newStatus);
    await pool.query('UPDATE discussions SET status = $1, concluded_at = NOW(), outcome = $2 WHERE id = $3', [newStatus, outcome, discussionId]);
    await pool.query(
        `UPDATE discussion_participants SET status = 'left'
         WHERE discussion_id = $1 AND status IN ('invited', 'joined', 'deferred')`,
        [discussionId]
    );

    logDiscussion(newStatus === 'cancelled' ? 'cancel' : 'conclude', { discussion_id: discussionId, agent });

    // Notify other participants (fire-and-forget)
    const agentActor = await requireByName(agent);
    const participantRows = await pool.query(
        `SELECT ac.name AS agent FROM discussion_participants dp
         JOIN actors ac ON ac.id = dp.actor_id
         WHERE dp.discussion_id = $1 AND dp.actor_id != $2`,
        [discussionId, agentActor.id]
    );
    const topic = discussion.rows[0].topic;
    if (participantRows.rows.length > 0) {
        const others = participantRows.rows.map(r => r.agent);
        const msg = `Discussion #${discussionId} ("${topic}") was ${newStatus} by ${agent}`;
        sendSystemMessageToMany(others, msg, null).catch(err => {
            console.error('Failed to notify participants of conclude:', err.message);
        });
    }

    getDiscussionChannel(discussionId).then(ch => {
        sendDiscussionEvent(ch, `Discussion ${newStatus} by ${agent}`).catch(err => {
            console.error('Failed to send conclude event:', err.message);
        });
    });

    return { discussion_id: discussionId, status: newStatus, outcome };
}

async function discussionJoin(discussionId, agent) {
    if (!discussionId || !agent) {
        throw Object.assign(new Error('Required fields: discussion_id, agent'), { statusCode: 400 });
    }

    const discussion = await pool.query('SELECT status FROM discussions WHERE id = $1', [discussionId]);
    if (discussion.rows.length === 0) {
        throw Object.assign(new Error('Discussion not found'), { statusCode: 404 });
    }

    if (discussion.rows[0].status === 'waiting') {
        await evaluateReadiness(discussionId);
    }
    const refreshed = await pool.query('SELECT status FROM discussions WHERE id = $1', [discussionId]);
    const dStatus = refreshed.rows[0].status;
    if (dStatus !== 'waiting' && dStatus !== 'active') {
        throw Object.assign(new Error('Discussion is ' + dStatus + ' and not accepting joins'), { statusCode: 400 });
    }

    const actor = await requireByName(agent);

    const existing = await pool.query(
        'SELECT status FROM discussion_participants WHERE discussion_id = $1 AND actor_id = $2',
        [discussionId, actor.id]
    );

    if (existing.rows.length > 0) {
        if (existing.rows[0].status === 'joined') {
            return { discussion_id: discussionId, agent, status: 'joined', discussion_status: dStatus, message: 'Already joined' };
        }
        await pool.query(
            'UPDATE discussion_participants SET status = $1, joined_at = NOW() WHERE discussion_id = $2 AND actor_id = $3',
            ['joined', discussionId, actor.id]
        );
    } else {
        await pool.query(
            'INSERT INTO discussion_participants (discussion_id, actor_id, status, role, joined_at) VALUES ($1, $2, $3, $4, NOW())',
            [discussionId, actor.id, 'joined', 'required']
        );
    }

    logDiscussion('join', { discussion_id: discussionId, agent });

    getDiscussionChannel(discussionId).then(ch => {
        sendDiscussionEvent(ch, `${agent} joined the discussion`).catch(err => {
            console.error('Failed to send join event:', err.message);
        });
    });

    if (dStatus === 'waiting') {
        await evaluateReadiness(discussionId);
    }

    const current = await pool.query('SELECT status FROM discussions WHERE id = $1', [discussionId]);
    return { discussion_id: discussionId, agent, status: 'joined', discussion_status: current.rows[0].status };
}

async function discussionDefer(discussionId, agent) {
    if (!discussionId || !agent) {
        throw Object.assign(new Error('Required fields: discussion_id, agent'), { statusCode: 400 });
    }

    const discussion = await pool.query('SELECT status FROM discussions WHERE id = $1', [discussionId]);
    if (discussion.rows.length === 0) {
        throw Object.assign(new Error('Discussion not found'), { statusCode: 404 });
    }

    const dStatus = discussion.rows[0].status;
    if (dStatus !== 'waiting' && dStatus !== 'active') {
        throw Object.assign(new Error('Discussion is ' + dStatus + ' and not accepting deferrals'), { statusCode: 400 });
    }

    const actor = await requireByName(agent);

    const existing = await pool.query(
        'SELECT status, defer_count FROM discussion_participants WHERE discussion_id = $1 AND actor_id = $2',
        [discussionId, actor.id]
    );
    if (existing.rows.length === 0) {
        throw Object.assign(new Error('Agent is not a participant in this discussion'), { statusCode: 404 });
    }

    const pStatus = existing.rows[0].status;
    if (pStatus !== 'invited' && pStatus !== 'deferred') {
        throw Object.assign(new Error('Can only defer from invited or deferred status, current status: ' + pStatus), { statusCode: 400 });
    }

    const maxDefers = parseInt(config.get('max_defer_count'));
    const currentCount = existing.rows[0].defer_count || 0;
    if (currentCount >= maxDefers) {
        throw Object.assign(
            new Error('Maximum deferrals reached (' + maxDefers + '). Must join or let timeout expire.'),
            { statusCode: 400, code: 'MAX_DEFERRALS_REACHED' }
        );
    }

    await pool.query(
        'UPDATE discussion_participants SET status = $1, deferred_at = NOW(), defer_count = defer_count + 1 WHERE discussion_id = $2 AND actor_id = $3',
        ['deferred', discussionId, actor.id]
    );

    const deferTimeout = parseInt(config.get('discussion_defer_timeout'));
    const newTimeoutAt = new Date(Date.now() + deferTimeout * 60 * 1000);
    await pool.query(
        'UPDATE discussions SET timeout_at = $1 WHERE id = $2',
        [newTimeoutAt, discussionId]
    );

    const updated = await pool.query(
        'SELECT deferred_at, defer_count FROM discussion_participants WHERE discussion_id = $1 AND actor_id = $2',
        [discussionId, actor.id]
    );

    logDiscussion('defer', { discussion_id: discussionId, agent, defer_count: updated.rows[0].defer_count, timeout_at: newTimeoutAt });

    return {
        discussion_id: discussionId,
        agent,
        deferred_at: updated.rows[0].deferred_at,
        defer_count: updated.rows[0].defer_count,
        timeout_at: newTimeoutAt
    };
}

async function discussionLeave(discussionId, agent) {
    if (!discussionId || !agent) {
        throw Object.assign(new Error('Required fields: discussion_id, agent'), { statusCode: 400 });
    }

    const actor = await requireByName(agent);

    const existing = await pool.query(
        'SELECT status FROM discussion_participants WHERE discussion_id = $1 AND actor_id = $2',
        [discussionId, actor.id]
    );

    if (existing.rows.length === 0) {
        throw Object.assign(new Error('Agent is not a participant in this discussion'), { statusCode: 404 });
    }

    await pool.query(
        'UPDATE discussion_participants SET status = $1 WHERE discussion_id = $2 AND actor_id = $3',
        ['left', discussionId, actor.id]
    );

    logDiscussion('leave', { discussion_id: discussionId, agent });

    getDiscussionChannel(discussionId).then(ch => {
        sendDiscussionEvent(ch, `${agent} left the discussion`).catch(err => {
            console.error('Failed to send leave event:', err.message);
        });
    });

    return { discussion_id: discussionId, agent, status: 'left' };
}

async function votePropose(discussionId, proposedBy, question, type, threshold, closesAt) {
    if (!discussionId || !proposedBy || !question) {
        throw Object.assign(new Error('Required fields: discussion_id, proposed_by, question'), { statusCode: 400 });
    }

    const check = await requireJoined(discussionId, proposedBy);
    if (!check.ok) {
        throw Object.assign(new Error(check.message), { statusCode: 403, code: check.code });
    }

    const discussion = await pool.query('SELECT status FROM discussions WHERE id = $1', [discussionId]);
    if (!['active', 'waiting'].includes(discussion.rows[0].status)) {
        throw Object.assign(new Error('Discussion is not active or waiting'), { statusCode: 400 });
    }

    const voteType = type || 'general';
    const voteThreshold = threshold || 'unanimous';

    if (!['general', 'conclude'].includes(voteType)) {
        throw Object.assign(new Error('Vote type must be "general" or "conclude"'), { statusCode: 400 });
    }
    if (!['unanimous', 'majority'].includes(voteThreshold)) {
        throw Object.assign(new Error('Threshold must be "unanimous" or "majority"'), { statusCode: 400 });
    }

    const proposerActor = await requireByName(proposedBy);

    const result = await pool.query(
        'INSERT INTO discussion_votes (discussion_id, proposed_by_actor_id, question, type, threshold, closes_at) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, created_at',
        [discussionId, proposerActor.id, question, voteType, voteThreshold, closesAt || null]
    );

    logDiscussion('vote_propose', { discussion_id: discussionId, vote_id: result.rows[0].id, proposed_by: proposedBy, question, type: voteType });

    getDiscussionChannel(discussionId).then(ch => {
        const msg = `${proposedBy} proposed ${voteType} vote #${result.rows[0].id}: ${question}`;
        sendDiscussionEvent(ch, msg).catch(err => {
            console.error('Failed to send vote propose event:', err.message);
        });
    });

    // Trigger virtual agents to cast their vote
    notifySystem({
        type: 'virtual-agent', discussionId,
        triggerType: 'vote-proposed', voteId: result.rows[0].id
    }).catch(() => {});

    return {
        vote: {
            id: result.rows[0].id,
            discussion_id: discussionId,
            proposed_by: proposedBy,
            question,
            type: voteType,
            threshold: voteThreshold,
            closes_at: closesAt || null,
            created_at: result.rows[0].created_at
        }
    };
}

async function voteCast(voteId, agent, choice, reason) {
    if (!voteId || !agent || choice === undefined || choice === null) {
        throw Object.assign(new Error('Required fields: vote_id, agent, choice'), { statusCode: 400 });
    }

    if (typeof choice !== 'number' || !Number.isInteger(choice)) {
        throw Object.assign(new Error('Choice must be an integer'), { statusCode: 400 });
    }

    const vote = await pool.query('SELECT * FROM discussion_votes WHERE id = $1', [voteId]);
    if (vote.rows.length === 0) {
        throw Object.assign(new Error('Vote not found'), { statusCode: 404 });
    }
    if (vote.rows[0].status !== 'open') {
        throw Object.assign(new Error('Vote is not open'), { statusCode: 400 });
    }

    const check = await requireJoined(vote.rows[0].discussion_id, agent);
    if (!check.ok) {
        throw Object.assign(new Error(check.message), { statusCode: 403, code: check.code });
    }

    const actor = await requireByName(agent);

    const existing = await pool.query(
        'SELECT 1 FROM discussion_ballots WHERE vote_id = $1 AND actor_id = $2',
        [voteId, actor.id]
    );
    if (existing.rows.length > 0) {
        throw Object.assign(new Error('Agent has already voted'), { statusCode: 400, code: 'ALREADY_VOTED' });
    }

    await pool.query(
        'INSERT INTO discussion_ballots (vote_id, actor_id, choice, reason) VALUES ($1, $2, $3, $4)',
        [voteId, actor.id, choice, reason || null]
    );

    await evaluateVote(voteId);

    const updated = await pool.query('SELECT * FROM discussion_votes WHERE id = $1', [voteId]);

    logDiscussion('vote_cast', { vote_id: voteId, agent, choice });

    // Post vote event to discussion channel (fire-and-forget)
    const discussionId = vote.rows[0].discussion_id;
    const voteType = vote.rows[0].type || 'general';
    const voteStatus = updated.rows[0].status;

    getDiscussionChannel(discussionId).then(async (ch) => {
        const participants = await pool.query(
            'SELECT actor_id FROM discussion_participants WHERE discussion_id = $1 AND status = $2',
            [discussionId, 'joined']
        );
        const totalJoined = participants.rows.length;
        const ballotResult = await pool.query('SELECT COUNT(*) as count FROM discussion_ballots WHERE vote_id = $1', [voteId]);
        const castCount = parseInt(ballotResult.rows[0].count);

        let msg = `${agent} voted ${choice} on ${voteType} vote #${voteId}.`;
        if (voteStatus === 'closed') {
            msg += ' Vote closed.';
        } else {
            msg += ` ${castCount}/${totalJoined} votes cast.`;
        }
        sendDiscussionEvent(ch, msg);
    }).catch(err => {
        console.error('Failed to send vote cast event:', err.message);
    });

    return {
        vote_id: voteId,
        agent,
        choice,
        reason: reason || null,
        vote_status: voteStatus
    };
}

async function voteStatus(voteId) {
    if (!voteId) {
        throw Object.assign(new Error('Required field: vote_id'), { statusCode: 400 });
    }

    const vote = await pool.query('SELECT * FROM discussion_votes WHERE id = $1', [voteId]);
    if (vote.rows.length === 0) {
        throw Object.assign(new Error('Vote not found'), { statusCode: 404 });
    }

    if (vote.rows[0].status === 'open') {
        await evaluateVote(voteId);
    }

    // Return vote with proposed_by name
    const updated = await pool.query(
        `SELECT v.*, ac.name AS proposed_by
         FROM discussion_votes v
         JOIN actors ac ON ac.id = v.proposed_by_actor_id
         WHERE v.id = $1`,
        [voteId]
    );
    const ballots = await pool.query(
        `SELECT ac.name AS agent, b.choice, b.reason, b.cast_at
         FROM discussion_ballots b
         JOIN actors ac ON ac.id = b.actor_id
         WHERE b.vote_id = $1 ORDER BY b.cast_at ASC`,
        [voteId]
    );

    logDiscussion('vote_status', { vote_id: voteId });

    return { vote: updated.rows[0], ballots: ballots.rows };
}

module.exports = {
    discussionCreate,
    discussionList,
    discussionStatus,
    discussionPending,
    discussionConclude,
    discussionJoin,
    discussionDefer,
    discussionLeave,
    votePropose,
    voteCast,
    voteStatus,
    evaluateVote,
    evaluateReadiness
};
