// ToolCallDisplay — render one parsed assistant tool_call from a chat row.
//
// Used in three places in the admin chat UI: the top-level message row, the
// scene-child row inside an expanded scene, and the chat-message detail
// dialog. All three previously inlined slightly different copies of the same
// branching that only handled `speak` (italic prose) and `move_to`/`chore`
// (chip with the destination/type baked into the label). For the chronicler
// tools (set_environment / record_event / recall) the inlined version
// dropped the actual text/query argument entirely, leaving the admin staring
// at a row that just said `[set_environment]`.
//
// Unified rendering:
//
//   speak(text)               -> italic quoted prose, no chip (speech is
//                                the message body, not a tool annotation)
//   set_environment(text)     -> [set_environment] + text inline
//   record_event(text, scope) -> [record_event] (+ scope chip if non-default
//                                'village') + text inline
//   recall(query)             -> [recall] + "query" inline
//   move_to(destination)      -> [move_to] + destination inline
//   chore(type)               -> [chore] + type inline
//   done() / unknown          -> [name] chip only
//
// Usage: <tool-call-display :tc="toolCall" />

const template = `
<template v-if="!tc || !tc.name"></template>
<em v-else-if="tc.name === 'speak'" class="tool-speak">&ldquo;{{ inputText }}&rdquo;</em>
<template v-else-if="tc.name === 'set_environment'">
    <span class="tool-chip">[set_environment]</span>
    <span v-if="inputText" class="tool-prose">{{ inputText }}</span>
</template>
<template v-else-if="tc.name === 'record_event'">
    <span class="tool-chip">[record_event<span v-if="nonDefaultScope"> · {{ inputScope }}</span>]</span>
    <span v-if="inputText" class="tool-prose">{{ inputText }}</span>
</template>
<template v-else-if="tc.name === 'recall'">
    <span class="tool-chip">[recall]</span>
    <span v-if="inputQuery" class="tool-prose">&ldquo;{{ inputQuery }}&rdquo;</span>
</template>
<template v-else-if="tc.name === 'move_to'">
    <span class="tool-chip">[move_to]</span>
    <span v-if="inputDestination" class="tool-prose">{{ inputDestination }}</span>
</template>
<template v-else-if="tc.name === 'chore'">
    <span class="tool-chip">[chore]</span>
    <span v-if="inputType" class="tool-prose">{{ inputType }}</span>
</template>
<span v-else class="tool-chip">[{{ tc.name }}]</span>
`;

import { computed } from 'vue';

export default {
    name: 'ToolCallDisplay',
    template,
    props: {
        tc: { type: Object, required: true }
    },
    setup(props) {
        // Defensive accessors — input shape comes from upstream providers and
        // can occasionally be missing or malformed; fall back to '' rather
        // than throwing on .text of undefined.
        const input = computed(() => (props.tc && props.tc.input) || {});
        const inputText = computed(() => input.value.text || '');
        const inputQuery = computed(() => input.value.query || '');
        const inputDestination = computed(() => input.value.destination || '');
        const inputType = computed(() => input.value.type || '');
        const inputScope = computed(() => input.value.scope || '');
        // 'village' is the chronicler's default scope; only surface a scope
        // chip when the tool call set something else (e.g. local/private).
        const nonDefaultScope = computed(() => {
            const s = inputScope.value;
            return s && s !== 'village';
        });
        return {
            inputText, inputQuery, inputDestination, inputType,
            inputScope, nonDefaultScope,
        };
    }
};
