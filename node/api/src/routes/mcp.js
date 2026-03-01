// MCP Streamable HTTP endpoint.
// Exposes all memory API tools over the MCP protocol via HTTP.
// Auth is via JWT bearer tokens issued by /oauth/token.

const { Router } = require('express');
const crypto = require('crypto');
const { Server } = require('@modelcontextprotocol/sdk/server/index.js');
const { StreamableHTTPServerTransport } = require('@modelcontextprotocol/sdk/server/streamableHttp.js');
const { CallToolRequestSchema, ListToolsRequestSchema } = require('@modelcontextprotocol/sdk/types.js');
const mcpAuth = require('../middleware/mcp-auth');
const pool = require('../db');

// Services
const { searchMemory, ingestContent, deleteMemory } = require('../services/memory');
const { saveNote, listNotes, readNote, deleteNote, editNote, grepNotes } = require('../services/documents');
const { chatSend, chatReceive, chatAck, chatStatus } = require('../services/chat');
const { mailSend, mailReceive, mailAck } = require('../services/mail');
const {
    discussionCreate, discussionList, discussionStatus, discussionPending,
    discussionConclude, discussionJoin, discussionLeave,
    votePropose, voteCast, voteStatus
} = require('../services/discussion');

const router = Router();

// Permission check — does the JWT include the required permission?
function hasPermission(req, permission) {
    return req.mcpPermissions.includes(permission);
}

