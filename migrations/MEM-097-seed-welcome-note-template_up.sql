-- MEM-097: Seed a default welcome-note template
-- Creates a getting-started note saved to new agents' namespaces

INSERT INTO templates (name, kind, description, content) VALUES (
    'default',
    'welcome-note',
    'Getting started guide saved as a note in the agent''s namespace',
    '---
title: Getting Started
slug: instructions/getting-started
---

# Getting Started

This note is a quick reference for working with your LLM Memory agent. Your agent can read it anytime, and so can you from the dashboard.

## Your credentials

Keep these safe. You''ll need them to connect from different tools.

- **Agent name:** {agent}
- **Passphrase:** {passphrase}
- **API Key:** {api_key}

The **passphrase** is used by CLI tools (Claude Code, Cursor, Windsurf) for file sync and agent authentication. The **API key** is used by web-based tools (claude.ai) for MCP integration.

## Connecting your agent

### Claude Code / Cursor / Windsurf (CLI tools)

1. Create a file called `.agent.json` in your project root:

```json
{
    "agent": "{agent}",
    "passphrase": "{passphrase}",
    "api_url": "https://llm-memory.net/v1"
}
```

2. Add llm-memory as an MCP server in your tool''s configuration. For Claude Code, add to `.mcp.json`:

```json
{
    "mcpServers": {
        "llm-memory": {
            "type": "http",
            "url": "https://llm-memory.net/mcp",
            "headers": {
                "Authorization": "Bearer {api_key}"
            }
        }
    }
}
```

3. Tell your agent: **"read your instructions"**

### claude.ai (web)

1. Go to Settings > Integrations
2. Add LLM Memory as an integration using your API key: `{api_key}`
3. Start a new conversation and say: **"read your instructions using the llm-memory tools"**

## Day-to-day usage

### Starting a session

Say **"check your instructions"** or **"read your instructions"** at the beginning of each conversation. This loads your agent''s memory and catches it up on where you left off.

### Teaching your agent

Your agent remembers what you tell it to remember. Be direct:

- "Remember that I prefer dark mode in all my projects"
- "Note that the deploy process changed — we use GitHub Actions now"
- "Keep in mind that I''m on PTO next week"

You don''t need special commands. Just tell it naturally and it will save the information to its notes.

### During sessions

When something noteworthy comes up — a decision, a preference, a correction — tell your agent to save it. Don''t wait until the end. If the conversation gets long, earlier details can be lost from context, but saved notes persist forever.

If your agent does something you don''t like, say so. "Don''t do that" or "I prefer it this way" — it will learn and remember the feedback.

### Ending a session

Say **"update your notes"** before you wrap up. This tells your agent to save its current state so the next session picks up where you left off.

## Dream processing

If you enabled dream processing during signup, your agent reviews each day''s conversations overnight and consolidates what it learned into memory. Over time this builds a picture of how you work together — your preferences, patterns, and communication style.

**Note:** Dream processing requires conversation logs, which are uploaded by the memory-sync tool. This works with tools that have file system access — Claude Code, Claude Desktop, Cursor, Windsurf, and similar. If you''re using claude.ai (website) or the Claude mobile app, there''s no way to capture conversations, so dream processing won''t have anything to work with.

You can change your dream mode anytime in the dashboard under your agent''s settings.

## The dashboard

Visit **llm-memory.net** and log in with your agent name and password to:

- **Memories** — browse and edit your agent''s notes
- **Communications** — see mail and chat messages
- **Agents** — manage settings, provider, and dream mode

## Tips

- **Be explicit about preferences early.** The more your agent knows about how you work, the better it gets.
- **Correct mistakes in the moment.** Your agent learns from corrections and remembers them.
- **Check the dashboard occasionally.** See what your agent has saved — you might be surprised, or notice something that needs correcting.
- **Start each session the same way.** "Check your instructions" is the simplest habit that makes everything else work.'
);
