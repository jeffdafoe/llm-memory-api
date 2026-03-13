// app.js — Thin shell that composes all feature modules
// Vue globals (ref, computed, etc.) are destructured in core.js

createApp({
    setup() {
        const currentView = ref('dashboard');

        const configSubTab = ref('system');
        const commSubTab = ref('mail');

        const viewTitles = {
            dashboard: 'Dashboard',
            agents: 'Agents',
            comms: 'Communications',
            notes: 'Notes',
            config: 'Configuration',
        };
        const viewTitle = computed(() => viewTitles[currentView.value] || currentView.value);

        // Core: auth, api, formatting, confirm/toast
        const core = useCore();

        // Real-time event stream via WebSocket
        const eventsModule = createEventsModule();

        // Feature composables — each gets the shared deps it needs
        const deps = { api: core.api, showToast: core.showToast, showConfirm: core.showConfirm, authenticated: core.authenticated, onEvent: eventsModule.onEvent };

        const agentsModule = useAgents(deps);
        const discussionsModule = useDiscussions(deps);
        const chatModule = useChat(deps);
        const mailModule = useMail(deps);
        const notesModule = useNotes(deps);
        const apiLogModule = useApiLog(deps);
        const errorLogModule = useErrorLog(deps);
        const configModule = useConfig(deps);
        const permissionsModule = usePermissions(deps);
        const dashboardModule = useDashboard(deps);

        // View loading — route data fetches to the right module
        function loadCurrentView() {
            if (currentView.value !== 'dashboard') {
                dashboardModule.stopLivePolling();
            }
            if (currentView.value !== 'dashboard' && currentView.value !== 'config') {
                apiLogModule.stopApiLogPolling();
                errorLogModule.stopErrorLogPolling();
            }
            if (currentView.value === 'dashboard') {
                dashboardModule.loadDashboard();
                apiLogModule.startApiLogPolling();
                errorLogModule.startErrorLogPolling();
            } else if (currentView.value === 'config') {
                configModule.loadConfig();
                if (configSubTab.value === 'permissions') permissionsModule.loadPermissions();
                apiLogModule.startApiLogPolling();
                errorLogModule.startErrorLogPolling();
            } else if (currentView.value === 'agents') {
                agentsModule.loadAgents();
                if (configModule.configEntries.value.length === 0) configModule.loadConfig();
            } else if (currentView.value === 'comms') {
                if (commSubTab.value === 'discussions') discussionsModule.loadDiscussions();
                else if (commSubTab.value === 'chat') chatModule.loadChat();
                else if (commSubTab.value === 'mail') {
                    mailModule.loadMail();
                    if (agentsModule.agents.value.length === 0) agentsModule.loadAgents();
                }
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
                errorLogModule.stopErrorLogPolling();
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
        watch(commSubTab, () => {
            if (currentView.value === 'comms') loadCurrentView();
        });
        watch(configSubTab, () => {
            if (currentView.value === 'config') {
                if (configSubTab.value === 'permissions') permissionsModule.loadPermissions();
            }
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
                eventsModule.connect(core.sessionToken.value);
            }
        });

        onUnmounted(() => {
            stopPolling();
            dashboardModule.stopLivePolling();
            apiLogModule.stopApiLogPolling();
            errorLogModule.stopErrorLogPolling();
            eventsModule.disconnect();
            document.removeEventListener('visibilitychange', handleVisibility);
        });

        // Login/logout wrappers that hook into polling
        function login() {
            core.login(() => {
                loadCurrentView();
                startPolling();
                notesModule.pollReindexStatus();
                eventsModule.connect(core.sessionToken.value);
            });
        }

        function logout() {
            core.logout(() => {
                stopPolling();
                dashboardModule.stopLivePolling();
                apiLogModule.stopApiLogPolling();
                errorLogModule.stopErrorLogPolling();
                eventsModule.disconnect();
            });
        }

        // Flatten everything into the template namespace
        return {
            currentView,
            viewTitle,
            configSubTab,
            commSubTab,
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
            // Error Log
            ...errorLogModule,
            // Config
            ...configModule,
            // Permissions
            ...permissionsModule,
            // Dashboard
            ...dashboardModule,
        };
    }
}).mount('#app');