// Tool definitions for the remote MCP server
const TOOLS = [
    // --- Memory tools ---
    {
        name: 'search',
        description: 'Search memory for relevant notes using semantic similarity',
        inputSchema: {
            type: 'object',
            properties: {
                query: { type: 'string', description: 'Search query' },
                namespace: { type: 'string', description: 'Namespace to search (default: agent namespace, "*" for all)' },
                limit: { type: 'number', description: 'Max results (default: 5)' }
            },
            required: ['query']
        }
    },
    {
        name: 'ingest',
        description: 'Ingest markdown content into the vector database for semantic search.',
        inputSchema: {
            type: 'object',
            properties: {
                content: { type: 'string', description: 'Markdown content to ingest' },
                source_file: { type: 'string', description: 'Name to store as (used as identifier for updates/deletes)' },
                namespace: { type: 'string', description: 'Namespace (default: agent namespace)' }
            },
            required: ['content', 'source_file']
        }
    },
    {
        name: 'delete',
        description: 'Delete all chunks for a specific source file from memory',
        inputSchema: {
            type: 'object',
            properties: {
                source_file: { type: 'string', description: 'Source file name to delete' },
                namespace: { type: 'string', description: 'Namespace (default: agent namespace)' }
            },
            required: ['source_file']
        }
    },
    // --- Document tools ---
    {
        name: 'save_note',
        description: 'Save a note to memory. Creates or updates based on slug. Auto-indexes into vector DB.',
        inputSchema: {
            type: 'object',
            properties: {
                title: { type: 'string', description: 'Note title' },
                content: { type: 'string', description: 'Note content (markdown)' },
                namespace: { type: 'string', description: 'Namespace (default: agent namespace)' },
                slug: { type: 'string', description: 'URL slug (auto-generated from title if omitted)' }
            },
            required: ['title', 'content']
        }
    },
    {
        name: 'list_notes',
        description: 'List all notes in a namespace',
        inputSchema: {
            type: 'object',
            properties: {
                namespace: { type: 'string', description: 'Namespace (default: agent namespace)' },
                limit: { type: 'number', description: 'Max results (default: 50)' },
                offset: { type: 'number', description: 'Pagination offset (default: 0)' },
                prefix: { type: 'string', description: 'Filter by slug prefix (e.g., "notes/" to list only notes in that path). Results sorted by slug when prefix is used.' }
            }
        }
    },
    {
        name: 'read_note',
        description: 'Read a specific note by slug',
        inputSchema: {
            type: 'object',
            properties: {
                namespace: { type: 'string', description: 'Namespace (default: agent namespace)' },
                slug: { type: 'string', description: 'Note slug' }
            },
            required: ['slug']
        }
    },
    {
        name: 'delete_note',
        description: 'Delete a note by slug (also removes vector chunks)',
        inputSchema: {
            type: 'object',
            properties: {
                namespace: { type: 'string', description: 'Namespace (default: agent namespace)' },
                slug: { type: 'string', description: 'Note slug' }
            },
            required: ['slug']
        }
    },
    {
        name: 'edit_note',
        description: 'Edit a note by replacing a specific text string. Like search-and-replace — finds old_string in the note and replaces it with new_string. Fails if old_string is not found or appears more than once (unless replace_all is true).',
        inputSchema: {
            type: 'object',
            properties: {
                slug: { type: 'string', description: 'Note slug to edit' },
                namespace: { type: 'string', description: 'Namespace (default: agent namespace)' },
                old_string: { type: 'string', description: 'The exact text to find and replace' },
                new_string: { type: 'string', description: 'The replacement text' },
                replace_all: { type: 'boolean', description: 'Replace all occurrences (default: false — requires old_string to be unique)' }
            },
            required: ['slug', 'old_string', 'new_string']
        }
    },
    {
        name: 'grep',
        description: 'Search notes for exact text matches (case-insensitive). Use this for finding specific strings, identifiers, file paths, or references across all notes.',
        inputSchema: {
            type: 'object',
            properties: {
                pattern: { type: 'string', description: 'Text pattern to search for (case-insensitive substring match)' },
                namespace: { type: 'string', description: 'Namespace to search (default: agent namespace, "*" for all)' },
                limit: { type: 'number', description: 'Max matching notes to return (default: 20)' }
            },
            required: ['pattern']
        }
    },
    // --- Instructions tools ---
    {
        name: 'read_instructions',
        description: 'Read your startup instructions. Returns the instructions text that was previously saved, or empty if none set.',
        inputSchema: {
            type: 'object',
            properties: {}
        }
    },
    {
        name: 'save_instructions',
        description: 'Save your startup instructions. These are read at session start to configure your behavior. Overwrites any existing instructions.',
        inputSchema: {
            type: 'object',
            properties: {
                content: { type: 'string', description: 'The instructions text to save (markdown)' }
            },
            required: ['content']
        }
    },
    // --- Chat tools ---
    {
        name: 'chat_send',
        description: 'Send a chat message to another agent. Use to="*" to broadcast to all registered agents.',
        inputSchema: {
            type: 'object',
            properties: {
                to: { type: 'string', description: 'Recipient agent (e.g., "home", "work") or "*" for broadcast' },
                message: { type: 'string', description: 'Message to send' },
                from: { type: 'string', description: 'Sender agent (default: configured agent)' },
                channel: { type: 'string', description: 'Optional channel for message isolation (e.g., "discussion"). Omit for regular chat.' },
                discussion_id: { type: 'number', description: 'Send to all joined participants in a discussion (alternative to "to")' }
            },
            required: ['message']
        }
    },
    {
        name: 'chat_receive',
        description: 'Check for unread chat messages. Returns unacked messages. Call chat_ack with the message IDs after processing.',
        inputSchema: {
            type: 'object',
            properties: {
                agent: { type: 'string', description: 'Agent to check messages for (default: configured agent)' },
                channel: { type: 'string', description: 'Optional channel to filter by (e.g., "discussion"). Omit for regular chat only.' }
            }
        }
    },
    {
        name: 'chat_ack',
        description: 'Acknowledge specific chat messages as read. Acked messages will not appear in future receives.',
        inputSchema: {
            type: 'object',
            properties: {
                agent: { type: 'string', description: 'Agent acking messages (default: configured agent)' },
                message_ids: { type: 'array', items: { type: 'number' }, description: 'Array of message IDs to ack (from receive results)' }
            },
            required: ['message_ids']
        }
    },
    {
        name: 'chat_status',
        description: 'Get chat status for an agent: pending message count, last message time, last ack time.',
        inputSchema: {
            type: 'object',
            properties: {
                agent: { type: 'string', description: 'Agent to check status for (default: configured agent)' },
                channel: { type: 'string', description: 'Optional channel to filter by. Omit for regular chat only.' }
            }
        }
    },
    // --- Mail tools ---
    {
        name: 'mail_send',
        description: 'Send mail to another agent. Mail is stored in the API database until the recipient acks it.',
        inputSchema: {
            type: 'object',
            properties: {
                to: { type: 'string', description: 'Recipient agent (e.g., "home", "work")' },
                subject: { type: 'string', description: 'Mail subject line' },
                body: { type: 'string', description: 'Mail body (markdown)' },
                from: { type: 'string', description: 'Sender agent (default: configured agent)' }
            },
            required: ['to', 'subject', 'body']
        }
    },
    {
        name: 'mail_receive',
        description: 'Check for new mail addressed to this agent. Returns unacked messages. Call mail_ack with the message IDs after processing.',
        inputSchema: {
            type: 'object',
            properties: {
                agent: { type: 'string', description: 'Agent to check mail for (default: configured agent)' }
            }
        }
    },
    {
        name: 'mail_ack',
        description: 'Manually ack specific mail messages by UUID. Use this if mail_receive failed after downloading but before acking.',
        inputSchema: {
            type: 'object',
            properties: {
                agent: { type: 'string', description: 'Agent acking messages (default: configured agent)' },
                ids: { type: 'array', items: { type: 'string' }, description: 'Array of mail UUIDs to ack' }
            },
            required: ['ids']
        }
    },
    // --- Agent tools ---
    {
        name: 'agent_status',
        description: 'Get presence info for all agents: online/offline status, last seen time, and unread chat/mail counts for the querying agent.',
        inputSchema: {
            type: 'object',
            properties: {
                agent: { type: 'string', description: 'Agent requesting presence info (default: configured agent). Unread counts are relative to this agent.' }
            }
        }
    },
    // --- Discussion tools ---
    {
        name: 'discussion_create',
        description: 'Create a new discussion with specified participants. Creator is auto-added as joined; others are invited.',
        inputSchema: {
            type: 'object',
            properties: {
                topic: { type: 'string', description: 'Discussion topic' },
                participants: { type: 'array', items: { type: 'string' }, description: 'List of participant agent names (must include creator)' },
                channel: { type: 'string', description: 'Optional chat channel name for this discussion' },
                created_by: { type: 'string', description: 'Creator agent (default: configured agent)' },
                mode: { type: 'string', description: 'Discussion mode: "realtime" (transport + subagent, live back-and-forth) or "async" (independent investigation + direct voting). Default: realtime' },
                context: { type: 'string', description: 'Optional background/context for the discussion (max 10k chars). Visible to joining agents via discussion_status.' }
            },
            required: ['topic', 'participants']
        }
    },
    {
        name: 'discussion_list',
        description: 'List discussions, optionally filtered by status and/or agent.',
        inputSchema: {
            type: 'object',
            properties: {
                status: { type: 'string', description: 'Filter by status: active, concluded, timed_out' },
                agent: { type: 'string', description: 'Filter to discussions this agent participates in' }
            }
        }
    },
    {
        name: 'discussion_status',
        description: 'Get full status of a discussion: details, participants, and votes.',
        inputSchema: {
            type: 'object',
            properties: {
                discussion_id: { type: 'number', description: 'Discussion ID' }
            },
            required: ['discussion_id']
        }
    },
    {
        name: 'discussion_pending',
        description: 'Check for discussions needing attention: pending invitations and open votes awaiting your ballot.',
        inputSchema: {
            type: 'object',
            properties: {
                agent: { type: 'string', description: 'Agent to check for (default: configured agent)' }
            }
        }
    },
    {
        name: 'discussion_conclude',
        description: 'Manually conclude an active discussion.',
        inputSchema: {
            type: 'object',
            properties: {
                discussion_id: { type: 'number', description: 'Discussion ID' },
                agent: { type: 'string', description: 'Agent concluding (default: configured agent)' }
            },
            required: ['discussion_id']
        }
    },
    {
        name: 'discussion_join',
        description: 'Join a discussion you have been invited to.',
        inputSchema: {
            type: 'object',
            properties: {
                discussion_id: { type: 'number', description: 'Discussion ID' },
                agent: { type: 'string', description: 'Agent joining (default: configured agent)' }
            },
            required: ['discussion_id']
        }
    },
    {
        name: 'discussion_leave',
        description: 'Leave a discussion you are participating in.',
        inputSchema: {
            type: 'object',
            properties: {
                discussion_id: { type: 'number', description: 'Discussion ID' },
                agent: { type: 'string', description: 'Agent leaving (default: configured agent)' }
            },
            required: ['discussion_id']
        }
    },
    {
        name: 'discussion_vote_propose',
        description: 'Propose a vote in a discussion. Use type "conclude" for ending the discussion, "general" for other decisions. Choices are integers; describe options in the question text.',
        inputSchema: {
            type: 'object',
            properties: {
                discussion_id: { type: 'number', description: 'Discussion ID' },
                question: { type: 'string', description: 'Vote question (include numbered options, e.g. "1=approve 2=reject")' },
                type: { type: 'string', description: 'Vote type: "general" or "conclude" (default: general)' },
                threshold: { type: 'string', description: 'Pass threshold: "unanimous" or "majority" (default: unanimous)' },
                closes_at: { type: 'string', description: 'Optional ISO timestamp when vote auto-closes' },
                proposed_by: { type: 'string', description: 'Proposing agent (default: configured agent)' }
            },
            required: ['discussion_id', 'question']
        }
    },
    {
        name: 'discussion_vote_cast',
        description: 'Cast a ballot on an open vote. Choice is an integer matching the options in the question.',
        inputSchema: {
            type: 'object',
            properties: {
                vote_id: { type: 'number', description: 'Vote ID' },
                choice: { type: 'number', description: 'Integer choice (matching options in the question)' },
                reason: { type: 'string', description: 'Optional reason for your choice' },
                agent: { type: 'string', description: 'Voting agent (default: configured agent)' }
            },
            required: ['vote_id', 'choice']
        }
    },
    {
        name: 'discussion_vote_status',
        description: 'Get status and ballots for a specific vote.',
        inputSchema: {
            type: 'object',
            properties: {
                vote_id: { type: 'number', description: 'Vote ID' }
            },
            required: ['vote_id']
        }
    }
];

