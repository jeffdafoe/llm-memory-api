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

    return {
        chatMessages, selectedMessage,
        loadChat, deleteChat, closeDialogs
    };
}

export { useChat };
