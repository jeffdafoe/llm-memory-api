// Speaker label helpers for virtual-agent discussion prompts.
//
// The discussion-path prompt format embeds agent names as structured speaker
// labels. To keep guardrails consistent, all three places that care about
// "what exactly did we put in the prompt" — transcript rendering, stop
// sequence generation, and the post-generation impersonation scan — derive
// from the same helpers.
//
// Two concerns, split into two functions:
//
//   canonicalSpeakerId(rawName): the comparison form. NFKC-normalized so
//   Unicode equivalents compare equal in the post-generation scan. Use this
//   when building the set of distinct speakers from chat history and when
//   comparing a line from the response against that set.
//
//   renderSpeakerLabel(rawName): the serialized form that actually appears
//   in the prompt as the `sender` value inside a JSON object. The JSON
//   serializer handles all escaping (quotes, backslashes, control chars,
//   non-BMP code points), so this helper just returns the canonical form
//   and lets JSON.stringify do the heavy lifting at the render site.
//
// History note: agent names are validated as alphanumeric+hyphens+underscores
// at insert time today, but we can't assume every row in chat_message_texts
// was created under the current validation regime. Treat raw names as
// untrusted — escape defensively, never interpolate into regex or JSON strings
// without going through JSON.stringify or escapeRegExp.

function canonicalSpeakerId(rawName) {
    if (rawName == null) return '';
    return String(rawName).normalize('NFKC');
}

// Serialized form placed in the prompt as the JSON sender value.
// Same as canonical for now, but kept as a separate function so the render
// site has one explicit call that could grow its own escaping/mapping rules
// later without changing every call site.
function renderSpeakerLabel(rawName) {
    return canonicalSpeakerId(rawName);
}

// Escape a string for use in a regex. Used when building the post-generation
// impersonation scan from historical speaker labels.
function escapeRegExp(s) {
    return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

module.exports = {
    canonicalSpeakerId,
    renderSpeakerLabel,
    escapeRegExp
};
