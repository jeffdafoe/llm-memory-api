// MCP Streamable HTTP endpoint.
// Exposes all memory API tools over the MCP protocol via HTTP.
// Auth is via HMAC bearer tokens issued by /oauth/token, or API keys.

const { Router } = require('express');
const crypto = require('crypto');
const { Server } = require('@modelcontextprotocol/sdk/server/index.js');
const { StreamableHTTPServerTransport } = require('@modelcontextprotocol/sdk/server/streamableHttp.js');
const { CallToolRequestSchema, ListToolsRequestSchema } = require('@modelcontextprotocol/sdk/types.js');
const mcpAuth = require('../middleware/mcp-auth');
const pool = require('../db');
const { logError } = require('../services/logger');
const config = require('../services/config');

// Services
const { searchMemory, deleteMemory } = require('../services/memory');
const { saveNote, listNotes, readNote, deleteNote, restoreNote, editNote, grepNotes, moveNote } = require('../services/documents');
const { chatSend, chatReceive, chatAck, chatStatus } = require('../services/chat');
const { mailSend, mailReceive, mailCheck, mailAck, mailEdit, mailUnsend, mailSent, mailHistory } = require('../services/mail');
const {
    discussionCreate, discussionList, discussionStatus, discussionPending,
    discussionConclude, discussionJoin, discussionDefer, discussionLeave,
    votePropose, voteCast, voteStatus
} = require('../services/discussion');
const { broadcast } = require('../services/events');
const { requireByName, resolveByName } = require('../services/actors');
const { requireAccess, hasAccess, getReadableNamespaces, validateNamespace } = require('../services/namespace-permissions');
const { hasNoteAccess } = require('../services/note-permissions');

const router = Router();

const MCP_PROTOCOL_VERSION = '2025-03-26';

