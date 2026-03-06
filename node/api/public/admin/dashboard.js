// dashboard.js — Dashboard view with live discussion polling

function useDashboard({ api, authenticated }) {
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

    async function loadLiveDiscussions() {
        if (!dashboard.value) return;

        const active = dashboard.value.discussions.filter(d => d.status === 'active');
        if (active.length === 0) {
            liveDiscussions.value = [];
            stopLivePolling();
            return;
        }

        const results = [];
        for (const d of active) {
            try {
                const detail = await api('/admin/discussions/detail', { discussion_id: d.id });
                const chat = await api('/admin/chat', { channel: 'discuss-' + d.id, limit: 500 });
                results.push({
                    discussion: detail.discussion,
                    participants: detail.participants,
                    votes: detail.votes,
                    chat: chat.messages.reverse()
                });
            } catch (err) {
                console.error('Failed to load live discussion ' + d.id + ':', err);
            }
        }

        liveDiscussions.value = results;
        scrollLiveChats(true);
        startLivePolling();
    }

    async function refreshLiveChats() {
        for (const live of liveDiscussions.value) {
            try {
                const chat = await api('/admin/chat', { channel: 'discuss-' + live.discussion.id, limit: 500 });
                live.chat = chat.messages.reverse();
            } catch (err) {
                console.error('Failed to refresh live chat:', err);
            }
        }
        scrollLiveChats();
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

    return {
        dashboard, liveDiscussions,
        loadDashboard, startLivePolling, stopLivePolling
    };
}

window.useDashboard = useDashboard;
