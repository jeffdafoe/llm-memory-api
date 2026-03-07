// agents.js — Agents list, detail, creation, welcome templates

function useAgents({ api, showToast, showConfirm, onEvent }) {
    const agents = ref([]);
    const selectedAgent = ref(null);
    const agentSubTab = ref('list');

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

    // Welcome templates
    const welcomeTemplates = ref([]);
    const templateEditing = ref(false);
    const templateEditId = ref(null);
    const templateEditName = ref('');
    const templateEditDescription = ref('');
    const templateEditSubject = ref('');
    const templateEditBody = ref('');
    const templateSaving = ref(false);

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

    function startEditProfile() {
        agentProfileEditing.value = true;
        agentProfileProvider.value = selectedAgent.value.provider || '';
        agentProfileModel.value = selectedAgent.value.model || '';
        agentProfileApiKey.value = '';
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

    async function viewAgent(agent) {
        selectedAgent.value = agent;
        agentInstructionsEditing.value = false;
        agentExpertiseEditing.value = false;
        agentProfileEditing.value = false;
        agentPassphraseConfirming.value = false;
        agentNewPassphrase.value = null;
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

    // Agent creation
    function startCreateAgent() {
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
        loadTemplates();
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

    // Welcome templates
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

    // Real-time activity updates from WebSocket
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
        agentInstructions, agentInstructionsEditing, agentInstructionsEditContent, agentInstructionsSaving,
        agentExpertise, agentExpertiseEditing, agentExpertiseEditText, agentExpertiseSaving,
        agentPassphraseConfirming, agentNewPassphrase,
        agentProfileEditing, agentProfileProvider, agentProfileModel, agentProfileApiKey, agentProfileSaving,
        loadAgents, viewAgent,
        startEditProfile, saveProfile,
        startEditInstructions, cancelEditInstructions, saveInstructions,
        startEditExpertise, cancelEditExpertise, saveExpertise,
        resetAgentPassphrase,
        agentCreating, newAgentName, newAgentProvider, newAgentModel,
        newAgentTemplateId, newAgentCreating, newAgentPassphrase,
        newAgentVirtual, newAgentPersonality, newAgentApiKey, newAgentCost,
        startCreateAgent, createAgent,
        welcomeTemplates, templateEditing, templateEditId,
        templateEditName, templateEditDescription, templateEditSubject, templateEditBody, templateSaving,
        loadTemplates, startNewTemplate, editTemplate, saveTemplate, confirmDeleteTemplate,
        closeDialogs
    };
}

window.useAgents = useAgents;
