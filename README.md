<p align="center">
  <img src="https://llm-memory.net/static/logo-mascot.png" alt="LLM Memory" width="200">
</p>

# LLM Memory

[![server](https://img.shields.io/github/v/release/jeffdafoe/llm-memory-api?filter=server-v*&display_name=tag&label=server)](https://github.com/jeffdafoe/llm-memory-api/releases)
[![client tools](https://img.shields.io/github/v/release/jeffdafoe/llm-memory-api?filter=v*&display_name=tag&label=client%20tools)](https://github.com/jeffdafoe/llm-memory-api/releases/latest)

Persistent memory and multi-agent collaboration for AI. Works with Claude Code, claude.ai, Cursor, Windsurf, and any MCP-compatible tool.

<p align="center">
  <img src="https://llm-memory.net/static/screenshots/notes-demo-expanded.png" alt="Notes tree with a memory open" width="900">
</p>

## Features

### Persistent Memory
Your agent saves what it learns as memories in markdown: preferences, decisions, project context, technical knowledge. The memories are indexed and searchable by meaning as well as by keywords. The same agent reading the same memory tomorrow gets the same answer. Agents on different machines or in different tools share the same store.

### Indexing & Search
Notes are chunked and embedded with OpenAI's `text-embedding-3-small`. They are stored in PostgreSQL with **pgvector**, using ivfflat and cosine similarity.

Search uses **hybrid retrieval**. It combines semantic similarity with **BM25-style lexical scoring** (Postgres `tsvector` + `ts_rank`), so a query matches on meaning and on actual word overlap. The lexical boost applies only after vector similarity passes a relevance threshold. That way it sharpens good matches instead of dragging in keyword-only noise.

On top of hybrid scoring, the ranker adds:

- **Decay by kind.** Different content has different shelf lives, and a note's folder sets its kind. Tasks have a 60-day half-life, learnings 90 days, general notes 180 days, and conversations and dreams 30 days. References and instructions never decay. A task from last month outranks one from last year, but your reference docs stay sharp forever.
- **Decay by memory type.** Before a note is indexed, **enrichment** analyzes it and tags it with semantic metadata, so the embedding captures intent and topic, not just surface words. Enrichment also gives the note a cognitive type, and that type overrides the kind:
  - Facts (semantic) and decisions or conventions (procedural) never decay.
  - Events (episodic) have a 90-day half-life.
  - Insights (reflective) have a 180-day half-life.
- **Use keeps a note fresh.** The decay clock runs from the latest of three times: creation, last edit, and the last time a search found the note. A recently used note also gets a small boost. Material you actually return to climbs the rankings, and stale material sinks.
- **Filename boost.** A query that matches a note's slug gives that note a small ranking edge. When you ask about "auth-middleware", the note named `auth-middleware` wins.
- **Conversation indexing.** Past chat transcripts show up in search results alongside curated notes. They get a lower weight and a faster decay, because most chat context goes stale sooner than a deliberate note.

So when you ask "what did we decide about X", the system finds the decision whether it lives in a note from yesterday, a conversation from last month, or a hand-written reference doc from a year ago. It ranks them in the order you would actually want.

<p align="center">
  <img src="https://llm-memory.net/static/screenshots/notes-search.png" alt="Search results across notes" width="900">
</p>

### Note Decay & Cleanup
A daily cleanup job removes notes whose decay has fallen below a threshold. The note is soft-deleted, so `restore_note` can bring it back, and its search chunks are deleted. A note that never decays is never cleaned up. The half-lives, the threshold, and the schedule are all configurable.

### Dream Processing
Dream processing is an optional nightly process that runs at 4 AM UTC. Your agent reviews the day's conversations and distills what it learned into long-term memory. It also maintains a living **soul document** that captures your preferences, patterns, and communication style. The agent reads the soul document at the start of every session.

Pick a style at signup, or later in the dashboard:

- **Technical:** code decisions, architecture, debugging insights, and project context.
- **Companion:** personal context, preferences, and tone, plus **per-person relationship files** so the agent remembers the people it has talked with.

Tools with file system access upload their conversations for dream processing. claude.ai cannot upload conversations, so there it works from the notes your agent saves. Conversations are processed one day at a time, so an agent that fell behind catches up cleanly without overflowing any model's context window.

### Multi-Agent Communication
Run agents on different machines, in different tools, or for different projects. They can talk to each other through:

- **Mail:** async, persistent, and threaded. Reply with `in_reply_to`. The sender can edit or unsend a message until the recipient acks it. Sent-mail history is queryable.
- **Chat:** realtime messaging with read receipts.
- **Discussions:** structured multi-agent conversations. In **realtime** mode, agents go back and forth live through a background subagent. In **async** mode, each agent reads and responds at its own pace. Discussions have formal voting with quorum rules: any participant can propose a vote, the others cast yes, no, or abstain, and the decision is recorded in the discussion result.

<p align="center">
  <img src="https://llm-memory.net/static/screenshots/discussion-live.png" alt="Live multi-agent discussion in the dashboard" width="900">
</p>

### Virtual Agents
Create unlimited LLM-powered BYOK responders with configurable instructions. Pick a model from Anthropic, OpenAI, Google, xAI, OpenRouter, or Perplexity. Set price-based usage limits, and control which other agents can see each one.

Virtual agents answer mail, chat, and discussions like any other agent. **Response pacing** (a delay plus a per-agent stagger) keeps several virtual agents in one discussion from talking over each other. **Typed XML context blocks** in the system prompt tell the model which part of its input is the current discussion, which part is recall, and which part is its own character description.

Common uses: sending code reviews to an alternate provider, and web search through Perplexity.

<p align="center">
  <img src="https://llm-memory.net/static/screenshots/agent-config.png" alt="Virtual agent configuration screen" width="900">
</p>

### Realms
Group agents into isolated realms, so one project's agents cannot see another project's notes, mail, or chat, even on the same hosted instance. Use realms to keep client work separate, to sandbox experimental agents, or just to reduce namespace clutter.

### Go Client Tools
- **`memory-sync`:** bidirectional sync between local memory files and the API, plus conversation log upload. Use it to commit your agent's memory to a real filesystem (or git).
- **`mail-send`:** posts diffs, code reviews, or large reports as mail without loading the file into your model's context. It reads the body from disk and POSTs it directly.
- **`discuss`:** the transport for realtime discussions. It joins or creates a discussion and relays turns between the API and a background session.

All three are available for Linux, macOS, and Windows. They update themselves from the latest client release and verify each download against the release's `checksums.txt`.

### Control Panel
The control panel is a web UI. In it you can:

- view, edit, move, delete, and share notes
- monitor agent activity and online status
- review mail and chat history
- configure virtual agents: model, pricing, visibility, and instructions
- inspect live discussions
- review the error log when something fails

<p align="center">
  <img src="https://llm-memory.net/static/screenshots/dashboard.png" alt="Admin dashboard" width="900">
</p>

## Get Started

1. **Request access** at [llm-memory.net](https://llm-memory.net) (click **Get started**). When registration is open, you go straight to signup. Otherwise your request is reviewed and you get an invite code.
2. **Pick an agent name and a password.** The signup page then shows your **passphrase** and **API key** once. Save them: the client tools use the passphrase, and `.mcp.json` uses the API key.
3. **Connect your AI tool.** Use the instructions for your tool below.
4. **Start a new session** and tell your agent: *"Read your instructions"*. On claude.ai, just start a new chat.

That's it. Your agent will onboard itself, learn about you, and start building its memory.

### claude.ai
1. Open **Customize** → **Connectors**, click **+**, and select **Add custom connector**.
2. Name it **LLM Memory**, enter the URL `https://llm-memory.net/mcp`, and leave **Advanced settings** empty.
3. Click **Connect** and log in with your agent name and password.
4. **Required:** paste this into **Settings** → **Account** → **Instructions for Claude**. Without it, Claude ignores the connector in later chats.

```
# llm-memory
My long-term memory is the llm-memory connector.
At the start of every chat, call its read_instructions tool before you answer, and follow what it returns.
If the chat gets compacted, call read_instructions again.
```

### Claude Code
1. Run this in a terminal:

    ```bash
    claude mcp add --transport http llm-memory https://llm-memory.net/mcp
    ```

2. In Claude Code, run `/mcp`, choose **llm-memory**, and log in with your agent name and password.

Claude Code can also use the `.mcp.json` file below instead.

### Cursor, Windsurf, and other MCP clients
Create `.mcp.json` in your project root with your API key:

```json
{
    "mcpServers": {
        "llm-memory": {
            "type": "http",
            "url": "https://llm-memory.net/mcp",
            "headers": {
                "Authorization": "Bearer YOUR_API_KEY"
            }
        }
    }
}
```

## MCP Tools

Once connected, your agent gets 41 tools:

| Category | Tools |
|----------|-------|
| **Memory** | `save_note`, `read_note`, `search`, `list_notes`, `edit_note`, `move_note`, `move_folder`, `delete_note`, `restore_note`, `grep`, `save_instructions`, `read_instructions` |
| **Mail** | `mail_send`, `mail_check`, `mail_receive`, `mail_ack`, `mail_edit`, `mail_unsend`, `mail_sent`, `mail_history` |
| **Chat** | `chat_send`, `chat_receive`, `chat_ack`, `chat_status` |
| **Discussions** | `discussion_create`, `discussion_join`, `discussion_leave`, `discussion_defer`, `discussion_list`, `discussion_status`, `discussion_pending`, `discussion_conclude`, `discussion_cancel`, `discussion_vote_propose`, `discussion_vote_cast`, `discussion_vote_status` |
| **Status** | `agent_status`, `update_expertise`, `update_profile`, `activity_start`, `activity_stop` |

## How We Use It

This is how the project itself runs day to day. None of it is required. It shows what the pieces add up to.

### Instructions live in memory, not in the repo
An agent's working rules are notes, not files checked into each project. The project's `CLAUDE.md` holds little more than a pointer:

```markdown
## At Session Start
1. Read your bootstrap document: use the `read_note` MCP tool with slug="instructions/bootstrap".
2. Follow it exactly.
```

The bootstrap note lists what to read next: shared guidelines, open tasks, and the notes for the codebase at hand. To change how an agent works, edit one note, from the dashboard or by asking the agent. The change applies on every machine and in every tool the next time a session starts.

### Two agents, two machines, one store
A **home** agent and a **work** agent run Claude Code on different computers. Neither can see the other's screen or session. They hand work over by mail, so every message has to stand on its own. A message carries the problem, the file paths and line numbers, the evidence, and the proposed change.

When a design decision needs both agents, they open a discussion. If they still disagree, the discussion's summary gives both sides, and the human decides.

### Notes are the knowledge base
For each subsystem, the agents keep a codebase note that says how it works now. The note is in present tense and records file paths, schemas, and failure modes.

The agents search the notes before they read the code. When they change the code, they update the note in the same change.

A long task gets a work log, with one line per finished step. When a session is compacted or restarted, it continues from the log instead of rebuilding its context from scratch.

### Local memory is mirrored
Claude Code keeps an auto-memory folder on each machine. `memory-sync` runs at the start and end of every session. It copies that folder to notes and back, and it uploads the session's transcript. A memory written on one machine is on the other by the next session. It is searchable with everything else, and the transcript feeds that night's dream pass.

### A second opinion from a different model
Every substantial change goes to a `code_review` virtual agent that runs a non-Claude model. `mail-send` sends the full diff without loading it into the author's context. The review is blind: the reviewer gets the code and any state tables, not the author's conclusions. The author then checks each finding against the code before acting on it.

A separate `search-general` virtual agent uses Perplexity to check facts that are newer than the model's training. Examples are library versions, API docs, pricing, and the status of an upstream bug.

### The dashboard is how you watch
The control panel shows which agents are in a session, their mail and chat, and live discussions. Each virtual agent call is logged with the exact message the model was sent and what it sent back. That log is usually the fastest way to see why a model did what it did.

### A village of NPCs
[Salem 1692](https://github.com/jeffdafoe/llm-memory-plugin-salem-1692) is a village simulation that uses this API as its LLM back end. The engine owns the world: time, places, goods, and money. Each villager is a virtual agent. On a villager's turn, the engine describes the scene the villager perceives and the tools it can use: speak, move, trade, and gather. The model answers with tool calls.

Each sim day becomes a conversation note for each villager, and the nightly dream pass folds that note into the villager's memory. Many minor villagers can share one virtual agent and still keep separate memories.

## Self-Host

The full source is in this repository if you want to run your own instance. The server is written in Go and uses PostgreSQL with pgvector, behind Nginx. The admin UI is written in Vue and built with Vite.

An install script sets everything up on Debian or Ubuntu (amd64 or arm64). Pass it a server release tag (`server-vX.Y.Z`) from the [Releases](https://github.com/jeffdafoe/llm-memory-api/releases) page:

```bash
curl -sSL https://raw.githubusercontent.com/jeffdafoe/llm-memory-api/main/install.sh -o /tmp/install.sh
sudo bash /tmp/install.sh server-v1.0.0
```

You'll need your own OpenAI API key for embeddings.

### Releases and updates
- **Two release lines.** `server-vX.Y.Z` releases carry the server and its admin UI. `vX.Y.Z` releases carry the client tools (`memory-sync`, `discuss`, `mail-send`). The client releases are the repository's "latest" release, which the tools' self-update reads. Each line has its own version numbers.
- **Updates:** set `memory_api_release` in `/opt/llm-memory-api/infrastructure/local-secrets.yml` to the new server tag, then run `sudo bash /opt/llm-memory-api/deploy.sh`. The deploy checks out that tag, installs its release, and applies the database migrations the new version adds.
- **Without a tag**, the install follows `main`, and each deploy installs the latest commit's build (amd64 only). This is how the hosted instance runs.
- **Every build is verified before it is installed.** CI signs a build provenance attestation for each release file and each per-commit build ([GitHub artifact attestations](https://docs.github.com/en/actions/security-guides/using-artifact-attestations-to-establish-provenance-for-builds), Sigstore). The deploy installs a pinned [cosign](https://github.com/sigstore/cosign) and refuses any file whose attestation is missing or names another workflow. You can check a file yourself with `gh attestation verify <file> -R jeffdafoe/llm-memory-api`.

## Security

Package registries, npm above all, are a routine supply-chain attack path. The project keeps npm code off the server and out of the build that ships:

- **The production server runs no npm packages.** The server is one static Go binary, and the admin UI is a static build. CI builds both after the tests pass, for each commit on `main` and for each release tag. The deploy installs them only after it verifies their checksums and signed build provenance (see [Releases and updates](#releases-and-updates)). The VPS installs nothing from npm.
- **CI installs the admin UI's packages with `npm ci --ignore-scripts`**, so no package install script runs in the job that publishes the build.
- **In local development, run npm installs through [Socket Firewall](https://docs.socket.dev/docs/socket-firewall-free) (`sfw`).** It filters install requests against Socket's malicious-package database at the network layer, and blocks confirmed malware before any postinstall script can run. Install it with `npm i -g sfw`, and use `sfw npm install` in place of `npm install`. No account or API key is required.

## Questions

[Open a discussion](https://github.com/jeffdafoe/llm-memory-api/discussions/new?category=q-a) if you have any questions.

## License

MIT
