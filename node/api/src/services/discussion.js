// Service layer for discussion operations.
// Extracted from routes/discussion.js so both REST routes and MCP handler can share the same logic.

const pool = require('../db');
const { log } = require('./logger');
const { notifyDiscussionInvite, sendSystemMessageToMany, sendDiscussionEvent } = require('./system-notify');

function logDiscussion(action, details) {
    log('discussion', action, details);
}

async function getDiscussionChannel(discussionId) {
    const result = await pool.query('SELECT channel FROM discussions WHERE id = $1', [discussionId]);
    if (result.rows.length === 0) {
        return `discuss-${discussionId}`;
    }
    return result.rows[0].channel || `discuss-${discussionId}`;
}

async function getConfig(key, defaultValue) {
    const result = await pool.query('SELECT value FROM config WHERE key = $1', [key]);
    if (result.rows.length === 0) {
        return defaultValue;
    }
    return result.rows[0].value;
}

async function evaluateReadiness(discussionId) {
    const discussion = await pool.query('SELECT * FROM discussions WHERE id = $1', [discussionId]);
    if (discussion.rows.length === 0 || discussion.rows[0].status !== 'waiting') {
        return;
    }

    const d = discussion.rows[0];
    const participants = await pool.query(
        'SELECT agent, status, role FROM discussion_participants WHERE discussion_id = $1',
        [discussionId]
    );

    const required = participants.rows.filter(p => p.role === 'required');
    const optional = participants.rows.filter(p => p.role === 'optional');

    const allRequiredJoined = required.every(p => p.status === 'joined');
    const allOptionalJoined = optional.every(p => p.status === 'joined');

    if (allRequiredJoined && allOptionalJoined) {
        await pool.query('UPDATE discussions SET status = $1 WHERE id = $2', ['active', discussionId]);
        logDiscussion('ready', { discussion_id: discussionId, reason: 'all_joined' });
        return;
    }

    const now = new Date();
    if (d.timeout_at && now >= new Date(d.timeout_at)) {
        if (allRequiredJoined) {
            for (const p of optional) {
                if (p.status !== 'joined') {
                    await pool.query(
                        'UPDATE discussion_participants SET status = $1 WHERE discussion_id = $2 AND agent = $3',
                        ['timed_out', discussionId, p.agent]
                    );
                }
            }
            await pool.query('UPDATE discussions SET status = $1 WHERE id = $2', ['active', discussionId]);
            logDiscussion('ready', { discussion_id: discussionId, reason: 'timeout_required_present' });
        } else {
            const timeoutOutcome = await computeOutcome(discussionId, 'timed_out');
            await pool.query('UPDATE discussions SET status = $1, outcome = $2 WHERE id = $3', ['timed_out', timeoutOutcome, discussionId]);
            // Mark remaining participants as 'left' so they aren't blocked
            // from creating new discussions by the conflict check
            await pool.query(
                `UPDATE discussion_participants SET status = 'left'
                 WHERE discussion_id = $1 AND status IN ('invited', 'joined')`,
                [discussionId]
            );
            logDiscussion('timed_out', { discussion_id: discussionId, reason: 'missing_required', outcome: timeoutOutcome });
        }
    }
}

async function requireJoined(discussionId, agent) {
    const result = await pool.query(
        'SELECT status FROM discussion_participants WHERE discussion_id = $1 AND agent = $2',
        [discussionId, agent]
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
        'SELECT agent FROM discussion_participants WHERE discussion_id = $1 AND status = $2',
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
            // Mark all joined/invited participants as 'left' so they aren't
            // blocked from creating new discussions by the conflict check
            await pool.query(
                `UPDATE discussion_participants SET status = 'left'
                 WHERE discussion_id = $1 AND status IN ('invited', 'joined')`,
                [v.discussion_id]
            );
            logDiscussion('auto_conclude', { discussion_id: v.discussion_id, vote_id: voteId, outcome: voteOutcome });
        }
    }
}

