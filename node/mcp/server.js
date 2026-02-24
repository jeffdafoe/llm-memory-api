const { Server } = require('@modelcontextprotocol/sdk/server/index.js');
const { StdioServerTransport } = require('@modelcontextprotocol/sdk/server/stdio.js');
const { CallToolRequestSchema, ListToolsRequestSchema } = require('@modelcontextprotocol/sdk/types.js');
const { readFileSync, readdirSync, statSync } = require('fs');
const path = require('path');

const API_URL = process.env.MEMORY_API_URL || 'http://localhost:3100/v1';
const AGENT_PASSPHRASE = process.env.MEMORY_AGENT_PASSPHRASE || '';
const DEFAULT_NAMESPACE = process.env.MEMORY_DEFAULT_NAMESPACE || 'default';
const DEFAULT_AGENT = process.env.MEMORY_DEFAULT_AGENT || DEFAULT_NAMESPACE;
const MEMORY_REPO_PATH = process.env.MEMORY_REPO_PATH || '';

// Session token obtained at login, used for all authenticated API calls
let sessionToken = null;

// Unauthenticated API call — used only for login
async function apiCallNoAuth(endpoint, body) {
    const response = await fetch(`${API_URL}${endpoint}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
    });

    const data = await response.json();

    if (!response.ok) {
        const msg = data.error ? data.error.message : JSON.stringify(data);
        throw new Error(`API error ${response.status}: ${msg}`);
    }

    return data;
}

async function apiCall(endpoint, body) {
    if (!sessionToken) {
        throw new Error('Not logged in — no session token available');
    }

    const response = await fetch(`${API_URL}${endpoint}`, {
        method: 'POST',
        headers: {
            'Content-Type': 'application/json',
            'Authorization': `Bearer ${sessionToken}`
        },
        body: JSON.stringify(body)
    });

    const data = await response.json();

    if (!response.ok) {
        const msg = data.error ? data.error.message : JSON.stringify(data);
        throw new Error(`API error ${response.status}: ${msg}`);
    }

    return data;
}

// Login with passphrase, store session token for the lifetime of the process
async function login() {
    if (!AGENT_PASSPHRASE) {
        throw new Error('MEMORY_AGENT_PASSPHRASE environment variable is not set');
    }

    const data = await apiCallNoAuth('/agent/login', {
        agent: DEFAULT_AGENT,
        passphrase: AGENT_PASSPHRASE,
        subsystem: 'mcp'
    });

    sessionToken = data.session_token;

    if (data.rotation_due) {
        console.error(`[llm-memory] Passphrase rotation recommended for agent "${DEFAULT_AGENT}"`);
    }
}

// Best-effort logout on process exit
async function logout() {
    if (!sessionToken) {
        return;
    }
    try {
        await apiCall('/agent/logout', { agent: DEFAULT_AGENT });
    } catch (err) {
        // Logout failure is non-fatal — session will expire on its own
    }
    sessionToken = null;
}

// Recursively find all .md files under a directory, skipping hidden dirs and node_modules
function walkMarkdownFiles(dir) {
    const results = [];
    let entries;
    try {
        entries = readdirSync(dir, { withFileTypes: true });
    } catch (err) {
        return results;
    }
    for (const entry of entries) {
        const fullPath = path.join(dir, entry.name);
        if (entry.isDirectory()) {
            if (entry.name.startsWith('.') || entry.name === 'node_modules') {
                continue;
            }
            results.push(...walkMarkdownFiles(fullPath));
        } else if (entry.name.endsWith('.md')) {
            results.push(fullPath);
        }
    }
    return results;
}

const server = new Server(
    { name: 'llm-memory', version: '2.0.0' },
    { capabilities: { tools: {} } }
);

server.setRequestHandler(ListToolsRequestSchema, async () => {
    return {
        tools: [
            {
                name: 'search',
                description: 'Search memory for relevant notes using semantic similarity',
                inputSchema: {
                    type: 'object',
                    properties: {
                        query: { type: 'string', description: 'Search query' },
                        namespace: { type: 'string', description: 'Namespace to search (default: configured namespace, "*" for all)' },
                        limit: { type: 'number', description: 'Max results (default: 5)' }
                    },
                    required: ['query']
                }
            },
            {
                name: 'ingest',
                description: 'Ingest a markdown file into memory. Reads the file from local disk and sends content to the API.',
                inputSchema: {
                    type: 'object',
                    properties: {
                        file_path: { type: 'string', description: 'Absolute path to the markdown file to ingest' },
                        source_file: { type: 'string', description: 'Name to store as (defaults to filename)' },
                        namespace: { type: 'string', description: 'Namespace (default: configured namespace)' }
                    },
                    required: ['file_path']
                }
            },
            {
                name: 'delete',
                description: 'Delete all chunks for a specific source file from memory',
                inputSchema: {
                    type: 'object',
                    properties: {
                        source_file: { type: 'string', description: 'Source file name to delete' },
                        namespace: { type: 'string', description: 'Namespace (default: configured namespace)' }
                    },
                    required: ['source_file']
                }
            },
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
            {
                name: 'ingest_notes',
                description: 'Ingest all markdown notes from the llm-memory repo that have changed since last indexing. Compares local file modification times against the API index. Requires MEMORY_REPO_PATH env var.',
                inputSchema: {
                    type: 'object',
                    properties: {
                        dry_run: { type: 'boolean', description: 'If true, only report what would be re-ingested without actually doing it (default: false)' }
                    }
                }
            },
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
        ]
    };
});

const SKIP_NOTICES = new Set(['chat_send', 'chat_receive', 'chat_ack', 'chat_status']);

// NOTE: from_agent filter not yet deployed to VPS. Until then,
// checkSystemNotices() returns ALL unacked messages, not just system ones.
// Safe to leave enabled when there are no unacked chat messages pending.
const PIGGYBACK_ENABLED = true;

server.setRequestHandler(CallToolRequestSchema, async (request) => {
    const { name, arguments: args } = request.params;
    const result = await handleToolCall(name, args);

    if (PIGGYBACK_ENABLED && !SKIP_NOTICES.has(name)) {
        const notices = await checkSystemNotices();
        if (notices) {
            result.content.push({ type: 'text', text: `[SYSTEM NOTICES]\n${notices}` });
        }
    }

    return result;
});

async function handleToolCall(name, args) {
    if (name === 'search') {
        const data = await apiCall('/memory/search', {
            query: args.query,
            namespace: args.namespace || DEFAULT_NAMESPACE,
            limit: args.limit || 5
        });

        const formatted = data.results.map(r => {
            return `[${r.namespace}] ${r.source_file} — ${r.heading || '(no heading)'} (${(r.similarity * 100).toFixed(1)}%)\n${r.chunk_text}`;
        }).join('\n\n---\n\n');

        return { content: [{ type: 'text', text: formatted || 'No results found.' }] };
    }

    if (name === 'ingest') {
        const content = readFileSync(args.file_path, 'utf-8');
        const sourceName = args.source_file || args.file_path.split(/[/\\]/).pop();

        const data = await apiCall('/memory/ingest', {
            namespace: args.namespace || DEFAULT_NAMESPACE,
            source_file: sourceName,
            content
        });

        return { content: [{ type: 'text', text: `Ingested ${data.chunks_created} chunks from ${data.source_file} into namespace "${data.namespace}"` }] };
    }

    if (name === 'delete') {
        const data = await apiCall('/memory/delete', {
            namespace: args.namespace || DEFAULT_NAMESPACE,
            source_file: args.source_file
        });

        return { content: [{ type: 'text', text: `Deleted ${data.chunks_deleted} chunks for ${args.source_file}` }] };
    }

    if (name === 'chat_send') {
        const body = {
            from_agent: args.from || DEFAULT_AGENT,
            message: args.message
        };
        if (args.discussion_id) {
            body.discussion_id = args.discussion_id;
        } else {
            body.to_agents = [args.to];
        }
        if (args.channel) {
            body.channel = args.channel;
        }
        const data = await apiCall('/chat/send', body);

        const targets = data.to_agents.map(r => r.agent).join(', ');
        return { content: [{ type: 'text', text: `Message sent to ${targets}` }] };
    }

    if (name === 'chat_receive') {
        const agent = args.agent || DEFAULT_AGENT;
        const body = { agent };
        if (args.channel) {
            body.channel = args.channel;
        }
        const data = await apiCall('/chat/receive', body);

        if (data.messages.length === 0) {
            return { content: [{ type: 'text', text: 'No new messages.' }] };
        }

        const ids = data.messages.map(m => m.id);
        const formatted = data.messages.map(m => {
            return `[${m.from_agent}] (id:${m.id}, ${m.sent_at}): ${m.message}`;
        }).join('\n');

        return { content: [{ type: 'text', text: `${formatted}\n\n(message_ids: [${ids.join(', ')}] — call chat_ack to mark as read)` }] };
    }

    if (name === 'chat_ack') {
        const agent = args.agent || DEFAULT_AGENT;
        const data = await apiCall('/chat/ack', {
            agent,
            message_ids: args.message_ids
        });

        return { content: [{ type: 'text', text: `Acked ${data.acked} message(s) for ${data.agent}: [${data.acked_ids.join(', ')}]` }] };
    }

    if (name === 'chat_status') {
        const agent = args.agent || DEFAULT_AGENT;
        const body = { agent };
        if (args.channel) {
            body.channel = args.channel;
        }
        const data = await apiCall('/chat/status', body);

        return { content: [{ type: 'text', text: `Chat status for ${data.agent}:\n  Pending: ${data.pending_count}\n  Max message ID: ${data.max_message_id}\n  Last message: ${data.last_message_at}\n  Last ack: ${data.last_ack_at}` }] };
    }

    if (name === 'mail_send') {
        const data = await apiCall('/mail/send', {
            to_agent: args.to,
            from_agent: args.from || DEFAULT_AGENT,
            subject: args.subject,
            body: args.body
        });

        return { content: [{ type: 'text', text: `Mail sent to ${data.to_agent} (id: ${data.id}, subject: "${data.subject}")` }] };
    }

    if (name === 'mail_receive') {
        const agent = args.agent || DEFAULT_AGENT;
        const data = await apiCall('/mail/receive', { agent });

        if (data.messages.length === 0) {
            return { content: [{ type: 'text', text: 'No new mail.' }] };
        }

        const ids = data.messages.map(m => m.id);
        const formatted = data.messages.map(msg =>
            `**From:** ${msg.from_agent}\n**Date:** ${msg.sent_at}\n**Subject:** ${msg.subject}\n**ID:** ${msg.id}\n\n${msg.body}`
        ).join('\n\n---\n\n');

        return { content: [{ type: 'text', text: `${formatted}\n\n(mail_ids: [${ids.join(', ')}] — call mail_ack to mark as read)` }] };
    }

    if (name === 'mail_ack') {
        const agent = args.agent || DEFAULT_AGENT;
        const data = await apiCall('/mail/ack', { agent, message_ids: args.ids });
        return { content: [{ type: 'text', text: `Acked ${data.acked} message(s) for ${data.agent}: [${data.acked_ids.join(', ')}]` }] };
    }

    if (name === 'ingest_notes') {
        if (!MEMORY_REPO_PATH) {
            throw new Error('MEMORY_REPO_PATH environment variable is not set');
        }

        const dryRun = args.dry_run || false;

        // Walk repo for .md files under the three namespace directories
        const namespaces = ['work', 'home', 'shared'];
        const localFiles = [];

        for (const ns of namespaces) {
            const nsDir = path.join(MEMORY_REPO_PATH, ns);
            const mdFiles = walkMarkdownFiles(nsDir);
            for (const filePath of mdFiles) {
                const stat = statSync(filePath);
                localFiles.push({
                    filePath,
                    fileName: path.basename(filePath),
                    namespace: ns,
                    mtime: stat.mtime
                });
            }
        }

        // Get current index state from the API
        const statusData = await apiCall('/memory/ingest/status', {});

        // Build lookup: "namespace:source_file" -> ingested_at Date
        const indexMap = {};
        for (const entry of statusData.files) {
            indexMap[`${entry.namespace}:${entry.source_file}`] = new Date(entry.ingested_at);
        }

        // Compare local files against index
        const toIngest = [];
        const upToDate = [];

        for (const file of localFiles) {
            const key = `${file.namespace}:${file.fileName}`;
            const indexedAt = indexMap[key];

            if (!indexedAt || file.mtime > indexedAt) {
                toIngest.push(file);
            } else {
                upToDate.push(file);
            }
        }

        if (dryRun) {
            const staleList = toIngest.map(f => `  ${f.namespace}/${f.fileName}`).join('\n');
            return {
                content: [{
                    type: 'text',
                    text: `Dry run: ${toIngest.length} file(s) need re-indexing, ${upToDate.length} up to date.\n\nStale/new:\n${staleList || '  (none)'}`
                }]
            };
        }

        // Re-ingest stale/new files
        const results = [];
        for (const file of toIngest) {
            try {
                const content = readFileSync(file.filePath, 'utf-8');
                const data = await apiCall('/memory/ingest', {
                    namespace: file.namespace,
                    source_file: file.fileName,
                    content
                });
                results.push(`  OK ${file.namespace}/${file.fileName} (${data.chunks_created} chunks)`);
            } catch (err) {
                results.push(`  FAIL ${file.namespace}/${file.fileName}: ${err.message}`);
            }
        }

        const summary = `Reindex complete: ${toIngest.length} file(s) re-ingested, ${upToDate.length} up to date.`;
        const details = results.length > 0 ? '\n\n' + results.join('\n') : '';

        return { content: [{ type: 'text', text: summary + details }] };
    }

    if (name === 'agent_status') {
        const agent = args.agent || DEFAULT_AGENT;
        const data = await apiCall('/agent/status', { agent });

        const lines = data.agents.map(a => {
            const parts = [`${a.agent}: ${a.status}`];
            if (a.last_seen) {
                parts.push(`last seen ${a.last_seen}`);
            }
            parts.push(`${a.unread_chat || 0} unread chat`);
            parts.push(`${a.unread_mail || 0} unread mail`);
            return parts.join(' | ');
        });

        return { content: [{ type: 'text', text: lines.join('\n') || 'No agents registered.' }] };
    }

    if (name === 'discussion_create') {
        const creator = args.created_by || DEFAULT_AGENT;
        const body = {
            topic: args.topic,
            created_by: creator,
            participants: args.participants,
            channel: args.channel || null
        };
        if (args.mode) {
            body.mode = args.mode;
        }
        if (args.context) {
            body.context = args.context;
        }
        const data = await apiCall('/discussion/create', body);

        const parts = data.participants.map(p => `${p.agent} (${p.status})`).join(', ');
        let text = `Discussion #${data.discussion.id} created [${data.discussion.mode}]: "${data.discussion.topic}"\nParticipants: ${parts}`;
        if (args.channel) {
            text += `\nChannel: ${args.channel}`;
        }
        return { content: [{ type: 'text', text }] };
    }

    if (name === 'discussion_list') {
        const body = {};
        if (args.status) {
            body.status = args.status;
        }
        if (args.agent) {
            body.agent = args.agent;
        }
        const data = await apiCall('/discussion/list', body);

        if (data.discussions.length === 0) {
            return { content: [{ type: 'text', text: 'No discussions found.' }] };
        }

        const lines = data.discussions.map(d => {
            let line = `#${d.id} [${d.status}] [${d.mode || 'realtime'}] "${d.topic}" (created ${d.created_at})`;
            if (d.channel) {
                line += ` channel: ${d.channel}`;
            }
            return line;
        });
        return { content: [{ type: 'text', text: lines.join('\n') }] };
    }

    if (name === 'discussion_status') {
        const data = await apiCall('/discussion/status', { discussion_id: args.discussion_id });

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

        return { content: [{ type: 'text', text }] };
    }

    if (name === 'discussion_pending') {
        const agent = args.agent || DEFAULT_AGENT;
        const data = await apiCall('/discussion/pending', { agent });

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
            return { content: [{ type: 'text', text: 'Nothing pending.' }] };
        }
        return { content: [{ type: 'text', text: sections.join('\n\n') }] };
    }

    if (name === 'discussion_conclude') {
        const agent = args.agent || DEFAULT_AGENT;
        const data = await apiCall('/discussion/conclude', { discussion_id: args.discussion_id, agent });
        return { content: [{ type: 'text', text: `Discussion #${data.discussion_id} concluded.` }] };
    }

    if (name === 'discussion_join') {
        const agent = args.agent || DEFAULT_AGENT;
        const data = await apiCall('/discussion/join', { discussion_id: args.discussion_id, agent });
        return { content: [{ type: 'text', text: `${data.agent} joined discussion #${data.discussion_id}.` }] };
    }

    if (name === 'discussion_leave') {
        const agent = args.agent || DEFAULT_AGENT;
        const data = await apiCall('/discussion/leave', { discussion_id: args.discussion_id, agent });
        return { content: [{ type: 'text', text: `${data.agent} left discussion #${data.discussion_id}.` }] };
    }

    if (name === 'discussion_vote_propose') {
        const proposer = args.proposed_by || DEFAULT_AGENT;
        const data = await apiCall('/discussion/vote/propose', {
            discussion_id: args.discussion_id,
            proposed_by: proposer,
            question: args.question,
            type: args.type || 'general',
            threshold: args.threshold || 'unanimous',
            closes_at: args.closes_at || null
        });

        return { content: [{ type: 'text', text: `Vote #${data.vote.id} proposed in discussion #${data.vote.discussion_id}: "${data.vote.question}" (${data.vote.type}, ${data.vote.threshold})` }] };
    }

    if (name === 'discussion_vote_cast') {
        const agent = args.agent || DEFAULT_AGENT;
        const data = await apiCall('/discussion/vote/cast', {
            vote_id: args.vote_id,
            agent,
            choice: args.choice,
            reason: args.reason || null
        });

        return { content: [{ type: 'text', text: `Vote cast on #${data.vote_id}: choice ${data.choice}. Vote status: ${data.vote_status}` }] };
    }

    if (name === 'discussion_vote_status') {
        const data = await apiCall('/discussion/vote/status', { vote_id: args.vote_id });

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

        return { content: [{ type: 'text', text }] };
    }

    throw new Error(`Unknown tool: ${name}`);
}

