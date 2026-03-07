// app.js — Thin shell that composes all feature modules
// Vue globals (ref, computed, etc.) are destructured in core.js

createApp({
    setup() {
        const currentView = ref('dashboard');

        const configSubTab = ref('system');

        const viewTitles = {
            dashboard: 'Dashboard',
            agents: 'Agents',
            discussions: 'Discussions',
            chat: 'Chat',
            mail: 'Mail',
            notes: 'Notes',
            config: 'Configuration',
        };
        const viewTitle = computed(() => viewTitles[currentView.value] || currentView.value);

        // Core: auth, api, formatting, confirm/toast
        const core = useCore();

        // Feature composables — each gets the shared deps it needs
        const deps = { api: core.api, showToast: core.showToast, showConfirm: core.showConfirm, authenticated: core.authenticated };

        const agentsModule = useAgents(deps);
        const discussionsModule = useDiscussions(deps);
        const chatModule = useChat(deps);
        const mailModule = useMail(deps);
        const notesModule = useNotes(deps);
        const apiLogModule = useApiLog(deps);
        const configModule = useConfig(deps);
        const dashboardModule = useDashboard(deps);

        // View loading — route data fetches to the right module
        function loadCurrentView() {
            if (currentView.value !== 'dashboard') {
                dashboardModule.stopLivePolling();
            }
            if (currentView.value !== 'dashboard' && currentView.value !== 'config') {
                apiLogModule.stopApiLogPolling();
            }
            if (currentView.value === 'dashboard') {
                dashboardModule.loadDashboard();
                apiLogModule.startApiLogPolling();
            } else if (currentView.value === 'config') {
                configModule.loadConfig();
                apiLogModule.startApiLogPolling();
            } else if (currentView.value === 'agents') {
                agentsModule.loadAgents();
            } else if (currentView.value === 'discussions') {
                discussionsModule.loadDiscussions();
            } else if (currentView.value === 'chat') {
                chatModule.loadChat();
            } else if (currentView.value === 'mail') {
                mailModule.loadMail();
                if (agentsModule.agents.value.length === 0) agentsModule.loadAgents();
            } else if (currentView.value === 'notes') {
                notesModule.loadNotes();
            }
        }

        function closeAllDialogs() {
            agentsModule.closeDialogs();
            discussionsModule.closeDialogs();
            chatModule.closeDialogs();
            mailModule.closeDialogs();
            core.confirmPrompt.value = null;
        }

        // Auto-refresh polling
        const POLL_INTERVAL_MS = 30000;
        let pollTimer = null;

        function startPolling() {
            stopPolling();
            pollTimer = setInterval(() => {
                if (core.authenticated.value) {
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
                dashboardModule.stopLivePolling();
                apiLogModule.stopApiLogPolling();
            } else {
                if (core.authenticated.value) {
                    loadCurrentView();
                }
                startPolling();
            }
        }

        // Watch view changes
        watch(currentView, () => {
            loadCurrentView();
        });

        // Lifecycle
        onMounted(() => {
            document.addEventListener('keydown', (e) => {
                if (e.key === 'Escape') closeAllDialogs();
            });
            document.addEventListener('click', (e) => {
                if (e.target.tagName === 'DIALOG') closeAllDialogs();
            });
            document.addEventListener('visibilitychange', handleVisibility);

            if (core.restoreSession()) {
                loadCurrentView();
                startPolling();
                notesModule.pollReindexStatus();
            }
        });

        onUnmounted(() => {
            stopPolling();
            dashboardModule.stopLivePolling();
            apiLogModule.stopApiLogPolling();
            document.removeEventListener('visibilitychange', handleVisibility);
        });

        // Login/logout wrappers that hook into polling
        function login() {
            core.login(() => {
                loadCurrentView();
                startPolling();
                notesModule.pollReindexStatus();
            });
        }

        function logout() {
            core.logout(() => {
                stopPolling();
                dashboardModule.stopLivePolling();
                apiLogModule.stopApiLogPolling();
            });
        }

        // Flatten everything into the template namespace
        return {
            currentView,
            viewTitle,
            configSubTab,
            // Core
            ...core,
            login,
            logout,
            // Agents
            ...agentsModule,
            // Discussions
            ...discussionsModule,
            // Chat
            ...chatModule,
            // Mail
            ...mailModule,
            // Notes
            ...notesModule,
            // API Log
            ...apiLogModule,
            // Config
            ...configModule,
            // Dashboard
            ...dashboardModule,
        };
    }
}).mount('#app');
