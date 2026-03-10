// agents.js — Agents list, detail, creation, welcome templates, provider registry

function useAgents({ api, showToast, showConfirm, onEvent }) {
    const agents = ref([]);
    const selectedAgent = ref(null);
    const agentSubTab = ref('list');

    // Provider registry (loaded once from backend)
    const providerRegistry = ref([]);
    const providerRegistryLoaded = ref(false);

    // Agent detail
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
    const agentProfileEditing = ref(false);
    const agentProfileProvider = ref('');
    const agentProfileModel = ref('');
    const agentProfileApiKey = ref('');
    const agentProfileSaving = ref(false);

    // Agent creation
    const agentCreating = ref(false);
    const newAgentName = ref('');
    const newAgentProvider = ref('');
    const newAgentModel = ref('');
    const newAgentTemplateId = ref(null);
    const newAgentCreating = ref(false);
    const newAgentPassphrase = ref(null);
    const newAgentVirtual = ref(false);
    const newAgentPersonality = ref('');
    const newAgentApiKey = ref('');
    const newAgentCost = ref('');
    const newAgentConfig = ref({});

    // Agent settings (dynamic configuration)
    const agentSettingsEditing = ref(false);
    const agentSettingsLearningEnabled = ref(true);
    const agentSettingsConfig = ref({});
    const agentSettingsSaving = ref(false);

    // Welcome templates
    const welcomeTemplates = ref([]);
    const templateEditing = ref(false);
    const templateEditId = ref(null);
    const templateEditName = ref('');
    const templateEditDescription = ref('');
    const templateEditSubject = ref('');
    const templateEditBody = ref('');
    const templateSaving = ref(false);

    // ── Provider registry ────────────────────────────────────────────────────

    async function loadProviderRegistry() {
        if (providerRegistryLoaded.value) return;
        try {
            const data = await api('/admin/providers/registry');
            providerRegistry.value = data.providers;
            providerRegistryLoaded.value = true;
        } catch (err) {
            console.error('Failed to load provider registry:', err);
        }
    }

    // Return models array for a given provider name: [{ id, label, deprecated }]
    function modelsForProvider(providerName) {
        if (!providerName) return [];
        const provider = providerRegistry.value.find(p => p.name === providerName);
        if (!provider) return [];
        return Object.entries(provider.models).map(([id, info]) => ({
            id,
            label: info.label,
            deprecated: info.deprecated || null
        }));
    }

    // Return capabilities object for a given provider + model
    function capabilitiesFor(providerName, modelId) {
        if (!providerName || !modelId) return {};
        const provider = providerRegistry.value.find(p => p.name === providerName);
        if (!provider) return {};
        const model = provider.models[modelId];
        if (!model) return {};
        return model.capabilities || {};
    }

    // Get deprecation warning for a model, or null
    function modelDeprecation(providerName, modelId) {
        if (!providerName || !modelId) return null;
        const provider = providerRegistry.value.find(p => p.name === providerName);
        if (!provider) return null;
        const model = provider.models[modelId];
        if (!model) return null;
        return model.deprecated || null;
    }

    // Build the current effective config from agent data.
    // Merges: configuration JSON (primary) + legacy columns (fallback).
    function parseAgentConfig(agent) {
        let conf = {};
        if (agent.configuration) {
            try {
                conf = typeof agent.configuration === 'string'
                    ? JSON.parse(agent.configuration)
                    : agent.configuration;
            } catch (e) { /* ignore */ }
        }
        // Legacy column fallbacks — only if not already in config JSON
        if (conf.cache_prompts === undefined && agent.cache_prompts !== undefined) {
            conf.cache_prompts = agent.cache_prompts;
        }
        if (conf.max_tokens === undefined && agent.max_tokens != null) {
            conf.max_tokens = agent.max_tokens;
        }
        if (conf.temperature === undefined && agent.temperature != null) {
            conf.temperature = agent.temperature;
        }
        return conf;
    }

    // Check if a capability's depends_on condition is satisfied
    function capabilityVisible(cap, config) {
        if (!cap.depends_on) return true;
        return !!config[cap.depends_on];
    }

    // Format a config value for display
    function formatConfigValue(key, value, cap) {
        if (value === undefined || value === null) return 'default';
        if (cap.type === 'boolean') return value ? 'on' : 'off';
        return String(value);
    }

    // ── Agent loading ────────────────────────────────────────────────────────

    async function loadAgents() {
        try {
            const data = await api('/admin/agents');
            agents.value = data.agents;
            if (selectedAgent.value) {
                const updated = data.agents.find(a => a.agent === selectedAgent.value.agent);
                if (updated) selectedAgent.value = updated;
            }
        } catch (err) {
            console.error('Failed to load agents:', err);
        }
    }

    // ── Profile editing ──────────────────────────────────────────────────────

    function startEditProfile() {
        loadProviderRegistry();
        agentProfileEditing.value = true;
        agentProfileProvider.value = selectedAgent.value.provider || '';
        agentProfileModel.value = selectedAgent.value.model || '';
        agentProfileApiKey.value = '';
    }

    function onProfileProviderChange() {
        // When provider changes, reset model if it doesn't exist in new provider
        const models = modelsForProvider(agentProfileProvider.value);
        const exists = models.find(m => m.id === agentProfileModel.value);
        if (!exists) {
            agentProfileModel.value = models.length > 0 ? models[0].id : '';
        }
    }

    async function saveProfile() {
        agentProfileSaving.value = true;
        try {
            const body = {
                agent: selectedAgent.value.agent,
                provider: agentProfileProvider.value || null,
                model: agentProfileModel.value || null
            };
            if (agentProfileApiKey.value) {
                body.api_key = agentProfileApiKey.value;
            }
            await api('/admin/agents/update', body);
            selectedAgent.value.provider = agentProfileProvider.value || null;
            selectedAgent.value.model = agentProfileModel.value || null;
            agentProfileEditing.value = false;
            showToast('Profile updated', 'success');
        } catch (err) {
            console.error('Failed to save profile:', err);
            showToast('Failed: ' + err.message, 'error');
        } finally {
            agentProfileSaving.value = false;
        }
    }

    // ── Token budget ─────────────────────────────────────────────────────────

    const tokenBudgetEditing = ref(false);
    const tokenBudgetEditValue = ref('');

    function startEditTokenBudget() {
        tokenBudgetEditing.value = true;
        tokenBudgetEditValue.value = selectedAgent.value.token_budget || '';
    }

    async function saveTokenBudget() {
        try {
            const val = tokenBudgetEditValue.value === '' ? null : parseInt(tokenBudgetEditValue.value);
            await api('/admin/agents/update', { agent: selectedAgent.value.agent, token_budget: val });
            selectedAgent.value.token_budget = val;
            tokenBudgetEditing.value = false;
            showToast('Token budget updated', 'success');
        } catch (err) {
            showToast('Failed: ' + err.message, 'error');
        }
    }

    async function resetTokenUsage() {
        try {
            await api('/admin/agents/update', { agent: selectedAgent.value.agent, reset_tokens: true });
            selectedAgent.value.tokens_used = 0;
            selectedAgent.value.tokens_reset_at = new Date().toISOString();
            showToast('Token usage reset', 'success');
        } catch (err) {
            showToast('Failed: ' + err.message, 'error');
        }
    }

    // ── Settings (dynamic configuration) ─────────────────────────────────────

    function startEditSettings() {
        loadProviderRegistry();
        agentSettingsEditing.value = true;
        agentSettingsLearningEnabled.value = selectedAgent.value.learning_enabled !== false;

        // Load config, filling in defaults from capabilities where not set
        const conf = parseAgentConfig(selectedAgent.value);
        const caps = capabilitiesFor(selectedAgent.value.provider, selectedAgent.value.model);
        for (const [key, cap] of Object.entries(caps)) {
            if (conf[key] === undefined && cap.default !== undefined) {
                conf[key] = cap.default;
            }
        }
        agentSettingsConfig.value = conf;
    }

    async function saveSettings() {
        agentSettingsSaving.value = true;
        try {
            const body = {
                agent: selectedAgent.value.agent,
                learning_enabled: agentSettingsLearningEnabled.value,
                configuration: agentSettingsConfig.value
            };
            await api('/admin/agents/update', body);
            selectedAgent.value.learning_enabled = body.learning_enabled;
            selectedAgent.value.configuration = JSON.stringify(agentSettingsConfig.value);
            // Sync legacy columns for display consistency
            if (agentSettingsConfig.value.cache_prompts !== undefined) {
                selectedAgent.value.cache_prompts = agentSettingsConfig.value.cache_prompts;
            }
            if (agentSettingsConfig.value.max_tokens !== undefined) {
                selectedAgent.value.max_tokens = agentSettingsConfig.value.max_tokens;
            }
            if (agentSettingsConfig.value.temperature !== undefined) {
                selectedAgent.value.temperature = agentSettingsConfig.value.temperature;
            }
            agentSettingsEditing.value = false;
            showToast('Settings updated', 'success');
        } catch (err) {
            console.error('Failed to save settings:', err);
            showToast('Failed: ' + err.message, 'error');
        } finally {
            agentSettingsSaving.value = false;
        }
    }

    // ── Agent detail view ────────────────────────────────────────────────────

    async function viewAgent(agent) {
        selectedAgent.value = agent;
        agentInstructionsEditing.value = false;
        agentExpertiseEditing.value = false;
        agentProfileEditing.value = false;
        tokenBudgetEditing.value = false;
        agentSettingsEditing.value = false;
        agentPassphraseConfirming.value = false;
        agentNewPassphrase.value = null;
        loadProviderRegistry();
        try {
            agentExpertise.value = typeof agent.expertise === 'string' ? JSON.parse(agent.expertise) : (agent.expertise || []);
        } catch (e) {
            agentExpertise.value = [];
        }
        // Fetch full agent detail (includes configuration) and instructions in parallel
        try {
            const [detail, instData] = await Promise.all([
                api('/admin/agents/read', { agent: agent.agent }),
                api('/admin/agents/instructions/read', { agent: agent.agent })
            ]);
            // Merge configuration from the detail endpoint onto the selectedAgent
            selectedAgent.value.configuration = detail.configuration || null;
            selectedAgent.value.has_api_key = detail.has_api_key || false;
            agentInstructions.value = instData.instructions;
        } catch (err) {
            console.error('Failed to load agent detail:', err);
            agentInstructions.value = '';
        }
    }

    // ── Instructions ─────────────────────────────────────────────────────────

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

    // ── Expertise ────────────────────────────────────────────────────────────

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

    // ── Passphrase ───────────────────────────────────────────────────────────

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

    // ── Agent creation ───────────────────────────────────────────────────────

    function startCreateAgent() {
        loadProviderRegistry();
        agentCreating.value = true;
        newAgentName.value = '';
        newAgentProvider.value = '';
        newAgentModel.value = '';
        newAgentTemplateId.value = null;
        newAgentCreating.value = false;
        newAgentPassphrase.value = null;
        newAgentVirtual.value = false;
        newAgentPersonality.value = '';
        newAgentApiKey.value = '';
        newAgentCost.value = '';
        newAgentConfig.value = {};
        loadTemplates();
    }

    function onNewProviderChange() {
        const models = modelsForProvider(newAgentProvider.value);
        newAgentModel.value = models.length > 0 ? models[0].id : '';
        seedNewAgentConfig();
    }

    function onNewModelChange() {
        seedNewAgentConfig();
    }

    function seedNewAgentConfig() {
        const caps = capabilitiesFor(newAgentProvider.value, newAgentModel.value);
        const conf = {};
        for (const [key, cap] of Object.entries(caps)) {
            if (cap.default !== undefined) {
                conf[key] = cap.default;
            }
        }
        newAgentConfig.value = conf;
    }

    async function createAgent() {
        if (!newAgentName.value) return;
        newAgentCreating.value = true;
        try {
            const body = { agent: newAgentName.value };
            if (newAgentProvider.value) body.provider = newAgentProvider.value;
            if (newAgentModel.value) body.model = newAgentModel.value;
            if (newAgentVirtual.value) {
                body.virtual = true;
                if (newAgentPersonality.value) body.personality = newAgentPersonality.value;
                if (newAgentCost.value) body.cost = newAgentCost.value;
                if (Object.keys(newAgentConfig.value).length > 0) {
                    body.configuration = newAgentConfig.value;
                }
            }
            if (newAgentTemplateId.value && !newAgentVirtual.value) body.welcome_template_id = newAgentTemplateId.value;
            const data = await api('/admin/agents/create', body);
            newAgentPassphrase.value = data.passphrase;

            // If virtual and API key provided, update it separately (encrypted)
            if (newAgentVirtual.value && newAgentApiKey.value) {
                await api('/admin/agents/update', {
                    agent: newAgentName.value,
                    api_key: newAgentApiKey.value
                });
            }

            const msg = data.virtual
                ? 'Virtual agent "' + data.agent + '" created'
                : 'Agent "' + data.agent + '" created' + (data.welcome_mail_sent ? ' with welcome mail' : '');
            showToast(msg, 'success');
            loadAgents();
        } catch (err) {
            console.error('Failed to create agent:', err);
            showToast('Failed: ' + err.message, 'error');
        } finally {
            newAgentCreating.value = false;
        }
    }

    // ── Welcome templates ────────────────────────────────────────────────────

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

    // ── Real-time events ─────────────────────────────────────────────────────

    if (onEvent) {
        onEvent('agent_activity', (data) => {
            const agent = agents.value.find(a => a.agent === data.agent);
            if (agent) {
                agent.active_since = data.active ? new Date().toISOString() : null;
            }
        });
    }

    function closeDialogs() {
        selectedAgent.value = null;
        agentCreating.value = false;
    }

    return {
        agents, selectedAgent, agentSubTab,
        // Provider registry
        providerRegistry, loadProviderRegistry, modelsForProvider, capabilitiesFor, modelDeprecation,
        parseAgentConfig, capabilityVisible, formatConfigValue,
        // Agent detail
        agentInstructions, agentInstructionsEditing, agentInstructionsEditContent, agentInstructionsSaving,
        agentExpertise, agentExpertiseEditing, agentExpertiseEditText, agentExpertiseSaving,
        agentPassphraseConfirming, agentNewPassphrase,
        agentProfileEditing, agentProfileProvider, agentProfileModel, agentProfileApiKey, agentProfileSaving,
        loadAgents, viewAgent,
        startEditProfile, onProfileProviderChange, saveProfile,
        tokenBudgetEditing, tokenBudgetEditValue, startEditTokenBudget, saveTokenBudget, resetTokenUsage,
        agentSettingsEditing, agentSettingsLearningEnabled, agentSettingsConfig, agentSettingsSaving,
        startEditSettings, saveSettings,
        startEditInstructions, cancelEditInstructions, saveInstructions,
        startEditExpertise, cancelEditExpertise, saveExpertise,
        resetAgentPassphrase,
        // Agent creation
        agentCreating, newAgentName, newAgentProvider, newAgentModel,
        newAgentTemplateId, newAgentCreating, newAgentPassphrase,
        newAgentVirtual, newAgentPersonality, newAgentApiKey, newAgentCost, newAgentConfig,
        startCreateAgent, onNewProviderChange, onNewModelChange, createAgent,
        // Templates
        welcomeTemplates, templateEditing, templateEditId,
        templateEditName, templateEditDescription, templateEditSubject, templateEditBody, templateSaving,
        loadTemplates, startNewTemplate, editTemplate, saveTemplate, confirmDeleteTemplate,
        closeDialogs
    };
}

window.useAgents = useAgents;
