const { Router } = require('express');
const pool = require('../db');
const { log, logError } = require('../services/logger');
const config = require('../services/config');
const { notifyDiscussionInvite, sendSystemMessageToMany, sendDiscussionEvent } = require('../services/system-notify');
const discussionService = require('../services/discussion');

const router = Router();

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

// Check if an agent is a joined participant in a discussion
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

router.post('/discussion/create', async (req, res) => {
    try {
        let { topic, created_by, participants, optional_participants, channel, mode, context } = req.body;

        // Enforce agent identity (skip for admin user sessions)
        if (req.authenticatedAgent) {
            if (created_by && created_by !== req.authenticatedAgent) {
                return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'created_by does not match authenticated agent' } });
            }
            created_by = req.authenticatedAgent;
        }

        if (!topic || !created_by || !participants || !Array.isArray(participants)) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required fields: topic, created_by, participants (array)' }
            });
        }

        const optionalList = Array.isArray(optional_participants) ? optional_participants : [];
        const allParticipants = [...participants, ...optionalList];

        if (allParticipants.length < 2) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'A discussion requires at least 2 participants' }
            });
        }

        if (!participants.includes(created_by)) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Creator must be in the participants list' }
            });
        }

        // Check for duplicates between required and optional
        const overlap = optionalList.filter(a => participants.includes(a));
        if (overlap.length > 0) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Agents cannot be both required and optional: ' + overlap.join(', ') }
            });
        }

        // Reject if creator is already in an active or waiting discussion.
        // Run evaluateReadiness first so timed-out discussions don't falsely block.
        const existing = await pool.query(
            `SELECT d.id, d.topic, d.status FROM discussions d
             JOIN discussion_participants dp ON dp.discussion_id = d.id
             WHERE dp.agent = $1 AND dp.status IN ('invited', 'joined', 'deferred')
             AND d.status IN ('waiting', 'active')
             ORDER BY d.id DESC LIMIT 1`,
            [created_by]
        );
        if (existing.rows.length > 0) {
            const d = existing.rows[0];
            // Lazily evaluate — the discussion may have timed out since last check
            if (d.status === 'waiting') {
                await discussionService.evaluateReadiness(d.id);
                const recheck = await pool.query(
                    'SELECT status FROM discussions WHERE id = $1',
                    [d.id]
                );
                // If it transitioned out of waiting/active, it's no longer a conflict
                if (recheck.rows.length > 0 &&
                    recheck.rows[0].status !== 'waiting' &&
                    recheck.rows[0].status !== 'active') {
                    logDiscussion('conflict_cleared', {
                        discussion_id: d.id,
                        new_status: recheck.rows[0].status,
                        agent: created_by
                    });
                    // Fall through to create
                } else {
                    return res.status(409).json({
                        error: {
                            code: 'DISCUSSION_CONFLICT',
                            message: `Agent "${created_by}" is already in discussion #${d.id} (${d.status}): "${d.topic}"`,
                            existing_discussion_id: d.id
                        }
                    });
                }
            } else {
                return res.status(409).json({
                    error: {
                        code: 'DISCUSSION_CONFLICT',
                        message: `Agent "${created_by}" is already in discussion #${d.id} (${d.status}): "${d.topic}"`,
                        existing_discussion_id: d.id
                    }
                });
            }
        }

        const client = await pool.connect();
        try {
            await client.query('BEGIN');

            const validModes = ['realtime', 'async'];
            const discussionMode = validModes.includes(mode) ? mode : 'realtime';

            // Enforce 10k char limit on context
            const contextText = context ? String(context).slice(0, 10000) : null;

            // Compute timeout_at from config (mode-specific)
            const timeoutKey = discussionMode === 'async'
                ? 'discussion_wait_timeout_async'
                : 'discussion_wait_timeout_realtime';
            const timeoutMinutes = parseInt(config.get(timeoutKey));
            const timeoutAt = new Date(Date.now() + timeoutMinutes * 60 * 1000);

            const result = await client.query(
                'INSERT INTO discussions (topic, created_by, status, channel, mode, context, timeout_at) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id, created_at',
                [topic, created_by, 'waiting', channel || null, discussionMode, contextText, timeoutAt]
            );
            const discussionId = result.rows[0].id;

            for (const agent of participants) {
                const isCreator = agent === created_by;
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
                discussion_id: discussionId, topic, created_by,
                required: participants, optional: optionalList, mode: discussionMode
            });

            // Notify invited agents (fire-and-forget, don't block response)
            const invitedAgents = allParticipants.filter(a => a !== created_by);
            if (invitedAgents.length > 0) {
                notifyDiscussionInvite(discussionId, topic, created_by, invitedAgents)
                    .catch(err => console.error('System notify error:', err.message));
            }

            // Evaluate readiness (creator is already joined)
            await discussionService.evaluateReadiness(discussionId);

            // Fetch current status after readiness check
            const current = await pool.query('SELECT status FROM discussions WHERE id = $1', [discussionId]);

            const discussion = {
                id: discussionId,
                topic,
                created_by,
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
                status: a === created_by ? 'joined' : 'invited'
            })).concat(optionalList.map(a => ({
                agent: a,
                role: 'optional',
                status: 'invited'
            })));

            res.json({ discussion, participants: participantList });
        } catch (err) {
            await client.query('ROLLBACK');
            throw err;
        } finally {
            client.release();
        }
    } catch (err) {
        logError('discussion', 'create', { agent: req.authenticatedAgent || req.body.created_by, message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/discussion/list', async (req, res) => {
    try {
        const { status, agent } = req.body;

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

        res.json({ discussions: result.rows });
    } catch (err) {
        logError('discussion', 'list', { message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/discussion/status', async (req, res) => {
    try {
        const { discussion_id } = req.body;

        if (!discussion_id) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required field: discussion_id' }
            });
        }

        const discussion = await pool.query(
            'SELECT * FROM discussions WHERE id = $1',
            [discussion_id]
        );
        if (discussion.rows.length === 0) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Discussion not found' }
            });
        }

        // Evaluate readiness if still waiting
        if (discussion.rows[0].status === 'waiting') {
            await discussionService.evaluateReadiness(discussion_id);
        }

        const participants = await pool.query(
            'SELECT agent, status, role, invited_at, joined_at, deferred_at, defer_count FROM discussion_participants WHERE discussion_id = $1',
            [discussion_id]
        );

        const votes = await pool.query(
            'SELECT * FROM discussion_votes WHERE discussion_id = $1 ORDER BY created_at DESC',
            [discussion_id]
        );

        // For open votes, check if they should be closed
        for (const vote of votes.rows) {
            if (vote.status === 'open') {
                await discussionService.evaluateVote(vote.id);
            }
        }

        // Re-fetch after evaluation in case anything changed
        const updatedDiscussion = await pool.query(
            'SELECT * FROM discussions WHERE id = $1',
            [discussion_id]
        );
        const updatedVotes = await pool.query(
            'SELECT * FROM discussion_votes WHERE discussion_id = $1 ORDER BY created_at DESC',
            [discussion_id]
        );

        logDiscussion('status', { discussion_id });

        res.json({
            discussion: updatedDiscussion.rows[0],
            participants: participants.rows,
            votes: updatedVotes.rows
        });
    } catch (err) {
        logError('discussion', 'status', { message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/discussion/pending', async (req, res) => {
    try {
        let { agent, discussion_id } = req.body;

        // Enforce agent identity (skip for admin user sessions)
        if (req.authenticatedAgent) {
            if (agent && agent !== req.authenticatedAgent) {
                return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'agent does not match authenticated agent' } });
            }
            agent = req.authenticatedAgent;
        }

        if (!agent) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required field: agent' }
            });
        }

        // Discussions where agent is invited but hasn't joined (waiting or active)
        const invited = await pool.query(
            'SELECT d.* FROM discussions d JOIN discussion_participants dp ON d.id = dp.discussion_id WHERE dp.agent = $1 AND dp.status = $2 AND d.status IN ($3, $4)',
            [agent, 'invited', 'waiting', 'active']
        );

        // Discussions this agent has deferred (still waiting or active)
        const deferred = await pool.query(
            'SELECT d.* FROM discussions d JOIN discussion_participants dp ON d.id = dp.discussion_id WHERE dp.agent = $1 AND dp.status = $2 AND d.status IN ($3, $4)',
            [agent, 'deferred', 'waiting', 'active']
        );

        // Open votes where agent hasn't voted yet — async discussions only.
        // Realtime discussion votes are discovered in-band through chat messages,
        // not through pending. Hiding them here prevents agents from voting
        // directly on realtime topics instead of launching the discussion protocol.
        // When discussion_id is provided (transport polling its own discussion),
        // include that discussion's votes regardless of mode.
        const voteParams = [agent, 'joined', 'open'];
        let modeFilter = `AND d.mode = 'async'`;
        if (discussion_id) {
            modeFilter = `AND (d.mode = 'async' OR d.id = $4)`;
            voteParams.push(discussion_id);
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

        logDiscussion('pending', { agent, invited: invited.rows.length, deferred: deferred.rows.length, open_votes: openVotes.rows.length });

        res.json({
            invited_discussions: invited.rows,
            deferred_discussions: deferred.rows,
            open_votes: openVotes.rows
        });
    } catch (err) {
        logError('discussion', 'pending', { agent: req.authenticatedAgent || req.body.agent, message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/discussion/conclude', async (req, res) => {
    try {
        let { discussion_id, agent } = req.body;

        // Enforce agent identity (skip for admin user sessions)
        if (req.authenticatedAgent) {
            if (agent && agent !== req.authenticatedAgent) {
                return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'agent does not match authenticated agent' } });
            }
            agent = req.authenticatedAgent;
        }

        const result = await discussionService.discussionConclude(discussion_id, agent);
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        if (status >= 500) logError('discussion', 'conclude', { agent: req.authenticatedAgent || req.body.agent, message: err.message, detail: err.stack });
        res.status(status).json({
            error: { code: err.code || 'INTERNAL_ERROR', message: err.message }
        });
    }
});

