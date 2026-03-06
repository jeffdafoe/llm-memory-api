// apilog.js — API request log with polling and filtering
const { ref, computed } = Vue;

function useApiLog({ api, authenticated }) {
    const apiLogEntries = ref([]);
    const apiLogPaused = ref(false);
    const apiLogContainer = ref(null);
    const apiLogLastId = ref(0);
    const apiLogFilterAgent = ref('');
    const apiLogFilterStatus = ref('');
    const apiLogFilterPath = ref('');
    let apiLogTimer = null;

    const API_LOG_POLL_MS = 2000;
    const API_LOG_MAX_ENTRIES = 500;

    function statusCategory(status) {
        if (!status) return 'pending';
        if (status < 300) return 'ok';
        if (status < 400) return 'redirect';
        if (status < 500) return 'client';
        return 'server';
    }

    const apiLogAgents = computed(() => {
        const agents = new Set();
        for (const e of apiLogEntries.value) {
            if (e.agent) agents.add(e.agent);
        }
        return [...agents].sort();
    });

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

    async function pollApiLog() {
        if (apiLogPaused.value) return;
        try {
            const data = await api('/admin/api-log', { since_id: apiLogLastId.value, limit: 200 });
            if (data.entries.length > 0) {
                apiLogEntries.value.unshift(...data.entries.reverse());
                if (apiLogEntries.value.length > API_LOG_MAX_ENTRIES) {
                    apiLogEntries.value.length = API_LOG_MAX_ENTRIES;
                }
                apiLogLastId.value = data.entries[0].id;
            }
        } catch (err) {
            console.error('Failed to poll API log:', err);
        }
    }

    function startApiLogPolling(currentView) {
        if (apiLogTimer) return;
        pollApiLog();
        apiLogTimer = setInterval(() => {
            if (authenticated.value) {
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

    return {
        apiLogEntries, apiLogPaused, apiLogContainer,
        apiLogFilterAgent, apiLogFilterStatus, apiLogFilterPath,
        apiLogAgents, apiLogFiltered,
        pollApiLog, startApiLogPolling, stopApiLogPolling,
        statusCategory
    };
}

window.useApiLog = useApiLog;
