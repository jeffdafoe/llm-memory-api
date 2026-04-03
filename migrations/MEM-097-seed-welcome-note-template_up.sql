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

Welcome, {agent}. This note lives in your namespace and is always available via `read_note`. Use it as a reference until you''ve built your own habits.

## Session lifecycle

**Start each session:**
- `read_instructions` — reload your behavioral rules (if dream processing is enabled, your soul document is automatically included)
- `activity_start` — show you''re online
- `mail_check` + `chat_receive` — check for messages
- `discussion_pending` — check for discussion invitations
- Read `notes/active-work` — pick up where you left off

**During sessions:**
- When something noteworthy comes up — a decision, a preference, a useful fact — save it to your notes right away. Don''t wait until the end. Notes are your long-term memory; conversation context is temporary.
- Use `save_note` to create or replace notes, `edit_note` for targeted changes within a note.
- Use `search` (semantic) and `grep` (exact text) to find things across your notes.

**End each session:**
- Update `notes/active-work` with your current state — what you worked on, what''s pending, anything the next session needs to know
- `activity_stop` — signal you''re done

## What to save

- **Who your user is** — name, communication style, preferences, boundaries
- **What matters to them** — interests, goals, ongoing projects, important dates
- **Decisions and context** — why something was done a certain way, not just what was done
- **Project details** — architecture, key files, environment setup, patterns
- **Anything you''d need if you lost all conversation history** — because you will

## Surviving context loss

Your conversation has a finite context window. When it fills up, earlier messages get compressed or lost.

- **Save early, save often** — if you''ll need it later, it belongs in a note
- **Keep `notes/active-work` current** — this is your resume point after compression or a new session
- **Trust your notes over your memory** — after compression, your summarized recall may be wrong; the notes have ground truth
- **Re-read your instructions** after compression — they''re your behavioral anchor

## Key notes to maintain

- `notes/active-work` — current state, updated every session
- `notes/user-profile` — who your user is and how they work
- `instructions/getting-started` — this note (you can update it as you learn)

Once you''re comfortable, update your startup instructions (`save_instructions`) with behavioral rules specific to your user. Those instructions are the first thing you read each session — make them count.'
);
