// actors-config.js — Actor permissions, visibility, and creation management (Configuration > Actors tab)

function useActorsConfig({ api, showToast, showConfirm, agentsModule }) {
    const actorsConfigList = ref([]);
    const actorsConfigLoading = ref(false);

    // Detail dialog state
    const selectedActorConfig = ref(null);
    const actorConfigLoading = ref(false);

    // Permissions state
    const actorPermissions = ref([]);
    const actorHasWildcardPerm = ref(false);
    const permissionsSaving = ref(false);

    // Visibility state
    const actorVisibilityGrants = ref([]);
    const actorHasWildcardVis = ref(false);
    const visibilitySaving = ref(false);

    // Available namespaces (for dropdown)
    const availableNamespaces = ref([]);
    const newNamespaceInput = ref('');

    // Available actors (for visibility dropdown)
    const newVisibilityTarget = ref('');

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
            // Load permissions, visibility, and available namespaces in parallel
            const [permData, visData, nsData] = await Promise.all([
                api('/admin/actors/permissions/read', { actor_id: actor.id }),
                api('/admin/actors/visibility/read', { actor_id: actor.id }),
                api('/admin/actors/namespaces')
            ]);

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

    async function savePermissions() {
        if (!selectedActorConfig.value) return;
        permissionsSaving.value = true;
        try {
            // Build the full list: wildcard + specific rows
            const permissions = [];
            if (actorHasWildcardPerm.value) {
                permissions.push({ namespace: '/', can_read: true, can_write: true, can_delete: true });
            }
            for (const p of actorPermissions.value) {
                permissions.push({
                    namespace: p.namespace,
                    can_read: !!p.can_read,
                    can_write: !!p.can_write,
                    can_delete: !!p.can_delete
                });
            }
            await api('/admin/actors/permissions/save', {
                actor_id: selectedActorConfig.value.id,
                permissions
            });
            showToast('Permissions saved', 'success');
        } catch (err) {
            console.error('Failed to save permissions:', err);
            showToast('Failed: ' + err.message, 'error');
        } finally {
            permissionsSaving.value = false;
        }
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

    async function saveVisibility() {
        if (!selectedActorConfig.value) return;
        visibilitySaving.value = true;
        try {
            await api('/admin/actors/visibility/save', {
                actor_id: selectedActorConfig.value.id,
                wildcard: actorHasWildcardVis.value,
                grants: actorVisibilityGrants.value.map(g => g.target_actor_id)
            });
            showToast('Visibility saved', 'success');
        } catch (err) {
            console.error('Failed to save visibility:', err);
            showToast('Failed: ' + err.message, 'error');
        } finally {
            visibilitySaving.value = false;
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
        actorPermissions, actorHasWildcardPerm, permissionsSaving,
        actorVisibilityGrants, actorHasWildcardVis, visibilitySaving,
        availableNamespaces, newNamespaceInput, availableNamespacesFiltered,
        newVisibilityTarget, availableVisibilityTargets,
        loadActorsConfig, openActorConfig, closeActorConfig,
        addPermissionRow, removePermissionRow, savePermissions,
        addVisibilityGrant, removeVisibilityGrant, saveVisibility,
        // Create actor
        actorCreating, newActorName, newActorProvider, newActorModel,
        newActorVirtual, newActorPersonality, newActorApiKey, newActorConfig,
        newActorUiAccess, newActorPassword,
        newActorTemplateId, newActorCreating, newActorPassphrase,
        startCreateActor, onNewActorProviderChange, createActor,
        closeDialogs
    };
}

window.useActorsConfig = useActorsConfig;
