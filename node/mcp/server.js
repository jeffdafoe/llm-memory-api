const { Server } = require('@modelcontextprotocol/sdk/server/index.js');
const { StdioServerTransport } = require('@modelcontextprotocol/sdk/server/stdio.js');
const { CallToolRequestSchema, ListToolsRequestSchema } = require('@modelcontextprotocol/sdk/types.js');
const { readFileSync } = require('fs');

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

    throw new Error(`Unknown tool: ${name}`);
});

async function main() {
    const transport = new StdioServerTransport();
    await server.connect(transport);
}

main().catch(console.error);
