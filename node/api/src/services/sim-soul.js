// Shared-NPC soul synthesis (LLM-199).
//
// A stateful salem NPC (e.g. Ezekiel) carries identity in a first-person
// `context/soul` note that the nightly dream→soul pipeline maintains in the
// NPC's own memory namespace (services/dream.js). A SHARED-VA NPC can't use
// that: one virtual agent (`salem-vendor`, `salem-visitor`) backs many bodies,
// so a single shared soul doc can't hold per-actor identity, and there's no
// per-actor namespace to write into. The result was a bare `## Who you are`
// block in shared-NPC perception (render.go muted the only field they had).
//
// This service gives shared NPCs an accreting per-actor soul WITHOUT per-actor
// namespaces or per-NPC agents: the salem engine assembles each shared NPC's
// day material itself (it already holds the ActionLog + per-peer summaries) and
// POSTs it here once per in-sim day. We run that material through the SAME
// `dream-sim-soul` agent that writes stateful souls — reusing its system prompt
// unchanged — and hand the prose back. The engine stores it in
// `actor_narrative_state.about_me` and renders it. The soul agent is stateless
// (we pass material in, get prose out; nothing is written to a per-actor
// namespace), so it's the single source of truth for soul prose across both
// stateful (nightly cron) and shared (this endpoint) NPCs.
//
// Why a new endpoint rather than `/agent/tick`: tick runs as the calling
// agent's own session, and the engine holds sessions for the actor VAs it
// drives but NOT for the system-owned `dream-sim-soul`. So the engine can't
// target the soul agent over tick; it has to hand the material to a route that
// resolves and invokes the soul agent server-side.

const { log } = require('./logger');

// Expertise tag for the shared soul agent — the same tag the nightly dream
// pipeline resolves (`findDreamAgent('dream-sim-soul')`, dream.js). Resolving
// by tag (not a hard-coded agent name) keeps this in lockstep with whatever
// agent is configured for sim soul synthesis.
const SOUL_EXPERTISE_TAG = 'dream-sim-soul';

// Per-field upper bound. The engine composes these from bounded sources (a
// short live seed, a prior soul paragraph, an event-capped day snapshot), so
// this is just a sanity ceiling against a runaway payload — the soul agent's
// own cost guard is the real backstop.
const MAX_FIELD_LEN = 100000;

// buildSoulUserMessage assembles the soul agent's user message in the same
// section shape the nightly dream pipeline uses for its soul block
// (dream.js — `## Character description` / `## Current soul document` /
// `## Dream snapshot`), so the `dream-sim-soul` system prompt (reused
// unchanged) sees the anchors it expects. The `## Character description`
// anchor is load-bearing for the soul prompt; for shared NPCs the engine
// composes it live (name + dwelling + household) rather than from a per-actor
// agent's `startup_instructions`.
//
// First run (empty `currentSoul`): mark the soul empty and frame the snapshot
// as an initial synthesis rather than a single-day incremental update, mirroring
// the dream pipeline's empty-soul rebuild framing — otherwise the model tries to
// "update" an empty document and produces something thin.
//
// Pure — exported for unit tests.
function buildSoulUserMessage({ characterDescription, currentSoul, daySnapshot, day }) {
    const soulIsEmpty = !currentSoul || currentSoul.trim() === '';
    const snapshotHeader = day ? '## Dream snapshot for ' + day : '## Dream snapshot';

    let msg = '## Character description\n\n' + characterDescription.trim() + '\n\n'
        + '## Current soul document\n\n'
        + (soulIsEmpty ? '(empty — first run)' : currentSoul.trim())
        + '\n\n' + snapshotHeader + '\n\n';

    if (soulIsEmpty) {
        msg += 'The current soul document is empty. Synthesize an initial soul '
            + 'from the day\'s material below; do not treat this as a single-day '
            + 'incremental update.\n\n';
    }

    msg += daySnapshot.trim();
    return msg;
}

// requireString throws a 400 when a required field is missing/blank/non-string.
function requireString(name, val) {
    if (typeof val !== 'string' || val.trim() === '') {
        throw Object.assign(new Error(name + ' required'), { statusCode: 400 });
    }
    if (val.length > MAX_FIELD_LEN) {
        throw Object.assign(new Error(name + ' is too long'), { statusCode: 400 });
    }
}

// optionalString throws a 400 only when a present field has the wrong type or is
// oversized; absent/empty is allowed (current_soul is empty on a first run).
function optionalString(name, val) {
    if (val === undefined || val === null) {
        return;
    }
    if (typeof val !== 'string') {
        throw Object.assign(new Error(name + ' must be a string'), { statusCode: 400 });
    }
    if (val.length > MAX_FIELD_LEN) {
        throw Object.assign(new Error(name + ' is too long'), { statusCode: 400 });
    }
}