// Derive the outcome of a finished discussion from its general vote history.
// Called when a discussion transitions to concluded, cancelled, or timed_out.
async function computeOutcome(discussionId, newStatus) {
    // Cancelled and timed_out discussions never reached a result
    if (newStatus === 'cancelled' || newStatus === 'timed_out') {
        return 'abandoned';
    }

    // For concluded discussions, examine general vote results
    const votes = await pool.query(
        "SELECT v.id, v.threshold FROM discussion_votes v WHERE v.discussion_id = $1 AND v.type = 'general' AND v.status = 'closed'",
        [discussionId]
    );

    // No general votes — agents discussed and concluded without formal decisions
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

    // Reject if creator is already in an active or waiting discussion
    const existing = await pool.query(
        `SELECT d.id, d.topic, d.status FROM discussions d
         JOIN discussion_participants dp ON dp.discussion_id = d.id
         WHERE dp.agent = $1 AND dp.status IN ('invited', 'joined')
         AND d.status IN ('waiting', 'active')
         ORDER BY d.id DESC LIMIT 1`,
        [createdBy]
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
        const timeoutDefault = discussionMode === 'async' ? '1440' : '5';
        const timeoutMinutes = parseInt(await getConfig(timeoutKey, timeoutDefault));
        const timeoutAt = new Date(Date.now() + timeoutMinutes * 60 * 1000);

        const result = await client.query(
            'INSERT INTO discussions (topic, created_by, status, channel, mode, context, timeout_at) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id, created_at',
            [topic, createdBy, 'waiting', channel || null, discussionMode, contextText, timeoutAt]
        );
        const discussionId = result.rows[0].id;

        for (const agent of participants) {
            const isCreator = agent === createdBy;
            await client.query(
                'INSERT INTO discussion_participants (discussion_id, agent, status, role, joined_at) VALUES ($1, $2, $3, $4, $5)',
                [discussionId, agent, isCreator ? 'joined' : 'invited', 'required', isCreator ? new Date() : null]
            );
        }

        for (const agent of optionalList) {
            await client.query(
                'INSERT INTO discussion_participants (discussion_id, agent, status, role, joined_at) VALUES ($1, $2, $3, $4, $5)',
                [discussionId, agent, 'invited', 'optional', null]
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
            status: a === createdBy ? 'joined' : 'invited'
        })).concat(optionalList.map(a => ({
            agent: a,
            role: 'optional',
            status: 'invited'
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
        query += ' JOIN discussion_participants dp ON d.id = dp.discussion_id';
        params.push(agent);
        conditions.push(`dp.agent = $${params.length}`);
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
        'SELECT agent, status, role, invited_at, joined_at FROM discussion_participants WHERE discussion_id = $1',
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
    const updatedVotes = await pool.query(
        'SELECT * FROM discussion_votes WHERE discussion_id = $1 ORDER BY created_at DESC',
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

    const invited = await pool.query(
        'SELECT d.* FROM discussions d JOIN discussion_participants dp ON d.id = dp.discussion_id WHERE dp.agent = $1 AND dp.status = $2 AND d.status IN ($3, $4)',
        [agent, 'invited', 'waiting', 'active']
    );

    const voteParams = [agent, 'joined', 'open'];
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
         WHERE dp.agent = $1
         AND dp.status = $2
         AND v.status = $3
         AND d.status = 'active'
         ${modeFilter}
         AND NOT EXISTS (
             SELECT 1 FROM discussion_ballots b WHERE b.vote_id = v.id AND b.agent = $1
         )
         ORDER BY v.created_at DESC`,
        voteParams
    );

    logDiscussion('pending', { agent, invited: invited.rows.length, open_votes: openVotes.rows.length });

    return {
        invited_discussions: invited.rows,
        open_votes: openVotes.rows
    };
}

async function discussionConclude(discussionId, agent) {
    if (!discussionId || !agent) {
        throw Object.assign(new Error('Required fields: discussion_id, agent'), { statusCode: 400 });
    }

    const check = await requireJoined(discussionId, agent);
    if (!check.ok) {
        throw Object.assign(new Error(check.message), { statusCode: 403, code: check.code });
    }

    const discussion = await pool.query(
        'SELECT status, created_by, topic FROM discussions WHERE id = $1',
        [discussionId]
    );
    if (discussion.rows.length === 0) {
        throw Object.assign(new Error('Discussion not found'), { statusCode: 404 });
    }

    const dStatus = discussion.rows[0].status;
    if (dStatus === 'waiting') {
        if (discussion.rows[0].created_by !== agent) {
            throw Object.assign(new Error('Only the creator can cancel a waiting discussion'), { statusCode: 403 });
        }
    } else if (dStatus !== 'active') {
        throw Object.assign(new Error('Discussion is not active'), { statusCode: 400 });
    }

    const newStatus = dStatus === 'waiting' ? 'cancelled' : 'concluded';
    const outcome = await computeOutcome(discussionId, newStatus);
    await pool.query('UPDATE discussions SET status = $1, concluded_at = NOW(), outcome = $2 WHERE id = $3', [newStatus, outcome, discussionId]);
    // Mark all joined/invited participants as 'left' so they aren't
    // blocked from creating new discussions by the conflict check
    await pool.query(
        `UPDATE discussion_participants SET status = 'left'
         WHERE discussion_id = $1 AND status IN ('invited', 'joined')`,
        [discussionId]
    );

    logDiscussion(newStatus === 'cancelled' ? 'cancel' : 'conclude', { discussion_id: discussionId, agent });

    // Notify other participants (fire-and-forget)
    const participantRows = await pool.query(
        'SELECT agent FROM discussion_participants WHERE discussion_id = $1 AND agent != $2',
        [discussionId, agent]
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

    const existing = await pool.query(
        'SELECT status FROM discussion_participants WHERE discussion_id = $1 AND agent = $2',
        [discussionId, agent]
    );

    if (existing.rows.length > 0) {
        if (existing.rows[0].status === 'joined') {
            return { discussion_id: discussionId, agent, status: 'joined', discussion_status: dStatus, message: 'Already joined' };
        }
        await pool.query(
            'UPDATE discussion_participants SET status = $1, joined_at = NOW() WHERE discussion_id = $2 AND agent = $3',
            ['joined', discussionId, agent]
        );
    } else {
        await pool.query(
            'INSERT INTO discussion_participants (discussion_id, agent, status, role, joined_at) VALUES ($1, $2, $3, $4, NOW())',
            [discussionId, agent, 'joined', 'required']
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

async function discussionLeave(discussionId, agent) {
    if (!discussionId || !agent) {
        throw Object.assign(new Error('Required fields: discussion_id, agent'), { statusCode: 400 });
    }

    const existing = await pool.query(
        'SELECT status FROM discussion_participants WHERE discussion_id = $1 AND agent = $2',
        [discussionId, agent]
    );

    if (existing.rows.length === 0) {
        throw Object.assign(new Error('Agent is not a participant in this discussion'), { statusCode: 404 });
    }

    await pool.query(
        'UPDATE discussion_participants SET status = $1 WHERE discussion_id = $2 AND agent = $3',
        ['left', discussionId, agent]
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

    const result = await pool.query(
        'INSERT INTO discussion_votes (discussion_id, proposed_by, question, type, threshold, closes_at) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, created_at',
        [discussionId, proposedBy, question, voteType, voteThreshold, closesAt || null]
    );

    logDiscussion('vote_propose', { discussion_id: discussionId, vote_id: result.rows[0].id, proposed_by: proposedBy, question, type: voteType });

    getDiscussionChannel(discussionId).then(ch => {
        const msg = `${proposedBy} proposed ${voteType} vote #${result.rows[0].id}: ${question}`;
        sendDiscussionEvent(ch, msg).catch(err => {
            console.error('Failed to send vote propose event:', err.message);
        });
    });

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

    const existing = await pool.query(
        'SELECT 1 FROM discussion_ballots WHERE vote_id = $1 AND agent = $2',
        [voteId, agent]
    );
    if (existing.rows.length > 0) {
        throw Object.assign(new Error('Agent has already voted'), { statusCode: 400, code: 'ALREADY_VOTED' });
    }

    await pool.query(
        'INSERT INTO discussion_ballots (vote_id, agent, choice, reason) VALUES ($1, $2, $3, $4)',
        [voteId, agent, choice, reason || null]
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
            'SELECT agent FROM discussion_participants WHERE discussion_id = $1 AND status = $2',
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

    const updated = await pool.query('SELECT * FROM discussion_votes WHERE id = $1', [voteId]);
    const ballots = await pool.query(
        'SELECT agent, choice, reason, cast_at FROM discussion_ballots WHERE vote_id = $1 ORDER BY cast_at ASC',
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
    discussionLeave,
    votePropose,
    voteCast,
    voteStatus
};