// Permission required for each tool
const TOOL_PERMISSIONS = {
    search: 'mcp_search',
    ingest: 'mcp_ingest',
    delete: 'mcp_delete_memory',
    save_note: 'mcp_save_note',
    list_notes: 'mcp_list_notes',
    read_note: 'mcp_read_note',
    delete_note: 'mcp_delete_note',
    edit_note: 'mcp_save_note',
    grep: 'mcp_search',
    read_instructions: 'mcp_read_note',
    save_instructions: 'mcp_save_note',
    chat_send: 'mcp_chat_send',
    chat_receive: 'mcp_chat_receive',
    chat_ack: 'mcp_chat_ack',
    chat_status: 'mcp_chat_status',
    mail_send: 'mcp_mail_send',
    mail_receive: 'mcp_mail_receive',
    mail_ack: 'mcp_mail_ack',
    agent_status: 'mcp_agent_status',
    discussion_create: 'mcp_discussion_create',
    discussion_list: 'mcp_discussion_list',
    discussion_status: 'mcp_discussion_status',
    discussion_pending: 'mcp_discussion_pending',
    discussion_conclude: 'mcp_discussion_conclude',
    discussion_join: 'mcp_discussion_join',
    discussion_leave: 'mcp_discussion_leave',
    discussion_vote_propose: 'mcp_discussion_vote_propose',
    discussion_vote_cast: 'mcp_discussion_vote_cast',
    discussion_vote_status: 'mcp_discussion_vote_status'
};

