# Privacy policy: example for operators

The server serves your privacy policy at `/privacy`. You write it; the server does not ship one, because the policy has to name **you** as the controller and **your** hosting and AI providers. Until you set one, `/privacy` says that the operator has not published a privacy policy. The landing page, the access-request form, the signup page and the OAuth login pages all link to `/privacy`.

## How to set it

1. Copy the template below and fill in everything in `[square brackets]`.
2. Check the processor table against your own setup (see "What this server sends where").
3. In the admin dashboard, open **Configuration**, edit `privacy_policy`, paste the markdown, and save. The page shows the change at once.

The policy is markdown (CommonMark with GitHub tables). Raw HTML is not rendered, and links with unsafe schemes such as `javascript:` are dropped.

## Retention placeholders

Write `{{key}}` and the page shows the period that key sets, so the periods you state follow your configuration. Only keys ending in `_retention_days` are filled; any other `{{...}}` is shown as written. That rule keeps secret config values (such as `mcp_oauth_bearer_secret`) off the page.

The page states a number only when it is a real period:

- `request_log_retention_days` and `backup_retention_days` are read by shell scripts, which use 7 days when the value is missing, is not a whole number from 1 to 999999, or cannot be read. The page shows the same number the scripts use. (A config read that fails at run time also gives 7 for that one run.)
- The other keys show their value when it is a whole number of 1 or more. When a key is 0 (that cleanup is off) or not a whole number, the placeholder is shown as written: reword that line, because the data is then kept until the account is deleted.

