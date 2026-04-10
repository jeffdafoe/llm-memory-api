// dashboard.js — Dashboard view with live discussion polling
import { ref, nextTick } from 'vue';

function useDashboard({ api, authenticated, onEvent }) {
    const dashboard = ref(null);
    const liveDiscussions = ref([]);

    const LIVE_POLL_MS = 5000;
    let liveTimer = null;

    async function loadDashboard() {
        try {
            const data = await api('/admin/dashboard');
            dashboard.value = data;
            loadLiveDiscussions();
        } catch (err) {
            console.error('Failed to load dashboard:', err);
        }
    }

    let livePolling = false;

    async function loadLiveDiscussions() {
        if (!dashboard.value) return;

        const active = dashboard.value.discussions.filter(d => d.status === 'active');
        if (active.length === 0) {
            liveDiscussions.value = [];
            stopLivePolling();
            return;
        }

        const results = await Promise.all(active.map(async d => {
            try {
                const [detail, chat] = await Promise.all([
                    api('/admin/discussions/detail', { discussion_id: d.id }),
                    api('/admin/chat', { discussion_id: d.id, limit: 500 })
                ]);
                return {
                    discussion: detail.discussion,
                    participants: detail.participants,
                    votes: detail.votes,
                    chat: chat.messages.reverse()
                };
            } catch (err) {
                console.error('Failed to load live discussion ' + d.id + ':', err);
                return null;
            }
        }));

        liveDiscussions.value = results.filter(Boolean);
        scrollLiveChats(true);
        startLivePolling();
    }

    async function refreshLiveChats() {
        if (livePolling) return; // Guard against overlapping polls
        livePolling = true;
        try {
            const results = await Promise.all(liveDiscussions.value.map(async live => {
                try {
                    const [detail, chat] = await Promise.all([
                        api('/admin/discussions/detail', { discussion_id: live.discussion.id }),
                        api('/admin/chat', { discussion_id: live.discussion.id, limit: 500 })
                    ]);
                    if (detail.discussion.status !== 'active') return null;
                    return {
                        discussion: detail.discussion,
                        participants: detail.participants,
                        votes: detail.votes,
                        chat: chat.messages.reverse()
                    };
                } catch (err) {
                    console.error('Failed to refresh live chat:', err);
                    return live; // Keep on error
                }
            }));
            const stillActive = results.filter(Boolean);
            liveDiscussions.value = stillActive;
            if (stillActive.length === 0) {
                stopLivePolling();
            }
            scrollLiveChats();
        } finally {
            livePolling = false;
        }
    }

    function scrollLiveChats(force) {
        nextTick(() => {
            document.querySelectorAll('.live-chat-transcript').forEach(el => {
                const atBottom = (el.scrollHeight - el.scrollTop - el.clientHeight) < 80;
                if (force || atBottom) {
                    el.scrollTop = el.scrollHeight;
                }
            });
        });
    }

    function startLivePolling() {
        if (liveTimer) return;
        liveTimer = setInterval(() => {
            if (authenticated.value) {
                refreshLiveChats();
            }
        }, LIVE_POLL_MS);
    }

    function stopLivePolling() {
        if (liveTimer) {
            clearInterval(liveTimer);
            liveTimer = null;
        }
    }

    // Real-time activity updates from WebSocket
    if (onEvent) {
        onEvent('agent_activity', (data) => {
            if (dashboard.value && dashboard.value.agents) {
                const agent = dashboard.value.agents.find(a => a.agent === data.agent);
                if (agent) {
                    agent.active_since = data.active ? new Date().toISOString() : null;
                }
            }
        });
    }

    // Live chat input
    const liveChatText = ref('');
    const liveChatSending = ref(false);

    async function sendLiveChat(discussionId) {
        const message = liveChatText.value.trim();
        if (!message || liveChatSending.value) return;
        liveChatSending.value = true;
        try {
            await api('/admin/discussions/send', { discussion_id: discussionId, message });
            liveChatText.value = '';
            await refreshLiveChats();
            scrollLiveChats(true);
        } catch (err) {
            console.error('Failed to send chat:', err);
        } finally {
            liveChatSending.value = false;
        }
    }

    return {
        dashboard, liveDiscussions,
        loadDashboard, startLivePolling, stopLivePolling,
        liveChatText, liveChatSending, sendLiveChat
    };
}

export { useDashboard };