// Hash of tool definitions — changes when tools are added/modified/removed.
// Used to detect stale sessions after deploys that changed the MCP tools.
const TOOLS_HASH = crypto.createHash('sha256')
    .update(JSON.stringify(TOOLS))
    .digest('hex')
    .slice(0, 16);

// Tool handler functions — each takes (args, agent, namespace) and returns a text string
const TOOL_HANDLERS = {
    // --- Memory ---
    async search(args, agent, namespace) {
        const data = await searchMemory(args.query, args.namespace || namespace, args.limit || 5);
        const lines = data.results.map(r => {
            return `[${r.namespace}] ${r.source_file} — ${r.heading || '(no heading)'} (${(r.similarity * 100).toFixed(1)}%)\n${r.chunk_text}`;
        });
        return lines.join('\n\n---\n\n') || 'No results found.';
    },

    async ingest(args, agent, namespace) {
        const data = await ingestContent(args.namespace || namespace, args.source_file, args.content);
        return `Ingested ${data.chunks_created} chunks from ${data.source_file} into namespace "${data.namespace}"`;
    },

    async delete(args, agent, namespace) {
        const data = await deleteMemory(args.namespace || namespace, args.source_file);
        return `Deleted ${data.chunks_deleted} chunks for ${args.source_file}`;
    },

    // --- Documents ---
    async save_note(args, agent, namespace) {
        const doc = await saveNote(args.namespace || namespace, args.title, args.content, args.slug, agent);
        return `Saved: ${doc.namespace}/${doc.slug}`;
    },

    async list_notes(args, agent, namespace) {
        const data = await listNotes(args.namespace || namespace, args.limit, args.offset, args.prefix);
        if (data.notes.length === 0) {
            return 'No notes found.';
        }
        return data.notes.map(n => `${n.slug} — ${n.title} (updated ${n.updated_at})`).join('\n');
    },

    async read_note(args, agent, namespace) {
        const doc = await readNote(args.namespace || namespace, args.slug);
        return `# ${doc.title}\n\n${doc.content}`;
    },

    async delete_note(args, agent, namespace) {
        await deleteNote(args.namespace || namespace, args.slug);
        return `Deleted: ${args.namespace || namespace}/${args.slug}`;
    },

    async edit_note(args, agent, namespace) {
        const result = await editNote(args.namespace || namespace, args.slug, args.old_string, args.new_string, args.replace_all);
        return `Edited: ${result.namespace}/${result.slug} (${result.replacements} replacement${result.replacements === 1 ? '' : 's'})`;
    },

    async grep(args, agent, namespace) {
        const results = await grepNotes(args.pattern, args.namespace || namespace, args.limit);
        if (results.length === 0) {
            return `No notes matching "${args.pattern}".`;
        }
        // Format like grep output: file header, then matching lines with context
        const sections = results.map(doc => {
            const header = `[${doc.namespace}] ${doc.slug} — ${doc.title} (${doc.matchCount} match${doc.matchCount === 1 ? '' : 'es'})`;
            const lines = doc.matches.map(m => {
                const prefix = m.isMatch ? '>' : ' ';
                return `${prefix} ${String(m.lineNumber).padStart(4)}: ${m.line}`;
            }).join('\n');
            return `${header}\n${lines}`;
        });
        return sections.join('\n\n---\n\n');
    },

    // --- Instructions ---
    async read_instructions(args, agent, namespace) {
        const result = await pool.query(
            'SELECT startup_instructions FROM agents WHERE agent = $1',
            [agent]
        );
        if (result.rows.length === 0) {
            throw new Error('Agent not found');
        }
        return result.rows[0].startup_instructions || '(no instructions set)';
    },

    async save_instructions(args, agent, namespace) {
        if (!args.content) {
            throw Object.assign(new Error('Required field: content'), { statusCode: 400 });
        }
        await pool.query(
            'UPDATE agents SET startup_instructions = $1 WHERE agent = $2',
            [args.content, agent]
        );
        return `Instructions saved (${args.content.length} characters)`;
    },

    // --- Chat ---
    async chat_send(args, agent, namespace) {
        const fromAgent = args.from || agent;
        let toAgents = null;
        if (args.to) {
            toAgents = [args.to];
        }
        const data = await chatSend(fromAgent, toAgents, args.discussion_id, args.message, args.channel);
        const targets = data.to_agents.map(r => r.agent).join(', ');
        return `Message sent to ${targets}`;
    },

    async chat_receive(args, agent, namespace) {
        const data = await chatReceive(args.agent || agent, args.channel);
        if (data.messages.length === 0) {
            return 'No new messages.';
        }
        const ids = data.messages.map(m => m.id);
        const formatted = data.messages.map(m => {
            return `[${m.from_agent}] (id:${m.id}, ${m.sent_at}): ${m.message}`;
        }).join('\n');
        return `${formatted}\n\n(message_ids: [${ids.join(', ')}] — call chat_ack to mark as read)`;
    },

    async chat_ack(args, agent, namespace) {
        const data = await chatAck(args.agent || agent, args.message_ids);
        return `Acked ${data.acked} message(s) for ${data.agent}: [${data.acked_ids.join(', ')}]`;
    },

    async chat_status(args, agent, namespace) {
        const data = await chatStatus(args.agent || agent, args.channel);
        return `Chat status for ${data.agent}:\n  Pending: ${data.pending_count}\n  Max message ID: ${data.max_message_id}\n  Last message: ${data.last_message_at}\n  Last ack: ${data.last_ack_at}`;
    },

    // --- Mail ---
    async mail_send(args, agent, namespace) {
        const data = await mailSend(args.to, args.from || agent, args.subject, args.body);
        return `Mail sent to ${data.to_agent} (id: ${data.id}, subject: "${data.subject}")`;
    },

    async mail_receive(args, agent, namespace) {
        const data = await mailReceive(args.agent || agent);
        if (data.messages.length === 0) {
            return 'No new mail.';
        }
        const ids = data.messages.map(m => m.id);
        const formatted = data.messages.map(msg =>
            `**From:** ${msg.from_agent}\n**Date:** ${msg.sent_at}\n**Subject:** ${msg.subject}\n**ID:** ${msg.id}\n\n${msg.body}`
        ).join('\n\n---\n\n');
        return `${formatted}\n\n(mail_ids: [${ids.join(', ')}] — call mail_ack to mark as read)`;
    },

    async mail_ack(args, agent, namespace) {
        const data = await mailAck(args.agent || agent, args.ids);
        return `Acked ${data.acked} message(s) for ${data.agent}: [${data.acked_ids.join(', ')}]`;
    },

    // --- Agent ---
    async agent_status(args, agent, namespace) {
        const queryAgent = args.agent || agent;
        const result = await pool.query(
            `SELECT
                a.agent,
                a.status,
                a.last_seen,
                COALESCE(c.unread_count, 0)::int AS unread_chat,
                COALESCE(m.unread_count, 0)::int AS unread_mail
            FROM agent_status a
            LEFT JOIN (
                SELECT from_agent, COUNT(*) AS unread_count
                FROM chat_messages
                WHERE to_agent = $1 AND acked_at IS NULL AND channel IS NULL
                GROUP BY from_agent
            ) c ON c.from_agent = a.agent
            LEFT JOIN (
                SELECT from_agent, COUNT(*) AS unread_count
                FROM mail
                WHERE to_agent = $1 AND acked_at IS NULL
                GROUP BY from_agent
            ) m ON m.from_agent = a.agent
            ORDER BY a.agent`,
            [queryAgent]
        );

        const lines = result.rows.map(a => {
            const parts = [`${a.agent}: ${a.status}`];
            if (a.last_seen) {
                parts.push(`last seen ${a.last_seen}`);
            }
            parts.push(`${a.unread_chat || 0} unread chat`);
            parts.push(`${a.unread_mail || 0} unread mail`);
            return parts.join(' | ');
        });
        return lines.join('\n') || 'No agents registered.';
    },

    // --- Discussion ---
    async discussion_create(args, agent, namespace) {
        const creator = args.created_by || agent;
        const data = await discussionCreate(
            args.topic, creator, args.participants,
            null, args.channel, args.mode, args.context
        );
        const parts = data.participants.map(p => `${p.agent} (${p.status})`).join(', ');
        let text = `Discussion #${data.discussion.id} created [${data.discussion.mode}]: "${data.discussion.topic}"\nParticipants: ${parts}`;
        if (args.channel) {
            text += `\nChannel: ${args.channel}`;
        }
        return text;
    },

    async discussion_list(args, agent, namespace) {
        const data = await discussionList(args.status, args.agent);
        if (data.discussions.length === 0) {
            return 'No discussions found.';
        }
        const lines = data.discussions.map(d => {
            let line = `#${d.id} [${d.status}] [${d.mode || 'realtime'}] "${d.topic}" (created ${d.created_at})`;
            if (d.channel) {
                line += ` channel: ${d.channel}`;
            }
            return line;
        });
        return lines.join('\n');
    },

    async discussion_status(args, agent, namespace) {
        const data = await discussionStatus(args.discussion_id);
        const d = data.discussion;
        const parts = data.participants.map(p => `${p.agent} (${p.status})`).join(', ');
        let text = `Discussion #${d.id}: "${d.topic}" [${d.status}] [${d.mode || 'realtime'}]\nParticipants: ${parts}`;
        if (d.channel) {
            text += `\nChannel: ${d.channel}`;
        }
        if (d.context) {
            text += `\nContext: ${d.context}`;
        }
        if (data.votes.length > 0) {
            const voteLines = data.votes.map(v => {
                return `  Vote #${v.id} [${v.status}] (${v.type}, ${v.threshold}): ${v.question}`;
            });
            text += '\nVotes:\n' + voteLines.join('\n');
        }
        return text;
    },

    async discussion_pending(args, agent, namespace) {
        const data = await discussionPending(args.agent || agent);
        const sections = [];
        if (data.invited_discussions.length > 0) {
            const lines = data.invited_discussions.map(d => `  #${d.id} [${d.mode || 'realtime'}] "${d.topic}"`);
            sections.push('Pending invitations:\n' + lines.join('\n'));
        }
        if (data.open_votes.length > 0) {
            const lines = data.open_votes.map(v => `  Vote #${v.id} [${v.discussion_mode || 'realtime'}] in "${v.discussion_topic}": ${v.question}`);
            sections.push('Open votes awaiting your ballot:\n' + lines.join('\n'));
        }
        if (sections.length === 0) {
            return 'Nothing pending.';
        }
        return sections.join('\n\n');
    },

    async discussion_conclude(args, agent, namespace) {
        const data = await discussionConclude(args.discussion_id, args.agent || agent);
        return `Discussion #${data.discussion_id} ${data.status}.`;
    },

    async discussion_join(args, agent, namespace) {
        const data = await discussionJoin(args.discussion_id, args.agent || agent);
        return `${data.agent} joined discussion #${data.discussion_id}.`;
    },

    async discussion_leave(args, agent, namespace) {
        const data = await discussionLeave(args.discussion_id, args.agent || agent);
        return `${data.agent} left discussion #${data.discussion_id}.`;
    },

    async discussion_vote_propose(args, agent, namespace) {
        const proposer = args.proposed_by || agent;
        const data = await votePropose(
            args.discussion_id, proposer, args.question,
            args.type, args.threshold, args.closes_at
        );
        return `Vote #${data.vote.id} proposed in discussion #${data.vote.discussion_id}: "${data.vote.question}" (${data.vote.type}, ${data.vote.threshold})`;
    },

    async discussion_vote_cast(args, agent, namespace) {
        const data = await voteCast(args.vote_id, args.agent || agent, args.choice, args.reason);
        return `Vote cast on #${data.vote_id}: choice ${data.choice}. Vote status: ${data.vote_status}`;
    },

    async discussion_vote_status(args, agent, namespace) {
        const data = await voteStatus(args.vote_id);
        const v = data.vote;
        let text = `Vote #${v.id} [${v.status}] (${v.type}, ${v.threshold}): ${v.question}`;
        if (data.ballots.length > 0) {
            const ballotLines = data.ballots.map(b => {
                let line = `  ${b.agent}: choice ${b.choice}`;
                if (b.reason) {
                    line += ` — ${b.reason}`;
                }
                return line;
            });
            text += '\nBallots:\n' + ballotLines.join('\n');
        } else {
            text += '\nNo ballots cast yet.';
        }
        return text;
    }
};

