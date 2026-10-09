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

// continueUrl is a same-origin /authorize URL; the caller builds it.
function renderConnectPage({ agent, continueUrl }) {
    return `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <link rel="icon" href="/static/favicon.ico" sizes="any">
    <link rel="icon" href="/static/favicon-32.png" type="image/png" sizes="32x32">
    <title>Connect — LLM Memory</title>
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
    </style>
</head>
<body>
<div class="card">
    <div class="logo">
        <img src="/static/logo-mascot.png" alt="">
        <span><span class="accent">LLM</span> Memory</span>
    </div>
    <h2>Connecting as ${escapeHtml(agent)}</h2>
    <div class="subtitle">One more step, so Claude uses your memory in every chat.</div>
    <p>claude.ai does not tell Claude to load your memory. Paste this into <strong>Settings</strong> → <strong>Account</strong> → <strong>Instructions for Claude</strong>:</p>
    <div class="config-block" id="claudeInstructionsBlock">${escapeHtml(CLAUDE_INSTRUCTIONS)}</div>
    <button class="copy-btn" type="button" id="copyButton">Copy instructions</button>
    <p>You only need to do this once. If you already did it, press Continue.</p>
    <a class="btn" href="${escapeHtml(continueUrl)}">Continue</a>
</div>
<script>
document.getElementById('copyButton').addEventListener('click', function (event) {
    navigator.clipboard.writeText(document.getElementById('claudeInstructionsBlock').textContent);
    event.target.textContent = 'Copied!';
    setTimeout(function () { event.target.textContent = 'Copy instructions'; }, 2000);
});
</script>
</body>
</html>`;
}

module.exports = { renderConnectPage, CLAUDE_INSTRUCTIONS };
