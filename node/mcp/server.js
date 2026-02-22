const { Server } = require('@modelcontextprotocol/sdk/server/index.js');
const { StdioServerTransport } = require('@modelcontextprotocol/sdk/server/stdio.js');
const { CallToolRequestSchema, ListToolsRequestSchema } = require('@modelcontextprotocol/sdk/types.js');
const { readFileSync, readdirSync, statSync } = require('fs');
const path = require('path');

const API_URL = process.env.MEMORY_API_URL || 'http://localhost:3100/v1';
const API_KEY = process.env.MEMORY_API_KEY || '';
const DEFAULT_NAMESPACE = process.env.MEMORY_DEFAULT_NAMESPACE || 'default';
const DEFAULT_AGENT = process.env.MEMORY_DEFAULT_AGENT || DEFAULT_NAMESPACE;
const MEMORY_REPO_PATH = process.env.MEMORY_REPO_PATH || '';

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

async function autoRegister() {
    try {
        await apiCall('/register', { agent: DEFAULT_AGENT });
    } catch (err) {
        // Registration failure is non-fatal — API may not be upgraded yet
    }
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
                        channel: { type: 'string', description: 'Optional channel for message isolation (e.g., "discussion"). Omit for regular chat.' }
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
                name: 'mail_check',
                description: 'Check for new mail addressed to this agent. Returns unacked messages and acks them.',
                inputSchema: {
                    type: 'object',
                    properties: {
                        agent: { type: 'string', description: 'Agent to check mail for (default: configured agent)' }
                    }
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
                name: 'presence',
                description: 'Get presence info for all agents: online/offline status, last seen time, and unread chat/mail counts for the querying agent.',
                inputSchema: {
                    type: 'object',
                    properties: {
                        agent: { type: 'string', description: 'Agent requesting presence info (default: configured agent). Unread counts are relative to this agent.' }
                    }
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
        const body = {
            from_agent: args.from || DEFAULT_AGENT,
            to_agent: args.to,
            message: args.message
        };
        if (args.channel) {
            body.channel = args.channel;
        }
        const data = await apiCall('/chat/send', body);

        if (data.broadcast) {
            const targets = data.recipients.map(r => r.to_agent).join(', ');
            return { content: [{ type: 'text', text: `Broadcast sent to: ${targets}` }] };
        }

        return { content: [{ type: 'text', text: `Message sent to ${data.to_agent} (id: ${data.id})` }] };
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

    if (name === 'mail_check') {
        const agent = args.agent || DEFAULT_AGENT;
        const data = await apiCall('/mail/check', { agent });

        if (data.messages.length === 0) {
            return { content: [{ type: 'text', text: 'No new mail.' }] };
        }

        // Ack all messages
        const ids = data.messages.map(m => m.id);
        const ackData = await apiCall('/mail/ack', { ids });

        // Format messages for display
        const formatted = data.messages.map(msg =>
            `**From:** ${msg.from_agent}\n**Date:** ${msg.sent_at}\n**Subject:** ${msg.subject}\n**ID:** ${msg.id}\n\n${msg.body}`
        ).join('\n\n---\n\n');

        return { content: [{ type: 'text', text: `${data.messages.length} message(s) (acked: ${ackData.acked}):\n\n${formatted}` }] };
    }

    if (name === 'mail_ack') {
        const data = await apiCall('/mail/ack', { ids: args.ids });
        return { content: [{ type: 'text', text: `Acked ${data.acked} message(s)` }] };
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
        const statusData = await apiCall('/ingest/status', {});

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
                const data = await apiCall('/ingest', {
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

    if (name === 'presence') {
        const agent = args.agent || DEFAULT_AGENT;
        const data = await apiCall('/presence', { agent });

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

    throw new Error(`Unknown tool: ${name}`);
});

async function sendHeartbeat() {
    try {
        await apiCall('/heartbeat', { agent: DEFAULT_AGENT });
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

        const statusData = await apiCall('/ingest/status', {});
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
            await apiCall('/ingest', { namespace: file.namespace, source_file: file.fileName, content });
        }
    } catch (err) {
        // Auto-ingest failure is non-fatal
    }
}

async function main() {
    const transport = new StdioServerTransport();
    await server.connect(transport);
    await autoRegister();
    await sendHeartbeat();
    setInterval(sendHeartbeat, 120000);
    autoIngest();
}

main().catch(console.error);