function createMcpServer(req) {
    const server = new Server(
        { name: 'llm-memory', version: '2.0.0' },
        { capabilities: { tools: {} } }
    );

    const agent = req.mcpAgent;
    const defaultNamespace = agent;

    server.setRequestHandler(ListToolsRequestSchema, async () => {
        const allowedTools = TOOLS.filter(tool => {
            const permission = TOOL_PERMISSIONS[tool.name];
            return hasPermission(req, permission);
        });
        return { tools: allowedTools };
    });

    server.setRequestHandler(CallToolRequestSchema, async (request) => {
        const { name, arguments: args } = request.params;
        const permission = TOOL_PERMISSIONS[name];

        if (!permission) {
            return {
                content: [{ type: 'text', text: `Unknown tool: ${name}` }],
                isError: true
            };
        }

        if (!hasPermission(req, permission)) {
            return {
                content: [{ type: 'text', text: `Permission denied: ${permission}` }],
                isError: true
            };
        }

        const handler = TOOL_HANDLERS[name];
        if (!handler) {
            return {
                content: [{ type: 'text', text: `No handler for tool: ${name}` }],
                isError: true
            };
        }

        try {
            const result = await handler(args || {}, agent, defaultNamespace);
            return { content: [{ type: 'text', text: result }] };
        } catch (err) {
            return {
                content: [{ type: 'text', text: `Error: ${err.message}` }],
                isError: true
            };
        }
    });

    return server;
}

