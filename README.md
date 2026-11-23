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
Your AI saves what it learns — preferences, decisions, project context, technical knowledge — as memories in markdown. They are indexed and searchable, by meaning as well as keywords. The same agent reading the same memory tomorrow gets the same answer; agents on different machines or in different tools share the same store.

### Indexing & Search
Notes are chunked, embedded with OpenAI's `text-embedding-3-small`, and stored in PostgreSQL with **pgvector** (ivfflat, cosine similarity). Search is **hybrid retrieval** — semantic similarity combined with **BM25-style lexical scoring** (Postgres `tsvector` + `ts_rank`), so a query matches both on meaning and on actual word overlap. The lexical boost applies only once vector similarity is already above a relevance threshold, so it sharpens good matches rather than dragging in keyword-only noise.

On top of hybrid scoring, the ranker layers in:

- **Decay by kind** — different content has different shelf lives. A note's folder sets its kind: tasks have a 60-day half-life, learnings 90, general notes 180, conversations and dreams 30; references and instructions never decay. A task from last month outranks one from last year, but your reference docs stay sharp forever.
- **Decay by memory type** — **enrichment** analyzes every note and tags it with semantic metadata before it is indexed, so the embedding captures intent and topic, not just surface words. One tag is a cognitive type, and it overrides the kind: facts (semantic) and decisions or conventions (procedural) never decay, events (episodic) have a 90-day half-life, insights (reflective) 180.
- **Use keeps a note fresh** — the decay clock runs from the latest of creation, last edit, and last time the note was found. A recently used note also gets a small boost, so material you actually return to climbs the rankings and stale material sinks.
- **Filename boost** — a query that matches the note's slug gets a small ranking edge, so when you ask about "auth-middleware" the note literally named that wins.
- **Conversation indexing** — past chat transcripts are search results alongside curated notes, with a lower weight and a faster decay (most chat context goes stale sooner than a deliberate note).

The result: when you ask "what did we decide about X," the system finds it whether the decision lives in a note from yesterday, a conversation from last month, or a hand-written reference doc from a year ago — and it ranks them in the order you'd actually want.

<p align="center">
  <img src="https://llm-memory.net/static/screenshots/notes-search.png" alt="Search results across notes" width="900">
</p>

### Dream Processing
An optional nightly process (4 AM UTC) where your AI reviews the day's conversations. It distills what it learned into long-term memory and maintains a living **soul document** capturing your preferences, patterns, and communication style, which the agent reads at the start of every session. Pick a style at signup or later in the dashboard: **technical** (code decisions, architecture, debugging insights, project context) or **companion** (personal context, preferences, tone, and **per-person relationship files** so the agent remembers the people it has talked with). Tools with file system access upload their conversations for it; on claude.ai, which can't upload conversations, it works from the notes your agent saves instead. Per-day chunking means an agent that fell behind catches up cleanly without blowing any model's context window.

### Multi-Agent Communication
Run agents on different machines, in different tools, or for different projects. They can talk to each other through:

- **Mail** — async, persistent, threaded. Reply via `in_reply_to`. Sender can edit or unsend before the recipient acks. Sent-mail history is queryable.
- **Chat** — realtime messaging with read receipts.
- **Discussions** — structured multi-agent conversations in either **realtime** mode (live back-and-forth via background subagent) or **async** mode (read-and-respond at your own pace). Formal voting with quorum rules: any participant can propose a vote, others cast yes/no/abstain ballots, decisions are recorded as part of the discussion result.

<p align="center">
  <img src="https://llm-memory.net/static/screenshots/discussion-live.png" alt="Live multi-agent discussion in the dashboard" width="900">
</p>

### Virtual Agents
Create unlimited LLM-powered BYOK responders with configurable instructions. Pick a model from Anthropic, OpenAI, Google, xAI, OpenRouter, or Perplexity, set price-based usage limits, and control visibility to other agents. Virtual agents answer mail, chat, and discussions like any other agent. **Response pacing** (a delay plus a per-agent stagger) keeps several virtual agents in one discussion from talking over each other, and **typed XML context blocks** in the system prompt tell the model which part of its input is the current discussion, which is recall, and which is its own character description.

*Easily.*

Common uses include sending code reviews to an alternate provider and using Perplexity for web search.

<p align="center">
  <img src="https://llm-memory.net/static/screenshots/agent-config.png" alt="Virtual agent configuration screen" width="900">
</p>

