const { Server } = require('@modelcontextprotocol/sdk/server/index.js');
const { StdioServerTransport } = require('@modelcontextprotocol/sdk/server/stdio.js');
const { CallToolRequestSchema, ListToolsRequestSchema } = require('@modelcontextprotocol/sdk/types.js');
const { readFileSync, writeFileSync, mkdirSync, existsSync } = require('fs');
const path = require('path');

const API_URL = process.env.MEMORY_API_URL || 'http://localhost:3100/v1';
const API_KEY = process.env.MEMORY_API_KEY || '';
const DEFAULT_NAMESPACE = process.env.MEMORY_DEFAULT_NAMESPACE || 'default';
const DEFAULT_AGENT = process.env.MEMORY_DEFAULT_AGENT || DEFAULT_NAMESPACE;

async function apiCall(endpoint, body) {
    const response = await fetch(`${API_URL}${endpoint}`, {
        method: 'POST',
        headers: {
            'Content-Type': 'application/json',
            'Authorization': `Bearer ${API_KEY}`
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

async function apiGet(endpoint) {
    const response = await fetch(`${API_URL}${endpoint}`, {
        method: 'GET',
        headers: {
            'Authorization': `Bearer ${API_KEY}`
        }
    });

    const data = await response.json();

    if (!response.ok) {
        const msg = data.error ? data.error.message : JSON.stringify(data);
        throw new Error(`API error ${response.status}: ${msg}`);
    }

    return data;
}

async function autoRegister() {
    try {
        await apiCall('/register', { agent: DEFAULT_AGENT });
    } catch (err) {
        // Registration failure is non-fatal — API may not be upgraded yet
    }
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
                        from: { type: 'string', description: 'Sender agent (default: configured agent)' }
                    },
                    required: ['to', 'message']
                }
            },
            {
                name: 'chat_receive',
                description: 'Check for unread chat messages. Returns unacked messages. Call chat_ack with the message IDs after processing.',
                inputSchema: {
                    type: 'object',
                    properties: {
                        agent: { type: 'string', description: 'Agent to check messages for (default: configured agent)' }
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
                        agent: { type: 'string', description: 'Agent to check status for (default: configured agent)' }
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
                name: 'mail_check',
                description: 'Check for new mail addressed to this agent. Downloads messages, writes them to the local mailbox directory, and acks receipt.',
                inputSchema: {
                    type: 'object',
                    properties: {
                        agent: { type: 'string', description: 'Agent to check mail for (default: configured agent)' },
                        mailbox_dir: { type: 'string', description: 'Local directory to write received mail files to' }
                    },
                    required: ['mailbox_dir']
                }
            },
            {
                name: 'mail_ack',
                description: 'Manually ack specific mail messages by UUID. Use this if mail_check failed after downloading but before acking.',
                inputSchema: {
                    type: 'object',
                    properties: {
                        ids: { type: 'array', items: { type: 'string' }, description: 'Array of mail UUIDs to ack' }
                    },
                    required: ['ids']
                }
            }
        ]
    };
});

server.setRequestHandler(CallToolRequestSchema, async (request) => {
    const { name, arguments: args } = request.params;

    if (name === 'search') {
        const data = await apiCall('/search', {
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

        const data = await apiCall('/ingest', {
            namespace: args.namespace || DEFAULT_NAMESPACE,
            source_file: sourceName,
            content
        });

        return { content: [{ type: 'text', text: `Ingested ${data.chunks_created} chunks from ${data.source_file} into namespace "${data.namespace}"` }] };
    }

    if (name === 'delete') {
        const data = await apiCall('/delete', {
            namespace: args.namespace || DEFAULT_NAMESPACE,
            source_file: args.source_file
        });

        return { content: [{ type: 'text', text: `Deleted ${data.chunks_deleted} chunks for ${args.source_file}` }] };
    }

    if (name === 'chat_send') {
        const data = await apiCall('/chat/send', {
            from_agent: args.from || DEFAULT_AGENT,
            to_agent: args.to,
            message: args.message
        });

        if (data.broadcast) {
            const targets = data.recipients.map(r => r.to_agent).join(', ');
            return { content: [{ type: 'text', text: `Broadcast sent to: ${targets}` }] };
        }

        return { content: [{ type: 'text', text: `Message sent to ${data.to_agent} (id: ${data.id})` }] };
    }

    if (name === 'chat_receive') {
        const agent = args.agent || DEFAULT_AGENT;
        const data = await apiCall('/chat/receive', { agent });

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
        const data = await apiGet(`/chat/status?agent=${encodeURIComponent(agent)}`);

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

    if (name === 'mail_check') {
        const agent = args.agent || DEFAULT_AGENT;
        const data = await apiCall('/mail/check', { agent });

        if (data.messages.length === 0) {
            return { content: [{ type: 'text', text: 'No new mail.' }] };
        }

        if (!existsSync(args.mailbox_dir)) {
            mkdirSync(args.mailbox_dir, { recursive: true });
        }

        const written = [];
        const failed = [];
        for (const msg of data.messages) {
            const slug = msg.subject.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/(^-|-$)/g, '');
            const filename = `${msg.id.slice(0, 8)}-${slug}.md`;
            const filepath = path.join(args.mailbox_dir, filename);

            const content = `# ${msg.subject}\n\n**From:** ${msg.from_agent}\n**Date:** ${msg.sent_at}\n**ID:** ${msg.id}\n\n${msg.body}`;
            try {
                writeFileSync(filepath, content, 'utf-8');
                const stat = require('fs').statSync(filepath);
                if (stat.size === 0) {
                    failed.push({ id: msg.id, filename, reason: 'empty file after write' });
                } else {
                    written.push({ id: msg.id, filename });
                }
            } catch (err) {
                failed.push({ id: msg.id, filename, reason: err.message });
            }
        }

        let ackCount = 0;
        if (written.length > 0) {
            const ackData = await apiCall('/mail/ack', {
                ids: written.map(w => w.id)
            });
            ackCount = ackData.acked;
        }

        const summary = written.map(w => `  ${w.filename}`).join('\n');
        let result = `Received ${written.length} message(s), wrote to ${args.mailbox_dir}:\n${summary}\n\nAcked: ${ackCount}`;
        if (failed.length > 0) {
            const failSummary = failed.map(f => `  ${f.filename}: ${f.reason}`).join('\n');
            result += `\n\nFailed (NOT acked, will retry next check):\n${failSummary}`;
        }
        return { content: [{ type: 'text', text: result }] };
    }

    if (name === 'mail_ack') {
        const data = await apiCall('/mail/ack', { ids: args.ids });
        return { content: [{ type: 'text', text: `Acked ${data.acked} message(s)` }] };
    }

    throw new Error(`Unknown tool: ${name}`);
});

async function main() {
    const transport = new StdioServerTransport();
    await server.connect(transport);
    await autoRegister();
}

main().catch(console.error);
