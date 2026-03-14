// main.js — Entry point for the admin dashboard (replaces app.js + CDN scripts)
import '@picocss/pico/css/pico.min.css';
import 'lucide-static/font/lucide.css';
import './style.css';

import { createApp, ref, computed, watch, onMounted, onUnmounted, provide } from 'vue';
import { useCore } from './core.js';
import { createEventsModule } from './events.js';
import { useAgents } from './agents.js';
import { useDiscussions } from './discussions.js';
import { useChat } from './chat.js';
import { useMail } from './mail.js';
import { useNotes } from './notes.js';
import { useApiLog } from './apilog.js';
import { useErrorLog } from './errorlog.js';
import { useConfig } from './config.js';
import { useActorsConfig } from './actors-config.js';
import { useDashboard } from './dashboard.js';

// View components
import DashboardView from './views/DashboardView.js';
import AgentsView from './views/AgentsView.js';
import CommsView from './views/CommsView.js';
import ConfigView from './views/ConfigView.js';
import NotesView from './views/NotesView.js';
import ActorDialogs from './views/ActorDialogs.js';
import AgentDialog from './views/AgentDialog.js';
import MiscDialogs from './views/MiscDialogs.js';

createApp({
    components: { DashboardView, AgentsView, CommsView, ConfigView, NotesView, ActorDialogs, AgentDialog, MiscDialogs },
    setup() {
        const currentView = ref('dashboard');

        const configSubTab = ref('actors');
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
        const dashboardModule = useDashboard(deps);
        const chatModule = useChat({ ...deps, dashboard: dashboardModule.dashboard });
        const mailModule = useMail({ ...deps, dashboard: dashboardModule.dashboard });
        const notesModule = useNotes(deps);
        const apiLogModule = useApiLog(deps);
        const errorLogModule = useErrorLog(deps);
        const configModule = useConfig(deps);
        const actorsConfigModule = useActorsConfig({ ...deps, agentsModule, user: core.user, permissions: core.permissions });

        // Navigate to the first permitted view (used on login/restore)
        function navigateToFirstPermitted() {
            const viewPerms = [
                ['dashboard', 'dashboard'],
                ['agents', 'agents'],
                ['comms', 'comms'],
                ['notes', 'notes'],
                ['config', 'config']
            ];
            for (const [view, resource] of viewPerms) {
                if (core.canDo(resource, 'read')) {
                    currentView.value = view;
                    return;
                }
            }
            // No permissions at all — stay on dashboard (will show empty)
            currentView.value = 'dashboard';
        }

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
                if (configSubTab.value === 'actors') actorsConfigModule.loadActorsConfig();
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
            actorsConfigModule.closeDialogs();
            core.cancelConfirm();
            core.showChangePassword.value = false;
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
        watch(configSubTab, (tab) => {
            if (currentView.value === 'config' && tab === 'actors') {
                actorsConfigModule.loadActorsConfig();
            }
        });

        // Lifecycle — store handler refs for proper cleanup
        function handleKeydown(e) {
            if (e.key === 'Escape') closeAllDialogs();
        }
        function handleDialogClick(e) {
            if (e.target.tagName === 'DIALOG') closeAllDialogs();
        }

        onMounted(() => {
            document.addEventListener('keydown', handleKeydown);
            document.addEventListener('click', handleDialogClick);
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
            notesModule.stopReindexPolling();
            eventsModule.disconnect();
            document.removeEventListener('keydown', handleKeydown);
            document.removeEventListener('click', handleDialogClick);
            document.removeEventListener('visibilitychange', handleVisibility);
        });

        // Login/logout wrappers that hook into polling
        function login() {
            core.login(() => {
                navigateToFirstPermitted();
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
                notesModule.stopReindexPolling();
                eventsModule.disconnect();
            });
        }

        // Flatten everything into the template namespace
        const appState = {
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
            // Actors Config
            ...actorsConfigModule,
            // Dashboard
            ...dashboardModule,
        };

        // Provide to child view components
        provide('app', appState);

        return appState;
    }
}).mount('#app');
