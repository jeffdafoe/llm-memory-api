// Writes goldens.json: what Node's own code answers for the inputs the Go
// port must match. Run from the repository root after `npm ci` in node/api:
//
//     node go/server/internal/oauth/testdata/goldens.js
//
// Covers isCimdClientId (the WHATWG URL parser), DCR client_id signing,
// body-parser's urlencoded parsing (qs at depth 0), Express's req.query
// (node:querystring), the connect page's continue URL (URLSearchParams),
// res.redirect's Accept negotiation, and the two HTML pages.

const path = require('path');
const fs = require('fs');

const api = path.join(__dirname, '..', '..', '..', '..', '..', 'node', 'api');
const nodeRequire = (m) => require(path.join(api, 'node_modules', m));

const config = require(path.join(api, 'src', 'services', 'config'));
config.get = (key) => (key === 'mcp_oauth_bearer_secret' ? 'golden-secret' : undefined);
const publicClients = require(path.join(api, 'src', 'services', 'oauth-public-clients'));
const pages = require(path.join(api, 'src', 'routes', 'oauth-connect-page'));
const qs = nodeRequire('qs');
const accepts = nodeRequire('accepts');
const querystring = require('querystring');

const cimdInputs = [
    'https://claude.ai/oauth/mcp-oauth-client-metadata',
    'https://claude.com/x',
    'HTTPS://CLAUDE.AI/x',
    'https://Claude.Ai/x',
    'https://claude.ai',
    'https://claude.ai/',
    'https://claude.ai/.',
    'https://claude.ai/..',
    'https://claude.ai/a/..',
    'https://claude.ai/a/../',
    'https://claude.ai/%2e',
    'https://claude.ai/%2E%2e',
    'https://claude.ai/./',
    'https://claude.ai//',
    'https://claude.ai/?x',
    'https://claude.ai?x',
    'https://claude.ai/x?y#',
    'https://claude.ai/x#',
    'https://claude.ai/x#frag',
    'https://user@claude.ai/x',
    'https://:pass@claude.ai/x',
    'https://@claude.ai/x',
    'https://:@claude.ai/x',
    'https://claude.ai@evil.example/x',
    'https://evil.example\\@claude.ai/x',
    'https://claude.ai\\x',
    'https:claude.ai/x',
    'https:/claude.ai/x',
    'https:///claude.ai/x',
    'https:\\\\claude.ai\\x',
    'https://claude.ai:443/x',
    'https://claude.ai:0443/x',
    'https://claude.ai:/x',
    'https://claude.ai:444/x',
    'https://claude.ai:65536/x',
    'https://claude.ai:4a/x',
    'https://claude.ai./x',
    'https://%43laude.ai/x',
    'https://claude%2Eai/x',
    'https://claude%2eai/x',
    'https://claude%zzai/x',
    'https://clau de.ai/x',
    'https://claude.ai.evil.example/x',
    'https://claude.ai%2F.evil/x',
    'http://claude.ai/x',
    'ftp://claude.ai/x',
    'wss://claude.ai/x',
    'claude.ai/x',
    '/x',
    '',
    ' https://claude.ai/x ',
    '\thttps://claude.ai/x',
    'https://cla\tude.ai/x',
    'https://claude.ai/x\n',
    'https://[::1]/x',
    'https://[::1/x',
    'https://127.0.0.1/x',
    'blob:https://claude.ai/x',
    'blob:https://claude.ai',
    'blob:https://claude.ai/',
    'blob:https://claude.ai/x#f',
    'blob:https://claude.ai/x#',
    'blob:https://claude.ai/x?q',
    'blob:http://claude.ai/x',
    'blob:https://u@claude.ai/x',
    'blob:https://claude.ai:444/x',
    'blob:/x',
    'blob://claude.ai/x',
    'blob:blob:https://claude.ai/x',
    'BLOB:https://claude.ai/x',
    'https://claude.ai/' + 'a'.repeat(493),
    'https://claude.ai/' + 'a'.repeat(494),
    'dcr.x.y',
    'remi',
    'https://ｃｌａｕｄｅ.ai/x',
];

const qsInputs = [
    'a=1',
    'a=1&a=2',
    'a=1&[a]=2',
    '[a]=2&a=1',
    'a=1&[a]=',
    'a=&[a]=2',
    'a=1&a=',
    'a',
    'a=',
    '=x',
    'a=b=c',
    'a]=b=c',
    'a=b]=c',
    'a%5B%5D=1',
    '%5Ba%5D=1',
    'a+b=c+d',
    'a=%41%42',
    'a=%E2%9C%93',
    'a=%E2%9C',
    'a=%zz+b',
    'a=%C0%80',
    'a=%ED%A0%80',
    'a=%F4%90%80%80',
    'grant%5Ftype=client_credentials',
    'a=1&&b=2',
    '&a=1&',
    '__proto__=x&a=1',
    'toString=1&a=2',
    'a[b]=1&a=2',
    '[]=1&a=2',
    '[0]=1&a=2',
    'a=%5Bx',
    'a=%5Bx%zz',
    'state=st%3C%22%26%3E',
];

