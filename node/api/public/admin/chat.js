// chat.js — Chat messages view
import { ref, computed } from 'vue';

function useChat({ api, showToast, dashboard }) {
    const chatMessages = ref([]);
    const selectedMessage = ref(null);
    // Set of scene_ids the admin has expanded. Scenes default to collapsed
    // so the top of the chat list isn't a wall of perception/tool-result
    // rows whenever Salem is busy.
    const expandedScenes = ref(new Set());

    // Group chat rows by scene_id for the admin chat list. Companion-mode
    // and any pre-MEM-121 row has a NULL scene_id and renders as a single
    // standalone row exactly as before. Sim-mode rows that share a
    // scene_id collapse into one expandable scene row.
    //
    // Ordering: a scene group occupies the slot of its most recent message,
    // so a fresh tavern conversation lands at the top. Inside the group the
    // messages render chronologically (oldest first) so the conversation
    // reads naturally when expanded.
    const chatGroups = computed(() => {
        const sceneIndex = new Map(); // scene_id -> group ref
        const groups = [];
        for (const msg of chatMessages.value) {
            if (msg.scene_id) {
                let group = sceneIndex.get(msg.scene_id);
                if (!group) {
                    group = { type: 'scene', scene_id: msg.scene_id, messages: [] };
                    sceneIndex.set(msg.scene_id, group);
                    groups.push(group);
                }
                group.messages.push(msg);
            } else {
                groups.push({ type: 'msg', msg });
            }
        }
        // Flatten single-message scenes back to plain rows. Pagination and
        // partial cascades both leave us with one-row groups that don't
        // benefit from a collapsed header — and forcing the admin to expand
        // a scene just to read one message is a regression versus the
        // pre-MEM-121 list.
        for (let i = 0; i < groups.length; i++) {
            const g = groups[i];
            if (g.type === 'scene' && g.messages.length === 1) {
                groups[i] = { type: 'msg', msg: g.messages[0] };
            }
        }
        for (const g of groups) {
            if (g.type !== 'scene') continue;
            g.messages.sort((a, b) => new Date(a.sent_at) - new Date(b.sent_at));
            g.earliest_at = g.messages[0].sent_at;
            g.latest_at = g.messages[g.messages.length - 1].sent_at;
            // Surface unacked status on the collapsed header so the admin
            // doesn't lose the ack indicator that single rows show. The
            // discussion-id branch of /admin/chat doesn't select acked_at
            // (delivery-row state is per-recipient, not per-text), so those
            // messages have `acked_at === undefined` and we treat them as
            // having no ack tracking — only an explicit NULL means unacked.
            g.hasUnacked = g.messages.some(m => m.acked_at === null);
            // Scene location (ZBBS-118): every chat row in a scene carries
            // the same scene_id and the same scenes-table join, so picking
            // any message's structure_name yields the same string. First
            // non-empty wins to defend against an unexpectedly-NULL row.
            g.location = '';
            for (const m of g.messages) {
                if (m.structure_name) {
                    g.location = m.structure_name;
                    break;
                }
            }
            const seen = new Set();
            const order = [];
            for (const m of g.messages) {
                for (const name of [m.from_agent, m.to_agent]) {
                    if (name && !seen.has(name)) {
                        seen.add(name);
                        order.push(name);
                    }
                }
            }
            g.participants = order;
        }
        groups.sort((a, b) => {
            const aTime = a.type === 'scene' ? a.latest_at : a.msg.sent_at;
            const bTime = b.type === 'scene' ? b.latest_at : b.msg.sent_at;
            return new Date(bTime) - new Date(aTime);
        });
        return groups;
    });

    function toggleScene(sceneId) {
        const next = new Set(expandedScenes.value);
        if (next.has(sceneId)) {
            next.delete(sceneId);
        } else {
            next.add(sceneId);
        }
        expandedScenes.value = next;
    }
    function isSceneExpanded(sceneId) {
        return expandedScenes.value.has(sceneId);
    }

    async function loadChat() {
        try {
            const data = await api('/admin/chat', { limit: 100 });
            chatMessages.value = data.messages;
        } catch (err) {
            console.error('Failed to load chat:', err);
        }
    }

    async function deleteChat(msg) {
        try {
            await api('/admin/chat/delete', { id: msg.id });
            chatMessages.value = chatMessages.value.filter(m => m.id !== msg.id);
            // Also remove from dashboard view so the row disappears there too
            if (dashboard && dashboard.value && dashboard.value.chat) {
                dashboard.value.chat = dashboard.value.chat.filter(m => m.id !== msg.id);
            }
            selectedMessage.value = null;
            showToast('Chat message deleted');
        } catch (err) {
            console.error('Failed to delete chat:', err);
            showToast('Failed to delete: ' + err.message, 'error');
        }
    }

    function closeDialogs() {
        selectedMessage.value = null;
    }

    return {
        chatMessages, selectedMessage,
        chatGroups, expandedScenes, toggleScene, isSceneExpanded,
        loadChat, deleteChat, closeDialogs,
    };
}

export { useChat };
