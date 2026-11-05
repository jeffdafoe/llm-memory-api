package oauth

// Generated from node/api/src/routes/oauth-connect-page.js (renderPage and
// COPY_SCRIPT); the text must stay byte-identical to Node's.

const pageHead = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <link rel="icon" href="/static/favicon.ico" sizes="any">
    <link rel="icon" href="/static/favicon-32.png" type="image/png" sizes="32x32">
    <title>`

const pageBodyOpen = `</title>
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
`

const pageBodyClose = `
</div>
`

const pageEnd = `
</body>
</html>`

const copyScript = `<script>
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
</script>`
