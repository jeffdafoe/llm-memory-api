// main.js — Entry point for the admin dashboard (replaces app.js + CDN scripts)
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

        // Theme
        const theme = ref(localStorage.getItem('admin_theme') || 'dark');
        document.documentElement.setAttribute('data-theme', theme.value);

        function toggleTheme() {
            theme.value = theme.value === 'dark' ? 'light' : 'dark';
            document.documentElement.setAttribute('data-theme', theme.value);
            localStorage.setItem('admin_theme', theme.value);
        }

        // ─── Hash-based tab persistence ───
        const validViews = new Set(['dashboard', 'agents', 'comms', 'notes', 'config']);
        const validConfigSubs = new Set(['actors', 'system', 'apilog', 'errorlog', 'templates']);
        const validCommSubs = new Set(['mail', 'chat', 'discussions']);
        let suppressHashUpdate = false;

        // Deep link for notes — stash namespace/slug from hash, open after notes load
        let pendingNoteLink = null;

        function readHash() {
            const hash = location.hash.replace(/^#\/?/, '');
            if (!hash) return;
            const slashIndex = hash.indexOf('/');
            const view = slashIndex === -1 ? hash : hash.substring(0, slashIndex);
            const rest = slashIndex === -1 ? '' : hash.substring(slashIndex + 1);
            if (!validViews.has(view)) return;
            suppressHashUpdate = true;
            currentView.value = view;
            if (view === 'config' && rest && validConfigSubs.has(rest)) configSubTab.value = rest;
            if (view === 'comms' && rest && validCommSubs.has(rest)) commSubTab.value = rest;
            // notes/namespace/slug — stash for opening after load
            if (view === 'notes' && rest) {
                const nsSlash = rest.indexOf('/');
                if (nsSlash !== -1) {
                    pendingNoteLink = {
                        namespace: rest.substring(0, nsSlash),
                        slug: rest.substring(nsSlash + 1)
                    };
                }
            }
            suppressHashUpdate = false;
        }

        function writeHash() {
            if (suppressHashUpdate) return;
            let hash = currentView.value;
            if (currentView.value === 'config') hash += '/' + configSubTab.value;
            if (currentView.value === 'comms') hash += '/' + commSubTab.value;
            if (currentView.value === 'notes' && notesModule.selectedNote.value) {
                hash += '/' + notesModule.selectedNote.value.namespace + '/' + notesModule.selectedNote.value.slug;
            }
            if (location.hash !== '#' + hash) {
                history.replaceState(null, '', '#' + hash);
            }
        }

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

        // Dashboard API log: deduplicate consecutive /mcp entries
        const dashboardApiLog = computed(() => {
            var result = [];
            var lastWasMcp = false;
            for (var entry of apiLogModule.apiLogEntries.value) {
                var isMcp = entry.path === '/mcp';
                if (isMcp && lastWasMcp) continue;
                result.push(entry);
                lastWasMcp = isMcp;
            }
            return result;
        });

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
                notesModule.loadNotes().then(() => {
                    if (pendingNoteLink) {
                        notesModule.openNote(pendingNoteLink.namespace, pendingNoteLink.slug);
                        pendingNoteLink = null;
                        writeHash();
                    }
                });
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

        // Watch view changes — also update URL hash
        watch(currentView, () => {
            writeHash();
            loadCurrentView();
        });
        // Update hash when a note is selected or deselected
        watch(() => notesModule.selectedNote.value, () => {
            if (currentView.value === 'notes') writeHash();
        });
        watch(commSubTab, () => {
            writeHash();
            if (currentView.value === 'comms') loadCurrentView();
        });
        watch(configSubTab, (tab) => {
            writeHash();
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

        function handleHashChange() {
            readHash();
            if (core.authenticated.value) loadCurrentView();
        }

        onMounted(() => {
            document.addEventListener('keydown', handleKeydown);
            document.addEventListener('click', handleDialogClick);
            document.addEventListener('visibilitychange', handleVisibility);
            window.addEventListener('hashchange', handleHashChange);

            if (core.restoreSession()) {
                readHash();
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
            window.removeEventListener('hashchange', handleHashChange);
        });

        // Login/logout wrappers that hook into polling
        function login() {
            core.login(() => {
                // If a hash was set before login (e.g. deep link), honor it
                readHash();
                if (!location.hash || location.hash === '#') {
                    navigateToFirstPermitted();
                }
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
            theme,
            toggleTheme,
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
            dashboardApiLog,
        };

        // Provide to child view components
        provide('app', appState);

        return appState;
    }
}).mount('#app');