// optionalDay validates the snapshot's date label. Unlike the other fields,
// `day` is NOT prompt material — it's interpolated verbatim into the
// "## Dream snapshot for <day>" section header, so an unconstrained string
// (newlines, markdown, instruction text) would let an operator-gated caller
// inject prompt content through the header. Accept only a strict YYYY-MM-DD
// label; reject anything else. Absent/empty is allowed (the header falls back
// to a bare "## Dream snapshot").
function optionalDay(name, val) {
    if (val === undefined || val === null || val === '') {
        return;
    }
    if (typeof val !== 'string' || !/^\d{4}-\d{2}-\d{2}$/.test(val)) {
        throw Object.assign(new Error(name + ' must be YYYY-MM-DD'), { statusCode: 400 });
    }
}

// synthesizeSimSoul resolves the shared soul agent, runs the assembled material
// through it, and returns the prose. Returns `{ text }` on success, or
// `{ text: '', rejected }` when the model produced nothing usable — the engine
// treats an empty result as "keep the prior soul" rather than overwriting
// `about_me` with junk.
//
// The heavy collaborators (findDreamAgent / invokeAgent / detectReasoningPreamble)
// are required lazily so the pure `buildSoulUserMessage` surface — and its
// unit test — don't pull the whole virtual-agent / dream / db module graph in
// at load time.
async function synthesizeSimSoul({ characterDescription, currentSoul, daySnapshot, day }) {
    requireString('character_description', characterDescription);
    requireString('day_snapshot', daySnapshot);
    optionalString('current_soul', currentSoul);
    optionalDay('day', day);

    const { findDreamAgent, detectReasoningPreamble } = require('./dream');
    const { invokeAgent } = require('./virtual-agent');

    const soulAgentName = await findDreamAgent(SOUL_EXPERTISE_TAG);
    if (!soulAgentName) {
        // Soul agent missing/misconfigured (findDreamAgent logs the specific
        // reason). 503 so the engine retries on its next sweep rather than
        // treating it as a permanent client error.
        throw Object.assign(
            new Error('soul agent (dream-sim-soul) not available'),
            { statusCode: 503 }
        );
    }

    const userMessage = buildSoulUserMessage({ characterDescription, currentSoul, daySnapshot, day });

    // Reuse the soul agent's own startup_instructions as the system prompt (do
    // NOT pass systemPrompt). Unlike the nightly cron (which skips both guards),
    // leave the rate + cost guards ENABLED: this is an operator-reachable route
    // driven by the engine, so `dream-sim-soul`'s configured rate/cost budget is
    // the backstop bounding engine-triggered volume. invokeAgent throws on
    // limit; that surfaces as a 5xx the engine retries next sweep. Retry
    // transient provider errors with backoff, as the cron does.
    const { text, truncated, finish_reason } = await invokeAgent(soulAgentName, {
        userMessage,
        context: 'soul',
        skipRetry: false,
    });

    // Length-stop: the soul came back cut off mid-thought. Reject → empty text
    // → the engine keeps the prior soul (same contract as the empty-reply and
    // reasoning-preamble guards below).
    if (truncated) {
        log('sim-soul', 'truncated-rejected', { agent: soulAgentName, finishReason: finish_reason });
        return { text: '', rejected: 'truncated' };
    }

    const trimmed = (text || '').trim();
    if (!trimmed) {
        log('sim-soul', 'empty-reply', { agent: soulAgentName });
        return { text: '', rejected: 'empty-reply' };
    }

    // Same reasoning-preamble guard the nightly soul path applies: some chat
    // models emit their analytical chain-of-thought as prose before the soul
    // body. Storing that would poison the rendered `## Who you are` AND compound
    // (the next synthesis reads its own prior output as input). Reject → empty
    // text → the engine keeps the prior soul.
    const marker = detectReasoningPreamble(trimmed);
    if (marker) {
        log('sim-soul', 'reasoning-preamble-rejected', { agent: soulAgentName, marker });
        return { text: '', rejected: 'reasoning-preamble' };
    }

    log('sim-soul', 'synthesized', { agent: soulAgentName, bytes: trimmed.length });
    return { text: trimmed };
}

// buildSoulUserMessage is exported for unit tests (sim-soul.test.js);
// synthesizeSimSoul is the production entry (routes/sim.js).
module.exports = { synthesizeSimSoul, buildSoulUserMessage };
