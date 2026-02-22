const { Router } = require('express');
const pool = require('../db');
const { log } = require('../services/logger');

const router = Router();

function logDiscussion(action, details) {
    log('discussion', action, details);
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

// Check and close votes that have met their conditions (all voted or time expired)
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

    // Close the vote
    await pool.query(
        'UPDATE discussion_votes SET status = $1, closed_at = NOW() WHERE id = $2',
        ['closed', voteId]
    );

    // For conclude votes, check if the result means the discussion should conclude
    if (v.type === 'conclude') {
        const passed = evaluateThreshold(ballots.rows, participantCount, v.threshold);
        if (passed) {
            await pool.query(
                'UPDATE discussions SET status = $1, concluded_at = NOW() WHERE id = $2',
                ['concluded', v.discussion_id]
            );
            logDiscussion('auto_conclude', { discussion_id: v.discussion_id, vote_id: voteId });
        }
    }
}

// Determine if a vote passed based on threshold.
// Convention: choice 1 = yes/approve/conclude, anything else = no.
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

router.post('/discussion/create', async (req, res) => {
    try {
        const { topic, created_by, participants, channel } = req.body;

        if (!topic || !created_by || !participants || !Array.isArray(participants)) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required fields: topic, created_by, participants (array)' }
            });
        }

        if (participants.length < 2) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'A discussion requires at least 2 participants' }
            });
        }

        if (!participants.includes(created_by)) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Creator must be in the participants list' }
            });
        }

        const client = await pool.connect();
        try {
            await client.query('BEGIN');

            const result = await client.query(
                'INSERT INTO discussions (topic, created_by, channel) VALUES ($1, $2, $3) RETURNING id, created_at',
                [topic, created_by, channel || null]
            );
            const discussionId = result.rows[0].id;

            for (const agent of participants) {
                const isCreator = agent === created_by;
                await client.query(
                    'INSERT INTO discussion_participants (discussion_id, agent, status, joined_at) VALUES ($1, $2, $3, $4)',
                    [discussionId, agent, isCreator ? 'joined' : 'invited', isCreator ? new Date() : null]
                );
            }

            await client.query('COMMIT');

            logDiscussion('create', { discussion_id: discussionId, topic, created_by, participants });

            res.json({
                id: discussionId,
                topic,
                created_by,
                participants: participants.map(a => ({
                    agent: a,
                    status: a === created_by ? 'joined' : 'invited'
                })),
                created_at: result.rows[0].created_at
            });
        } catch (err) {
            await client.query('ROLLBACK');
            throw err;
        } finally {
            client.release();
        }
    } catch (err) {
        console.error('Discussion create error:', err.message);
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
        console.error('Discussion list error:', err.message);
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

        const participants = await pool.query(
            'SELECT agent, status, invited_at, joined_at FROM discussion_participants WHERE discussion_id = $1',
            [discussion_id]
        );

        const votes = await pool.query(
            'SELECT * FROM discussion_votes WHERE discussion_id = $1 ORDER BY created_at DESC',
            [discussion_id]
        );

        // For open votes, check if they should be closed
        for (const vote of votes.rows) {
            if (vote.status === 'open') {
                await evaluateVote(vote.id);
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
        console.error('Discussion status error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/discussion/pending', async (req, res) => {
    try {
        const { agent } = req.body;

        if (!agent) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required field: agent' }
            });
        }

        // Discussions where agent is invited but hasn't joined
        const invited = await pool.query(
            'SELECT d.* FROM discussions d JOIN discussion_participants dp ON d.id = dp.discussion_id WHERE dp.agent = $1 AND dp.status = $2 AND d.status = $3',
            [agent, 'invited', 'active']
        );

        // Active discussions with open votes where agent hasn't voted yet
        const openVotes = await pool.query(
            `SELECT v.*, d.topic as discussion_topic
             FROM discussion_votes v
             JOIN discussions d ON v.discussion_id = d.id
             JOIN discussion_participants dp ON d.id = dp.discussion_id
             WHERE dp.agent = $1
             AND dp.status = $2
             AND v.status = $3
             AND NOT EXISTS (
                 SELECT 1 FROM discussion_ballots b WHERE b.vote_id = v.id AND b.agent = $1
             )
             ORDER BY v.created_at DESC`,
            [agent, 'joined', 'open']
        );

        logDiscussion('pending', { agent, invited: invited.rows.length, open_votes: openVotes.rows.length });

        res.json({
            invited_discussions: invited.rows,
            open_votes: openVotes.rows
        });
    } catch (err) {
        console.error('Discussion pending error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/discussion/conclude', async (req, res) => {
    try {
        const { discussion_id, agent } = req.body;

        if (!discussion_id || !agent) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required fields: discussion_id, agent' }
            });
        }

        const check = await requireJoined(discussion_id, agent);
        if (!check.ok) {
            return res.status(403).json({ error: { code: check.code, message: check.message } });
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
        if (discussion.rows[0].status !== 'active') {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Discussion is not active' }
            });
        }

        await pool.query(
            'UPDATE discussions SET status = $1, concluded_at = NOW() WHERE id = $2',
            ['concluded', discussion_id]
        );

        logDiscussion('conclude', { discussion_id, agent });

        res.json({ discussion_id, status: 'concluded' });
    } catch (err) {
        console.error('Discussion conclude error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/discussion/join', async (req, res) => {
    try {
        const { discussion_id, agent } = req.body;

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
        if (discussion.rows[0].status !== 'active') {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Discussion is not active' }
            });
        }

        const existing = await pool.query(
            'SELECT status FROM discussion_participants WHERE discussion_id = $1 AND agent = $2',
            [discussion_id, agent]
        );

        if (existing.rows.length > 0) {
            if (existing.rows[0].status === 'joined') {
                return res.json({ discussion_id, agent, status: 'joined', message: 'Already joined' });
            }
            await pool.query(
                'UPDATE discussion_participants SET status = $1, joined_at = NOW() WHERE discussion_id = $2 AND agent = $3',
                ['joined', discussion_id, agent]
            );
        } else {
            await pool.query(
                'INSERT INTO discussion_participants (discussion_id, agent, status, joined_at) VALUES ($1, $2, $3, NOW())',
                [discussion_id, agent, 'joined']
            );
        }

        logDiscussion('join', { discussion_id, agent });

        res.json({ discussion_id, agent, status: 'joined' });
    } catch (err) {
        console.error('Discussion join error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/discussion/leave', async (req, res) => {
    try {
        const { discussion_id, agent } = req.body;

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

        res.json({ discussion_id, agent, status: 'left' });
    } catch (err) {
        console.error('Discussion leave error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/discussion/vote/propose', async (req, res) => {
    try {
        const { discussion_id, proposed_by, question, type, threshold, closes_at } = req.body;

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
        if (discussion.rows[0].status !== 'active') {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Discussion is not active' }
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

        res.json({
            id: result.rows[0].id,
            discussion_id,
            proposed_by,
            question,
            type: voteType,
            threshold: voteThreshold,
            closes_at: closes_at || null,
            created_at: result.rows[0].created_at
        });
    } catch (err) {
        console.error('Discussion vote propose error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/discussion/vote/cast', async (req, res) => {
    try {
        const { vote_id, agent, choice, reason } = req.body;

        if (!vote_id || !agent || choice === undefined || choice === null) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required fields: vote_id, agent, choice' }
            });
        }

        if (typeof choice !== 'number' || !Number.isInteger(choice)) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Choice must be an integer' }
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
        if (vote.rows[0].status !== 'open') {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Vote is not open' }
            });
        }

        const check = await requireJoined(vote.rows[0].discussion_id, agent);
        if (!check.ok) {
            return res.status(403).json({ error: { code: check.code, message: check.message } });
        }

        // Check for existing ballot
        const existing = await pool.query(
            'SELECT 1 FROM discussion_ballots WHERE vote_id = $1 AND agent = $2',
            [vote_id, agent]
        );
        if (existing.rows.length > 0) {
            return res.status(400).json({
                error: { code: 'ALREADY_VOTED', message: 'Agent has already voted' }
            });
        }

        await pool.query(
            'INSERT INTO discussion_ballots (vote_id, agent, choice, reason) VALUES ($1, $2, $3, $4)',
            [vote_id, agent, choice, reason || null]
        );

        // Evaluate if vote should close
        await evaluateVote(vote_id);

        // Fetch updated vote status
        const updated = await pool.query(
            'SELECT * FROM discussion_votes WHERE id = $1',
            [vote_id]
        );

        logDiscussion('vote_cast', { vote_id, agent, choice });

        res.json({
            vote_id,
            agent,
            choice,
            reason: reason || null,
            vote_status: updated.rows[0].status
        });
    } catch (err) {
        console.error('Discussion vote cast error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
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
            await evaluateVote(vote_id);
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
        console.error('Discussion vote status error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

module.exports = router;
