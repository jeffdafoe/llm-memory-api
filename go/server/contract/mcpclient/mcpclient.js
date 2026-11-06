// The MCP client side of the contract harness's MCP client steps: a real
// v2 TypeScript SDK client (what Claude Code and other SDK clients run)
// connects to one server's /mcp and runs a script, and everything the
// client hands back is printed as JSON lines, so the two servers are
// compared through the client's eyes.
//
// Run from node/api (where the SDK is installed), with the script as JSON
// on stdin:
//
//     MCP_URL=http://127.0.0.1:<port>/mcp node ../../go/server/contract/mcpclient/mcpclient.js
//
// The script: {"bearer": "<token>", "mode": "legacy" | "auto", "calls": [...]}
// where each call is {"op": "listTools"} or {"op": "callTool", "name": ...,
// "arguments": ...} or {"op": "ping"}. "legacy" is the client's default (the
// 2025 initialize handshake); "auto" probes server/discover and negotiates
// the 2026-07-28 era. A failed connect or call prints its error and the
// script goes on; the exit status is 0 unless the script itself is bad.

// Resolved from the working directory (node/api), where the SDK is
// installed; this file sits outside that package.
const { Client, StreamableHTTPClientTransport } = require(require.resolve('@modelcontextprotocol/client', { paths: [process.cwd()] }));

async function readStdin() {
    const chunks = [];
    for await (const chunk of process.stdin) {
        chunks.push(chunk);
    }
    return Buffer.concat(chunks).toString('utf8');
}

function print(entry) {
    process.stdout.write(JSON.stringify(entry) + '\n');
}

function errorOf(err) {
    const out = { error: String(err && err.message) };
    if (err && err.code !== undefined) {
        out.code = err.code;
    }
    return out;
}

async function main() {
    const script = JSON.parse(await readStdin());
    const url = process.env.MCP_URL;
    if (!url) {
        throw new Error('MCP_URL is not set');
    }

    const options = {};
    if (script.mode && script.mode !== 'legacy') {
        options.versionNegotiation = { mode: script.mode };
    }
    const client = new Client({ name: 'contract-harness', version: '1.0.0' }, options);
    const requestInit = {};
    if (script.bearer) {
        requestInit.headers = { authorization: 'Bearer ' + script.bearer };
    }
    const transport = new StreamableHTTPClientTransport(new URL(url), { requestInit });

    try {
        await client.connect(transport);
        print({
            op: 'connect',
            protocolVersion: client.getNegotiatedProtocolVersion(),
            serverVersion: client.getServerVersion() || null,
            capabilities: client.getServerCapabilities() || null,
            instructions: client.getInstructions() || null,
        });
    } catch (err) {
        print({ op: 'connect', ...errorOf(err) });
        return;
    }

    for (const call of script.calls || []) {
        try {
            let result;
            if (call.op === 'listTools') {
                result = await client.listTools();
            } else if (call.op === 'callTool') {
                result = await client.callTool({ name: call.name, arguments: call.arguments });
            } else if (call.op === 'ping') {
                result = await client.ping();
            } else {
                throw new Error('unknown op ' + call.op);
            }
            print({ op: call.op, name: call.name, result });
        } catch (err) {
            print({ op: call.op, name: call.name, ...errorOf(err) });
        }
    }
    await client.close();
}

// The process ends on its own once the client has closed: process.exit with
// a fetch socket still closing trips a libuv assertion on Windows.
main().catch((err) => {
    process.stderr.write(String(err && err.stack) + '\n');
    process.exitCode = 1;
});
