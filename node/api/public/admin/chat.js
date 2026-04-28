// chat.js — Chat messages view
import { ref } from 'vue';

function useChat({ api, showToast, dashboard }) {
    const chatMessages = ref([]);
    const selectedMessage = ref(null);

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

    // Formats one tool_call for display when a chat message has no plain
    // text body. Returns just the inner label — the template wraps it in
    // brackets/styling. `speak` is rendered specially by the template (its
    // input.text is shown as quoted prose), so this fallback covers the
    // structured tool calls only: look_around / move_to / chore / done /
    // anything else the engine adds later.
    function formatToolCall(tc) {
        if (!tc || !tc.name) return '';
        const input = tc.input || {};
        if (tc.name === 'move_to' && input.destination) {
            return `move_to → "${input.destination}"`;
        }
        if (tc.name === 'chore' && input.type) {
            return `chore: ${input.type}`;
        }
        return tc.name;
    }

    return {
        chatMessages, selectedMessage,
        loadChat, deleteChat, closeDialogs,
        formatToolCall
    };
}

export { useChat };
