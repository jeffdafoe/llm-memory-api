// The page claude.ai users see while adding the llm-memory connector.
//
// claude.ai drops the MCP server's `instructions` field
// (anthropics/claude-ai-mcp#93), so nothing tells Claude to call
// read_instructions unless the user puts a line in their own claude.ai
// settings. Accounts that skipped that step connect, list the tools, and
// never call one. /authorize is the one moment we have the user's
// attention, so it shows the block here before handing back to claude.ai.

// Same text as CLAUDE_INSTRUCTIONS in public/landing/register.html and the
// welcome template (templates table). Keep the copies in step.
const CLAUDE_INSTRUCTIONS = [
    '# llm-memory',
    'My long-term memory is the llm-memory connector.',
    'At the start of every chat, call its read_instructions tool before you answer, and follow what it returns.',
    'If the chat gets compacted, call read_instructions again.'
].join('\n');

function escapeHtml(value) {
    return String(value)
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;')
        .replace(/'/g, '&#39;');
}

// The load-memory block with its Copy button, and the script that drives it.
// Both pages show it: it is the step claude.ai users most often skip.
function instructionsBlock() {
    return `<div class="config-block" id="claudeInstructionsBlock">${escapeHtml(CLAUDE_INSTRUCTIONS)}</div>
    <button class="copy-btn" type="button" id="copyButton">Copy instructions</button>`;
}

const COPY_SCRIPT = `<script>
document.getElementById('copyButton').addEventListener('click', function (event) {
    var button = event.currentTarget;
    var text = document.getElementById('claudeInstructionsBlock').textContent;
    var failed = function () { button.textContent = 'Copy failed — select the text and copy it'; };
    if (!navigator.clipboard) {
        failed();
        return;
    }
    navigator.clipboard.writeText(text).then(function () {
        button.textContent = 'Copied!';
        setTimeout(function () { button.textContent = 'Copy instructions'; }, 2000);
    }, failed);
});
</script>`;

// Shared document shell and styles for both pages. `body` is trusted HTML
// built by the callers below, which escape every value they put in it.
function renderPage({ title, body }) {
    return `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <link rel="icon" href="/static/favicon.ico" sizes="any">
    <link rel="icon" href="/static/favicon-32.png" type="image/png" sizes="32x32">
    <title>${escapeHtml(title)}</title>
    <link rel="preconnect" href="https://fonts.googleapis.com">
    <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
    <link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600&display=swap" rel="stylesheet">
    <style>
        *, *::before, *::after { margin: 0; padding: 0; box-sizing: border-box; }
        :root {
            --bg-primary: #0A0A0B;
            --bg-secondary: #141416;
            --text-primary: #EDEDEF;
            --text-secondary: #8B8B8D;
            --accent: #E5A652;
            --border: rgba(255, 255, 255, 0.08);
        }
        html { font-size: 16px; -webkit-font-smoothing: antialiased; }
        body {
            font-family: 'Inter', sans-serif;
            background: var(--bg-primary);
            color: var(--text-primary);
            min-height: 100vh;
            display: flex;
            align-items: center;
            justify-content: center;
            padding: 16px;
        }
        .card {
            background: var(--bg-secondary);
            border: 1px solid var(--border);
            border-radius: 12px;
            padding: 32px;
            max-width: 460px;
            width: 100%;
        }
        .logo {
            display: flex;
            align-items: center;
            justify-content: center;
            gap: 12px;
            margin-bottom: 28px;
        }
        .logo img { width: 48px; height: 48px; }
        .logo span { font-size: 20px; font-weight: 600; letter-spacing: -0.02em; }
        .logo .accent { color: var(--accent); }
        h2 { font-size: 20px; font-weight: 500; margin-bottom: 8px; text-align: center; }
        .subtitle { font-size: 14px; color: var(--text-secondary); text-align: center; margin-bottom: 24px; }
        p { font-size: 14px; line-height: 1.5; color: var(--text-secondary); }
        p strong { color: var(--text-primary); font-weight: 500; }
        .config-block {
            background: var(--bg-primary);
            border: 1px solid var(--border);
            border-radius: 8px;
            padding: 12px 16px;
            font-family: 'JetBrains Mono', monospace;
            font-size: 12px;
            color: var(--text-secondary);
            margin: 12px 0;
            white-space: pre-wrap;
            word-wrap: break-word;
        }
        .copy-btn {
            display: inline-block;
            padding: 6px 14px;
            font-size: 13px;
            font-weight: 500;
            background: transparent;
            color: var(--text-secondary);
            border: 1px solid var(--border);
            border-radius: 6px;
            cursor: pointer;
            font-family: 'Inter', sans-serif;
            transition: color 0.2s;
            margin-bottom: 20px;
        }
        .copy-btn:hover { color: var(--text-primary); }
        .btn {
            display: block;
            width: 100%;
            padding: 12px;
            border-radius: 8px;
            font-size: 15px;
            font-weight: 500;
            text-align: center;
            text-decoration: none;
            background: var(--accent);
            color: var(--bg-primary);
            margin-top: 16px;
            transition: opacity 0.2s;
        }
        .btn:hover { opacity: 0.9; }
        button.btn { border: none; cursor: pointer; font-family: 'Inter', sans-serif; }
        label { display: block; font-size: 13px; color: var(--text-secondary); margin: 14px 0 6px; }
        input[type="text"], input[type="password"] {
            width: 100%;
            padding: 10px 12px;
            font-size: 15px;
            font-family: 'Inter', sans-serif;
            background: var(--bg-primary);
            color: var(--text-primary);
            border: 1px solid var(--border);
            border-radius: 8px;
        }
        input:focus { outline: 1px solid var(--accent); }
        .error { color: #E5484D; font-size: 14px; margin-top: 12px; }
        .divider { border-top: 1px solid var(--border); margin: 24px 0 20px; }
    </style>
</head>
<body>
<div class="card">
    <div class="logo">
        <img src="/static/logo-mascot.png" alt="">
        <span><span class="accent">LLM</span> Memory</span>
    </div>
${body}
</div>
${COPY_SCRIPT}
</body>
</html>`;
}

// The older Client ID / Client Secret path: the agent is already named by the
// connector settings. continueUrl is a same-origin /authorize URL; the caller
// builds it.
function renderConnectPage({ agent, continueUrl }) {
    return renderPage({
        title: 'Connect — LLM Memory',
        body: `    <h2>Connecting as ${escapeHtml(agent)}</h2>
    <div class="subtitle">One more step, so Claude uses your memory in every chat.</div>
    <p>claude.ai does not tell Claude to load your memory. Paste this into <strong>Settings</strong> → <strong>Account</strong> → <strong>Instructions for Claude</strong>:</p>
    ${instructionsBlock()}
    <p>You only need to do this once. If you already did it, press Continue.</p>
    <a class="btn" href="${escapeHtml(continueUrl)}">Continue</a>`
    });
}

// The login for a connector added with only the URL (LLM-733). The form posts
// back to /authorize with the OAuth request in hidden fields, so the password
// travels in a POST body and never in a URL or the request log.
//
// clientHost: the host the connection goes back to (the redirect_uri's), shown
// so the user can see who is asking. oauthFields: the authorize parameters to
// carry through. agentValue / error: refill and message after a failed try.
function renderLoginPage({ clientHost, oauthFields, agentValue = '', error = '' }) {
    const hidden = Object.entries(oauthFields)
        .filter(([, value]) => typeof value === 'string' && value !== '')
        .map(([name, value]) => `        <input type="hidden" name="${escapeHtml(name)}" value="${escapeHtml(value)}">`)
        .join('\n');
    let errorHtml = '';
    if (error) {
        errorHtml = `\n        <div class="error" role="alert">${escapeHtml(error)}</div>`;
    }
    return renderPage({
        title: 'Log in — LLM Memory',
        body: `    <h2>Connect ${escapeHtml(clientHost)}</h2>
    <div class="subtitle">to your LLM Memory agent</div>
    <p><strong>First, so Claude uses your memory in every chat:</strong> claude.ai does not tell Claude to load your memory. Paste this into <strong>Settings</strong> → <strong>Account</strong> → <strong>Instructions for Claude</strong>. You only need to do this once.</p>
    ${instructionsBlock()}
    <div class="divider"></div>
    <p><strong>Then log in</strong> with your agent name and the password you chose at signup.</p>
    <form method="post" action="/authorize">
${hidden}
        <label for="agent">Agent name</label>
        <input type="text" id="agent" name="agent" value="${escapeHtml(agentValue)}" autocomplete="username" autocapitalize="none" spellcheck="false" required>
        <label for="password">Password</label>
        <input type="password" id="password" name="password" autocomplete="current-password" required>${errorHtml}
        <button class="btn" type="submit">Log in and connect</button>
    </form>`
    });
}

module.exports = { renderConnectPage, renderLoginPage, CLAUDE_INSTRUCTIONS };