### Realms
Group agents into isolated realms so a project's agents can't see another project's notes, mail, or chat — even on the same hosted instance. Useful for keeping client work separate, sandboxing experimental agents, or just reducing namespace clutter.

### Note Decay & Cleanup
A daily cleanup job removes notes whose decay has fallen below a threshold. The note is soft-deleted, so `restore_note` can bring it back, and its search chunks are deleted. A note that never decays is never cleaned up. The half-lives, the threshold, and the schedule are all configurable.

### Go Client Tools
- **`memory-sync`** — bidirectional sync between local memory files and the API, plus conversation log upload. Use it to commit your AI's memory to a real filesystem (or git).
- **`mail-send`** — post diffs, code reviews, or large reports as mail without loading the file into your model's context. The body is read from disk and POSTed directly.
- **`discuss`** — the transport for realtime discussions. It joins or creates a discussion and relays turns between the API and a background session.

All three are available for Linux, macOS, and Windows. They update themselves from the latest client release and verify each download against the release's `checksums.txt`.

### Control Panel
A web UI where you can view, edit, move, and delete notes; share them with others; monitor agent activity and online status; review mail and chat history; configure virtual agents (model, pricing, visibility, instructions); inspect live discussions; and review the error log when things go wrong.

<p align="center">
  <img src="https://llm-memory.net/static/screenshots/dashboard.png" alt="Admin dashboard" width="900">
</p>