// Cancel an active or waiting discussion — sets status to 'cancelled', outcome to 'abandoned'
router.post('/discussion/cancel', async (req, res) => {
    try {
        let { discussion_id, agent } = req.body;

        // Enforce agent identity (skip for admin user sessions)
        if (req.authenticatedAgent) {
            if (agent && agent !== req.authenticatedAgent) {
                return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'agent does not match authenticated agent' } });
            }
            agent = req.authenticatedAgent;
        }

        if (!discussion_id || !agent) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required fields: discussion_id, agent' }
            });
        }

        const result = await discussionService.discussionConclude(discussion_id, agent, { cancel: true });
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        if (status >= 500) logError('discussion', 'cancel', { agent: req.authenticatedAgent || req.body.agent, message: err.message, detail: err.stack });
        res.status(status).json({
            error: { code: err.code || 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/discussion/join', async (req, res) => {
    try {
        let { discussion_id, agent } = req.body;

        // Enforce agent identity (skip for admin user sessions)
        if (req.authenticatedAgent) {
            if (agent && agent !== req.authenticatedAgent) {
                return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'agent does not match authenticated agent' } });
            }
            agent = req.authenticatedAgent;
        }

        if (!discussion_id || !agent) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required fields: discussion_id, agent' }
            });
        }

        const discussion = await pool.query(
            'SELECT status FROM discussions WHERE id = $1',
            [discussion_id]
        );
        if (discussion.rows.length === 0) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Discussion not found' }
            });
        }

        if (discussion.rows[0].status === 'waiting') {
            await discussionService.evaluateReadiness(discussion_id);
        }
        const refreshed = await pool.query('SELECT status FROM discussions WHERE id = $1', [discussion_id]);
        const dStatus = refreshed.rows[0].status;
        if (dStatus !== 'waiting' && dStatus !== 'active') {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Discussion is ' + dStatus + ' and not accepting joins' }
            });
        }

        const existing = await pool.query(
            'SELECT status FROM discussion_participants WHERE discussion_id = $1 AND agent = $2',
            [discussion_id, agent]
        );

        if (existing.rows.length > 0) {
            if (existing.rows[0].status === 'joined') {
                return res.json({ discussion_id, agent, status: 'joined', discussion_status: dStatus, message: 'Already joined' });
            }
            // Allow timed_out, invited, or left agents to (re)join
            await pool.query(
                'UPDATE discussion_participants SET status = $1, joined_at = NOW() WHERE discussion_id = $2 AND agent = $3',
                ['joined', discussion_id, agent]
            );
        } else {
            await pool.query(
                'INSERT INTO discussion_participants (discussion_id, agent, status, role, joined_at) VALUES ($1, $2, $3, $4, NOW())',
                [discussion_id, agent, 'joined', 'required']
            );
        }

        logDiscussion('join', { discussion_id, agent });

        getDiscussionChannel(discussion_id).then(ch => {
            sendDiscussionEvent(ch, `${agent} joined the discussion`).catch(err => {
                console.error('Failed to send join event:', err.message);
            });
        });

        // Evaluate readiness if still waiting
        if (dStatus === 'waiting') {
            await discussionService.evaluateReadiness(discussion_id);
        }

        // Fetch current discussion status after readiness check
        const current = await pool.query('SELECT status FROM discussions WHERE id = $1', [discussion_id]);

        res.json({ discussion_id, agent, status: 'joined', discussion_status: current.rows[0].status });
    } catch (err) {
        logError('discussion', 'join', { agent: req.authenticatedAgent || req.body.agent, message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/discussion/defer', async (req, res) => {
    try {
        let { discussion_id, agent } = req.body;

        // Enforce agent identity (skip for admin user sessions)
        if (req.authenticatedAgent) {
            if (agent && agent !== req.authenticatedAgent) {
                return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'agent does not match authenticated agent' } });
            }
            agent = req.authenticatedAgent;
        }

        if (!discussion_id || !agent) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required fields: discussion_id, agent' }
            });
        }

        const discussion = await pool.query(
            'SELECT status FROM discussions WHERE id = $1',
            [discussion_id]
        );
        if (discussion.rows.length === 0) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Discussion not found' }
            });
        }

        // Can only defer discussions that are still waiting or active
        const dStatus = discussion.rows[0].status;
        if (dStatus !== 'waiting' && dStatus !== 'active') {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Discussion is ' + dStatus + ' and not accepting deferrals' }
            });
        }

        // Check participant exists and is in a deferrable state
        const existing = await pool.query(
            'SELECT status, defer_count FROM discussion_participants WHERE discussion_id = $1 AND agent = $2',
            [discussion_id, agent]
        );
        if (existing.rows.length === 0) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Agent is not a participant in this discussion' }
            });
        }

        const pStatus = existing.rows[0].status;
        if (pStatus !== 'invited' && pStatus !== 'deferred') {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Can only defer from invited or deferred status, current status: ' + pStatus }
            });
        }

        // Check defer count against max
        const maxDefers = parseInt(config.get('max_defer_count'));
        const currentCount = existing.rows[0].defer_count || 0;
        if (currentCount >= maxDefers) {
            return res.status(400).json({
                error: { code: 'MAX_DEFERRALS_REACHED', message: 'Maximum deferrals reached (' + maxDefers + '). Must join or let timeout expire.' }
            });
        }

        // Update participant status to deferred
        await pool.query(
            'UPDATE discussion_participants SET status = $1, deferred_at = NOW(), defer_count = defer_count + 1 WHERE discussion_id = $2 AND agent = $3',
            ['deferred', discussion_id, agent]
        );

        // Extend the discussion timeout
        const deferTimeout = parseInt(config.get('discussion_defer_timeout'));
        const newTimeoutAt = new Date(Date.now() + deferTimeout * 60 * 1000);
        await pool.query(
            'UPDATE discussions SET timeout_at = $1 WHERE id = $2',
            [newTimeoutAt, discussion_id]
        );

        // Fetch updated participant row for response
        const updated = await pool.query(
            'SELECT deferred_at, defer_count FROM discussion_participants WHERE discussion_id = $1 AND agent = $2',
            [discussion_id, agent]
        );

        logDiscussion('defer', { discussion_id, agent, defer_count: updated.rows[0].defer_count, timeout_at: newTimeoutAt });

        res.json({
            discussion_id,
            agent,
            deferred_at: updated.rows[0].deferred_at,
            defer_count: updated.rows[0].defer_count,
            timeout_at: newTimeoutAt
        });
    } catch (err) {
        logError('discussion', 'defer', { agent: req.authenticatedAgent || req.body.agent, message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/discussion/leave', async (req, res) => {
    try {
        let { discussion_id, agent } = req.body;

        // Enforce agent identity (skip for admin user sessions)
        if (req.authenticatedAgent) {
            if (agent && agent !== req.authenticatedAgent) {
                return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'agent does not match authenticated agent' } });
            }
            agent = req.authenticatedAgent;
        }

        if (!discussion_id || !agent) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required fields: discussion_id, agent' }
            });
        }

        const existing = await pool.query(
            'SELECT status FROM discussion_participants WHERE discussion_id = $1 AND agent = $2',
            [discussion_id, agent]
        );

        if (existing.rows.length === 0) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Agent is not a participant in this discussion' }
            });
        }

        await pool.query(
            'UPDATE discussion_participants SET status = $1 WHERE discussion_id = $2 AND agent = $3',
            ['left', discussion_id, agent]
        );

        logDiscussion('leave', { discussion_id, agent });

        getDiscussionChannel(discussion_id).then(ch => {
            sendDiscussionEvent(ch, `${agent} left the discussion`).catch(err => {
                console.error('Failed to send leave event:', err.message);
            });
        });

        res.json({ discussion_id, agent, status: 'left' });
    } catch (err) {
        logError('discussion', 'leave', { agent: req.authenticatedAgent || req.body.agent, message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/discussion/vote/propose', async (req, res) => {
    try {
        let { discussion_id, proposed_by, question, type, threshold, closes_at } = req.body;

        // Enforce agent identity (skip for admin user sessions)
        if (req.authenticatedAgent) {
            if (proposed_by && proposed_by !== req.authenticatedAgent) {
                return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'proposed_by does not match authenticated agent' } });
            }
            proposed_by = req.authenticatedAgent;
        }

        if (!discussion_id || !proposed_by || !question) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required fields: discussion_id, proposed_by, question' }
            });
        }

        const check = await requireJoined(discussion_id, proposed_by);
        if (!check.ok) {
            return res.status(403).json({ error: { code: check.code, message: check.message } });
        }

        const discussion = await pool.query(
            'SELECT status FROM discussions WHERE id = $1',
            [discussion_id]
        );
        if (!['active', 'waiting'].includes(discussion.rows[0].status)) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Discussion is not active or waiting' }
            });
        }

        const voteType = type || 'general';
        const voteThreshold = threshold || 'unanimous';

        if (!['general', 'conclude'].includes(voteType)) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Vote type must be "general" or "conclude"' }
            });
        }
        if (!['unanimous', 'majority'].includes(voteThreshold)) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Threshold must be "unanimous" or "majority"' }
            });
        }

        const result = await pool.query(
            'INSERT INTO discussion_votes (discussion_id, proposed_by, question, type, threshold, closes_at) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, created_at',
            [discussion_id, proposed_by, question, voteType, voteThreshold, closes_at || null]
        );

        logDiscussion('vote_propose', { discussion_id, vote_id: result.rows[0].id, proposed_by, question, type: voteType });

        getDiscussionChannel(discussion_id).then(ch => {
            const msg = `${proposed_by} proposed ${voteType} vote #${result.rows[0].id}: ${question}`;
            sendDiscussionEvent(ch, msg).catch(err => {
                console.error('Failed to send vote propose event:', err.message);
            });
        });

        res.json({
            vote: {
                id: result.rows[0].id,
                discussion_id,
                proposed_by,
                question,
                type: voteType,
                threshold: voteThreshold,
                closes_at: closes_at || null,
                created_at: result.rows[0].created_at
            }
        });
    } catch (err) {
        logError('discussion', 'vote-propose', { agent: req.authenticatedAgent || req.body.proposed_by, message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/discussion/vote/cast', async (req, res) => {
    try {
        let { vote_id, agent, choice, reason } = req.body;

        // Enforce agent identity (skip for admin user sessions)
        if (req.authenticatedAgent) {
            if (agent && agent !== req.authenticatedAgent) {
                return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'agent does not match authenticated agent' } });
            }
            agent = req.authenticatedAgent;
        }

        const result = await discussionService.voteCast(vote_id, agent, choice, reason);
        res.json(result);
    } catch (err) {
        const status = err.statusCode || 500;
        if (status >= 500) logError('discussion', 'vote-cast', { agent: req.authenticatedAgent || req.body.agent, message: err.message, detail: err.stack });
        res.status(status).json({
            error: { code: err.code || 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/discussion/vote/status', async (req, res) => {
    try {
        const { vote_id } = req.body;

        if (!vote_id) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required field: vote_id' }
            });
        }

        const vote = await pool.query(
            'SELECT * FROM discussion_votes WHERE id = $1',
            [vote_id]
        );
        if (vote.rows.length === 0) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Vote not found' }
            });
        }

        // Evaluate in case it should close
        if (vote.rows[0].status === 'open') {
            await discussionService.evaluateVote(vote_id);
        }

        const updated = await pool.query(
            'SELECT * FROM discussion_votes WHERE id = $1',
            [vote_id]
        );

        const ballots = await pool.query(
            'SELECT agent, choice, reason, cast_at FROM discussion_ballots WHERE vote_id = $1 ORDER BY cast_at ASC',
            [vote_id]
        );

        logDiscussion('vote_status', { vote_id });

        res.json({
            vote: updated.rows[0],
            ballots: ballots.rows
        });
    } catch (err) {
        logError('discussion', 'vote-status', { message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

module.exports = router;
