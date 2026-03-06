// chat.js — Chat messages view

function useChat({ api }) {
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

    function closeDialogs() {
        selectedMessage.value = null;
    }

    return {
        chatMessages, selectedMessage,
        loadChat, closeDialogs
    };
}

window.useChat = useChat;