const querystringInputs = [
    'a=1',
    'a=1&a=2&a=3',
    'a',
    'a=',
    '=b',
    '=',
    'a=b=c',
    '&&a=1&&',
    '+=+',
    'a+b=c+d',
    'a=%41',
    'a=%4',
    'a=%4+1',
    'a%4+1=x',
    'a=%E2%9C%93',
    'a=%E2%9C',
    'a=%zz',
    'a=%zz%41',
    'a=%FF%FF%41',
    'a=%E2%28%A1',
    'a%41=%42',
    'confirmed=1&confirmed=0',
    '__proto__=x',
    'state=a+b%2Bc%2Fd%3Fe%3Df%26g',
    'resource=https%3A%2F%2Fllm-memory.net%2Fmcp&resource=https%3A%2F%2Fllm-memory.net%2Fother',
];

const continueInputs = [
    'response_type=code&client_id=remi&state=a+b%2Bc',
    'confirmed=0&x=1&confirmed=2',
    'x=1',
    'x=%zz&y=%E2%9C%93&z=%E2%9C',
    'a=b=c&&d&=e',
    'sp=a%20b&star=*&tilde=~&bang=!&q=%3F',
    'state=%FF%FF',
    'state=%E2%82',
    'state=%E2%28%A1',
    'state=%C0%80%ED%A0%80%F4%90%80%80',
    'state=%F0%9F%98',
    'resource=https%3A%2F%2Fllm-memory.net%2Fmcp&resource=https%3A%2F%2Fllm-memory.net%2Fother&confirmed=0',
];

const acceptInputs = [
    undefined,
    '',
    '*/*',
    'text/html',
    'text/plain',
    'text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8',
    'application/json',
    'text/*',
    'text/html;q=0.5, text/plain;q=0.4',
    'text/plain;q=0, text/html',
    'text/html;level=1',
    'text/html;level',
    'text/html;q=abc',
    '*/*;q=0.1, text/html;q=0.1',
    'TEXT/HTML',
    'text/html ; q=0.9, text/plain',
    'text/html;foo="a,b";q=0.5, text/plain;q=0.4',
    'garbage',
    'text/html;q=1e-1, text/plain;q=.05',
];

function cimd() {
    return cimdInputs.map((input) => ({ input, cimd: publicClients.isCimdClientId(input) }));
}

function dcr() {
    const out = [];
    for (const uris of [['https://claude.ai/api/mcp/auth_callback'], ['https://claude.ai/api/mcp/auth_callback', 'https://claude.com/api/mcp/auth_callback']]) {
        const id = publicClients.issueDcrClientId(uris, 1762378800);
        out.push({ uris, issuedAt: 1762378800, clientId: id, verified: publicClients.verifyDcrClientId(id) });
    }
    return out;
}

function form() {
    return qsInputs.map((input) => {
        let count = input.split('&').length;
        const parsed = qs.parse(input, {
            allowPrototypes: true,
            arrayLimit: count,
            depth: 0,
            charset: 'utf-8',
            parameterLimit: 1000,
            strictDepth: true,
        });
        return { input, parsed: JSON.parse(JSON.stringify(parsed)) };
    });
}

function query() {
    return querystringInputs.map((input) => ({ input, parsed: JSON.parse(JSON.stringify(querystring.parse(input))) }));
}

function continueURLs() {
    return continueInputs.map((input) => {
        const u = new URL('/authorize?' + input, 'http://placeholder');
        u.searchParams.set('confirmed', '1');
        return { input, url: u.pathname + u.search };
    });
}

function negotiation() {
    return acceptInputs.map((accept) => {
        const headers = {};
        if (accept !== undefined) {
            headers.accept = accept;
        }
        const chosen = accepts({ headers }).types(['text', 'html']);
        return { accept: accept === undefined ? null : accept, chosen: chosen || '' };
    });
}

function renderedPages() {
    return {
        connect: pages.renderConnectPage({ agent: '<img src=x onerror=alert(1)>', continueUrl: '/authorize?a=1&b=%22' }),
        login: pages.renderLoginPage({
            clientHost: 'claude.ai',
            oauthFields: {
                response_type: 'code',
                client_id: 'https://claude.ai/oauth/mcp-oauth-client-metadata',
                redirect_uri: 'https://claude.ai/api/mcp/auth_callback',
                state: '"><script>alert(1)</script>',
                code_challenge: 'abc',
                code_challenge_method: ['S256', 'x'],
            },
        }),
        loginError: pages.renderLoginPage({
            clientHost: 'claude.com',
            oauthFields: { response_type: 'code', client_id: '', state: undefined },
            agentValue: "o'brien & co",
            error: 'Wrong agent name or password.',
        }),
    };
}

const out = {
    cimd: cimd(),
    dcr: dcr(),
    form: form(),
    query: query(),
    continueURLs: continueURLs(),
    negotiation: negotiation(),
    pages: renderedPages(),
};
fs.writeFileSync(path.join(__dirname, 'goldens.json'), JSON.stringify(out, null, 2) + '\n');
console.log('wrote goldens.json');