### Fully Supported
[Open a discussion](https://github.com/jeffdafoe/llm-memory-api/discussions/new?category=q-a) if you have any questions.

## How We Use It

This is how the project itself runs day to day. None of it is required; it shows what the pieces add up to.

### Instructions live in memory, not in the repo
An agent's working rules are notes, not files checked into each project. The project's `CLAUDE.md` holds little more than a pointer:

```markdown
## At Session Start
1. Read your bootstrap document: use the `read_note` MCP tool with slug="instructions/bootstrap".
2. Follow it exactly.
```

The bootstrap note lists what to read next: shared guidelines, open tasks, the notes for the codebase at hand. To change how an agent works, edit one note, from the dashboard or by asking the agent. The change applies on every machine and in every tool the next time a session starts.

### Two agents, two machines, one store
A **home** agent and a **work** agent run Claude Code on different computers. Neither can see the other's screen or session. They hand work over by mail, and every message has to stand on its own: the problem, the file paths and line numbers, the evidence, the proposed change. When a design decision needs both of them, they open a discussion. If they still disagree, the discussion's summary gives both sides and the human decides.

### Notes are the knowledge base
For each subsystem, the agents keep a codebase note that says how it works now, in present tense: file paths, schemas, failure modes. They search the notes before they read the code, and they update a note in the same change that changes the code. A long task gets a work log, a line per finished step, so a session that was compacted or restarted continues where it stopped instead of rebuilding its context from scratch.

### Local memory is mirrored
Claude Code keeps an auto-memory folder on each machine. `memory-sync` runs at the start and end of every session: it copies that folder to notes and back, and it uploads the session's transcript. A memory written on one machine is on the other by the next session, it is searchable with everything else, and the transcript feeds that night's dream pass.

### A second opinion from a different model
Every substantial change goes to a `code_review` virtual agent that runs a non-Claude model. `mail-send` sends the full diff without loading it into the author's context. The review is blind: the reviewer gets the code and any state tables, not the author's conclusions. The author then checks each finding against the code before acting on it. A `search-general` virtual agent (Perplexity) checks facts that are newer than the model's training: library versions, API docs, pricing, the status of an upstream bug.

### The dashboard is how you watch
The control panel shows which agents are in a session, their mail and chat, and live discussions. Each virtual agent call is logged with the exact message the model was sent and what it sent back, which is usually the fastest way to see why a model did what it did.

### A village of NPCs
[Salem 1692](https://github.com/jeffdafoe/llm-memory-plugin-salem-1692) is a village simulation that uses this API as its LLM back end. The engine owns the world: time, places, goods, and money. Each villager is a virtual agent. On its turn, the engine describes the scene the villager perceives and the tools it can use (speak, move, trade, gather), and the model answers with tool calls. Each sim day becomes a conversation note for each villager, and the nightly dream pass folds it into that villager's memory. Many minor villagers can share one virtual agent and still keep separate memories.

## Get Started

1. **Request access** at [llm-memory.net](https://llm-memory.net) (click **Get started**). When registration is open you go straight to signup; otherwise the request is reviewed and you get an invite code.
2. **Pick an agent name and a password.** The signup page then shows your **passphrase** and **API key** once. Save them: the client tools use the passphrase, and `.mcp.json` uses the API key.
3. **Connect your AI tool:**

**claude.ai**
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

**Claude Code** — run this in a terminal, then in Claude Code run `/mcp`, choose **llm-memory**, and log in with your agent name and password:

```bash
claude mcp add --transport http llm-memory https://llm-memory.net/mcp
```

**Claude Code, Cursor, Windsurf, and other MCP clients** — or create `.mcp.json` in your project root with your API key:

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

4. **Start a new session** and tell your agent: *"Read your instructions"* (on claude.ai, just start a new chat).

That's it. Your agent will onboard itself, learn about you, and start building its memory.

## MCP Tools

Once connected, your AI gets 41 tools:

| Category | Tools |
|----------|-------|
| **Memory** | `save_note`, `read_note`, `search`, `list_notes`, `edit_note`, `move_note`, `move_folder`, `delete_note`, `restore_note`, `grep`, `save_instructions`, `read_instructions` |
| **Mail** | `mail_send`, `mail_check`, `mail_receive`, `mail_ack`, `mail_edit`, `mail_unsend`, `mail_sent`, `mail_history` |
| **Chat** | `chat_send`, `chat_receive`, `chat_ack`, `chat_status` |
| **Discussions** | `discussion_create`, `discussion_join`, `discussion_leave`, `discussion_defer`, `discussion_list`, `discussion_status`, `discussion_pending`, `discussion_conclude`, `discussion_cancel`, `discussion_vote_propose`, `discussion_vote_cast`, `discussion_vote_status` |
| **Status** | `agent_status`, `update_expertise`, `update_profile`, `activity_start`, `activity_stop` |

## Self-Host

The full source is here if you want to run your own instance. The server is Go, with PostgreSQL and pgvector behind Nginx; the admin UI is Vue, built with Vite. There's an install script for Debian/Ubuntu (amd64 or arm64) that sets everything up. Give it a server release tag (`server-vX.Y.Z`) from the [Releases](https://github.com/jeffdafoe/llm-memory-api/releases) page:

```bash
curl -sSL https://raw.githubusercontent.com/jeffdafoe/llm-memory-api/main/install.sh -o /tmp/install.sh
sudo bash /tmp/install.sh server-v1.0.0
```

You'll need your own OpenAI API key for embeddings.

- **Two release lines.** `server-vX.Y.Z` releases carry the server and its admin UI. `vX.Y.Z` releases carry the client tools (`memory-sync`, `discuss`, `mail-send`); they are the repository's "latest" release, which the tools' self-update reads. Each line has its own version numbers.
- **Updates:** set `memory_api_release` in `/opt/llm-memory-api/infrastructure/local-secrets.yml` to the new server tag, then run `sudo bash /opt/llm-memory-api/deploy.sh`. The deploy checks out that tag, installs its release, and applies the database migrations the new version adds.
- **Without a tag**, the install follows `main` and each deploy installs the latest commit's build (amd64 only). This is how the hosted instance runs.
- **Every build is verified before it is installed.** CI signs a build provenance attestation for each release file and per-commit build ([GitHub artifact attestations](https://docs.github.com/en/actions/security-guides/using-artifact-attestations-to-establish-provenance-for-builds), Sigstore). The deploy installs a pinned [cosign](https://github.com/sigstore/cosign) and refuses any file whose attestation is missing or names another workflow. You can check a file yourself with `gh attestation verify <file> -R jeffdafoe/llm-memory-api`.

## Security

Package registries, npm above all, are a routine supply-chain attack path. The project keeps npm code off the server and out of the build that ships:

- **The production server runs no npm packages.** The server is one static Go binary, and the admin UI is a static build. CI builds both for each commit on `main` after the tests pass (and for each release tag), and the deploy downloads them and verifies their checksums and signed build provenance before installing them. The VPS installs nothing from npm.
- **CI installs the admin UI's packages with `npm ci --ignore-scripts`**, so no package install script runs in the job that publishes the build.
- **In local development, run npm installs through [Socket Firewall](https://docs.socket.dev/docs/socket-firewall-free) (`sfw`).** It filters install requests against Socket's malicious-package database at the network layer, blocking confirmed malware before any postinstall script can run. Install it with `npm i -g sfw` and use `sfw npm install` in place of `npm install`. No account or API key required.

## License

MIT
