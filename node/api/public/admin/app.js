const { createApp, ref, onMounted, onUnmounted, watch, nextTick } = Vue;

const API_BASE = '/v1';

createApp({
    setup() {
        const authenticated = ref(false);
        const sessionToken = ref(null);
        const user = ref(null);
        const currentView = ref('dashboard');
        const loginForm = ref({ username: '', password: '' });
        const loginError = ref('');
        const loggingIn = ref(false);

        // Data
        const dashboard = ref(null);
        const agents = ref([]);
        const discussions = ref([]);
        const discussionFilter = ref('');
        const selectedDiscussion = ref(null);
        const discussionChat = ref(null);
        const chatMessages = ref([]);
        const selectedMessage = ref(null);
        const mailMessages = ref([]);
        const selectedMail = ref(null);
        const liveDiscussions = ref([]);

        // API helper
        async function api(endpoint, body = {}) {
            const headers = { 'Content-Type': 'application/json' };
            if (sessionToken.value) {
                headers['Authorization'] = 'Bearer ' + sessionToken.value;
            }
            const response = await fetch(API_BASE + endpoint, {
                method: 'POST',
                headers,
                body: JSON.stringify(body)
            });
            if (response.status === 401 || response.status === 403) {
                authenticated.value = false;
                sessionToken.value = null;
                localStorage.removeItem('admin_session');
                throw new Error('Session expired');
            }
            const data = await response.json();
            if (!response.ok) {
                throw new Error(data.error?.message || 'Request failed');
            }
            return data;
        }

        // Auth
        async function login() {
            loggingIn.value = true;
            loginError.value = '';
            try {
                const data = await api('/admin/login', loginForm.value);
                sessionToken.value = data.session_token;
                user.value = data.user;
                authenticated.value = true;
                localStorage.setItem('admin_session', JSON.stringify({
                    token: data.session_token,
                    user: data.user,
                    expires: data.expires_at
                }));
                loginForm.value = { username: '', password: '' };
                loadCurrentView();
                startPolling();
            } catch (err) {
                loginError.value = err.message;
            } finally {
                loggingIn.value = false;
            }
        }

        async function logout() {
            try {
                await api('/admin/logout');
            } catch (err) {
                // Ignore — clear local state regardless
            }
            stopPolling();
            stopLivePolling();
            authenticated.value = false;
            sessionToken.value = null;
            user.value = null;
            localStorage.removeItem('admin_session');
        }

        // Data loading
        async function loadDashboard() {
            try {
                const data = await api('/admin/dashboard');
                dashboard.value = data;
                loadLiveDiscussions();
            } catch (err) {
                console.error('Failed to load dashboard:', err);
            }
        }

        // Live discussion polling
        const LIVE_POLL_MS = 5000;
        let liveTimer = null;

        async function loadLiveDiscussions() {
            if (!dashboard.value) return;

            // MOCK DATA — remove after testing
            const now = new Date().toISOString();
            const fiveAgo = new Date(Date.now() - 5 * 60000).toISOString();
            const tenAgo = new Date(Date.now() - 10 * 60000).toISOString();
            const fifteenAgo = new Date(Date.now() - 15 * 60000).toISOString();
            liveDiscussions.value = [{
                discussion: {
                    id: 99,
                    topic: 'Best approach for implementing agent identity verification across all API endpoints',
                    status: 'active',
                    mode: 'realtime',
                    created_by: 'home',
                    created_at: fifteenAgo
                },
                participants: [
                    { agent: 'home', status: 'joined' },
                    { agent: 'work', status: 'joined' }
                ],
                votes: [
                    { id: 50, question: 'Should we enforce identity at middleware level? 1=yes 2=no 3=per-route', status: 'open', type: 'general' }
                ],
                chat: [
                    { id: 1001, from_agent: 'home', message: 'I think we should enforce agent identity at the middleware level. Every authenticated request already has a session — we just need to compare the agent field in the request body against the session agent.', sent_at: fifteenAgo },
                    { id: 1002, from_agent: 'work', message: 'Agreed on middleware approach. But what about routes that legitimately need to act on behalf of another agent? Like admin endpoints or system messages?', sent_at: tenAgo },
                    { id: 1003, from_agent: 'home', message: 'Good point. I was thinking we could have an allowlist of routes that skip the identity check. Or a flag on the session like "admin" that permits cross-agent operations.', sent_at: fiveAgo },
                    { id: 1004, from_agent: 'work', message: 'The session flag approach is cleaner. We already have subsystems on sessions — could add a capabilities array. I\'ll propose a vote on the middleware vs per-route question.', sent_at: now }
                ]
            }];
            scrollLiveChats();
            return;
            // END MOCK DATA

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
            scrollLiveChats();
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

        function scrollLiveChats() {
            nextTick(() => {
                document.querySelectorAll('.live-chat-transcript').forEach(el => {
                    el.scrollTop = el.scrollHeight;
                });
            });
        }

        function startLivePolling() {
            if (liveTimer) return;
            liveTimer = setInterval(() => {
                if (authenticated.value && currentView.value === 'dashboard') {
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

        async function loadAgents() {
            try {
                const data = await api('/admin/agents');
                agents.value = data.agents;
            } catch (err) {
                console.error('Failed to load agents:', err);
            }
        }

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

        async function loadChat() {
            try {
                const data = await api('/admin/chat', { limit: 100 });
                chatMessages.value = data.messages;
            } catch (err) {
                console.error('Failed to load chat:', err);
            }
        }

        async function loadMail() {
            try {
                const data = await api('/admin/mail', { limit: 100 });
                mailMessages.value = data.messages;
            } catch (err) {
                console.error('Failed to load mail:', err);
            }
        }

        function viewMail(msg) {
            selectedMail.value = msg;
        }

        function loadCurrentView() {
            if (currentView.value !== 'dashboard') {
                stopLivePolling();
            }
            if (currentView.value === 'dashboard') {
                loadDashboard();
            } else if (currentView.value === 'agents') {
                loadAgents();
            } else if (currentView.value === 'discussions') {
                loadDiscussions();
            } else if (currentView.value === 'chat') {
                loadChat();
            } else if (currentView.value === 'mail') {
                loadMail();
            }
        }

        // Formatting
        function formatDate(dateStr) {
            if (!dateStr) {
                return '—';
            }
            const d = new Date(dateStr);
            return d.toLocaleDateString() + ' ' + d.toLocaleTimeString();
        }

        const statusIcons = {
            active: 'icon-circle',
            concluded: 'icon-check',
            timed_out: 'icon-clock',
            pending: 'icon-circle',
            cancelled: 'icon-x'
        };

        function statusIcon(status) {
            return statusIcons[status] || 'icon-help-circle';
        }

        function closeAllDialogs() {
            selectedDiscussion.value = null;
            discussionChat.value = null;
            selectedMessage.value = null;
            selectedMail.value = null;
        }

        // Auto-refresh polling
        const POLL_INTERVAL_MS = 30000;
        let pollTimer = null;

        function startPolling() {
            stopPolling();
            pollTimer = setInterval(() => {
                if (authenticated.value) {
                    loadCurrentView();
                }
            }, POLL_INTERVAL_MS);
        }

        function stopPolling() {
            if (pollTimer) {
                clearInterval(pollTimer);
                pollTimer = null;
            }
        }

        function handleVisibility() {
            if (document.hidden) {
                stopPolling();
                stopLivePolling();
            } else {
                if (authenticated.value) {
                    loadCurrentView();
                }
                startPolling();
            }
        }

        // Watch view changes to load data
        watch(currentView, loadCurrentView);

        // Restore session from localStorage + global key/click handlers
        onMounted(() => {
            document.addEventListener('keydown', (e) => {
                if (e.key === 'Escape') {
                    closeAllDialogs();
                }
            });
            document.addEventListener('click', (e) => {
                if (e.target.tagName === 'DIALOG') {
                    closeAllDialogs();
                }
            });
            document.addEventListener('visibilitychange', handleVisibility);
            const saved = localStorage.getItem('admin_session');
            if (saved) {
                try {
                    const session = JSON.parse(saved);
                    if (new Date(session.expires) > new Date()) {
                        sessionToken.value = session.token;
                        user.value = session.user;
                        authenticated.value = true;
                        loadCurrentView();
                        startPolling();
                    } else {
                        localStorage.removeItem('admin_session');
                    }
                } catch (err) {
                    localStorage.removeItem('admin_session');
                }
            }
        });

        onUnmounted(() => {
            stopPolling();
            stopLivePolling();
            document.removeEventListener('visibilitychange', handleVisibility);
        });

        return {
            authenticated,
            currentView,
            loginForm,
            loginError,
            loggingIn,
            login,
            logout,
            dashboard,
            agents,
            discussions,
            discussionFilter,
            selectedDiscussion,
            discussionChat,
            viewDiscussion,
            loadDiscussionChat,
            chatMessages,
            selectedMessage,
            mailMessages,
            selectedMail,
            viewMail,
            liveDiscussions,
            loadDiscussions,
            formatDate,
            statusIcon
        };
    }
}).mount('#app');
