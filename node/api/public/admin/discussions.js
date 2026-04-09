// discussions.js — Discussions list, detail, chat transcript, create, send, conclude
import { ref, nextTick } from 'vue';

function useDiscussions({ api, showToast, onEvent }) {
    const discussions = ref([]);
    const discussionFilter = ref('');
    const selectedDiscussion = ref(null);
    const discussionChat = ref(null);

    // Create discussion form state
    const discussionCreating = ref(false);
    const discussionTopic = ref('');
    const discussionParticipants = ref([]);
    const discussionMode = ref('realtime');
    const discussionContext = ref('');
    const discussionCreateSaving = ref(false);

    // Chat input state
    const discussionMessage = ref('');
    const discussionSending = ref(false);

    // Stale-response guard for viewDiscussion
    let currentDiscussionRequest = 0;

    async function loadDiscussions() {
        try {
            const body = {};
            if (discussionFilter.value) {
                body.status = discussionFilter.value;
            }
            const data = await api('/admin/discussions', body);
            discussions.value = data.discussions;
        } catch (err) {
            console.error('Failed to load discussions:', err);
        }
    }

    async function viewDiscussion(id) {
        const requestId = ++currentDiscussionRequest;
        discussionChat.value = null;
        try {
            const data = await api('/admin/discussions/detail', { discussion_id: id });
            if (requestId !== currentDiscussionRequest) return;
            selectedDiscussion.value = data;
            // Auto-load transcript
            const chatData = await api('/admin/chat', { discussion_id: id, limit: 500 });
            if (requestId !== currentDiscussionRequest) return;
            discussionChat.value = chatData.messages.reverse();
            // Scroll dialog to bottom so chat input is visible
            await nextTick();
            scrollDiscussionToBottom();
        } catch (err) {
            console.error('Failed to load discussion:', err);
        }
    }

    // Scroll the discussion dialog body to the bottom so the chat input is visible
    function scrollDiscussionToBottom() {
        const dialog = document.querySelector('.discussion-detail-dialog .dialog-body');
        if (dialog) {
            dialog.scrollTop = dialog.scrollHeight;
        }
    }

    async function loadDiscussionChat(id) {
        try {
            const data = await api('/admin/chat', { discussion_id: id, limit: 500 });
            discussionChat.value = data.messages.reverse();
            await nextTick();
            scrollDiscussionToBottom();
        } catch (err) {
            console.error('Failed to load discussion chat:', err);
        }
    }

    function startCreateDiscussion() {
        discussionCreating.value = true;
        discussionTopic.value = '';
        discussionParticipants.value = [];
        discussionMode.value = 'realtime';
        discussionContext.value = '';
    }

    function cancelCreateDiscussion() {
        discussionCreating.value = false;
    }

    async function createDiscussion() {
        discussionTopic.value = discussionTopic.value.trim();
        if (!discussionTopic.value || discussionParticipants.value.length === 0) return;
        discussionCreateSaving.value = true;
        try {
            const data = await api('/admin/discussions/create', {
                topic: discussionTopic.value,
                participants: discussionParticipants.value,
                mode: discussionMode.value,
                context: discussionContext.value || undefined
            });
            discussionCreating.value = false;
            const newId = data.discussion ? data.discussion.id : data.discussion_id;
            showToast('Discussion #' + newId + ' created');
            await loadDiscussions();
            // Open the new discussion
            await viewDiscussion(newId);
        } catch (err) {
            showToast(err.message || 'Failed to create discussion', 'error');
        } finally {
            discussionCreateSaving.value = false;
        }
    }

    async function sendDiscussionMessage() {
        discussionMessage.value = discussionMessage.value.trim();
        if (!discussionMessage.value || !selectedDiscussion.value) return;
        discussionSending.value = true;
        try {
            await api('/admin/discussions/send', {
                discussion_id: selectedDiscussion.value.discussion.id,
                message: discussionMessage.value
            });
            discussionMessage.value = '';
        } catch (err) {
            showToast(err.message || 'Failed to send message', 'error');
        } finally {
            discussionSending.value = false;
        }
    }

    async function concludeDiscussion() {
        if (!selectedDiscussion.value) return;
        try {
            await api('/admin/discussions/conclude', {
                discussion_id: selectedDiscussion.value.discussion.id
            });
            showToast('Discussion concluded');
            // Refresh the detail
            await viewDiscussion(selectedDiscussion.value.discussion.id);
            await loadDiscussions();
        } catch (err) {
            showToast(err.message || 'Failed to conclude discussion', 'error');
        }
    }

    // Handle incoming WebSocket chat_message events — append to transcript if relevant
    function handleChatMessage(data) {
        if (!selectedDiscussion.value || !discussionChat.value) return;
        if (data.discussion_id !== selectedDiscussion.value.discussion.id) return;
        // Avoid duplicates (use message_text_id to prevent fan-out duplication)
        var dedupId = data.message_text_id || data.id;
        if (discussionChat.value.some(function(m) { return (m.message_text_id || m.id) === dedupId; })) return;
        discussionChat.value.push({
            id: data.id,
            message_text_id: data.message_text_id,
            from_agent: data.from_agent,
            message: data.message,
            discussion_id: data.discussion_id,
            sent_at: data.sent_at
        });
        nextTick().then(scrollDiscussionToBottom);
    }

    // Register WebSocket handler for live transcript updates
    if (onEvent) {
        onEvent('chat_message', handleChatMessage);
    }

    function closeDialogs() {
        selectedDiscussion.value = null;
        discussionChat.value = null;
        discussionCreating.value = false;
    }

    return {
        discussions, discussionFilter, selectedDiscussion, discussionChat,
        discussionCreating, discussionTopic, discussionParticipants, discussionMode, discussionContext, discussionCreateSaving,
        discussionMessage, discussionSending,
        loadDiscussions, viewDiscussion, loadDiscussionChat,
        startCreateDiscussion, cancelCreateDiscussion, createDiscussion,
        sendDiscussionMessage, concludeDiscussion,
        handleChatMessage, closeDialogs
    };
}

export { useDiscussions };
