package oauth

import "strings"

// The pages claude.ai users see while adding the llm-memory connector (Node's
// routes/oauth-connect-page.js). claude.ai drops the MCP server's
// instructions field, so /authorize is the one moment to show the user the
// line that makes Claude call read_instructions.

// claudeInstructions is the same text as CLAUDE_INSTRUCTIONS in
// public/landing/register.html and the welcome template.
const claudeInstructions = "# llm-memory\n" +
	"My long-term memory is the llm-memory connector.\n" +
	"At the start of every chat, call its read_instructions tool before you answer, and follow what it returns.\n" +
	"If the chat gets compacted, call read_instructions again."

func instructionsBlock() string {
	return `<div class="config-block" id="claudeInstructionsBlock">` + escapeHTML(claudeInstructions) + `</div>
    <button class="copy-btn" type="button" id="copyButton">Copy instructions</button>`
}

// renderPage is the shared document shell with the copy button's script;
// body is trusted HTML whose values the callers escaped.
func renderPage(title, body string) string {
	return renderShell(title, body, copyScript)
}

// renderShell is the document shell with the given script, which may be
// empty for a page without the copy button.
func renderShell(title, body, script string) string {
	return pageHead + escapeHTML(title) + pageBodyOpen + body + pageBodyClose + script + pageEnd
}

// renderConnectPage is the page for an agent-name client_id. continueURL is
// a same-origin /authorize URL.
func renderConnectPage(agent, continueURL string) string {
	return renderPage("Connect — LLM Memory", `    <h2>Connecting as `+escapeHTML(agent)+`</h2>
    <div class="subtitle">One more step, so Claude uses your memory in every chat.</div>
    <p>claude.ai does not tell Claude to load your memory. Paste this into <strong>Settings</strong> → <strong>Account</strong> → <strong>Instructions for Claude</strong>:</p>
    `+instructionsBlock()+`
    <p>You only need to do this once. If you already did it, press Continue.</p>
    <a class="btn" href="`+escapeHTML(continueURL)+`">Continue</a>`)
}

// oauthField is one authorize parameter the login form carries through.
type oauthField struct {
	name  string
	value any
}

// loginPage is the login page for a public client's redirect_uri. A loopback
// redirect_uri belongs to an app on this computer, such as Claude Code, which
// reads the server's instructions itself, so its page has no paste step.
func loginPage(redirectURI string, fields []oauthField, agentValue, errText string) string {
	if _, ok := parseLoopback(redirectURI); ok {
		return renderLocalLoginPage(fields, agentValue, errText)
	}
	return renderLoginPage(redirectHost(redirectURI), fields, agentValue, errText)
}

// renderLoginPage is the login for a claude.ai public client.
func renderLoginPage(clientHost string, fields []oauthField, agentValue, errText string) string {
	return renderPage("Log in — LLM Memory", `    <h2>Connect `+escapeHTML(clientHost)+`</h2>
    <div class="subtitle">to your LLM Memory agent</div>
    <p><strong>First, so Claude uses your memory in every chat:</strong> claude.ai does not tell Claude to load your memory. Paste this into <strong>Settings</strong> → <strong>Account</strong> → <strong>Instructions for Claude</strong>. You only need to do this once.</p>
    `+instructionsBlock()+`
    <div class="divider"></div>
    <p><strong>Then log in</strong> with your agent name and the password you chose at signup.</p>
`+loginForm(fields, agentValue, errText))
}

// renderLocalLoginPage is the login for a client on a loopback redirect_uri.
func renderLocalLoginPage(fields []oauthField, agentValue, errText string) string {
	return renderShell("Log in — LLM Memory", `    <h2>Connect an app on this computer</h2>
    <div class="subtitle">to your LLM Memory agent</div>
    <p>Log in only if you just added LLM Memory to an app on this computer, such as Claude Code. Use your agent name and the password you chose at signup.</p>
`+loginForm(fields, agentValue, errText), "")
}

// loginForm is the login form. Only string, non-empty fields become hidden
// inputs. agentValue and errText refill the form after a failed try; the
// password is never echoed.
func loginForm(fields []oauthField, agentValue, errText string) string {
	var hidden []string
	for _, f := range fields {
		s, ok := f.value.(string)
		if !ok || s == "" {
			continue
		}
		hidden = append(hidden, `        <input type="hidden" name="`+escapeHTML(f.name)+`" value="`+escapeHTML(s)+`">`)
	}
	errorHTML := ""
	if errText != "" {
		errorHTML = "\n        <div class=\"error\" role=\"alert\">" + escapeHTML(errText) + "</div>"
	}
	return `    <form method="post" action="/authorize">
` + strings.Join(hidden, "\n") + `
        <label for="agent">Agent name</label>
        <input type="text" id="agent" name="agent" value="` + escapeHTML(agentValue) + `" autocomplete="username" autocapitalize="none" spellcheck="false" required>
        <label for="password">Password</label>
        <input type="password" id="password" name="password" autocomplete="current-password" required>` + errorHTML + `
        <button class="btn" type="submit">Log in and connect</button>
    </form>`
}
