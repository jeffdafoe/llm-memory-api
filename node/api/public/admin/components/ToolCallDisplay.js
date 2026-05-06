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
//   consume(item, qty?)       -> [consume] + "qty × item" (or just "item"
//                                when qty == 1, the default)
//   gather(qty?)              -> [gather] + "×qty" only when qty > 1; the
//                                source/product isn't in the tool input
//                                (resolved engine-side from where the NPC
//                                is loitering), so the chip alone tells the
//                                story for the common qty=1 case
//   pay(recipient, amount,    -> [pay] + "<amount>c → <recipient>" plus an
//       item?, qty?, for?)       optional " — <item>" (with " ×<qty>" when
//                                qty > 1) or " — <for>" when item absent.
//                                consume_now / consumers fields are skipped
//                                — meaningful mechanically but verbose for
//                                a one-line chat-row chip.
//   attend_to(villager)       -> [attend_to] + villager name. Without the
//                                name it's impossible to tell at a glance
//                                which dispatch is which when the
//                                chronicler emits multiple attend_to calls
//                                in the same assistant message (parallel
//                                tool use), and that's the common case
//                                during arrival or shift-boundary scenes.
//   accept_pay()              -> [accept_pay] chip only — no args, the
//                                tool itself is the meaning ("recipient
//                                accepted the held-tx pay deliberation").
//   decline_pay(reason)       -> [decline_pay] + the reason in quotes.
//                                The reason IS the recipient's spoken
//                                refusal (engine emits it as a synthetic
//                                npc_spoke for the room), so showing it
//                                here gives the admin the exchange.
//   counter_pay(new_amount,   -> [counter_pay] + "<amount>c — <message>"
//               message)         where the message is the recipient's
//                                spoken counter (also fed to npc_spoke).
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
<template v-else-if="tc.name === 'consume'">
    <span class="tool-chip">[consume]</span>
    <span v-if="consumeLabel" class="tool-prose">{{ consumeLabel }}</span>
</template>
<template v-else-if="tc.name === 'gather'">
    <span class="tool-chip">[gather]</span>
    <span v-if="gatherLabel" class="tool-prose">{{ gatherLabel }}</span>
</template>
<template v-else-if="tc.name === 'pay'">
    <span class="tool-chip">[pay]</span>
    <span v-if="payLabel" class="tool-prose">{{ payLabel }}</span>
</template>
<template v-else-if="tc.name === 'attend_to'">
    <span class="tool-chip">[attend_to]</span>
    <span v-if="inputVillager" class="tool-prose">{{ inputVillager }}</span>
</template>
<template v-else-if="tc.name === 'accept_pay'">
    <span class="tool-chip">[accept_pay]</span>
</template>
<template v-else-if="tc.name === 'decline_pay'">
    <span class="tool-chip">[decline_pay]</span>
    <span v-if="inputReason" class="tool-prose">&ldquo;{{ inputReason }}&rdquo;</span>
</template>
<template v-else-if="tc.name === 'counter_pay'">
    <span class="tool-chip">[counter_pay]</span>
    <span v-if="counterPayLabel" class="tool-prose">{{ counterPayLabel }}</span>
</template>
<span v-else class="tool-chip">[{{ tc.name }}]</span>
`;

import { computed } from 'vue';
import { safeInt, stringOrEmpty } from '../util.js';

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
        const inputVillager = computed(() => input.value.villager || '');
        const inputReason = computed(() => input.value.reason || '');
        // 'village' is the chronicler's default scope; only surface a scope
        // chip when the tool call set something else (e.g. local/private).
        const nonDefaultScope = computed(() => {
            const s = inputScope.value;
            return s && s !== 'village';
        });

        // consume(item, qty?) — show "qty × item" when qty > 1 so the
        // common single-portion case stays terse ('bread' rather than
        // '1 × bread'). qty defaults to 1 per the tool schema.
        const consumeLabel = computed(() => {
            const item = stringOrEmpty(input.value.item);
            if (!item) return '';
            const q = safeInt(input.value.qty) ?? 1;
            if (q > 1) return q + ' × ' + item;
            return item;
        });

        // gather(qty?) — the source/product isn't in the input (the engine
        // resolves it from the loiter slot), so a chip alone reads fine for
        // qty=1. Surface qty only when explicit and >1 to flag bulk pulls.
        const gatherLabel = computed(() => {
            const q = safeInt(input.value.qty) ?? 1;
            if (q > 1) return '×' + q;
            return '';
        });

        // pay(recipient, amount, item?, qty?, for?) — primary line is the
        // money flow. Append item (with optional qty multiplier) when
        // present; otherwise fall back to 'for' flavor text. 'for' is a JS
        // reserved word so it must be accessed via bracket notation. The
        // head/suffix split avoids labels that lead with the ' — ' separator
        // when amount + recipient are both missing/malformed but item or
        // for survived.
        const payLabel = computed(() => {
            const recipient = stringOrEmpty(input.value.recipient);
            const item = stringOrEmpty(input.value.item);
            const forText = stringOrEmpty(input.value['for']);
            // amount is rendered only when it parses to a clean integer;
            // null skips the chip prefix rather than fake-rendering 'NaNc'
            // or a partially-coerced value like '4c' from '4abc'.
            const amt = safeInt(input.value.amount);
            const headParts = [];
            if (amt !== null) headParts.push(amt + 'c');
            if (recipient) headParts.push('→ ' + recipient);
            const head = headParts.join(' ');
            let suffix = '';
            if (item) {
                const q = safeInt(input.value.qty) ?? 1;
                suffix = item + (q > 1 ? ' ×' + q : '');
            } else if (forText) {
                suffix = forText;
            }
            if (head && suffix) return head + ' — ' + suffix;
            return head || suffix;
        });

        // counter_pay(new_amount, message) — the message is a synthetic
        // speak the engine emits on the recipient's behalf, so it reads
        // as the recipient's own quoted dialogue. Format mirrors pay's
        // money-then-prose shape: "5c — 'Make it five and we have a
        // deal.'" Drops the amount when it doesn't parse to a clean
        // integer, same defensive pattern as payLabel.
        const counterPayLabel = computed(() => {
            const amt = safeInt(input.value.new_amount);
            const message = stringOrEmpty(input.value.message);
            const head = amt !== null ? amt + 'c' : '';
            if (head && message) return head + ' — “' + message + '”';
            if (message) return '“' + message + '”';
            return head;
        });

        return {
            inputText, inputQuery, inputDestination, inputType,
            inputScope, nonDefaultScope, inputVillager, inputReason,
            consumeLabel, gatherLabel, payLabel, counterPayLabel,
        };
    }
};