// Map of session ID -> { server, transport }
const sessions = new Map();

// Rehydrate a session from the database after a server restart.
// Creates fresh transport + server objects and marks the transport as initialized
// so it can handle non-initialize requests with the existing session ID.
async function rehydrateSession(sessionId, req) {
    const transport = new StreamableHTTPServerTransport({
        sessionIdGenerator: () => sessionId
    });

    const server = createMcpServer(req);
    await server.connect(transport);

    // Mark the transport as initialized with the existing session ID.
    // Normally this happens when handleRequest processes an initialize message,
    // but rehydrated sessions skip that — the client already initialized before the restart.
    const inner = transport._webStandardTransport;
    inner.sessionId = sessionId;
    inner._initialized = true;

    sessions.set(sessionId, { server, transport });

    transport.onclose = () => {
        sessions.delete(sessionId);
        pool.query('DELETE FROM mcp_sessions WHERE session_id = $1', [sessionId]).catch(() => {});
    };

    return { server, transport };
}

// Handle POST /mcp (new request or existing session message)
router.post('/mcp', mcpAuth, async (req, res) => {
    const sessionId = req.headers['mcp-session-id'];

    // Fast path — session is already in memory
    if (sessionId && sessions.has(sessionId)) {
        const session = sessions.get(sessionId);
        await session.transport.handleRequest(req, res, req.body);
        return;
    }

    // Session ID provided but not in memory — check the database for rehydration
    if (sessionId) {
        try {
            const { rows } = await pool.query(
                'SELECT agent, tools_hash FROM mcp_sessions WHERE session_id = $1',
                [sessionId]
            );

            if (rows.length > 0 && rows[0].tools_hash === TOOLS_HASH) {
                // Tools haven't changed — rehydrate the session
                const session = await rehydrateSession(sessionId, req);
                await session.transport.handleRequest(req, res, req.body);
                return;
            }

            // Either not in DB or tools changed — clean up stale DB row if present
            if (rows.length > 0) {
                await pool.query('DELETE FROM mcp_sessions WHERE session_id = $1', [sessionId]);
            }
        } catch (err) {
            // DB error — fall through to 404 so the client re-initializes
            console.error('MCP session rehydration DB error:', err.message);
        }

        return res.status(404).json({
            jsonrpc: '2.0',
            error: { code: -32000, message: 'Session not found. Please re-initialize.' },
            id: null
        });
    }

    // No session ID — new session (initialize request)
    const transport = new StreamableHTTPServerTransport({
        sessionIdGenerator: () => crypto.randomUUID()
    });

    const server = createMcpServer(req);

    await server.connect(transport);

    // handleRequest processes the initialize and generates the session ID
    await transport.handleRequest(req, res, req.body);

    // Store session in memory and database
    const newSessionId = transport.sessionId;
    if (newSessionId) {
        sessions.set(newSessionId, { server, transport });

        transport.onclose = () => {
            sessions.delete(newSessionId);
            pool.query('DELETE FROM mcp_sessions WHERE session_id = $1', [newSessionId]).catch(() => {});
        };

        // Persist to DB for rehydration after restart
        pool.query(
            'INSERT INTO mcp_sessions (session_id, agent, tools_hash) VALUES ($1, $2, $3)',
            [newSessionId, req.mcpAgent, TOOLS_HASH]
        ).catch(err => console.error('MCP session persist error:', err.message));
    }
});

