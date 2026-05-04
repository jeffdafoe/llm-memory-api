// actors-config.js — Actor permissions, visibility, and creation management (Configuration > Actors tab)
import { ref, computed, watch } from 'vue';
import { useSortable } from './core.js';
import { safeInt } from './util.js';

function useActorsConfig({ api, showToast, showConfirm, agentsModule, user, permissions }) {
    const actorsConfigList = ref([]);
    const actorsConfigLoading = ref(false);

    // Sortable table — default sort by name
    const actorSort = useSortable(actorsConfigList, 'name', 'asc');

    // Detail dialog state
    const selectedActorConfig = ref(null);
    const actorConfigLoading = ref(false);
    const actorDialogMode = ref('edit'); // 'edit' or 'create'

    // Agent configuration editing state (provider, model, personality, etc.)
    const editAgentProvider = ref('');
    const editAgentModel = ref('');
    const editAgentPersonality = ref('');
    const editAgentApiKey = ref('');
    const editAgentDreamMode = ref('none');
    const editAgentLearningEnabled = ref(true);
    const editAgentConfig = ref({});
    const agentConfigSaving = ref(false);

    // Permissions state
    const actorPermissions = ref([]);
    const actorHasWildcardPerm = ref(false);

    // Visibility state
    const actorVisibilityGrants = ref([]);
    const actorHasWildcardVis = ref(false);

    // Combined save state
    const actorConfigSaving = ref(false);

    // Available namespaces (for dropdown)
    const availableNamespaces = ref([]);
    const newNamespaceInput = ref('');

    // Available actors (for visibility dropdown)
    const newVisibilityTarget = ref('');

    // Admin permissions state
    const actorAdminPerms = ref([]);
    const actorHasWildcardAdmin = ref(false);

    // Known admin resources and their possible actions
    const adminResources = [
        { id: 'dashboard', label: 'Dashboard', actions: ['read'], hint: 'System overview: agent status, activity, stats' },
        { id: 'agents', label: 'Agents', actions: ['read', 'write'], hint: 'Agent list, instructions, passphrase reset, configuration' },
        { id: 'comms', label: 'Communications', actions: ['read', 'write', 'delete'], hint: 'Mail, chat, and discussions between agents' },
        { id: 'notes', label: 'Memories', actions: ['read', 'write', 'delete'], hint: 'Browse, edit, and manage stored memories across namespaces' },
        { id: 'config', label: 'Configuration', actions: ['read', 'write'], hint: 'System settings: search tuning, reindexing, maintenance' },
        { id: 'actors', label: 'Actors', actions: ['read', 'write'], hint: 'Actor management: permissions, visibility, passwords, create/edit actors' },
        { id: 'templates', label: 'Templates', actions: ['read', 'write', 'delete'], hint: 'Welcome and onboarding message templates' },
        { id: 'logs', label: 'Logs', actions: ['read'], hint: 'API request logs and error logs' },
        { id: 'access', label: 'Access', actions: ['read', 'write'], hint: 'Access requests from the landing page' }
    ];

    // Virtual agent access state
    const vaAccessList = ref([]);
    const newVaAccessTarget = ref('');
    const vaAccessPublic = ref(false);

    // UI Access (password) state
    const actorPasswordInput = ref('');
    const actorPasswordSaving = ref(false);

    // Delete state
    const actorDeleting = ref(false);

    // ─── Create Actor ───
    const newActorName = ref('');
    const newActorVirtual = ref(false);
    const newActorUiAccess = ref(false);
    const newActorPassword = ref('');
    const newActorTemplateId = ref(null);
    const newActorNoteTemplateId = ref(null);
    const newActorCreating = ref(false);
    const newActorPassphrase = ref(null);
    const createSource = ref('config'); // 'config' (full) or 'agents' (streamlined virtual agent)

    // Live name availability check — debounced, calls /api/check-name
    const nameCheckStatus = ref(''); // '', 'checking', 'available', 'taken', 'invalid'
    const nameCheckMessage = ref('');
    let nameCheckTimer = null;

    watch(newActorName, (val) => {
        const name = (val || '').trim().toLowerCase();
        clearTimeout(nameCheckTimer);

        if (!name) {
            nameCheckStatus.value = '';
            nameCheckMessage.value = '';
            return;
        }

        // Client-side format validation (same regex as registration form)
        if (!/^[a-z][a-z0-9_-]{1,30}$/.test(name)) {
            nameCheckStatus.value = 'invalid';
            nameCheckMessage.value = 'Letters, numbers, hyphens, underscores. Must start with a letter.';
            return;
        }

        nameCheckStatus.value = 'checking';
        nameCheckMessage.value = 'Checking...';

        nameCheckTimer = setTimeout(async () => {
            try {
                const res = await fetch('/api/check-name', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ name })
                });
                const data = await res.json();
                // Guard against stale responses — only update if the name still matches
                if ((newActorName.value || '').trim().toLowerCase() !== name) return;
                if (data.available) {
                    nameCheckStatus.value = 'available';
                    nameCheckMessage.value = 'Available';
                } else {
                    nameCheckStatus.value = 'taken';
                    nameCheckMessage.value = data.reason || 'Name taken';
                }
            } catch (err) {
                nameCheckStatus.value = 'taken';
                nameCheckMessage.value = 'Error checking name';
            }
        }, 400);
    });

    async function loadActorsConfig() {
        actorsConfigLoading.value = true;
        try {
            const data = await api('/admin/actors/list');
            actorsConfigList.value = data.actors;
        } catch (err) {
            console.error('Failed to load actors:', err);
        } finally {
            actorsConfigLoading.value = false;
        }
    }

    async function openActorConfig(actor) {
        // Accept actor object or name string
        if (typeof actor === 'string') {
            // Load actors list if needed, then find by name
            if (!actorsConfigList.value.length) {
                await loadActorsConfig();
            }
            actor = actorsConfigList.value.find(a => a.name === actor);
            if (!actor) return;
        }
        actorDialogMode.value = 'edit';
        selectedActorConfig.value = actor;
        actorConfigLoading.value = true;
        newNamespaceInput.value = '';
        newVisibilityTarget.value = '';

        // Load provider registry for agent config editing
        agentsModule.loadProviderRegistry();
        try {
            // Load permissions, visibility, namespaces, admin perms, and VA access in parallel
            const promises = [
                api('/admin/actors/permissions/read', { actor_id: actor.id }),
                api('/admin/actors/visibility/read', { actor_id: actor.id }),
                api('/admin/actors/namespaces'),
                actor.is_user ? api('/admin/actors/admin-permissions/read', { actor_id: actor.id }) : Promise.resolve(null),
                actor.virtual ? api('/admin/virtual-agent-access/list') : Promise.resolve(null)
            ];
            const [permData, visData, nsData, adminPermData, vaData] = await Promise.all(promises);

            // Permissions: separate wildcard from specific
            const wildPerm = permData.permissions.find(p => p.namespace === '/');
            actorHasWildcardPerm.value = !!wildPerm;
            actorPermissions.value = permData.permissions
                .filter(p => p.namespace !== '/')
                .map(p => ({ ...p }));

            // Visibility
            actorHasWildcardVis.value = visData.wildcard;
            actorVisibilityGrants.value = visData.grants.map(g => ({
                ...g,
                target_actor_id: safeInt(g.target_actor_id)
            }));

            // Available namespaces
            availableNamespaces.value = nsData.namespaces;

            // Admin permissions (only for UI users)
            if (adminPermData) {
                const hasWild = adminPermData.permissions.some(p => p.resource === '*' && p.action === '*');
                actorHasWildcardAdmin.value = hasWild;
                // Build a map of resource -> highest action granted
                const permMap = {};
                for (const p of adminPermData.permissions) {
                    if (p.resource === '*') continue;
                    if (!permMap[p.resource]) permMap[p.resource] = new Set();
                    permMap[p.resource].add(p.action);
                }
                actorAdminPerms.value = adminResources.map(r => ({
                    resource: r.id,
                    label: r.label,
                    hint: r.hint,
                    actions: r.actions,
                    granted: permMap[r.id] ? Array.from(permMap[r.id]) : []
                }));
            } else {
                actorHasWildcardAdmin.value = false;
                actorAdminPerms.value = [];
            }

            // Agent configuration (provider, model, personality, config)
            if (actor.is_agent) {
                editAgentProvider.value = actor.provider || '';
                editAgentModel.value = actor.model || '';
                editAgentPersonality.value = actor.personality || '';
                editAgentApiKey.value = '';
                // Load configuration from the agent's stored config
                let cfg = {};
                if (actor.configuration) {
                    if (typeof actor.configuration === 'object') cfg = actor.configuration;
                    else try { cfg = JSON.parse(actor.configuration); } catch (e) { /* ignore */ }
                }
                editAgentConfig.value = { ...cfg };
            } else {
                editAgentProvider.value = '';
                editAgentModel.value = '';
                editAgentPersonality.value = '';
                editAgentApiKey.value = '';
                editAgentConfig.value = {};
            }

            // Virtual agent access
            if (vaData) {
                const myAccess = vaData.access.filter(a => a.virtual_agent_id === actor.id);
                vaAccessPublic.value = myAccess.some(a => a.grantee_actor_id === null);
                vaAccessList.value = myAccess.filter(a => a.grantee_actor_id !== null);
            } else {
                vaAccessPublic.value = false;
                vaAccessList.value = [];
            }
        } catch (err) {
            console.error('Failed to load actor config:', err);
            showToast('Failed to load actor config', 'error');
        } finally {
            actorConfigLoading.value = false;
        }
    }

    function closeActorConfig() {
        selectedActorConfig.value = null;
        actorPermissions.value = [];
        actorHasWildcardPerm.value = false;
        actorVisibilityGrants.value = [];
        actorHasWildcardVis.value = false;
        newNamespaceInput.value = '';
        newVisibilityTarget.value = '';
        actorPasswordInput.value = '';
        actorAdminPerms.value = [];
        actorHasWildcardAdmin.value = false;
    }

    // ─── Permissions ───

    function addPermissionRow() {
        const ns = newNamespaceInput.value.trim();
        if (!ns) return;
        if (ns === '/' || ns === '*') {
            showToast('Reserved namespace — use the wildcard toggle instead', 'error');
            return;
        }
        // Check for duplicate
        if (actorPermissions.value.some(p => p.namespace === ns)) {
            showToast('Namespace already in list', 'error');
            return;
        }
        actorPermissions.value.push({
            namespace: ns,
            can_read: true,
            can_write: false,
            can_delete: false
        });
        newNamespaceInput.value = '';
    }

    function removePermissionRow(index) {
        actorPermissions.value.splice(index, 1);
    }

    // ─── Visibility ───

    function addVisibilityGrant() {
        const targetId = safeInt(newVisibilityTarget.value);
        if (!targetId) return;
        // Check for duplicate
        if (actorVisibilityGrants.value.some(g => g.target_actor_id === targetId)) {
            showToast('Actor already in list', 'error');
            return;
        }
        // Find actor info from the actors list
        const target = actorsConfigList.value.find(a => a.id === targetId);
        if (!target) return;
        actorVisibilityGrants.value.push({
            target_actor_id: target.id,
            target_name: target.name,
            target_is_agent: target.is_agent,
            target_is_user: target.is_user
        });
        newVisibilityTarget.value = '';
    }

    function removeVisibilityGrant(index) {
        actorVisibilityGrants.value.splice(index, 1);
    }

    // ─── Save All (permissions + visibility) ───

    async function saveActorConfig() {
        if (!selectedActorConfig.value) return;
        actorConfigSaving.value = true;
        try {
            // Build namespace permissions list
            const nsPerms = [];
            if (actorHasWildcardPerm.value) {
                nsPerms.push({ namespace: '/', can_read: true, can_write: true, can_delete: true });
            }
            for (const p of actorPermissions.value) {
                nsPerms.push({
                    namespace: p.namespace,
                    can_read: !!p.can_read,
                    can_write: !!p.can_write,
                    can_delete: !!p.can_delete
                });
            }

            // Build admin permissions list (only for UI users)
            const savePromises = [
                api('/admin/actors/permissions/save', {
                    actor_id: selectedActorConfig.value.id,
                    permissions: nsPerms
                }),
                api('/admin/actors/visibility/save', {
                    actor_id: selectedActorConfig.value.id,
                    wildcard: actorHasWildcardVis.value,
                    grants: actorVisibilityGrants.value.map(g => g.target_actor_id)
                })
            ];

            if (selectedActorConfig.value.is_user) {
                const adminPerms = [];
                if (actorHasWildcardAdmin.value) {
                    adminPerms.push({ resource: '*', action: '*' });
                } else {
                    for (const row of actorAdminPerms.value) {
                        for (const action of row.granted) {
                            adminPerms.push({ resource: row.resource, action });
                        }
                    }
                }
                savePromises.push(api('/admin/actors/admin-permissions/save', {
                    actor_id: selectedActorConfig.value.id,
                    permissions: adminPerms
                }));
            }

            const results = await Promise.all(savePromises);

            // If we edited our own admin permissions, refresh the client-side permission cache
            if (selectedActorConfig.value.is_user && user && user.value && selectedActorConfig.value.id === user.value.id) {
                const adminPermResult = results[results.length - 1];
                if (adminPermResult && adminPermResult.updated_permissions) {
                    permissions.value = adminPermResult.updated_permissions;
                    // Update localStorage too
                    const saved = localStorage.getItem('admin_session');
                    if (saved) {
                        try {
                            const session = JSON.parse(saved);
                            session.permissions = adminPermResult.updated_permissions;
                            localStorage.setItem('admin_session', JSON.stringify(session));
                        } catch (e) { /* ignore */ }
                    }
                }
            }
            showToast('Actor configuration saved', 'success');
        } catch (err) {
            console.error('Failed to save actor config:', err);
            showToast('Failed: ' + err.message, 'error');
        } finally {
            actorConfigSaving.value = false;
        }
    }

    // ─── Agent Configuration Save ───

    // Rebuild editAgentConfig from the new model's capability defaults,
    // preserving values for keys that exist in both old and new models
    // (e.g. temperature). Drops keys that don't belong to the new model.
    function rebuildConfigForModel(providerName, modelId) {
        const caps = agentsModule.capabilitiesFor(providerName, modelId);
        const oldConfig = editAgentConfig.value;
        const newConfig = {};
        for (const [key, cap] of Object.entries(caps)) {
            if (oldConfig[key] !== undefined) {
                newConfig[key] = oldConfig[key];
            } else if (cap.default !== undefined) {
                newConfig[key] = cap.default;
            }
        }
        editAgentConfig.value = newConfig;
    }

    function onEditProviderChange() {
        // For providers that support custom model IDs (e.g. OpenRouter), don't
        // auto-reset the model — let the user type whatever they want.
        if (editAgentProvider.value === 'openrouter') {
            agentsModule.loadOpenRouterCatalog();
            return;
        }
        const models = agentsModule.modelsForProvider(editAgentProvider.value);
        if (models.length > 0) {
            editAgentModel.value = models[0].id;
        } else {
            editAgentModel.value = '';
        }
        rebuildConfigForModel(editAgentProvider.value, editAgentModel.value);
    }

    function onEditModelChange() {
        rebuildConfigForModel(editAgentProvider.value, editAgentModel.value);
    }

    async function saveAgentConfiguration() {
        if (!selectedActorConfig.value) return;
        // Block save if provider registry hasn't loaded — without it,
        // configVersionFor returns null and the version stamp gets skipped,
        // leaving stale _configVersion values in the stored config.
        if (editAgentProvider.value && editAgentModel.value && !agentsModule.configVersionFor(editAgentProvider.value, editAgentModel.value)) {
            showToast('Provider registry not loaded yet — please wait and try again', 'error');
            return;
        }
        agentConfigSaving.value = true;
        try {
            const body = { agent: selectedActorConfig.value.name };
            body.provider = editAgentProvider.value || null;
            body.model = editAgentModel.value || null;
            if (selectedActorConfig.value.virtual) {
                body.personality = editAgentPersonality.value || null;
                if (editAgentApiKey.value) body.api_key = editAgentApiKey.value;
                if (Object.keys(editAgentConfig.value).length > 0) {
                    const configCopy = { ...editAgentConfig.value };
                    const version = agentsModule.configVersionFor(editAgentProvider.value, editAgentModel.value);
                    if (version != null) configCopy._configVersion = version;
                    body.configuration = configCopy;
                }
            }
            await api('/admin/agents/update', body);
            // Update the local actor data
            selectedActorConfig.value.provider = body.provider;
            selectedActorConfig.value.model = body.model;
            if (body.personality !== undefined) selectedActorConfig.value.personality = body.personality;
            editAgentApiKey.value = '';
            showToast('Agent configuration saved', 'success');
            agentsModule.loadAgents();
        } catch (err) {
            console.error('Failed to save agent config:', err);
            showToast('Failed: ' + err.message, 'error');
        } finally {
            agentConfigSaving.value = false;
        }
    }

    // Computed: actors available to add as visibility grants (exclude self and already-granted)
    const availableVisibilityTargets = computed(() => {
        if (!selectedActorConfig.value) return [];
        const selfId = selectedActorConfig.value.id;
        const grantedIds = new Set(actorVisibilityGrants.value.map(g => g.target_actor_id));
        return actorsConfigList.value.filter(a => a.id !== selfId && !grantedIds.has(a.id));
    });

    // Computed: namespaces available for the dropdown (exclude already-added ones)
    const availableNamespacesFiltered = computed(() => {
        const existing = new Set(actorPermissions.value.map(p => p.namespace));
        return availableNamespaces.value.filter(ns => !existing.has(ns));
    });

    // ─── Admin Permissions ───

    function toggleAdminPerm(resourceIndex, action) {
        const row = actorAdminPerms.value[resourceIndex];
        if (!row) return;
        const has = row.granted.includes(action);
        if (has) {
            // Remove this action and any lower-ranked actions that become orphaned
            const rank = { read: 1, write: 2, delete: 3 };
            const removedRank = rank[action];
            row.granted = row.granted.filter(a => rank[a] < removedRank);
        } else {
            // Add this action and any implied lower actions
            const rank = { read: 1, write: 2, delete: 3 };
            const addedRank = rank[action];
            const newGranted = new Set(row.granted);
            for (const [a, r] of Object.entries(rank)) {
                if (r <= addedRank && row.actions.includes(a)) {
                    newGranted.add(a);
                }
            }
            row.granted = Array.from(newGranted);
        }
    }

    function adminPermChecked(resourceIndex, action) {
        const row = actorAdminPerms.value[resourceIndex];
        if (!row) return false;
        return row.granted.includes(action);
    }

    // ─── UI Access (Password) ───

    async function setActorPassword() {
        if (!selectedActorConfig.value || !actorPasswordInput.value) return;
        actorPasswordSaving.value = true;
        try {
            const data = await api('/admin/actors/password', {
                actor_id: selectedActorConfig.value.id,
                password: actorPasswordInput.value
            });
            selectedActorConfig.value.is_user = data.is_user;
            actorPasswordInput.value = '';
            showToast('Password updated', 'success');
            loadActorsConfig();
        } catch (err) {
            console.error('Failed to set password:', err);
            showToast('Failed: ' + err.message, 'error');
        } finally {
            actorPasswordSaving.value = false;
        }
    }

    async function clearActorPassword() {
        if (!selectedActorConfig.value) return;
        const confirmed = await showConfirm('Remove UI access for ' + selectedActorConfig.value.name + '?');
        if (!confirmed) return;
        actorPasswordSaving.value = true;
        try {
            const data = await api('/admin/actors/password', {
                actor_id: selectedActorConfig.value.id,
                password: null
            });
            selectedActorConfig.value.is_user = data.is_user;
            actorPasswordInput.value = '';
            showToast('UI access removed', 'success');
            loadActorsConfig();
        } catch (err) {
            console.error('Failed to clear password:', err);
            showToast('Failed: ' + err.message, 'error');
        } finally {
            actorPasswordSaving.value = false;
        }
    }

    // ─── Create Actor ───

    function startCreateActor(source) {
        agentsModule.loadProviderRegistry();
        agentsModule.loadTemplates();
        actorDialogMode.value = 'create';
        createSource.value = source || 'config';
        const isAgentMode = createSource.value === 'agents';
        // Use a stub object so the dialog opens
        selectedActorConfig.value = { id: null, name: '', is_agent: true, is_user: false, virtual: isAgentMode };
        actorConfigLoading.value = false;
        newActorName.value = '';
        newActorVirtual.value = isAgentMode;
        editAgentProvider.value = '';
        editAgentModel.value = '';
        editAgentPersonality.value = '';
        editAgentApiKey.value = '';
        editAgentDreamMode.value = 'none';
        editAgentLearningEnabled.value = true;
        editAgentConfig.value = {};
        newActorUiAccess.value = false;
        newActorPassword.value = '';
        newActorTemplateId.value = null;
        newActorNoteTemplateId.value = null;
        newActorCreating.value = false;
        newActorPassphrase.value = null;
    }

    async function createActor() {
        if (!newActorName.value.trim()) return;
        if (editAgentProvider.value && editAgentModel.value && !agentsModule.configVersionFor(editAgentProvider.value, editAgentModel.value)) {
            showToast('Provider registry not loaded yet — please wait and try again', 'error');
            return;
        }
        newActorCreating.value = true;
        try {
            const body = { name: newActorName.value.trim() };
            if (editAgentProvider.value) body.provider = editAgentProvider.value;
            if (editAgentModel.value) body.model = editAgentModel.value;
            if (newActorVirtual.value) {
                body.virtual = true;
                if (editAgentPersonality.value) body.personality = editAgentPersonality.value;
                if (Object.keys(editAgentConfig.value).length > 0) {
                    const configCopy = { ...editAgentConfig.value };
                    const version = agentsModule.configVersionFor(editAgentProvider.value, editAgentModel.value);
                    if (version != null) configCopy._configVersion = version;
                    body.configuration = configCopy;
                }
            }
            if (newActorUiAccess.value && newActorPassword.value) {
                body.ui_access = true;
                body.password = newActorPassword.value;
            }
            if (editAgentDreamMode.value && editAgentDreamMode.value !== 'none') {
                body.dream_mode = editAgentDreamMode.value;
            }
            if (!editAgentLearningEnabled.value) {
                body.learning_enabled = false;
            }
            if (newActorTemplateId.value && !newActorVirtual.value) {
                body.welcome_template_id = newActorTemplateId.value;
            }
            if (newActorNoteTemplateId.value && !newActorVirtual.value) {
                body.welcome_note_template_id = newActorNoteTemplateId.value;
            }
            const data = await api('/admin/actors/create', body);
            newActorPassphrase.value = data.passphrase;

            // If virtual and API key provided, update it separately (encrypted)
            if (newActorVirtual.value && editAgentApiKey.value) {
                await api('/admin/agents/update', {
                    agent: data.name,
                    api_key: editAgentApiKey.value
                });
            }

            var extras = [];
            if (data.welcome_mail_sent) extras.push('welcome mail');
            if (data.welcome_note_saved) extras.push('getting-started note');
            const msg = data.virtual
                ? 'Virtual agent "' + data.name + '" created'
                : 'Actor "' + data.name + '" created' + (extras.length ? ' with ' + extras.join(' + ') : '');
            showToast(msg, 'success');
            loadActorsConfig();
            await agentsModule.loadAgents();

            // Open the new agent in the detail editor
            const newAgent = agentsModule.agents.value.find(a => a.agent === data.name);
            if (newAgent) {
                closeActorConfig();
                agentsModule.viewAgent(newAgent);
            }
        } catch (err) {
            console.error('Failed to create actor:', err);
            showToast('Failed: ' + err.message, 'error');
        } finally {
            newActorCreating.value = false;
        }
    }

    async function toggleActorVisibleToOthers() {
        if (!selectedActorConfig.value) return;
        const newVal = selectedActorConfig.value.visible_to_others;
        try {
            await api('/admin/profile/visibility', { actor_id: selectedActorConfig.value.id, visible_to_others: newVal });
            showToast(newVal ? 'Now visible for sharing' : 'Now hidden from sharing', 'success');
        } catch (err) {
            selectedActorConfig.value.visible_to_others = !newVal;
            showToast(err.message || 'Failed to update', 'error');
        }
    }

    // ─── Virtual Agent Access ───

    async function toggleVaPublicAccess() {
        if (!selectedActorConfig.value) return;
        try {
            if (vaAccessPublic.value) {
                // Grant public access
                await api('/admin/virtual-agent-access/grant', {
                    virtual_agent_id: selectedActorConfig.value.id,
                    grantee_actor_id: null
                });
            } else {
                // Revoke public access — find the public row
                const vaData = await api('/admin/virtual-agent-access/list');
                const publicRow = vaData.access.find(a => a.virtual_agent_id === selectedActorConfig.value.id && a.grantee_actor_id === null);
                if (publicRow) {
                    await api('/admin/virtual-agent-access/revoke', { id: publicRow.id });
                }
            }
            showToast(vaAccessPublic.value ? 'Public access granted' : 'Public access revoked', 'success');
        } catch (err) {
            vaAccessPublic.value = !vaAccessPublic.value;
            showToast(err.message || 'Failed', 'error');
        }
    }

    async function addVaAccessGrant() {
        const targetId = safeInt(newVaAccessTarget.value);
        if (!targetId || !selectedActorConfig.value) return;
        try {
            await api('/admin/virtual-agent-access/grant', {
                virtual_agent_id: selectedActorConfig.value.id,
                grantee_actor_id: targetId
            });
            const target = actorsConfigList.value.find(a => a.id === targetId);
            // Reload to get the row ID
            const vaData = await api('/admin/virtual-agent-access/list');
            const myAccess = vaData.access.filter(a => a.virtual_agent_id === selectedActorConfig.value.id);
            vaAccessList.value = myAccess.filter(a => a.grantee_actor_id !== null);
            newVaAccessTarget.value = '';
            showToast('Access granted to ' + (target?.name || 'actor'), 'success');
        } catch (err) {
            showToast(err.message || 'Failed', 'error');
        }
    }

    async function removeVaAccessGrant(accessId) {
        try {
            await api('/admin/virtual-agent-access/revoke', { id: accessId });
            vaAccessList.value = vaAccessList.value.filter(a => a.id !== accessId);
            showToast('Access revoked', 'success');
        } catch (err) {
            showToast(err.message || 'Failed', 'error');
        }
    }

    // ─── Delete Actor ───

    async function deleteActor() {
        if (!selectedActorConfig.value) return;
        const name = selectedActorConfig.value.name;
        const confirmed = await showConfirm('Permanently delete "' + name + '" and ALL associated data (notes, mail, chat, discussions, virtual agents they own)? This cannot be undone.');
        if (!confirmed) return;
        actorDeleting.value = true;
        try {
            const data = await api('/admin/actors/delete', { actor_id: selectedActorConfig.value.id });
            let msg = 'Deleted actor "' + name + '"';
            if (data.deleted.virtual_agents && data.deleted.virtual_agents.length > 0) {
                msg += ' and virtual agents: ' + data.deleted.virtual_agents.join(', ');
            }
            showToast(msg, 'success');
            closeActorConfig();
            loadActorsConfig();
            agentsModule.loadAgents();
        } catch (err) {
            console.error('Failed to delete actor:', err);
            showToast('Failed: ' + err.message, 'error');
        } finally {
            actorDeleting.value = false;
        }
    }

    // Actors available for VA access (exclude self, already-granted, and virtual agents)
    const availableVaAccessTargets = computed(() => {
        if (!selectedActorConfig.value) return [];
        const selfId = selectedActorConfig.value.id;
        const grantedIds = new Set(vaAccessList.value.map(a => a.grantee_actor_id));
        return actorsConfigList.value.filter(a => a.id !== selfId && !grantedIds.has(a.id) && !a.virtual);
    });

    function closeDialogs() {
        selectedActorConfig.value = null;
    }

    return {
        actorsConfigList, actorsConfigLoading,
        actorsSorted: actorSort.sorted, actorSortKey: actorSort.sortKey, actorSortDir: actorSort.sortDir,
        toggleActorSort: actorSort.toggleSort, actorSortArrow: actorSort.sortArrow,
        selectedActorConfig, actorConfigLoading, actorDialogMode,
        actorPermissions, actorHasWildcardPerm,
        actorVisibilityGrants, actorHasWildcardVis,
        actorConfigSaving,
        availableNamespaces, newNamespaceInput, availableNamespacesFiltered,
        newVisibilityTarget, availableVisibilityTargets,
        loadActorsConfig, openActorConfig, closeActorConfig,
        addPermissionRow, removePermissionRow,
        addVisibilityGrant, removeVisibilityGrant,
        saveActorConfig,
        // Admin Permissions
        actorAdminPerms, actorHasWildcardAdmin, adminResources,
        toggleAdminPerm, adminPermChecked,
        // Sharing
        toggleActorVisibleToOthers,
        // Virtual Agent Access
        vaAccessList, vaAccessPublic, newVaAccessTarget, availableVaAccessTargets,
        toggleVaPublicAccess, addVaAccessGrant, removeVaAccessGrant,
        // UI Access
        actorPasswordInput, actorPasswordSaving, setActorPassword, clearActorPassword,
        // Agent configuration editing
        editAgentProvider, editAgentModel, editAgentPersonality, editAgentApiKey, editAgentDreamMode, editAgentLearningEnabled, editAgentConfig,
        agentConfigSaving, onEditProviderChange, onEditModelChange, saveAgentConfiguration,
        // Delete actor
        actorDeleting, deleteActor,
        // Create actor
        createSource, newActorName, newActorVirtual,
        newActorUiAccess, newActorPassword,
        newActorTemplateId, newActorNoteTemplateId, newActorCreating, newActorPassphrase,
        nameCheckStatus, nameCheckMessage,
        startCreateActor, createActor,
        closeDialogs
    };
}

export { useActorsConfig };
