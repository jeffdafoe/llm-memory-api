// config.js — System configuration viewer/editor
import { ref } from 'vue';

function useConfig({ api, showToast }) {
    const configEntries = ref([]);
    const configLoading = ref(false);
    const configEditKey = ref(null);
    const configEditValue = ref('');
    const configSaving = ref(false);

    async function loadConfig() {
        configLoading.value = true;
        try {
            const data = await api('/admin/config/list');
            configEntries.value = data.config;
        } catch (err) {
            console.error('Failed to load config:', err);
        } finally {
            configLoading.value = false;
        }
    }

    function startEditConfig(entry) {
        configEditKey.value = entry.key;
        configEditValue.value = entry.value;
    }

    function cancelEditConfig() {
        configEditKey.value = null;
    }

    async function saveConfig() {
        configSaving.value = true;
        try {
            await api('/admin/config/update', {
                key: configEditKey.value,
                value: configEditValue.value
            });
            const entry = configEntries.value.find(e => e.key === configEditKey.value);
            if (entry) entry.value = configEditValue.value;
            configEditKey.value = null;
            showToast('Config updated', 'success');
        } catch (err) {
            console.error('Failed to save config:', err);
            showToast('Failed: ' + err.message, 'error');
        } finally {
            configSaving.value = false;
        }
    }

    return {
        configEntries, configLoading,
        configEditKey, configEditValue, configSaving,
        loadConfig, startEditConfig, cancelEditConfig, saveConfig
    };
}

export { useConfig };
