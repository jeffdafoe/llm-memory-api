// actors-config.js — Actor permissions, visibility, and creation management (Configuration > Actors tab)
import { ref, computed } from 'vue';

function useActorsConfig({ api, showToast, showConfirm, agentsModule, user, permissions }) {
    const actorsConfigList = ref([]);
    const actorsConfigLoading = ref(false);

    // Detail dialog state
    const selectedActorConfig = ref(null);
    const actorConfigLoading = ref(false);

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
        { id: 'dashboard', label: 'Dashboard', actions: ['read'] },
        { id: 'agents', label: 'Agents', actions: ['read', 'write'] },
        { id: 'comms', label: 'Communications', actions: ['read', 'write', 'delete'] },
        { id: 'notes', label: 'Notes', actions: ['read', 'write', 'delete'] },
        { id: 'config', label: 'Configuration', actions: ['read', 'write'] },
        { id: 'actors', label: 'Actors', actions: ['read', 'write'] },
        { id: 'templates', label: 'Templates', actions: ['read', 'write', 'delete'] },
        { id: 'logs', label: 'Logs', actions: ['read'] }
    ];

    // UI Access (password) state
    const actorPasswordInput = ref('');
    const actorPasswordSaving = ref(false);

    // ─── Create Actor ───
    const actorCreating = ref(false);
    const newActorName = ref('');
    const newActorProvider = ref('');
    const newActorModel = ref('');
    const newActorVirtual = ref(false);
    const newActorPersonality = ref('');
    const newActorApiKey = ref('');
    const newActorConfig = ref({});
    const newActorUiAccess = ref(false);
    const newActorPassword = ref('');
    const newActorTemplateId = ref(null);
    const newActorCreating = ref(false);
    const newActorPassphrase = ref(null);

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
        selectedActorConfig.value = actor;
        actorConfigLoading.value = true;
        newNamespaceInput.value = '';
        newVisibilityTarget.value = '';
        try {
            // Load permissions, visibility, namespaces, and admin perms in parallel
            const promises = [
                api('/admin/actors/permissions/read', { actor_id: actor.id }),
                api('/admin/actors/visibility/read', { actor_id: actor.id }),
                api('/admin/actors/namespaces'),
                actor.is_user ? api('/admin/actors/admin-permissions/read', { actor_id: actor.id }) : Promise.resolve(null)
            ];
            const [permData, visData, nsData, adminPermData] = await Promise.all(promises);

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
                target_actor_id: parseInt(g.target_actor_id)
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
                    actions: r.actions,
                    granted: permMap[r.id] ? Array.from(permMap[r.id]) : []
                }));
            } else {
                actorHasWildcardAdmin.value = false;
                actorAdminPerms.value = [];
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
        const targetId = parseInt(newVisibilityTarget.value);
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

    function startCreateActor() {
        agentsModule.loadProviderRegistry();
        agentsModule.loadTemplates();
        actorCreating.value = true;
        newActorName.value = '';
        newActorProvider.value = '';
        newActorModel.value = '';
        newActorVirtual.value = false;
        newActorPersonality.value = '';
        newActorApiKey.value = '';
        newActorConfig.value = {};
        newActorUiAccess.value = false;
        newActorPassword.value = '';
        newActorTemplateId.value = null;
        newActorCreating.value = false;
        newActorPassphrase.value = null;
    }

    function onNewActorProviderChange() {
        const models = agentsModule.modelsForProvider(newActorProvider.value);
        if (models.length > 0) {
            newActorModel.value = models[0].id;
        } else {
            newActorModel.value = '';
        }
    }

    async function createActor() {
        if (!newActorName.value.trim()) return;
        newActorCreating.value = true;
        try {
            const body = { name: newActorName.value.trim() };
            if (newActorProvider.value) body.provider = newActorProvider.value;
            if (newActorModel.value) body.model = newActorModel.value;
            if (newActorVirtual.value) {
                body.virtual = true;
                if (newActorPersonality.value) body.personality = newActorPersonality.value;
                if (Object.keys(newActorConfig.value).length > 0) {
                    const configCopy = Object.assign({}, newActorConfig.value);
                    const version = agentsModule.configVersionFor(newActorProvider.value, newActorModel.value);
                    if (version != null) {
                        configCopy._configVersion = version;
                    }
                    body.configuration = configCopy;
                }
            }
            if (newActorUiAccess.value && newActorPassword.value) {
                body.ui_access = true;
                body.password = newActorPassword.value;
            }
            if (newActorTemplateId.value && !newActorVirtual.value) {
                body.welcome_template_id = newActorTemplateId.value;
            }
            const data = await api('/admin/actors/create', body);
            newActorPassphrase.value = data.passphrase;

            // If virtual and API key provided, update it separately (encrypted)
            if (newActorVirtual.value && newActorApiKey.value) {
                await api('/admin/agents/update', {
                    agent: data.name,
                    api_key: newActorApiKey.value
                });
            }

            const msg = data.virtual
                ? 'Virtual agent "' + data.name + '" created'
                : 'Actor "' + data.name + '" created' + (data.welcome_mail_sent ? ' with welcome mail' : '');
            showToast(msg, 'success');
            loadActorsConfig();
            agentsModule.loadAgents();
        } catch (err) {
            console.error('Failed to create actor:', err);
            showToast('Failed: ' + err.message, 'error');
        } finally {
            newActorCreating.value = false;
        }
    }

    function closeDialogs() {
        selectedActorConfig.value = null;
        actorCreating.value = false;
    }

    return {
        actorsConfigList, actorsConfigLoading,
        selectedActorConfig, actorConfigLoading,
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
        // UI Access
        actorPasswordInput, actorPasswordSaving, setActorPassword, clearActorPassword,
        // Create actor
        actorCreating, newActorName, newActorProvider, newActorModel,
        newActorVirtual, newActorPersonality, newActorApiKey, newActorConfig,
        newActorUiAccess, newActorPassword,
        newActorTemplateId, newActorCreating, newActorPassphrase,
        startCreateActor, onNewActorProviderChange, createActor,
        closeDialogs
    };
}

export { useActorsConfig };
