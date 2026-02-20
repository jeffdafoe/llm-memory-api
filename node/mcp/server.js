const { Server } = require('@modelcontextprotocol/sdk/server/index.js');
const { StdioServerTransport } = require('@modelcontextprotocol/sdk/server/stdio.js');
const { CallToolRequestSchema, ListToolsRequestSchema } = require('@modelcontextprotocol/sdk/types.js');
const { readFileSync, writeFileSync, mkdirSync, existsSync } = require('fs');
const path = require('path');

const API_URL = process.env.MEMORY_API_URL || 'http://localhost:3100/v1';
const API_KEY = process.env.MEMORY_API_KEY || '';
const DEFAULT_NAMESPACE = process.env.MEMORY_DEFAULT_NAMESPACE || 'default';

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

const server = new Server(
    { name: 'llm-memory', version: '1.0.0' },
    { capabilities: { tools: {} } }
);

server.setRequestHandler(ListToolsRequestSchema, async () => {
    return {
        tools: [
            {
                name: 'memory_search',
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
                name: 'memory_ingest',
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
                name: 'memory_delete',
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
                name: 'memory_chat_send',
                description: 'Send a message to a chat channel for inter-instance communication',
                inputSchema: {
                    type: 'object',
                    properties: {
                        channel: { type: 'string', description: 'Channel name (e.g., "work-home")' },
                        message: { type: 'string', description: 'Message to send' },
                        from_namespace: { type: 'string', description: 'Sender namespace (default: configured namespace)' }
                    },
                    required: ['channel', 'message']
                }
            },
            {
                name: 'memory_chat_receive',
                description: 'Check for new messages on a chat channel',
                inputSchema: {
                    type: 'object',
                    properties: {
                        channel: { type: 'string', description: 'Channel name (e.g., "work-home")' },
                        since_id: { type: 'number', description: 'Only return messages after this ID (default: 0 for all)' }
                    },
                    required: ['channel']
                }
            },
            {
                name: 'memory_mail_send',
                description: 'Send mail to another instance. Mail is stored in the API database until the recipient acks it.',
                inputSchema: {
                    type: 'object',
                    properties: {
                        to_namespace: { type: 'string', description: 'Recipient namespace (e.g., "home", "work")' },
                        subject: { type: 'string', description: 'Mail subject line' },
                        body: { type: 'string', description: 'Mail body (markdown)' },
                        from_namespace: { type: 'string', description: 'Sender namespace (default: configured namespace)' }
                    },
                    required: ['to_namespace', 'subject', 'body']
                }
            },
            {
                name: 'memory_mail_check',
                description: 'Check for new mail addressed to this instance. Downloads messages, writes them to the local mailbox directory, and acks receipt.',
                inputSchema: {
                    type: 'object',
                    properties: {
                        namespace: { type: 'string', description: 'Namespace to check mail for (default: configured namespace)' },
                        mailbox_dir: { type: 'string', description: 'Local directory to write received mail files to' }
                    },
                    required: ['mailbox_dir']
                }
            },
            {
                name: 'memory_mail_ack',
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

    if (name === 'memory_search') {
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

    if (name === 'memory_ingest') {
        const content = readFileSync(args.file_path, 'utf-8');
        const sourceName = args.source_file || args.file_path.split(/[/\\]/).pop();

        const data = await apiCall('/ingest', {
            namespace: args.namespace || DEFAULT_NAMESPACE,
            source_file: sourceName,
            content
        });

        return { content: [{ type: 'text', text: `Ingested ${data.chunks_created} chunks from ${data.source_file} into namespace "${data.namespace}"` }] };
    }

    if (name === 'memory_delete') {
        const data = await apiCall('/delete', {
            namespace: args.namespace || DEFAULT_NAMESPACE,
            source_file: args.source_file
        });

        return { content: [{ type: 'text', text: `Deleted ${data.chunks_deleted} chunks for ${args.source_file}` }] };
    }

    if (name === 'memory_chat_send') {
        const data = await apiCall('/chat/send', {
            channel: args.channel,
            from_namespace: args.from_namespace || DEFAULT_NAMESPACE,
            message: args.message
        });

        return { content: [{ type: 'text', text: `Message sent to #${data.channel} (id: ${data.id})` }] };
    }

    if (name === 'memory_chat_receive') {
        const data = await apiCall('/chat/receive', {
            channel: args.channel,
            since_id: args.since_id || 0
        });

        if (data.messages.length === 0) {
            return { content: [{ type: 'text', text: 'No new messages.' }] };
        }

        const formatted = data.messages.map(m => {
            return `[${m.from_namespace}] (${m.sent_at}): ${m.message}`;
        }).join('\n');

        return { content: [{ type: 'text', text: formatted }] };
    }

    if (name === 'memory_mail_send') {
        const data = await apiCall('/mail/send', {
            to_namespace: args.to_namespace,
            from_namespace: args.from_namespace || DEFAULT_NAMESPACE,
            subject: args.subject,
            body: args.body
        });

        return { content: [{ type: 'text', text: `Mail sent to ${data.to_namespace} (id: ${data.id}, subject: "${data.subject}")` }] };
    }

    if (name === 'memory_mail_check') {
        const namespace = args.namespace || DEFAULT_NAMESPACE;
        const data = await apiCall('/mail/check', { namespace });

        if (data.messages.length === 0) {
            return { content: [{ type: 'text', text: 'No new mail.' }] };
        }

        if (!existsSync(args.mailbox_dir)) {
            mkdirSync(args.mailbox_dir, { recursive: true });
        }

        const written = [];
        for (const msg of data.messages) {
            const slug = msg.subject.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/(^-|-$)/g, '');
            const filename = `${msg.id.slice(0, 8)}-${slug}.md`;
            const filepath = path.join(args.mailbox_dir, filename);

            const content = `# ${msg.subject}\n\n**From:** ${msg.from_namespace}\n**Date:** ${msg.sent_at}\n**ID:** ${msg.id}\n\n${msg.body}`;
            writeFileSync(filepath, content, 'utf-8');
            written.push({ id: msg.id, filename });
        }

        const ackData = await apiCall('/mail/ack', {
            ids: written.map(w => w.id)
        });

        const summary = written.map(w => `  ${w.filename}`).join('\n');
        return { content: [{ type: 'text', text: `Received ${written.length} message(s), wrote to ${args.mailbox_dir}:\n${summary}\n\nAcked: ${ackData.acked}` }] };
    }

    if (name === 'memory_mail_ack') {
        const data = await apiCall('/mail/ack', { ids: args.ids });
        return { content: [{ type: 'text', text: `Acked ${data.acked} message(s)` }] };
    }

    throw new Error(`Unknown tool: ${name}`);
});

async function main() {
    const transport = new StdioServerTransport();
    await server.connect(transport);
}

main().catch(console.error);
