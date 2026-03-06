const { createApp, ref, computed, onMounted, onUnmounted, watch, nextTick } = Vue;

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
        const selectedAgent = ref(null);
        const agentInstructions = ref('');
        const agentInstructionsEditing = ref(false);
        const agentInstructionsEditContent = ref('');
        const agentInstructionsSaving = ref(false);
        const agentExpertise = ref([]);
        const agentExpertiseEditing = ref(false);
        const agentExpertiseEditText = ref('');
        const agentExpertiseSaving = ref(false);
        const agentPassphraseConfirming = ref(false);
        const agentNewPassphrase = ref(null);
        const discussions = ref([]);
        const discussionFilter = ref('');
        const selectedDiscussion = ref(null);
        const discussionChat = ref(null);
        const chatMessages = ref([]);
        const selectedMessage = ref(null);
        const mailMessages = ref([]);
        const selectedMail = ref(null);
        const mailComposing = ref(false);
        const mailTo = ref('');
        const mailSubject = ref('');
        const mailBody = ref('');
        const mailSending = ref(false);
        const liveDiscussions = ref([]);

        // Agent sub-tabs
        const agentSubTab = ref('list');

        // Agent creation
        const agentCreating = ref(false);
        const newAgentName = ref('');
        const newAgentProvider = ref('');
        const newAgentModel = ref('');
        const newAgentTemplateId = ref(null);
        const newAgentCreating = ref(false);
        const newAgentPassphrase = ref(null);

        // Welcome templates
        const welcomeTemplates = ref([]);
        const templateEditing = ref(false);
        const templateEditId = ref(null);
        const templateEditName = ref('');
        const templateEditDescription = ref('');
        const templateEditSubject = ref('');
        const templateEditBody = ref('');
        const templateSaving = ref(false);

        // API Log data
        const apiLogEntries = ref([]);
        const apiLogLastId = ref(0);
        const apiLogPaused = ref(false);
        const apiLogContainer = ref(null);
        const apiLogFilterAgent = ref('');
        const apiLogFilterStatus = ref('');
        const apiLogFilterPath = ref('');
        let apiLogTimer = null;

        // Notes data
        const notesNamespaces = ref([]);
        const notesTreesRaw = ref({});
        const expandedNamespaces = ref({});
        const expandedFolders = ref({});
        const selectedNote = ref(null);
        const notesEditing = ref(false);
        const notesEditTitle = ref('');
        const notesEditContent = ref('');
        const notesSaving = ref(false);
        const notesSearchQuery = ref('');
        const notesSearchResults = ref(null);
        const notesReindexing = ref(false);
        const reindexStatus = ref(null); // { running, current, total, result }
        let reindexPollTimer = null;

        // Reusable confirm/toast
        const confirmPrompt = ref(null);
        const toast = ref(null);
        let toastTimer = null;

        function showConfirm(message, action) {
            confirmPrompt.value = { message, action };
        }

        function executeConfirm() {
            if (confirmPrompt.value && confirmPrompt.value.action) {
                confirmPrompt.value.action();
            }
            confirmPrompt.value = null;
        }

        function showToast(text, type, duration) {
            if (toastTimer) clearTimeout(toastTimer);
            toast.value = { text, type: type || 'info' };
            toastTimer = setTimeout(() => { toast.value = null; }, duration || 5000);
        }

        // Computed: visible tree nodes per namespace (filters by expanded folders)
        const notesTrees = computed(() => {
            const result = {};
            for (const ns of notesNamespaces.value) {
                result[ns.namespace] = visibleTree(ns.namespace);
            }
            return result;
        });

        // Computed: unique agent names from log entries for filter dropdown
        const apiLogAgents = computed(() => {
            const agents = new Set();
            for (const e of apiLogEntries.value) {
                if (e.agent) agents.add(e.agent);
            }
            return [...agents].sort();
        });

        // Computed: filtered API log entries
        const apiLogFiltered = computed(() => {
            let entries = apiLogEntries.value;
            if (apiLogFilterAgent.value) {
                entries = entries.filter(e => e.agent === apiLogFilterAgent.value);
            }
            if (apiLogFilterStatus.value) {
                entries = entries.filter(e => statusCategory(e.status) === apiLogFilterStatus.value);
            }
            if (apiLogFilterPath.value) {
                const q = apiLogFilterPath.value.toLowerCase();
                entries = entries.filter(e => e.path && e.path.toLowerCase().includes(q));
            }
            return entries;
        });

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
                pollReindexStatus();
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
            stopApiLogPolling();
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
                    // Only auto-scroll if the user is already near the bottom,
                    // or if this is the initial load (force=true)
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

        // API Log polling
        const API_LOG_POLL_MS = 2000;
        const API_LOG_MAX_ENTRIES = 500;

        async function pollApiLog() {
            if (apiLogPaused.value) return;
            try {
                const data = await api('/admin/api-log', { since_id: apiLogLastId.value, limit: 200 });
                if (data.entries.length > 0) {
                    // Newest first — prepend in reverse so newest is at top
                    apiLogEntries.value.unshift(...data.entries.reverse());
                    // Trim the tail if we're over capacity
                    if (apiLogEntries.value.length > API_LOG_MAX_ENTRIES) {
                        apiLogEntries.value.length = API_LOG_MAX_ENTRIES;
                    }
                    apiLogLastId.value = data.entries[0].id; // entries already reversed, [0] is newest
                }
            } catch (err) {
                console.error('Failed to poll API log:', err);
            }
        }

        function startApiLogPolling() {
            if (apiLogTimer) return;
            pollApiLog();
            apiLogTimer = setInterval(() => {
                if (authenticated.value && (currentView.value === 'dashboard' || currentView.value === 'apilog')) {
                    pollApiLog();
                }
            }, API_LOG_POLL_MS);
        }

        function stopApiLogPolling() {
            if (apiLogTimer) {
                clearInterval(apiLogTimer);
                apiLogTimer = null;
            }
        }

        function statusCategory(status) {
            if (!status) return 'pending';
            if (status < 300) return 'ok';
            if (status < 400) return 'redirect';
            if (status < 500) return 'client';
            return 'server';
        }

        function formatBytes(bytes) {
            if (bytes === null || bytes === undefined) return '';
            if (bytes < 1024) return bytes + ' B';
            if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB';
            return (bytes / (1024 * 1024)).toFixed(1) + ' MB';
        }

        function formatTime(dateStr) {
            if (!dateStr) return '';
            const d = new Date(dateStr);
            return d.toLocaleTimeString();
        }

        async function loadAgents() {
            try {
                const data = await api('/admin/agents');
                agents.value = data.agents;
                // Re-select the same agent after refresh so the detail panel stays open
                if (selectedAgent.value) {
                    const updated = data.agents.find(a => a.agent === selectedAgent.value.agent);
                    if (updated) {
                        selectedAgent.value = updated;
                    }
                }
            } catch (err) {
                console.error('Failed to load agents:', err);
            }
        }

        async function viewAgent(agent) {
            selectedAgent.value = agent;
            agentInstructionsEditing.value = false;
            agentExpertiseEditing.value = false;
            agentPassphraseConfirming.value = false;
            agentNewPassphrase.value = null;
            // Parse expertise from the agent row (comes from agent_status view as JSON string)
            try {
                agentExpertise.value = typeof agent.expertise === 'string' ? JSON.parse(agent.expertise) : (agent.expertise || []);
            } catch (e) {
                agentExpertise.value = [];
            }
            try {
                const data = await api('/admin/agents/instructions/read', { agent: agent.agent });
                agentInstructions.value = data.instructions;
            } catch (err) {
                console.error('Failed to load agent instructions:', err);
                agentInstructions.value = '';
            }
        }

        function startEditInstructions() {
            agentInstructionsEditing.value = true;
            agentInstructionsEditContent.value = agentInstructions.value;
        }

        function cancelEditInstructions() {
            agentInstructionsEditing.value = false;
        }

        async function saveInstructions() {
            agentInstructionsSaving.value = true;
            try {
                await api('/admin/agents/instructions/save', {
                    agent: selectedAgent.value.agent,
                    content: agentInstructionsEditContent.value
                });
                agentInstructions.value = agentInstructionsEditContent.value;
                agentInstructionsEditing.value = false;
            } catch (err) {
                console.error('Failed to save instructions:', err);
                showToast('Failed to save: ' + err.message, 'error');
            } finally {
                agentInstructionsSaving.value = false;
            }
        }

        function startEditExpertise() {
            agentExpertiseEditing.value = true;
            agentExpertiseEditText.value = agentExpertise.value.join(', ');
        }

        function cancelEditExpertise() {
            agentExpertiseEditing.value = false;
        }

        async function saveExpertise() {
            agentExpertiseSaving.value = true;
            try {
                const items = agentExpertiseEditText.value
                    .split(',')
                    .map(s => s.trim())
                    .filter(s => s.length > 0);
                const data = await api('/admin/agents/expertise/save', {
                    agent: selectedAgent.value.agent,
                    expertise: items
                });
                agentExpertise.value = data.expertise;
                agentExpertiseEditing.value = false;
            } catch (err) {
                console.error('Failed to save expertise:', err);
                showToast('Failed to save: ' + err.message, 'error');
            } finally {
                agentExpertiseSaving.value = false;
            }
        }

        async function resetAgentPassphrase() {
            const agent = selectedAgent.value.agent;
            agentPassphraseConfirming.value = false;
            try {
                const data = await api('/admin/agents/reset-passphrase', { agent });
                agentNewPassphrase.value = data.passphrase;
                await loadAgents();
                const updated = agents.value.find(a => a.agent === agent);
                if (updated) selectedAgent.value = updated;
            } catch (err) {
                console.error('Failed to reset passphrase:', err);
            }
        }

        // ---- Agent Creation ----

        function startCreateAgent() {
            agentCreating.value = true;
            newAgentName.value = '';
            newAgentProvider.value = '';
            newAgentModel.value = '';
            newAgentTemplateId.value = null;
            newAgentCreating.value = false;
            newAgentPassphrase.value = null;
            // Load templates for dropdown
            loadTemplates();
        }

        async function createAgent() {
            if (!newAgentName.value) return;
            newAgentCreating.value = true;
            try {
                const body = { agent: newAgentName.value };
                if (newAgentProvider.value) body.provider = newAgentProvider.value;
                if (newAgentModel.value) body.model = newAgentModel.value;
                if (newAgentTemplateId.value) body.welcome_template_id = newAgentTemplateId.value;
                const data = await api('/admin/agents/create', body);
                newAgentPassphrase.value = data.passphrase;
                showToast('Agent "' + data.agent + '" created' + (data.welcome_mail_sent ? ' with welcome mail' : ''), 'success');
                loadAgents();
            } catch (err) {
                console.error('Failed to create agent:', err);
                showToast('Failed: ' + err.message, 'error');
            } finally {
                newAgentCreating.value = false;
            }
        }

        // ---- Welcome Templates ----

        async function loadTemplates() {
            try {
                const data = await api('/admin/templates/list');
                welcomeTemplates.value = data.templates;
            } catch (err) {
                console.error('Failed to load templates:', err);
            }
        }

        function startNewTemplate() {
            templateEditing.value = true;
            templateEditId.value = null;
            templateEditName.value = '';
            templateEditDescription.value = '';
            templateEditSubject.value = '';
            templateEditBody.value = '';
        }

        async function editTemplate(t) {
            try {
                const data = await api('/admin/templates/read', { id: t.id });
                const tpl = data.template;
                templateEditing.value = true;
                templateEditId.value = tpl.id;
                templateEditName.value = tpl.name;
                templateEditDescription.value = tpl.description || '';
                templateEditSubject.value = tpl.subject;
                templateEditBody.value = tpl.body;
            } catch (err) {
                console.error('Failed to read template:', err);
                showToast('Failed to load template: ' + err.message, 'error');
            }
        }

        async function saveTemplate() {
            templateSaving.value = true;
            try {
                const body = {
                    name: templateEditName.value,
                    description: templateEditDescription.value || null,
                    subject: templateEditSubject.value,
                    body: templateEditBody.value
                };
                if (templateEditId.value) body.id = templateEditId.value;
                await api('/admin/templates/save', body);
                templateEditing.value = false;
                showToast('Template saved', 'success');
                loadTemplates();
            } catch (err) {
                console.error('Failed to save template:', err);
                showToast('Failed: ' + err.message, 'error');
            } finally {
                templateSaving.value = false;
            }
        }

        function confirmDeleteTemplate(t) {
            showConfirm('Delete template "' + t.name + '"?', async () => {
                try {
                    await api('/admin/templates/delete', { id: t.id });
                    showToast('Template deleted', 'success');
                    loadTemplates();
                } catch (err) {
                    console.error('Failed to delete template:', err);
                    showToast('Failed: ' + err.message, 'error');
                }
            });
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

        function startComposeMail() {
            mailComposing.value = true;
            mailTo.value = '';
            mailSubject.value = '';
            mailBody.value = '';
        }

        function cancelComposeMail() {
            mailComposing.value = false;
        }

        async function sendMail() {
            if (!mailTo.value || !mailSubject.value || !mailBody.value) return;
            mailSending.value = true;
            try {
                await api('/admin/mail/send', {
                    to: mailTo.value,
                    subject: mailSubject.value,
                    body: mailBody.value
                });
                mailComposing.value = false;
                loadMail();
            } catch (err) {
                console.error('Failed to send mail:', err);
                showToast('Failed to send: ' + err.message, 'error');
            } finally {
                mailSending.value = false;
            }
        }

        // Notes functions

        // Build a flat tree structure from a list of slugs.
        // Each slug like "instructions/codebase/architecture.md" becomes
        // folder nodes for "instructions" and "codebase", plus a file node.
        function buildTree(notes) {
            const tree = [];
            const folders = new Set();

            // Sort slugs so folders appear before their children
            const sorted = [...notes].sort((a, b) => a.slug.localeCompare(b.slug));

            for (const note of sorted) {
                const parts = note.slug.split('/');
                // Add folder nodes for each directory level
                let path = '';
                for (let i = 0; i < parts.length - 1; i++) {
                    path = path ? path + '/' + parts[i] : parts[i];
                    if (!folders.has(path)) {
                        folders.add(path);
                        tree.push({
                            type: 'folder',
                            name: parts[i],
                            path: path,
                            depth: i + 1
                        });
                    }
                }

                // Add file node
                tree.push({
                    type: 'file',
                    name: parts[parts.length - 1],
                    slug: note.slug,
                    title: note.title,
                    depth: parts.length,
                    updated_at: note.updated_at
                });
            }

            // Filter: only show items whose parent folder is expanded (or top-level)
            return tree;
        }

        // Return visible tree nodes — only show children if their parent folder is expanded
        function visibleTree(namespace) {
            const allNodes = notesTreesRaw.value[namespace];
            if (!allNodes) return [];

            const result = [];
            for (const node of allNodes) {
                // Top-level items (depth 1) always visible
                if (node.depth === 1) {
                    result.push(node);
                    continue;
                }

                // For deeper items, check if the parent folder path is expanded
                let parentPath;
                if (node.type === 'folder') {
                    const lastSlash = node.path.lastIndexOf('/');
                    parentPath = lastSlash > 0 ? node.path.substring(0, lastSlash) : null;
                } else {
                    const lastSlash = node.slug.lastIndexOf('/');
                    parentPath = lastSlash > 0 ? node.slug.substring(0, lastSlash) : null;
                }

                if (!parentPath) {
                    result.push(node);
                } else if (expandedFolders.value[namespace + '/' + parentPath]) {
                    result.push(node);
                }
            }
            return result;
        }

        async function loadNotes() {
            try {
                const data = await api('/admin/notes/namespaces');
                notesNamespaces.value = data.namespaces;

                // Load notes for each namespace and build trees
                for (const ns of data.namespaces) {
                    const notesData = await api('/admin/notes/list', { namespace: ns.namespace, limit: 500 });
                    notesTreesRaw.value[ns.namespace] = buildTree(notesData.notes);
                }
            } catch (err) {
                console.error('Failed to load notes:', err);
            }
        }

        function toggleNamespace(namespace) {
            expandedNamespaces.value[namespace] = !expandedNamespaces.value[namespace];
        }

        function toggleFolder(namespace, path) {
            const key = namespace + '/' + path;
            expandedFolders.value[key] = !expandedFolders.value[key];
        }

        async function openNote(namespace, slug) {
            notesEditing.value = false;
            try {
                const data = await api('/admin/notes/read', { namespace, slug });
                selectedNote.value = { ...data.note, namespace };
            } catch (err) {
                console.error('Failed to open note:', err);
            }
        }

        async function openNoteFromSearch(result) {
            // Search results have namespace and source_file (which is the slug)
            await openNote(result.namespace, result.source_file);
        }

        function startEditNote() {
            notesEditing.value = true;
            notesEditTitle.value = selectedNote.value.title;
            notesEditContent.value = selectedNote.value.content;
        }

        function cancelEditNote() {
            notesEditing.value = false;
        }

        async function saveEditedNote() {
            notesSaving.value = true;
            try {
                await api('/admin/notes/save', {
                    namespace: selectedNote.value.namespace,
                    slug: selectedNote.value.slug,
                    title: notesEditTitle.value,
                    content: notesEditContent.value
                });
                // Refresh the note
                selectedNote.value.title = notesEditTitle.value;
                selectedNote.value.content = notesEditContent.value;
                notesEditing.value = false;
            } catch (err) {
                console.error('Failed to save note:', err);
                showToast('Failed to save: ' + err.message, 'error');
            } finally {
                notesSaving.value = false;
            }
        }

        function confirmDeleteNote() {
            const ns = selectedNote.value.namespace;
            const slug = selectedNote.value.slug;
            showConfirm('Delete "' + slug + '"?', async () => {
                try {
                    await api('/admin/notes/delete', { namespace: ns, slug });
                    selectedNote.value = null;
                    await loadNotes();
                    showToast('Note deleted', 'success');
                } catch (err) {
                    console.error('Failed to delete note:', err);
                    showToast('Failed to delete: ' + err.message, 'error');
                }
            });
        }

        async function searchNotes() {
            if (!notesSearchQuery.value.trim()) return;
            try {
                const data = await api('/admin/notes/search', {
                    query: notesSearchQuery.value,
                    namespace: '*',
                    limit: 15
                });
                notesSearchResults.value = data.results;
            } catch (err) {
                console.error('Search failed:', err);
            }
        }

        function reindexNotes() {
            showConfirm('Delete ALL vector chunks and re-ingest every note? This may take a while.', async () => {
                try {
                    await api('/admin/notes/reindex');
                    startReindexPolling();
                } catch (err) {
                    console.error('Reindex failed:', err);
                    showToast('Reindex failed: ' + err.message, 'error');
                }
            });
        }

        async function pollReindexStatus() {
            try {
                const data = await api('/admin/notes/reindex-status');
                reindexStatus.value = data;
                notesReindexing.value = data.running;
                if (!data.running) {
                    stopReindexPolling();
                    if (data.result && !data.result.error) {
                        let msg = 'Reindex complete: ' + data.result.docs_indexed + ' docs, ' + data.result.chunks_created + ' chunks';
                        if (data.result.errors && data.result.errors.length > 0) {
                            msg += ' (' + data.result.errors.length + ' errors)';
                        }
                        showToast(msg, (data.result.errors && data.result.errors.length > 0) ? 'error' : 'success', 8000);
                    } else if (data.result && data.result.error) {
                        showToast('Reindex failed: ' + data.result.error, 'error');
                    }
                    // Clear server-side state
                    api('/admin/notes/reindex-clear').catch(() => {});
                    reindexStatus.value = null;
                }
            } catch (err) {
                console.error('Reindex status poll failed:', err);
            }
        }

        function startReindexPolling() {
            pollReindexStatus();
            if (!reindexPollTimer) {
                reindexPollTimer = setInterval(pollReindexStatus, 2000);
            }
        }

        function stopReindexPolling() {
            if (reindexPollTimer) {
                clearInterval(reindexPollTimer);
                reindexPollTimer = null;
            }
        }

        function loadCurrentView() {
            if (currentView.value !== 'dashboard') {
                stopLivePolling();
            }
            if (currentView.value !== 'dashboard' && currentView.value !== 'apilog') {
                stopApiLogPolling();
            }
            if (currentView.value === 'dashboard') {
                loadDashboard();
                startApiLogPolling();
            } else if (currentView.value === 'apilog') {
                startApiLogPolling();
            } else if (currentView.value === 'agents') {
                loadAgents();
            } else if (currentView.value === 'discussions') {
                loadDiscussions();
            } else if (currentView.value === 'chat') {
                loadChat();
            } else if (currentView.value === 'mail') {
                loadMail();
                if (agents.value.length === 0) loadAgents();
            } else if (currentView.value === 'notes') {
                loadNotes();
            }
        }

        // Formatting
        function formatDate(dateStr) {
            if (!dateStr) {
                return '—';
            }
            const d = new Date(dateStr);
            return d.toLocaleDateString() + ' ' + d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
        }

        function formatShortDate(dateStr) {
            if (!dateStr) return '';
            const d = new Date(dateStr);
            return d.toLocaleDateString('en-US', { month: 'short', day: 'numeric' }) + ', ' +
                   d.toLocaleTimeString('en-US', { hour: 'numeric', minute: '2-digit' });
        }

        function timeAgo(dateStr) {
            if (!dateStr) return '';
            const now = Date.now();
            const then = new Date(dateStr).getTime();
            const seconds = Math.floor((now - then) / 1000);
            if (seconds < 60) return 'just now';
            const minutes = Math.floor(seconds / 60);
            if (minutes < 60) return minutes + 'm ago';
            const hours = Math.floor(minutes / 60);
            if (hours < 24) return hours + 'h ago';
            const days = Math.floor(hours / 24);
            if (days < 30) return days + 'd ago';
            const months = Math.floor(days / 30);
            return months + 'mo ago';
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

        const agentColors = {
            home: '#5b9bd5',
            work: '#e07b53',
            system: '#888'
        };
        const fallbackColors = ['#8e6bbf', '#4caf88', '#c9a83e', '#d46a8e'];

        function agentColor(agent) {
            if (agentColors[agent]) {
                return agentColors[agent];
            }
            let hash = 0;
            for (let i = 0; i < agent.length; i++) {
                hash = agent.charCodeAt(i) + ((hash << 5) - hash);
            }
            return fallbackColors[Math.abs(hash) % fallbackColors.length];
        }

        function voteQuestion(text) {
            const match = text.match(/^(.*?\?)\s*(.+)$/);
            if (!match) {
                return { question: text, choices: [] };
            }
            const choiceMatches = match[2].match(/\d+=\S+/g);
            if (choiceMatches) {
                return { question: match[1], choices: choiceMatches };
            }
            return { question: text, choices: [] };
        }

        function closeAllDialogs() {
            selectedDiscussion.value = null;
            discussionChat.value = null;
            selectedMessage.value = null;
            selectedMail.value = null;
            selectedAgent.value = null;
            agentCreating.value = false;
            confirmPrompt.value = null;
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
                stopApiLogPolling();
            } else {
                if (authenticated.value) {
                    loadCurrentView();
                }
                startPolling();
            }
        }

        // Watch view changes to load data — clear agent selection on navigate
        watch(currentView, () => {
            loadCurrentView();
        });

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
                        pollReindexStatus();
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
            stopApiLogPolling();
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
            selectedAgent,
            agentInstructions,
            agentInstructionsEditing,
            agentInstructionsEditContent,
            agentInstructionsSaving,
            agentExpertise,
            agentExpertiseEditing,
            agentExpertiseEditText,
            agentExpertiseSaving,
            agentPassphraseConfirming,
            agentNewPassphrase,
            viewAgent,
            startEditInstructions,
            cancelEditInstructions,
            saveInstructions,
            startEditExpertise,
            cancelEditExpertise,
            saveExpertise,
            resetAgentPassphrase,
            agentSubTab,
            agentCreating,
            newAgentName,
            newAgentProvider,
            newAgentModel,
            newAgentTemplateId,
            newAgentCreating,
            newAgentPassphrase,
            startCreateAgent,
            createAgent,
            welcomeTemplates,
            templateEditing,
            templateEditId,
            templateEditName,
            templateEditDescription,
            templateEditSubject,
            templateEditBody,
            templateSaving,
            startNewTemplate,
            editTemplate,
            saveTemplate,
            confirmDeleteTemplate,
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
            mailComposing,
            mailTo,
            mailSubject,
            mailBody,
            mailSending,
            startComposeMail,
            cancelComposeMail,
            sendMail,
            liveDiscussions,
            loadDiscussions,
            apiLogEntries,
            apiLogPaused,
            apiLogContainer,
            apiLogFilterAgent,
            apiLogFilterStatus,
            apiLogFilterPath,
            apiLogAgents,
            apiLogFiltered,
            pollApiLog,
            statusCategory,
            formatBytes,
            formatTime,
            formatDate,
            timeAgo,
            formatShortDate,
            statusIcon,
            agentColor,
            voteQuestion,
            notesNamespaces,
            notesTrees,
            expandedNamespaces,
            expandedFolders,
            selectedNote,
            notesEditing,
            notesEditTitle,
            notesEditContent,
            notesSaving,
            notesSearchQuery,
            notesSearchResults,
            notesReindexing,
            reindexStatus,
            reindexNotes,
            toggleNamespace,
            toggleFolder,
            openNote,
            openNoteFromSearch,
            startEditNote,
            cancelEditNote,
            saveEditedNote,
            confirmDeleteNote,
            searchNotes,
            confirmPrompt,
            executeConfirm,
            toast,
            showToast
        };
    }
}).mount('#app');
