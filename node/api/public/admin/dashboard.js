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
            // Snapshot which transcripts are near the bottom BEFORE we replace
            // liveDiscussions.value. After the replace, Vue rerenders and the
            // element's scrollHeight grows for any new messages — checking
            // "atBottom" after the update misclassifies a user who WAS at the
            // bottom as no-longer-at-bottom, and auto-scroll skips. Keying by
            // discussion id lets us restore the right transcript's scroll.
            const nearBottomByDiscussionId = new Map();
            document.querySelectorAll('.live-discussion-panel').forEach(panel => {
                const transcript = panel.querySelector('.live-chat-transcript');
                if (!transcript) return;
                const discussionId = panel.getAttribute('data-discussion-id');
                if (!discussionId) return;
                // 200px of slack so a fresh long message doesn't exit the
                // "near bottom" zone in the tiny window between arrival and
                // the scroll fire.
                const nearBottom = (transcript.scrollHeight - transcript.scrollTop - transcript.clientHeight) < 200;
                nearBottomByDiscussionId.set(discussionId, nearBottom);
            });

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
            scrollLiveChats(false, nearBottomByDiscussionId);
        } finally {
            livePolling = false;
        }
    }

    // Scrolls each live-discussion transcript to the bottom.
    // - force=true: always scroll (used on initial load and after sending).
    // - nearBottomByDiscussionId: pre-refresh snapshot keyed by discussion id.
    //   An entry of `true` means the user was at the bottom before the refresh,
    //   so we should re-pin them to the bottom after the new messages render.
    //   Omitted entries / falsy values mean "leave scroll alone".
    function scrollLiveChats(force, nearBottomByDiscussionId) {
        nextTick(() => {
            document.querySelectorAll('.live-discussion-panel').forEach(panel => {
                const transcript = panel.querySelector('.live-chat-transcript');
                if (!transcript) return;
                if (force) {
                    transcript.scrollTop = transcript.scrollHeight;
                    return;
                }
                const discussionId = panel.getAttribute('data-discussion-id');
                if (nearBottomByDiscussionId && nearBottomByDiscussionId.get(discussionId)) {
                    transcript.scrollTop = transcript.scrollHeight;
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