// Permission check — does the token include the required permission?
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
        name: 'restore_note',
        description: 'Restore a previously deleted note. Recovers the most recently deleted version and its vector chunks.',
        inputSchema: {
            type: 'object',
            properties: {
                namespace: { type: 'string', description: 'Namespace (default: agent namespace)' },
                slug: { type: 'string', description: 'Note slug to restore' }
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
        name: 'move_note',
        description: 'Move/rename a note by changing its slug. Also updates associated vector chunks. Optionally move to a different namespace.',
        inputSchema: {
            type: 'object',
            properties: {
                slug: { type: 'string', description: 'Current slug of the note' },
                new_slug: { type: 'string', description: 'New slug for the note' },
                namespace: { type: 'string', description: 'Current namespace (default: agent namespace)' },
                new_namespace: { type: 'string', description: 'Target namespace (optional, defaults to current namespace)' }
            },
            required: ['slug', 'new_slug']
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
                discussion_id: { type: 'number', description: 'Send to all joined participants in a discussion (alternative to "to"). Auto-derives channel to discussion-{id} and resolves recipients from joined participants.' }
            },
            required: ['message']
        }
    },
    {
        name: 'chat_receive',
        description: 'Check for unread chat messages. Returns unacked messages. Call chat_ack with the IDs after processing.',
        inputSchema: {
            type: 'object',
            properties: {
                agent: { type: 'string', description: 'Agent to check messages for (default: configured agent)' },
                channel: { type: 'string', description: 'Optional channel to filter by (e.g., "discussion"). Omit for regular chat only.' },
                discussion_id: { type: 'number', description: 'Filter to messages from a specific discussion. Auto-derives channel to discussion-{id}.' }
            }
        }
    },
    {
        name: 'chat_ack',
        description: 'Acknowledge specific chat messages as read. Acked messages will not appear in future receives.',
        inputSchema: {
            type: 'object',
            properties: {
                ids: { type: 'array', items: { type: 'number' }, description: 'Array of message IDs to ack (from receive results)' }
            },
            required: ['ids']
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
        description: 'Send mail to another agent. Mail is stored in the API database until the recipient acks it. Returns the mail UUID — save it to match replies via in_reply_to.',
        inputSchema: {
            type: 'object',
            properties: {
                to: { type: 'string', description: 'Recipient agent (e.g., "home", "work")' },
                subject: { type: 'string', description: 'Mail subject line' },
                body: { type: 'string', description: 'Mail body (markdown)' },
                from: { type: 'string', description: 'Sender agent (default: configured agent)' },
                in_reply_to: { type: 'string', description: 'UUID of the message this is replying to (for threading)' }
            },
            required: ['to', 'subject', 'body']
        }
    },
    {
        name: 'mail_check',
        description: 'List unread mail with subject, sender, date, and body preview (no full body). Use this to see what is waiting, then call mail_receive with specific IDs to read individual messages.',
        inputSchema: {
            type: 'object',
            properties: {
                agent: { type: 'string', description: 'Agent to check mail for (default: configured agent)' }
            }
        }
    },
    {
        name: 'mail_receive',
        description: 'Read full mail content by ID. Requires specific message IDs — use mail_check first to list unread mail and get IDs, then call this with those IDs, then mail_ack after processing.',
        inputSchema: {
            type: 'object',
            properties: {
                agent: { type: 'string', description: 'Agent to check mail for (default: configured agent)' },
                ids: { type: 'array', items: { type: 'string' }, description: 'Mail UUIDs to read (required — get these from mail_check)' }
            },
            required: ['ids']
        }
    },
    {
        name: 'mail_ack',
        description: 'Manually ack specific mail messages by UUID. Use this if mail_receive failed after downloading but before acking.',
        inputSchema: {
            type: 'object',
            properties: {
                ids: { type: 'array', items: { type: 'string' }, description: 'Array of mail UUIDs to ack' }
            },
            required: ['ids']
        }
    },
    {
        name: 'mail_edit',
        description: 'Edit an unsent (unacked) mail message. Only the sender can edit, and only before the recipient acks it. Use to fix mistakes in mail you just sent.',
        inputSchema: {
            type: 'object',
            properties: {
                id: { type: 'string', description: 'Mail UUID to edit' },
                subject: { type: 'string', description: 'New subject (optional if body provided)' },
                body: { type: 'string', description: 'New body (optional if subject provided)' }
            },
            required: ['id']
        }
    },
    {
        name: 'mail_unsend',
        description: 'Unsend (delete) a mail message you sent, before the recipient acks it. Only the sender can unsend.',
        inputSchema: {
            type: 'object',
            properties: {
                id: { type: 'string', description: 'Mail UUID to unsend' }
            },
            required: ['id']
        }
    },
    {
        name: 'mail_sent',
        description: 'Get mail you have sent. Returns sent messages with delivery status (whether recipient has acked).',
        inputSchema: {
            type: 'object',
            properties: {
                agent: { type: 'string', description: 'Agent to check sent mail for (default: configured agent)' },
                to: { type: 'string', description: 'Filter by recipient agent name' },
                limit: { type: 'number', description: 'Max results (default: 50)' },
                offset: { type: 'number', description: 'Pagination offset (default: 0)' }
            }
        }
    },
    {
        name: 'mail_history',
        description: 'Get previously received and acked mail. Returns your read inbox history.',
        inputSchema: {
            type: 'object',
            properties: {
                agent: { type: 'string', description: 'Agent to check history for (default: configured agent)' },
                from: { type: 'string', description: 'Filter by sender agent name' },
                limit: { type: 'number', description: 'Max results (default: 50)' },
                offset: { type: 'number', description: 'Pagination offset (default: 0)' }
            }
        }
    },
    // --- Agent tools ---
    {
        name: 'agent_status',
        description: 'Get presence info for all agents: online/offline status, last seen time, expertise areas, and unread chat/mail counts for the querying agent.',
        inputSchema: {
            type: 'object',
            properties: {
                agent: { type: 'string', description: 'Agent requesting presence info (default: configured agent). Unread counts are relative to this agent.' }
            }
        }
    },
    {
        name: 'update_expertise',
        description: 'Update your areas of expertise. Other agents see this when deciding who to include in discussions.',
        inputSchema: {
            type: 'object',
            properties: {
                expertise: { type: 'array', items: { type: 'string' }, description: 'List of expertise areas (e.g. ["codebase", "devops", "payments"])' }
            },
            required: ['expertise']
        }
    },
    {
        name: 'update_profile',
        description: 'Update your agent profile (provider and/or model). Provider is the AI company (e.g. "anthropic", "openai"). Model is the specific model (e.g. "claude-4-sonnet", "gpt-5.3-codex").',
        inputSchema: {
            type: 'object',
            properties: {
                provider: { type: 'string', description: 'AI provider (e.g. "anthropic", "openai", "google")' },
                model: { type: 'string', description: 'Model name (e.g. "claude-4-sonnet", "gpt-5.3-codex")' }
            }
        }
    },
    // --- Activity tools ---
    {
        name: 'activity_start',
        description: 'Signal that you are actively working on a task. Shows an activity indicator to other agents and in the admin dashboard.',
        inputSchema: {
            type: 'object',
            properties: {}
        }
    },
    {
        name: 'activity_stop',
        description: 'Signal that you have finished working. Clears the activity indicator.',
        inputSchema: {
            type: 'object',
            properties: {}
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
        name: 'discussion_cancel',
        description: 'Cancel an active or waiting discussion. Sets status to cancelled and outcome to abandoned.',
        inputSchema: {
            type: 'object',
            properties: {
                discussion_id: { type: 'number', description: 'Discussion ID' },
                agent: { type: 'string', description: 'Agent cancelling (default: configured agent)' }
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
        name: 'discussion_defer',
        description: 'Defer a discussion invitation for later. Acknowledges the invitation without joining, pauses the timeout clock. Can re-defer up to a configurable maximum (default: 3). Resume later by calling discussion_join.',
        inputSchema: {
            type: 'object',
            properties: {
                discussion_id: { type: 'number', description: 'Discussion ID' },
                agent: { type: 'string', description: 'Agent deferring (default: configured agent)' }
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
    delete: 'mcp_delete_memory',
    save_note: 'mcp_save_note',
    list_notes: 'mcp_list_notes',
    read_note: 'mcp_read_note',
    delete_note: 'mcp_delete_note',
    restore_note: 'mcp_delete_note',
    edit_note: 'mcp_save_note',
    move_note: 'mcp_save_note',
    grep: 'mcp_search',
    read_instructions: 'mcp_read_note',
    save_instructions: 'mcp_save_note',
    chat_send: 'mcp_chat_send',
    chat_receive: 'mcp_chat_receive',
    chat_ack: 'mcp_chat_ack',
    chat_status: 'mcp_chat_status',
    mail_send: 'mcp_mail_send',
    mail_check: 'mcp_mail_receive',
    mail_receive: 'mcp_mail_receive',
    mail_ack: 'mcp_mail_ack',
    mail_edit: 'mcp_mail_send',
    mail_unsend: 'mcp_mail_send',
    mail_sent: 'mcp_mail_send',
    mail_history: 'mcp_mail_receive',
    agent_status: 'mcp_agent_status',
    update_expertise: 'mcp_agent_status',
    update_profile: 'mcp_agent_status',
    activity_start: 'mcp_agent_status',
    activity_stop: 'mcp_agent_status',
    discussion_create: 'mcp_discussion_create',
    discussion_list: 'mcp_discussion_list',
    discussion_status: 'mcp_discussion_status',
    discussion_pending: 'mcp_discussion_pending',
    discussion_conclude: 'mcp_discussion_conclude',
    discussion_cancel: 'mcp_discussion_conclude',
    discussion_join: 'mcp_discussion_join',
    discussion_leave: 'mcp_discussion_leave',
    discussion_defer: 'mcp_discussion_join',
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

// Validate that an identity parameter matches the authenticated agent.
// Keeps params in schemas for visibility but prevents impersonation.
function validateIdentity(argValue, authAgent, paramName) {
    if (argValue && argValue !== authAgent) {
        throw Object.assign(
            new Error(`Identity mismatch: ${paramName} "${argValue}" does not match authenticated agent "${authAgent}"`),
            { statusCode: 403 }
        );
    }
    return authAgent;
}

// Tool handler functions — each takes (args, agent, namespace) and returns a text string
const TOOL_HANDLERS = {
    // --- Memory ---
    async search(args, agent, namespace, actorId) {
        const targetNs = args.namespace || namespace;
        if (targetNs && targetNs !== '*') {
            validateNamespace(targetNs);
            await requireAccess(actorId, agent, 'agent', targetNs, 'read');
        }
        // For wildcard searches, push namespace filtering into the query
        let readable = null;
        if (!targetNs || targetNs === '*') {
            readable = await getReadableNamespaces(actorId, agent, 'agent');
        }
        const data = await searchMemory(args.query, targetNs, args.limit || 5, readable, actorId);
        const lines = data.results.map(r => {
            return `[${r.namespace}] ${r.source_file} — ${r.heading || '(no heading)'} (${(r.similarity * 100).toFixed(1)}%)\n${r.chunk_text}`;
        });
        return lines.join('\n\n---\n\n') || 'No results found.';
    },

    async delete(args, agent, namespace, actorId) {
        const targetNs = args.namespace || namespace;
        validateNamespace(targetNs);
        await requireAccess(actorId, agent, 'agent', targetNs, 'delete');
        const data = await deleteMemory(targetNs, args.source_file);
        return `Deleted ${data.chunks_deleted} chunks for ${args.source_file}`;
    },

    // --- Documents ---
    async save_note(args, agent, namespace, actorId) {
        const targetNs = args.namespace || namespace;
        validateNamespace(targetNs);
        const nsAccess = await hasAccess(actorId, agent, 'agent', targetNs, 'write');
        if (!nsAccess) {
            const noteAccess = await hasNoteAccess(targetNs, args.slug, actorId, 'write');
            if (!noteAccess) {
                throw Object.assign(
                    new Error(`Actor "${agent}" does not have write access to "${targetNs}/${args.slug}"`),
                    { statusCode: 403 }
                );
            }
        }
        const doc = await saveNote(targetNs, args.title, args.content, args.slug, agent);
        // Refresh activity indicator
        pool.query('UPDATE actors SET active_since = NOW() WHERE id = $1', [actorId])
            .then(() => broadcast('agent_activity', { agent, active: true }))
            .catch(() => {});
        return `Saved: ${doc.namespace}/${doc.slug}`;
    },

    async list_notes(args, agent, namespace, actorId) {
        const targetNs = args.namespace || namespace;
        validateNamespace(targetNs);
        await requireAccess(actorId, agent, 'agent', targetNs, 'read');
        const data = await listNotes(targetNs, args.limit, args.offset, args.prefix);
        if (data.notes.length === 0) {
            return 'No notes found.';
        }
        return data.notes.map(n => `${n.slug} — ${n.title} (updated ${n.updated_at})`).join('\n');
    },

    async read_note(args, agent, namespace, actorId) {
        const targetNs = args.namespace || namespace;
        validateNamespace(targetNs);
        // Check namespace-level access first, then fall back to note-level share
        const nsAccess = await hasAccess(actorId, agent, 'agent', targetNs, 'read');
        if (!nsAccess) {
            const noteAccess = await hasNoteAccess(targetNs, args.slug, actorId, 'read');
            if (!noteAccess) {
                throw Object.assign(
                    new Error(`Actor "${agent}" does not have read access to "${targetNs}/${args.slug}"`),
                    { statusCode: 403 }
                );
            }
        }
        const doc = await readNote(targetNs, args.slug);
        return doc.content;
    },

    async delete_note(args, agent, namespace, actorId) {
        const targetNs = args.namespace || namespace;
        validateNamespace(targetNs);
        const nsAccess = await hasAccess(actorId, agent, 'agent', targetNs, 'delete');
        if (!nsAccess) {
            const noteAccess = await hasNoteAccess(targetNs, args.slug, actorId, 'delete');
            if (!noteAccess) {
                throw Object.assign(
                    new Error(`Actor "${agent}" does not have delete access to "${targetNs}/${args.slug}"`),
                    { statusCode: 403 }
                );
            }
        }
        await deleteNote(targetNs, args.slug);
        return `Deleted: ${targetNs}/${args.slug}`;
    },

    async restore_note(args, agent, namespace, actorId) {
        const targetNs = args.namespace || namespace;
        validateNamespace(targetNs);
        await requireAccess(actorId, agent, 'agent', targetNs, 'write');
        const doc = await restoreNote(targetNs, args.slug);
        return `Restored: ${doc.namespace}/${doc.slug} — "${doc.title}"`;
    },

    async edit_note(args, agent, namespace, actorId) {
        const targetNs = args.namespace || namespace;
        validateNamespace(targetNs);
        const nsAccess = await hasAccess(actorId, agent, 'agent', targetNs, 'write');
        if (!nsAccess) {
            const noteAccess = await hasNoteAccess(targetNs, args.slug, actorId, 'write');
            if (!noteAccess) {
                throw Object.assign(
                    new Error(`Actor "${agent}" does not have write access to "${targetNs}/${args.slug}"`),
                    { statusCode: 403 }
                );
            }
        }
        const result = await editNote(targetNs, args.slug, args.old_string, args.new_string, args.replace_all);
        return `Edited: ${result.namespace}/${result.slug} (${result.replacements} replacement${result.replacements === 1 ? '' : 's'})`;
    },

    async move_note(args, agent, namespace, actorId) {
        const sourceNs = args.namespace || namespace;
        const targetNs = args.new_namespace || sourceNs;
        validateNamespace(sourceNs);
        if (targetNs !== sourceNs) validateNamespace(targetNs);
        // Need write on source (to remove) and write on target (to create)
        await requireAccess(actorId, agent, 'agent', sourceNs, 'write');
        if (targetNs !== sourceNs) {
            await requireAccess(actorId, agent, 'agent', targetNs, 'write');
        }
        const doc = await moveNote(sourceNs, args.slug, args.new_slug, args.new_namespace);
        return `Moved: ${sourceNs}/${args.slug} → ${doc.namespace}/${doc.slug}`;
    },

    async grep(args, agent, namespace, actorId) {
        const targetNs = args.namespace || namespace;
        if (targetNs && targetNs !== '*') {
            validateNamespace(targetNs);
            await requireAccess(actorId, agent, 'agent', targetNs, 'read');
        }
        // For wildcard searches, push namespace filtering into the query
        let readable = null;
        if (!targetNs || targetNs === '*') {
            readable = await getReadableNamespaces(actorId, agent, 'agent');
        }
        let results = await grepNotes(args.pattern, targetNs, args.limit, readable);
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
    async read_instructions(args, agent, namespace, actorId) {
        const result = await pool.query(
            'SELECT startup_instructions FROM agent_configuration WHERE actor_id = $1',
            [actorId]
        );
        if (result.rows.length === 0) {
            throw new Error('Agent not found');
        }
        const agentInstructions = result.rows[0].startup_instructions || '';
        const globalBootstrap = config.get('global_bootstrap') || '';
        var parts = [];
        if (globalBootstrap) {
            parts.push(globalBootstrap);
        }
        if (agentInstructions) {
            parts.push(agentInstructions);
        }
        return parts.length > 0 ? parts.join('\n\n') : '(no instructions set)';
    },

    async save_instructions(args, agent, namespace, actorId) {
        if (!args.content) {
            throw Object.assign(new Error('Required field: content'), { statusCode: 400 });
        }
        await pool.query(
            'UPDATE agent_configuration SET startup_instructions = $1 WHERE actor_id = $2',
            [args.content, actorId]
        );
        return `Instructions saved (${args.content.length} characters)`;
    },

    // --- Chat ---
    async chat_send(args, agent, namespace) {
        const fromAgent = validateIdentity(args.from, agent, 'from');
        let toAgents = null;
        if (args.to) {
            toAgents = [args.to];
        }
        // Auto-detect discussion_id from channel name pattern
        let discussionId = args.discussion_id;
        if (!discussionId && args.channel) {
            const match = args.channel.match(/^discussion-(\d+)$/);
            if (match) {
                discussionId = parseInt(match[1], 10);
            }
        }
        const data = await chatSend(fromAgent, toAgents, discussionId, args.message, args.channel);
        const targets = data.to_agents.map(r => r.agent).join(', ');
        return `Message sent to ${targets}`;
    },

    async chat_receive(args, agent, namespace) {
        const channel = args.channel || (args.discussion_id ? `discussion-${args.discussion_id}` : undefined);
        const data = await chatReceive(validateIdentity(args.agent, agent, 'agent'), channel);
        if (data.messages.length === 0) {
            return 'No new messages.';
        }
        const ids = data.messages.map(m => m.id);
        const formatted = data.messages.map(m => {
            return `[${m.from_agent}] (id:${m.id}, ${m.sent_at}): ${m.message}`;
        }).join('\n');
        return `${formatted}\n\n(ids: [${ids.join(', ')}] — call chat_ack to mark as read)`;
    },

    async chat_ack(args, agent, namespace) {
        // Accept both 'ids' (canonical) and 'message_ids' (legacy) — remove message_ids after 2026-04-15
        const ids = args.ids || args.message_ids;
        const data = await chatAck(agent, ids);
        return `Acked ${data.acked} message(s) for ${data.agent}: [${data.acked_ids.join(', ')}]`;
    },

    async chat_status(args, agent, namespace) {
        const data = await chatStatus(validateIdentity(args.agent, agent, 'agent'), args.channel);
        return `Chat status for ${data.agent}:\n  Pending: ${data.pending_count}\n  Max message ID: ${data.max_message_id}\n  Last message: ${data.last_message_at}\n  Last ack: ${data.last_ack_at}`;
    },

    // --- Mail ---
    async mail_send(args, agent, namespace, actorId) {
        const data = await mailSend(args.to, validateIdentity(args.from, agent, 'from'), args.subject, args.body, args.in_reply_to);
        // Refresh activity indicator
        pool.query('UPDATE actors SET active_since = NOW() WHERE id = $1', [actorId])
            .then(() => broadcast('agent_activity', { agent, active: true }))
            .catch(() => {});
        return `Mail sent to ${data.to_agent} (id: ${data.id}, subject: "${data.subject}")`;
    },

    async mail_check(args, agent, namespace) {
        const data = await mailCheck(validateIdentity(args.agent, agent, 'agent'));
        if (data.messages.length === 0) {
            return 'No unread mail.';
        }
        const formatted = data.messages.map(msg => {
            let line = `**From:** ${msg.from_agent} | **Date:** ${msg.sent_at} | **Subject:** ${msg.subject} | **ID:** ${msg.id}`;
            if (msg.in_reply_to) {
                line += ` | **In-Reply-To:** ${msg.in_reply_to}`;
            }
            if (msg.body_preview) {
                line += `\n> ${msg.body_preview}${msg.body_preview.length >= 200 ? '...' : ''}`;
            }
            return line;
        }).join('\n\n');
        return `${data.messages.length} unread message(s):\n\n${formatted}`;
    },

    async mail_receive(args, agent, namespace) {
        const data = await mailReceive(validateIdentity(args.agent, agent, 'agent'), args.ids);
        if (data.messages.length === 0) {
            return 'No new mail.';
        }
        const ids = data.messages.map(m => m.id);
        const formatted = data.messages.map(msg => {
            let header = `**From:** ${msg.from_agent}\n**Date:** ${msg.sent_at}\n**Subject:** ${msg.subject}\n**ID:** ${msg.id}`;
            if (msg.in_reply_to) {
                header += `\n**In-Reply-To:** ${msg.in_reply_to}`;
            }
            return `${header}\n\n${msg.body}`;
        }).join('\n\n---\n\n');
        return `${formatted}\n\n(ids: [${ids.join(', ')}] — call mail_ack to mark as read)`;
    },

    async mail_ack(args, agent, namespace) {
        const data = await mailAck(agent, args.ids);
        return `Acked ${data.acked} message(s) for ${data.agent}: [${data.acked_ids.join(', ')}]`;
    },

    async mail_edit(args, agent, namespace) {
        const data = await mailEdit(args.id, agent, args.subject, args.body);
        return `Mail ${data.id} updated (to: ${data.to_agent}, subject: "${data.subject}")`;
    },

    async mail_unsend(args, agent, namespace) {
        const data = await mailUnsend(args.id, agent);
        return `Mail ${data.id} unsent (was to: ${data.to_agent}, subject: "${data.subject}")`;
    },

    async mail_sent(args, agent, namespace) {
        const data = await mailSent(validateIdentity(args.agent, agent, 'agent'), { to: args.to, limit: args.limit, offset: args.offset });
        if (data.messages.length === 0) {
            return 'No sent mail found.';
        }
        const formatted = data.messages.map(msg => {
            const status = msg.acked_at ? 'read' : 'unread';
            let line = `**To:** ${msg.to_agent} [${status}] | **Date:** ${msg.sent_at} | **Subject:** ${msg.subject} | **ID:** ${msg.id}`;
            if (msg.in_reply_to) {
                line += ` | **In-Reply-To:** ${msg.in_reply_to}`;
            }
            if (msg.body_preview) {
                line += `\n> ${msg.body_preview}${msg.body_preview.length >= 200 ? '...' : ''}`;
            }
            return line;
        }).join('\n\n');
        return formatted;
    },

    async mail_history(args, agent, namespace) {
        const data = await mailHistory(validateIdentity(args.agent, agent, 'agent'), { from: args.from, limit: args.limit, offset: args.offset });
        if (data.messages.length === 0) {
            return 'No mail history found.';
        }
        const formatted = data.messages.map(msg => {
            let header = `**From:** ${msg.from_agent}\n**Date:** ${msg.sent_at}\n**Subject:** ${msg.subject}\n**ID:** ${msg.id}`;
            if (msg.in_reply_to) {
                header += `\n**In-Reply-To:** ${msg.in_reply_to}`;
            }
            return `${header}\n\n${msg.body}`;
        }).join('\n\n---\n\n');
        return formatted;
    },

    // --- Agent ---
    async agent_status(args, agent, namespace, actorId) {
        const queryAgent = validateIdentity(args.agent, agent, 'agent');
        const result = await pool.query(
            `SELECT
                a.agent,
                a.status,
                a.last_seen,
                a.expertise,
                a.provider,
                a.model,
                a.virtual,
                a.active_since,
                COALESCE(c.unread_count, 0)::int AS unread_chat,
                COALESCE(m.unread_count, 0)::int AS unread_mail
            FROM agent_status a
            LEFT JOIN (
                SELECT cm.from_actor_id, COUNT(*) AS unread_count
                FROM chat_messages cm
                WHERE cm.to_actor_id = $1 AND cm.acked_at IS NULL AND cm.deleted_at IS NULL AND cm.channel IS NULL
                GROUP BY cm.from_actor_id
            ) c ON c.from_actor_id = a.actor_id
            LEFT JOIN (
                SELECT ml.from_actor_id, COUNT(*) AS unread_count
                FROM mail ml
                WHERE ml.to_actor_id = $1 AND ml.acked_at IS NULL AND ml.deleted_at IS NULL
                GROUP BY ml.from_actor_id
            ) m ON m.from_actor_id = a.actor_id
            ORDER BY CASE a.status WHEN 'online' THEN 0 WHEN 'available' THEN 1 WHEN 'degraded' THEN 2 WHEN 'offline' THEN 3 ELSE 4 END, a.last_seen DESC NULLS LAST`,
            [actorId]
        );

        const lines = result.rows.map(a => {
            const parts = [`${a.agent}: ${a.status}`];
            if (a.active_since) {
                parts.push('ACTIVE');
            }
            if (a.virtual) {
                parts.push('virtual');
            }
            if (a.provider || a.model) {
                const identity = [a.provider, a.model].filter(Boolean).join('/');
                parts.push(identity);
            }
            if (a.last_seen) {
                parts.push(`last seen ${a.last_seen}`);
            }
            const expertise = JSON.parse(a.expertise || '[]');
            if (expertise.length > 0) {
                parts.push(`expertise: ${expertise.join(', ')}`);
            }
            parts.push(`${a.unread_chat || 0} unread chat`);
            parts.push(`${a.unread_mail || 0} unread mail`);
            return parts.join(' | ');
        });
        return lines.join('\n') || 'No agents registered.';
    },

    async update_expertise(args, agent, namespace, actorId) {
        if (!Array.isArray(args.expertise)) {
            throw Object.assign(new Error('expertise must be an array of strings'), { statusCode: 400 });
        }

        const cleaned = args.expertise
            .filter(e => typeof e === 'string' && e.trim().length > 0)
            .map(e => e.trim().toLowerCase());

        const json = JSON.stringify(cleaned);
        await pool.query('UPDATE actors SET expertise = $1 WHERE id = $2', [json, actorId]);
        return `Expertise updated for ${agent}: ${cleaned.join(', ') || '(none)'}`;
    },

    async update_profile(args, agent, namespace, actorId) {
        if (args.provider === undefined && args.model === undefined) {
            throw Object.assign(new Error('At least one field required: provider, model'), { statusCode: 400 });
        }

        const sets = [];
        const vals = [];
        let idx = 1;

        if (args.provider !== undefined) {
            sets.push(`provider = $${idx++}`);
            vals.push(args.provider);
        }
        if (args.model !== undefined) {
            sets.push(`model = $${idx++}`);
            vals.push(args.model);
        }
        vals.push(actorId);

        await pool.query(`UPDATE agent_configuration SET ${sets.join(', ')} WHERE actor_id = $${idx}`, vals);

        const parts = [];
        if (args.provider !== undefined) {
            parts.push(`provider: ${args.provider}`);
        }
        if (args.model !== undefined) {
            parts.push(`model: ${args.model}`);
        }
        return `Profile updated for ${agent}: ${parts.join(', ')}`;
    },

    // --- Activity ---
    async activity_start(args, agent, namespace, actorId) {
        await pool.query('UPDATE actors SET active_since = NOW() WHERE id = $1', [actorId]);
        broadcast('agent_activity', { agent, active: true });
        return `Activity started for ${agent}.`;
    },

    async activity_stop(args, agent, namespace, actorId) {
        await pool.query('UPDATE actors SET active_since = NULL WHERE id = $1', [actorId]);
        broadcast('agent_activity', { agent, active: false });
        return `Activity stopped for ${agent}.`;
    },

    // --- Discussion ---
    async discussion_create(args, agent, namespace) {
        const creator = validateIdentity(args.created_by, agent, 'created_by');
        const data = await discussionCreate(
            args.topic, creator, args.participants,
            null, args.channel, args.mode, args.context
        );
        const parts = data.participants.map(p => `${p.agent} (${p.status})`).join(', ');
        const channel = args.channel || `discussion-${data.discussion.id}`;
        let text = `Discussion #${data.discussion.id} created [${data.discussion.mode}]: "${data.discussion.topic}"\nParticipants: ${parts}\nChannel: ${channel}\nUse discussion_id: ${data.discussion.id} with chat_send to message participants.`;
        return text;
    },

    async discussion_list(args, agent, namespace) {
        const data = await discussionList(args.status, args.agent);
        if (data.discussions.length === 0) {
            return 'No discussions found.';
        }
        const lines = data.discussions.map(d => {
            let line = `#${d.id} [${d.status}] [${d.mode || 'realtime'}] "${d.topic}" (created ${d.created_at})`;
            if (d.outcome) {
                line += ` outcome: ${d.outcome}`;
            }
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
        if (d.outcome) {
            text += `\nOutcome: ${d.outcome}`;
        }
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
        const data = await discussionPending(validateIdentity(args.agent, agent, 'agent'));
        const sections = [];
        if (data.invited_discussions.length > 0) {
            const lines = data.invited_discussions.map(d => `  #${d.id} [${d.mode || 'realtime'}] "${d.topic}"`);
            sections.push('Pending invitations:\n' + lines.join('\n'));
        }
        if (data.deferred_discussions.length > 0) {
            const lines = data.deferred_discussions.map(d => `  #${d.id} [${d.mode || 'realtime'}] "${d.topic}"`);
            sections.push('Deferred discussions (join when ready):\n' + lines.join('\n'));
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
        const data = await discussionConclude(args.discussion_id, validateIdentity(args.agent, agent, 'agent'));
        return `Discussion #${data.discussion_id} ${data.status}. Outcome: ${data.outcome}.`;
    },

    async discussion_cancel(args, agent, namespace) {
        const data = await discussionConclude(args.discussion_id, validateIdentity(args.agent, agent, 'agent'), { cancel: true });
        return `Discussion #${data.discussion_id} ${data.status}. Outcome: ${data.outcome}.`;
    },

    async discussion_join(args, agent, namespace) {
        const data = await discussionJoin(args.discussion_id, validateIdentity(args.agent, agent, 'agent'));
        return `${data.agent} joined discussion #${data.discussion_id}.`;
    },

    async discussion_leave(args, agent, namespace) {
        const data = await discussionLeave(args.discussion_id, validateIdentity(args.agent, agent, 'agent'));
        return `${data.agent} left discussion #${data.discussion_id}.`;
    },

    async discussion_defer(args, agent, namespace) {
        const data = await discussionDefer(args.discussion_id, validateIdentity(args.agent, agent, 'agent'));
        return `${data.agent} deferred discussion #${data.discussion_id}. Defer count: ${data.defer_count}. Timeout extended to ${data.timeout_at}.`;
    },

    async discussion_vote_propose(args, agent, namespace) {
        const proposer = validateIdentity(args.proposed_by, agent, 'proposed_by');
        const data = await votePropose(
            args.discussion_id, proposer, args.question,
            args.type, args.threshold, args.closes_at
        );
        return `Vote #${data.vote.id} proposed in discussion #${data.vote.discussion_id}: "${data.vote.question}" (${data.vote.type}, ${data.vote.threshold})`;
    },

    async discussion_vote_cast(args, agent, namespace) {
        const data = await voteCast(args.vote_id, validateIdentity(args.agent, agent, 'agent'), args.choice, args.reason);
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

async function createMcpServer(req) {
    const agent = req.mcpAgent;

    // Load agent's startup instructions from DB to return in the initialize response.
    // This way Claude gets instructions on connect — no tool call needed.
    let instructions = 'At the start of every conversation, call the read_instructions tool to load your context and instructions. Follow whatever it returns.';
    try {
        const result = await pool.query('SELECT startup_instructions FROM agent_configuration WHERE actor_id = $1', [req.mcpActorId]);
        var agentInst = (result.rows.length > 0 && result.rows[0].startup_instructions) ? result.rows[0].startup_instructions : '';
        var globalBootstrap = config.get('global_bootstrap') || '';
        var parts = [];
        if (globalBootstrap) {
            parts.push(globalBootstrap);
        }
        if (agentInst) {
            parts.push(agentInst);
        }
        if (parts.length > 0) {
            instructions = parts.join('\n\n');
        }
    } catch (err) {
        // Fall back to generic instructions if DB query fails
    }

    const server = new Server(
        { name: 'llm-memory', version: '2.0.0' },
        {
            capabilities: { tools: {} },
            instructions
        }
    );
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
            const result = await handler(args || {}, agent, defaultNamespace, req.mcpActorId);
            return { content: [{ type: 'text', text: result }] };
        } catch (err) {
            // Log all errors with status code so the dashboard can distinguish
            // expected 4xx (note not found, old_string not found) from real 5xx failures.
            logError('mcp', `tool-${name}`, {
                agent,
                message: err.message,
                detail: err.stack,
                statusCode: err.statusCode || 500
            });
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

    const server = await createMcpServer(req);
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

// Handle HEAD /mcp (protocol discovery — no auth required)
router.head('/mcp', (req, res) => {
    res.setHeader('Mcp-Protocol-Version', MCP_PROTOCOL_VERSION);
    res.status(200).end();
});

// Handle POST /mcp (new request or existing session message)
router.post('/mcp', mcpAuth, async (req, res) => {
    const sessionId = req.headers['mcp-session-id'];

    // Fast path — session is already in memory
    if (sessionId && sessions.has(sessionId)) {
        const session = sessions.get(sessionId);
        await session.transport.handleRequest(req, res, req.body);
        return;
    }

    // Session ID provided but not in memory — rehydrate it.
    // Works whether the session is in the DB or not, and whether tools changed or not.
    // The client is already authenticated, so we just rebuild the server-side state.
    if (sessionId) {
        try {
            const session = await rehydrateSession(sessionId, req);

            // Persist session to DB (replace any stale row)
            pool.query('DELETE FROM mcp_sessions WHERE session_id = $1', [sessionId])
                .then(() => pool.query(
                    'INSERT INTO mcp_sessions (session_id, actor_id, tools_hash) VALUES ($1, $2, $3)',
                    [sessionId, req.mcpActorId, TOOLS_HASH]
                )).catch(() => {});

            await session.transport.handleRequest(req, res, req.body);
            return;
        } catch (err) {
            logError('mcp', 'session-rehydrate', { agent: req.mcpAgent, message: err.message, detail: err.stack, statusCode: 500 });
            return res.status(500).json({
                jsonrpc: '2.0',
                error: { code: -32000, message: 'Session rehydration failed' },
                id: null
            });
        }
    }

    // No session ID — new session (initialize request)
    // Diagnostic: log headers on new MCP session to differentiate connectors (DIAG-001)
    const ip = req.headers['x-forwarded-for']?.split(',')[0]?.trim() || req.ip;
    console.log('[DIAG-001] New MCP session', JSON.stringify({
        agent: req.mcpAgent,
        ip,
        userAgent: req.headers['user-agent'] || null,
        origin: req.headers['origin'] || null,
        referer: req.headers['referer'] || null,
        host: req.headers['host'] || null,
        allHeaders: Object.keys(req.headers).sort()
    }));

    const transport = new StreamableHTTPServerTransport({
        sessionIdGenerator: () => crypto.randomUUID()
    });

    const server = await createMcpServer(req);

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
            'INSERT INTO mcp_sessions (session_id, actor_id, tools_hash) VALUES ($1, $2, $3)',
            [newSessionId, req.mcpActorId, TOOLS_HASH]
        ).catch(err => logError('mcp', 'session-persist', { agent: req.mcpAgent, message: err.message, detail: err.stack, statusCode: 500 }));

        // Activate the activity spinner on new MCP session.
        // Agents that don't explicitly call activity_start (e.g. ChatGPT, third-party clients)
        // still show as active when they connect.
        pool.query('UPDATE actors SET active_since = NOW() WHERE id = $1', [req.mcpActorId])
            .then(() => broadcast('agent_activity', { agent: req.mcpAgent, active: true }))
            .catch(() => {});
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

    // Try in-memory first, then rehydrate unconditionally
    if (!sessions.has(sessionId)) {
        try {
            await rehydrateSession(sessionId, req);

            pool.query(`
                INSERT INTO mcp_sessions (session_id, actor_id, tools_hash) VALUES ($1, $2, $3)
                ON CONFLICT (session_id) DO UPDATE SET tools_hash = $3
            `, [sessionId, req.mcpActorId, TOOLS_HASH]).catch(() => {});
        } catch (err) {
            logError('mcp', 'session-rehydrate-sse', { agent: req.mcpAgent, message: err.message, detail: err.stack, statusCode: 500 });
            return res.status(500).json({
                jsonrpc: '2.0',
                error: { code: -32000, message: 'Session rehydration failed' },
                id: null
            });
        }
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
