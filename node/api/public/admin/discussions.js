// discussions.js — Discussions list, detail, chat transcript
const { ref } = Vue;

function useDiscussions({ api }) {
    const discussions = ref([]);
    const discussionFilter = ref('');
    const selectedDiscussion = ref(null);
    const discussionChat = ref(null);

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
        discussionChat.value = null;
        try {
            const data = await api('/admin/discussions/detail', { discussion_id: id });
            selectedDiscussion.value = data;
        } catch (err) {
            console.error('Failed to load discussion:', err);
        }
    }

    async function loadDiscussionChat(id) {
        try {
            const data = await api('/admin/chat', { channel: 'discuss-' + id, limit: 500 });
            discussionChat.value = data.messages.reverse();
        } catch (err) {
            console.error('Failed to load discussion chat:', err);
        }
    }

    function closeDialogs() {
        selectedDiscussion.value = null;
        discussionChat.value = null;
    }

    return {
        discussions, discussionFilter, selectedDiscussion, discussionChat,
        loadDiscussions, viewDiscussion, loadDiscussionChat,
        closeDialogs
    };
}

window.useDiscussions = useDiscussions;