// Check for unread system messages. Returns formatted text or null.
// Does NOT auto-ack — notices repeat on every tool call until the LLM
// explicitly calls chat_ack, ensuring important notices aren't lost.
async function checkSystemNotices() {
    try {
        const data = await apiCall('/chat/receive', {
            agent: DEFAULT_AGENT,
            from_agent: 'system'
        });
        if (data.messages.length === 0) {
            return null;
        }
        const ids = data.messages.map(m => m.id);
        const lines = data.messages.map(m => m.message);
        return `${lines.join('\n')}\n(system message_ids: [${ids.join(', ')}] — call chat_ack to dismiss)`;
    } catch (err) {
        return null;
    }
}

async function sendHeartbeat() {
    try {
        await apiCall('/agent/heartbeat', { agent: DEFAULT_AGENT });
    } catch (err) {
        // Heartbeat failure is non-fatal
    }
}

async function autoIngest() {
    if (!MEMORY_REPO_PATH) {
        return;
    }
    try {
        const namespaces = ['work', 'home', 'shared'];
        const localFiles = [];
        for (const ns of namespaces) {
            const nsDir = path.join(MEMORY_REPO_PATH, ns);
            const mdFiles = walkMarkdownFiles(nsDir);
            for (const filePath of mdFiles) {
                const stat = statSync(filePath);
                localFiles.push({ filePath, fileName: path.basename(filePath), namespace: ns, mtime: stat.mtime });
            }
        }

        const statusData = await apiCall('/memory/ingest/status', {});
        const indexMap = {};
        for (const entry of statusData.files) {
            indexMap[`${entry.namespace}:${entry.source_file}`] = new Date(entry.ingested_at);
        }

        const toIngest = localFiles.filter(f => {
            const indexedAt = indexMap[`${f.namespace}:${f.fileName}`];
            return !indexedAt || f.mtime > indexedAt;
        });

        if (toIngest.length === 0) {
            return;
        }

        for (const file of toIngest) {
            const content = readFileSync(file.filePath, 'utf-8');
            await apiCall('/memory/ingest', { namespace: file.namespace, source_file: file.fileName, content });
        }
    } catch (err) {
        // Auto-ingest failure is non-fatal
    }
}

async function main() {
    const transport = new StdioServerTransport();
    await server.connect(transport);
    await login();
    await sendHeartbeat();
    setInterval(sendHeartbeat, 120000);
    autoIngest();

    // Best-effort logout on shutdown
    process.on('SIGTERM', async () => {
        await logout();
        process.exit(0);
    });
    process.on('SIGINT', async () => {
        await logout();
        process.exit(0);
    });
}

main().catch(console.error);