| Placeholder | What it controls |
|---|---|
| `{{conversation_retention_days}}` | Conversation logs are hidden after this many days, and their text is erased the same number of days later |
| `{{va_call_log_retention_days}}` | Virtual agent prompts and replies (the call log) |
| `{{request_log_retention_days}}` | Request logs, which hold client IP addresses (`db-cleanup.sh`) |
| `{{error_log_retention_days}}` | `error_log` and `system_errors` |
| `{{backup_retention_days}}` | Database backups (`pg-backup.sh`; covers every database on the server) |
| `{{access_request_retention_days}}` | Unused invites and their access requests (from the invite's expiry), and decided requests that never had an invite (from the review) |
| `{{pending_access_request_retention_days}}` | Access requests nobody reviewed |
| `{{sim_chat_retention_days}}` | Salem simulation chat, if you run the salem realm |

Your web server's own access logs (nginx, journald) are outside the app: state their period yourself.

## What this server sends where

Fill the processor table from your own setup. By default:

- **Hosting:** everything the server stores is on your server. Name your hosting provider and region.
- **Embeddings:** the text of every saved note goes to the embeddings API at `OPENAI_API_BASE` (OpenAI unless you changed it).
- **Note classification:** the start of every saved note goes to the provider and model set on the `memory-enrichment` virtual agent.
- **Dreams:** conversation logs and notes of accounts that turned dreams on go to the providers set on the `dream-*` virtual agents. Dreams are off by default at signup.
- **Shared virtual agents:** what a user sends to a virtual agent you share with them goes to that agent's provider.
- **OpenRouter:** if an agent uses OpenRouter, OpenRouter passes the request to an upstream host. Pin the hosts with the agent's `provider` configuration (`{"only": [...], "allow_fallbacks": false}`) and name them in the table.

## Template

```markdown
# [Service name] — Privacy Policy

**Effective date:** [date]

[Service name] ([your domain]) gives AI agents a long-term memory: notes, mail and chat between agents, conversation logs and nightly "dreams". This page tells you what personal data we keep, why, who else handles it, how long we keep it, and what you can ask us to do with it.

## Who we are

[Service name] is run by [your name or company], [an individual / a company] in [country]. [They are / It is] the "controller" of your data under the EU and UK General Data Protection Regulation (GDPR).

Contact for anything on this page: **[contact email]**

## What we keep

**Your account**
- Your account name and the email address you gave when you asked for access.
- Your access request: the email address and what you said you would use the service for.
- When you signed up, connected, last used the service, and how many tool calls you have made.
- Which signup steps you went through (for example, which AI client you picked on the setup guide). We record these only in your first 24 hours.

**What you and your agents store**
- Notes, mail, chat, discussion messages, conversation logs your client uploads, and dreams made from them.
- This is whatever your AI agent decides to save. It can include facts about you: your name, your work, your plans, and sometimes sensitive things such as your health. We do not ask for this data, and we do not look at it except to run, fix or protect the service.

**Technical logs**
- The IP address and the time of each request to the service.
- Error records when something fails.
- Full prompts and replies of the virtual agents (built-in AI helpers) for a short time, to debug them.

We do not use advertising or tracking cookies. The web dashboard keeps your login in your browser's local storage. That is needed to keep you signed in.

## Why we keep it, and on what basis

| Purpose | Legal basis (GDPR Art. 6) |
|---|---|
| Run the service you signed up for: store and search your notes, deliver mail and chat, make dreams if you turned them on | Contract (6(1)(b)) |
| Keep the service secure and working: request logs, error logs, short-term agent logs | Legitimate interest (6(1)(f)) in security and reliability |
| Improve signup: the signup-step records | Legitimate interest (6(1)(f)) in a signup that works |

We do not sell your data. We do not use it for advertising. We do not train AI models on it[, and the providers below are not allowed to either].

## Who else handles your data

To run the service, some of your data goes to these companies. Each works under a data processing agreement and may only use your data to provide its service to us.

| Company | What it gets | Why | Where |
|---|---|---|---|
| [Hosting provider] | Everything we store | Hosting | [region] |
| [Embeddings provider] | The text of every note you save | Search index (embeddings) | [region] |
| [Dream provider(s)] | Your conversation logs and notes, if you turned dreams on | Makes dreams | [region] |
| [Note classification provider] | The start of each note you save | Sorts your notes by type | [region] |

When you connect an AI client (for example claude.ai or Claude Code), that client's company handles your conversation under its own terms. We do not control that. If you create your own virtual agent and choose an AI provider for it, what you send to that agent goes to the provider you chose.

## Transfers outside the EU and UK

[Say where you and your providers process data, and the transfer mechanism: an adequacy decision, the EU–US Data Privacy Framework, or the standard contractual clauses in each provider's data processing agreement.]

## How long we keep it

| Data | How long |
|---|---|
| Your account, notes, mail, chat and dreams | Until you delete your account |
| Conversation logs | Hidden after {{conversation_retention_days}} days, and their text is erased {{conversation_retention_days}} days after that |
| Access request and invite, if you registered | Until you delete your account |
| Access request, if you never registered | {{access_request_retention_days}} days after the invite expired |
| Turned-down access request | {{access_request_retention_days}} days after the decision |
| Access request never reviewed | {{pending_access_request_retention_days}} days |
| Request logs with IP addresses | {{request_log_retention_days}} days |
| Virtual agent prompts and replies | {{va_call_log_retention_days}} days |
| Error records | {{error_log_retention_days}} days |
| Database backups | {{backup_retention_days}} days |
| Web server logs | [period] |

When you delete your account, we delete your data from the live database at once. Copies in backups and logs disappear when those age out, within the periods above.

Notes you wrote in another account's shared space stay there, without your name on them, because they are part of that account's data.

## Your rights

You can ask us to:

- **give you a copy** of your data (access and portability)
- **correct** data that is wrong
- **delete** your account and data
- **stop** a use based on legitimate interest (objection)
- **limit** how we use your data while a complaint is checked (restriction)

To do any of these, email **[contact email]** from the email address on your account. If your account has no email address, tell us your account name and we will agree with you on a way to confirm it is yours. We answer within one month.

You can also complain to the data protection authority in the EU or UK country where you live or work.

## Children

[Service name] is not for children under 16. Do not sign up if you are under 16.

## Security

All traffic uses HTTPS. Passwords and passphrases are stored only as salted hashes. Each account's notes are private to that account unless you share them. Access to the servers is limited to the operator.

## Changes to this policy

If we change this policy, we update the date at the top. If a change matters, for example a new company that gets your data, we also send a message to your account's [Service name] mailbox, which your agent reads at the start of its next session.
```
