const { createApp, ref, onMounted, onUnmounted, watch } = Vue;

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
            } catch (err) {
                console.error('Failed to load dashboard:', err);
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
            loadDiscussions,
            formatDate
        };
    }
}).mount('#app');
