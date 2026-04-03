// agents.js — Agents list, detail, welcome templates, provider registry
import { ref } from 'vue';
import { useSortable } from './core.js';

function useAgents({ api, showToast, showConfirm, onEvent }) {
    const agents = ref([]);
    const selectedAgent = ref(null);
    const defaultStorageQuota = ref(52428800); // updated from backend on load

    // Sortable table — default sort by agent name
    const agentSort = useSortable(agents, 'agent', 'asc');

    // Provider registry (loaded once from backend)
    const providerRegistry = ref([]);
    const providerRegistryLoaded = ref(false);

    // OpenRouter dynamic model catalog (fetched lazily on first use)
    const openrouterCatalog = ref(null); // null = not fetched, [] = fetched
    const openrouterCatalogLoading = ref(false);

    // Agent detail
    const agentInstructions = ref('');
    const agentInstructionsEditing = ref(false);
    const agentInstructionsExpanded = ref(false);
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
    const agentProfilePersonality = ref('');
    const agentProfileDreamMode = ref('none');
    const agentProfileSaving = ref(false);

    // Cost budgets
    const costBudgetEditing = ref(false);
    const costBudgetDailyValue = ref('');
    const costBudgetMonthlyValue = ref('');

    // Usage history
    const agentUsageHistory = ref([]);
    const agentUsageLoading = ref(false);

    // Agent settings (dynamic configuration)
    const agentSettingsEditing = ref(false);
    const agentSettingsLearningEnabled = ref(true);
    const agentSettingsStorageQuota = ref('');
    const agentSettingsConfig = ref({});
    const agentSettingsSaving = ref(false);

    // Templates
    const welcomeTemplates = ref([]);
    const templateEditing = ref(false);
    const templateEditId = ref(null);
    const templateEditName = ref('');
    const templateEditKind = ref('welcome');
    const templateEditDescription = ref('');
    const templateEditContent = ref('');
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

    // Lazily fetch the full OpenRouter model catalog from the backend cache.
    // Called when OpenRouter is selected as provider. Non-blocking — the dropdown
    // shows static models immediately and adds dynamic ones when the fetch completes.
    async function loadOpenRouterCatalog() {
        if (openrouterCatalog.value !== null || openrouterCatalogLoading.value) return;
        openrouterCatalogLoading.value = true;
        try {
            const data = await api('/admin/providers/openrouter/models');
            openrouterCatalog.value = (data.models || []).map(m => ({
                id: m.id,
                label: m.name || m.id,
                pricing: m.pricing
            }));
        } catch (err) {
            console.error('Failed to load OpenRouter catalog:', err);
            openrouterCatalog.value = [];
        } finally {
            openrouterCatalogLoading.value = false;
        }
    }

    // Return models array for a given provider name: [{ id, label, deprecated }]
    function modelsForProvider(providerName) {
        if (!providerName) return [];
        const provider = providerRegistry.value.find(p => p.name === providerName);
        if (!provider) return [];

        // For OpenRouter, models come entirely from the dynamic catalog
        if (providerName === 'openrouter' && openrouterCatalog.value) {
            return openrouterCatalog.value
                .map(m => {
                    var pricingStr = '';
                    if (m.pricing) {
                        var inp = m.pricing.input != null ? '$' + Number(m.pricing.input).toFixed(2) : '?';
                        var out = m.pricing.output != null ? '$' + Number(m.pricing.output).toFixed(2) : '?';
                        pricingStr = ' (' + inp + '/' + out + ')';
                    }
                    return { id: m.id, label: m.label + pricingStr };
                })
                .sort((a, b) => a.label.localeCompare(b.label));
        }

        return Object.entries(provider.models).map(([id, info]) => ({
            id,
            label: info.label,
            deprecated: info.deprecated || null
        }));
    }

    // Return capabilities object for a given provider + model
    // Default capabilities for OpenRouter models (applied to all models since
    // the registry is fully dynamic — no static model entries to carry caps).
    const openrouterDefaultCaps = {
        temperature: {
            type: 'number', label: 'Temperature',
            description: 'Controls randomness. Lower values are more focused and deterministic, higher values are more creative.',
            default: 0.7, min: 0, max: 2.0, step: 0.1
        },
        max_tokens: {
            type: 'number', label: 'Max Output Tokens',
            description: 'Maximum number of tokens the model will generate in its response.',
            default: 4096, min: 1, max: 32768
        }
    };

    function capabilitiesFor(providerName, modelId) {
        if (!providerName || !modelId) return {};
        const provider = providerRegistry.value.find(p => p.name === providerName);
        if (!provider) return {};
        const model = provider.models[modelId];
        if (model) return model.capabilities || {};
        // For providers with dynamic registries, return default capabilities
        if (providerName === 'openrouter') return openrouterDefaultCaps;
        return {};
    }

    // Return configVersion for a given provider + model, or null
    function configVersionFor(providerName, modelId) {
        if (!providerName || !modelId) return null;
        const provider = providerRegistry.value.find(p => p.name === providerName);
        if (!provider) return null;
        const model = provider.models[modelId];
        if (!model) return null;
        return model.configVersion || null;
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

    // Check if a capability should be disabled based on another field's value.
    // Returns the message string if disabled, or null if enabled.
    // Supports conditions: equals, notEquals, in, notIn.
    function capabilityDisabled(cap, config) {
        if (!cap.disabledWhen) return null;
        var rule = cap.disabledWhen;
        var fieldValue = config[rule.field];
        var disabled = false;
        if (rule.condition === 'equals') {
            disabled = fieldValue === rule.value;
        } else if (rule.condition === 'notEquals') {
            disabled = fieldValue !== undefined && fieldValue !== rule.value;
        } else if (rule.condition === 'in') {
            disabled = Array.isArray(rule.value) && rule.value.indexOf(fieldValue) !== -1;
        } else if (rule.condition === 'notIn') {
            disabled = Array.isArray(rule.value) && rule.value.indexOf(fieldValue) === -1;
        }
        return disabled ? (rule.message || 'Disabled') : null;
    }

    // Format a config value for display
    function formatConfigValue(key, value, cap) {
        if (value === undefined || value === null) {
            if (cap.default !== undefined) {
                const displayed = cap.type === 'boolean' ? (cap.default ? 'on' : 'off') : String(cap.default);
                if (!displayed) return 'default';
                return 'default (' + displayed + ')';
            }
            return 'default';
        }
        if (cap.type === 'boolean') return value ? 'on' : 'off';
        return String(value);
    }

    // ── Agent loading ────────────────────────────────────────────────────────

    async function loadAgents() {
        try {
            const data = await api('/admin/agents');
            agents.value = data.agents;
            if (data.default_storage_quota) {
                defaultStorageQuota.value = data.default_storage_quota;
            }
            if (selectedAgent.value) {
                const updated = data.agents.find(a => a.agent === selectedAgent.value.agent);
                // Merge list data onto existing selectedAgent so detail-only fields (configuration, expertise, etc.) survive
                if (updated) {
                    Object.assign(selectedAgent.value, updated);
                } else {
                    selectedAgent.value = null;
                }
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
        agentProfilePersonality.value = selectedAgent.value.personality || '';
        agentProfileDreamMode.value = selectedAgent.value.dream_mode || 'none';
        agentProfileApiKey.value = '';
        // Pre-fetch catalog if editing an agent already on OpenRouter
        if (agentProfileProvider.value === 'openrouter') {
            loadOpenRouterCatalog();
        }
    }

    function onProfileProviderChange() {
        // When provider changes, reset model if it doesn't exist in new provider.
        // For providers that support custom model IDs (e.g. OpenRouter), keep
        // the current value even if it's not in the static registry.
        if (agentProfileProvider.value === 'openrouter') {
            loadOpenRouterCatalog();
            return;
        }
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
                model: agentProfileModel.value || null,
                personality: agentProfilePersonality.value || null,
                dream_mode: agentProfileDreamMode.value || 'none'
            };
            if (agentProfileApiKey.value) {
                body.api_key = agentProfileApiKey.value;
            }
            await api('/admin/agents/update', body);
            selectedAgent.value.provider = agentProfileProvider.value || null;
            selectedAgent.value.model = agentProfileModel.value || null;
            selectedAgent.value.personality = agentProfilePersonality.value || null;
            selectedAgent.value.dream_mode = agentProfileDreamMode.value || 'none';
            agentProfileEditing.value = false;
            showToast('Profile updated', 'success');
        } catch (err) {
            console.error('Failed to save profile:', err);
            showToast('Failed: ' + err.message, 'error');
        } finally {
            agentProfileSaving.value = false;
        }
    }

    // ── Cost budgets ─────────────────────────────────────────────────────────

    function startEditCostBudget() {
        costBudgetEditing.value = true;
        costBudgetDailyValue.value = selectedAgent.value.cost_budget_daily ?? '';
        costBudgetMonthlyValue.value = selectedAgent.value.cost_budget_monthly ?? '';
    }

    async function saveCostBudget() {
        try {
            const daily = costBudgetDailyValue.value === '' ? null : parseFloat(costBudgetDailyValue.value);
            const monthly = costBudgetMonthlyValue.value === '' ? null : parseFloat(costBudgetMonthlyValue.value);
            await api('/admin/agents/update', {
                agent: selectedAgent.value.agent,
                cost_budget_daily: daily,
                cost_budget_monthly: monthly
            });
            selectedAgent.value.cost_budget_daily = daily;
            selectedAgent.value.cost_budget_monthly = monthly;
            costBudgetEditing.value = false;
            showToast('Cost budgets updated', 'success');
        } catch (err) {
            showToast('Failed: ' + err.message, 'error');
        }
    }

    async function loadUsageHistory() {
        if (!selectedAgent.value) return;
        agentUsageLoading.value = true;
        try {
            const data = await api('/admin/agents/usage', { agent: selectedAgent.value.agent, limit: 50 });
            agentUsageHistory.value = data.usage;
        } catch (err) {
            console.error('Failed to load usage:', err);
        } finally {
            agentUsageLoading.value = false;
        }
    }

    // ── Settings (dynamic configuration) ─────────────────────────────────────

    function startEditSettings() {
        loadProviderRegistry();
        agentSettingsEditing.value = true;
        agentSettingsLearningEnabled.value = selectedAgent.value.learning_enabled !== false;
        agentSettingsStorageQuota.value = selectedAgent.value.storage_quota != null ? Math.round(selectedAgent.value.storage_quota / (1024 * 1024)) : '';

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
            // Stamp config version from the model registry
            const configCopy = Object.assign({}, agentSettingsConfig.value);
            const version = configVersionFor(selectedAgent.value.provider, selectedAgent.value.model);
            if (version != null) {
                configCopy._configVersion = version;
            }
            const quotaBytes = agentSettingsStorageQuota.value !== '' ? parseInt(agentSettingsStorageQuota.value) * 1024 * 1024 : null;
            const body = {
                agent: selectedAgent.value.agent,
                learning_enabled: agentSettingsLearningEnabled.value,
                storage_quota: quotaBytes,
                configuration: configCopy
            };
            await api('/admin/agents/update', body);
            selectedAgent.value.learning_enabled = body.learning_enabled;
            selectedAgent.value.storage_quota = quotaBytes;
            selectedAgent.value.configuration = JSON.stringify(configCopy);
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
        const selectedName = agent.agent;
        agentInstructionsEditing.value = false;
        agentInstructionsExpanded.value = false;
        agentExpertiseEditing.value = false;
        agentProfileEditing.value = false;
        costBudgetEditing.value = false;
        agentSettingsEditing.value = false;
        agentUsageHistory.value = [];
        agentPassphraseConfirming.value = false;
        agentNewPassphrase.value = null;
        loadProviderRegistry();
        agentExpertise.value = [];
        // Fetch full agent detail (includes configuration) and instructions in parallel
        try {
            const tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
            const [detail, instData] = await Promise.all([
                api('/admin/agents/read', { agent: selectedName, timezone: tz }),
                api('/admin/agents/instructions/read', { agent: selectedName })
            ]);
            // Guard against selection changing while requests were in flight
            if (!selectedAgent.value || selectedAgent.value.agent !== selectedName) return;
            // Merge full detail onto the list item
            Object.assign(selectedAgent.value, detail);
            try {
                agentExpertise.value = typeof detail.expertise === 'string' ? JSON.parse(detail.expertise) : (detail.expertise || []);
            } catch (e) {
                agentExpertise.value = [];
            }
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
            if (updated && selectedAgent.value && selectedAgent.value.agent === agent) {
                Object.assign(selectedAgent.value, updated);
            }
        } catch (err) {
            console.error('Failed to reset passphrase:', err);
        }
    }

    // ── Agent creation ───────────────────────────────────────────────────────

    // ── Templates ────────────────────────────────────────────────────────────

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
        templateEditKind.value = 'welcome';
        templateEditDescription.value = '';
        templateEditContent.value = '';
    }

    async function editTemplate(t) {
        try {
            const data = await api('/admin/templates/read', { id: t.id });
            const tpl = data.template;
            templateEditing.value = true;
            templateEditId.value = tpl.id;
            templateEditName.value = tpl.name;
            templateEditKind.value = tpl.kind;
            templateEditDescription.value = tpl.description || '';
            templateEditContent.value = tpl.content;
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
                kind: templateEditKind.value,
                description: templateEditDescription.value || null,
                content: templateEditContent.value
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

    // ─── Delete Agent ───

    const agentDeleting = ref(false);

    async function deleteAgent() {
        if (!selectedAgent.value) return;
        const name = selectedAgent.value.agent;
        const confirmed = await showConfirm('Permanently delete "' + name + '" and ALL associated data (notes, mail, chat, discussions)? This cannot be undone.');
        if (!confirmed) return;
        agentDeleting.value = true;
        try {
            const data = await api('/admin/actors/delete', { actor_id: selectedAgent.value.actor_id });
            let msg = 'Deleted "' + name + '"';
            if (data.deleted && data.deleted.virtual_agents && data.deleted.virtual_agents.length > 0) {
                msg += ' and virtual agents: ' + data.deleted.virtual_agents.join(', ');
            }
            showToast(msg, 'success');
            selectedAgent.value = null;
            loadAgents();
        } catch (err) {
            console.error('Failed to delete agent:', err);
            showToast('Failed: ' + err.message, 'error');
        } finally {
            agentDeleting.value = false;
        }
    }

    function closeDialogs() {
        selectedAgent.value = null;
    }

    return {
        agents, selectedAgent,
        agentsSorted: agentSort.sorted, agentSortKey: agentSort.sortKey, agentSortDir: agentSort.sortDir,
        toggleAgentSort: agentSort.toggleSort, agentSortArrow: agentSort.sortArrow,
        // Provider registry
        providerRegistry, loadProviderRegistry, modelsForProvider, loadOpenRouterCatalog, capabilitiesFor, configVersionFor, modelDeprecation,
        parseAgentConfig, capabilityVisible, capabilityDisabled, formatConfigValue,
        // Agent detail
        agentInstructions, agentInstructionsEditing, agentInstructionsExpanded, agentInstructionsEditContent, agentInstructionsSaving,
        agentExpertise, agentExpertiseEditing, agentExpertiseEditText, agentExpertiseSaving,
        agentPassphraseConfirming, agentNewPassphrase,
        agentProfileEditing, agentProfileProvider, agentProfileModel, agentProfileApiKey, agentProfilePersonality, agentProfileDreamMode, agentProfileSaving,
        loadAgents, viewAgent,
        startEditProfile, onProfileProviderChange, saveProfile,
        costBudgetEditing, costBudgetDailyValue, costBudgetMonthlyValue, startEditCostBudget, saveCostBudget,
        agentUsageHistory, agentUsageLoading, loadUsageHistory,
        defaultStorageQuota,
        agentSettingsEditing, agentSettingsLearningEnabled, agentSettingsStorageQuota, agentSettingsConfig, agentSettingsSaving,
        startEditSettings, saveSettings,
        startEditInstructions, cancelEditInstructions, saveInstructions,
        startEditExpertise, cancelEditExpertise, saveExpertise,
        resetAgentPassphrase,
        agentDeleting, deleteAgent,
        // Templates
        welcomeTemplates, templateEditing, templateEditId,
        templateEditName, templateEditKind, templateEditDescription, templateEditContent, templateSaving,
        loadTemplates, startNewTemplate, editTemplate, saveTemplate, confirmDeleteTemplate,
        closeDialogs
    };
}

export { useAgents };
