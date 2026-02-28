// MCP Streamable HTTP endpoint.
// Exposes memory tools (search, save_note, list_notes, read_note, delete_note)
// over the MCP protocol via HTTP. Auth is via JWT bearer tokens issued by /oauth/token.

const { Router } = require('express');
const { Server } = require('@modelcontextprotocol/sdk/server/index.js');
const { StreamableHTTPServerTransport } = require('@modelcontextprotocol/sdk/server/streamableHttp.js');
const { CallToolRequestSchema, ListToolsRequestSchema } = require('@modelcontextprotocol/sdk/types.js');
const mcpAuth = require('../middleware/mcp-auth');
const { searchMemory } = require('../services/memory');
const { saveNote, listNotes, readNote, deleteNote } = require('../services/documents');

const router = Router();

// Permission check — does the JWT include the required permission?
function hasPermission(req, permission) {
    return req.mcpPermissions.includes(permission);
}

// Tool definitions for the remote MCP server
const TOOLS = [
    {
        name: 'search',
        description: 'Search memory for relevant notes using semantic similarity',
        inputSchema: {
            type: 'object',
            properties: {
                query: { type: 'string', description: 'Search query' },
                namespace: { type: 'string', description: 'Namespace to search (default: agent namespace)' },
                limit: { type: 'number', description: 'Max results (default: 5)' }
            },
            required: ['query']
        }
    },
    {
        name: 'save_note',
        description: 'Save a note to memory. Creates or updates based on slug.',
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
                offset: { type: 'number', description: 'Pagination offset (default: 0)' }
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
        description: 'Delete a note by slug',
        inputSchema: {
            type: 'object',
            properties: {
                namespace: { type: 'string', description: 'Namespace (default: agent namespace)' },
                slug: { type: 'string', description: 'Note slug' }
            },
            required: ['slug']
        }
    }
];

// Permission required for each tool
const TOOL_PERMISSIONS = {
    search: 'mcp_search',
    save_note: 'mcp_save_note',
    list_notes: 'mcp_list_notes',
    read_note: 'mcp_read_note',
    delete_note: 'mcp_delete_note'
};

// Create a fresh MCP server + transport pair for each request (stateless mode).
// The JWT identifies the agent — no MCP-level session needed.
function createMcpServer(req) {
    const server = new Server(
        { name: 'llm-memory', version: '2.0.0' },
        { capabilities: { tools: {} } }
    );

    const agent = req.mcpAgent;
    const defaultNamespace = agent;

    server.setRequestHandler(ListToolsRequestSchema, async () => {
        // Only list tools the agent has permission to use
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

        const namespace = args.namespace || defaultNamespace;

        try {
            let result;

            if (name === 'search') {
                const data = await searchMemory(args.query, namespace, args.limit || 5);
                const lines = data.results.map(r => {
                    return `[${r.namespace}] ${r.source_file} — ${r.heading} (${(r.similarity * 100).toFixed(1)}%)\n${r.chunk_text}`;
                });
                result = lines.join('\n\n') || 'No results found.';
            } else if (name === 'save_note') {
                const doc = await saveNote(namespace, args.title, args.content, args.slug, agent);
                result = `Saved: ${doc.namespace}/${doc.slug}`;
            } else if (name === 'list_notes') {
                const data = await listNotes(namespace, args.limit, args.offset);
                if (data.notes.length === 0) {
                    result = 'No notes found.';
                } else {
                    result = data.notes.map(n => `${n.slug} — ${n.title} (updated ${n.updated_at})`).join('\n');
                }
            } else if (name === 'read_note') {
                const doc = await readNote(namespace, args.slug);
                result = `# ${doc.title}\n\n${doc.content}`;
            } else if (name === 'delete_note') {
                await deleteNote(namespace, args.slug);
                result = `Deleted: ${namespace}/${args.slug}`;
            }

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

// Handle POST /mcp (new request or existing session message)
router.post('/mcp', mcpAuth, async (req, res) => {
    const sessionId = req.headers['mcp-session-id'];

    if (sessionId && sessions.has(sessionId)) {
        // Existing session — forward to its transport
        const session = sessions.get(sessionId);
        await session.transport.handleRequest(req, res, req.body);
        return;
    }

    // New session — create server + transport
    const transport = new StreamableHTTPServerTransport({
        sessionIdGenerator: () => require('crypto').randomUUID()
    });

    const server = createMcpServer(req);

    await server.connect(transport);

    // handleRequest processes the initialize and generates the session ID
    await transport.handleRequest(req, res, req.body);

    // Store session so subsequent requests can find it
    const newSessionId = transport.sessionId;
    if (newSessionId) {
        sessions.set(newSessionId, { server, transport });

        transport.onclose = () => {
            sessions.delete(newSessionId);
        };
    }
});

// Handle GET /mcp (SSE stream for server-initiated messages)
router.get('/mcp', mcpAuth, async (req, res) => {
    const sessionId = req.headers['mcp-session-id'];

    if (!sessionId || !sessions.has(sessionId)) {
        return res.status(400).json({
            error: 'invalid_session',
            error_description: 'Missing or invalid session ID'
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

    res.status(200).end();
});

module.exports = router;