// Handle GET /mcp (SSE stream for server-initiated messages)
router.get('/mcp', mcpAuth, async (req, res) => {
    const sessionId = req.headers['mcp-session-id'];

    if (!sessionId) {
        return res.status(400).json({
            jsonrpc: '2.0',
            error: { code: -32000, message: 'Bad Request: Mcp-Session-Id header is required' },
            id: null
        });
    }

    // Try in-memory first, then rehydrate from DB
    if (!sessions.has(sessionId)) {
        try {
            const { rows } = await pool.query(
                'SELECT agent, tools_hash FROM mcp_sessions WHERE session_id = $1',
                [sessionId]
            );
            if (rows.length > 0 && rows[0].tools_hash === TOOLS_HASH) {
                await rehydrateSession(sessionId, req);
            }
        } catch (err) {
            console.error('MCP session rehydration DB error:', err.message);
        }
    }

    if (!sessions.has(sessionId)) {
        return res.status(404).json({
            jsonrpc: '2.0',
            error: { code: -32000, message: 'Session not found. Please re-initialize.' },
            id: null
        });
    }

    const session = sessions.get(sessionId);
    await session.transport.handleRequest(req, res);
});

// Handle DELETE /mcp (session termination)
router.delete('/mcp', mcpAuth, async (req, res) => {
    const sessionId = req.headers['mcp-session-id'];

    if (sessionId && sessions.has(sessionId)) {
        const session = sessions.get(sessionId);
        await session.transport.close();
        sessions.delete(sessionId);
    }

    // Clean up DB row regardless of whether it was in memory
    if (sessionId) {
        pool.query('DELETE FROM mcp_sessions WHERE session_id = $1', [sessionId]).catch(() => {});
    }

    res.status(200).end();
});

module.exports = router;
