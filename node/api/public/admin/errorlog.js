// errorlog.js — Error log viewer with polling and filtering
import { ref, computed } from 'vue';

function useErrorLog({ api, authenticated }) {
    const errorLogEntries = ref([]);
    const errorLogPaused = ref(false);
    const errorLogLastId = ref(0);
    const errorLogFilterAgent = ref('');
    const errorLogFilterSubsystem = ref('');
    const errorLogFilterStatus = ref('');
    const errorLogExpandedId = ref(null);
    let errorLogTimer = null;
    let errorLogPolling = false;

    const ERROR_LOG_POLL_MS = 3000;
    const ERROR_LOG_MAX_ENTRIES = 500;

    const errorLogAgents = computed(() => {
        const agents = new Set();
        for (const e of errorLogEntries.value) {
            if (e.agent) agents.add(e.agent);
        }
        return [...agents].sort();
    });

    const errorLogSubsystems = computed(() => {
        const subs = new Set();
        for (const e of errorLogEntries.value) {
            if (e.subsystem) subs.add(e.subsystem);
        }
        return [...subs].sort();
    });

    const errorLogFiltered = computed(() => {
        let entries = errorLogEntries.value;
        if (errorLogFilterAgent.value) {
            entries = entries.filter(e => e.agent === errorLogFilterAgent.value);
        }
        if (errorLogFilterSubsystem.value) {
            entries = entries.filter(e => e.subsystem === errorLogFilterSubsystem.value);
        }
        if (errorLogFilterStatus.value === '5xx') {
            entries = entries.filter(e => !e.status_code || e.status_code >= 500);
        } else if (errorLogFilterStatus.value === '4xx') {
            entries = entries.filter(e => e.status_code && e.status_code >= 400 && e.status_code < 500);
        }
        return entries;
    });

    function toggleErrorDetail(id) {
        if (errorLogExpandedId.value === id) {
            errorLogExpandedId.value = null;
        } else {
            errorLogExpandedId.value = id;
        }
    }

    async function pollErrorLog() {
        if (errorLogPaused.value || errorLogPolling) return;
        errorLogPolling = true;
        try {
            const data = await api('/admin/error-log', { since_id: errorLogLastId.value, limit: 200 });
            if (data.entries.length > 0) {
                errorLogEntries.value.unshift(...data.entries.reverse());
                if (errorLogEntries.value.length > ERROR_LOG_MAX_ENTRIES) {
                    errorLogEntries.value.length = ERROR_LOG_MAX_ENTRIES;
                }
                errorLogLastId.value = data.entries[0].id;
            }
        } catch (err) {
            console.error('Failed to poll error log:', err);
        } finally {
            errorLogPolling = false;
        }
    }

    function startErrorLogPolling() {
        if (errorLogTimer) return;
        pollErrorLog();
        errorLogTimer = setInterval(() => {
            if (authenticated.value) {
                pollErrorLog();
            }
        }, ERROR_LOG_POLL_MS);
    }

    function stopErrorLogPolling() {
        if (errorLogTimer) {
            clearInterval(errorLogTimer);
            errorLogTimer = null;
        }
    }

    return {
        errorLogEntries, errorLogPaused, errorLogLastId,
        errorLogFilterAgent, errorLogFilterSubsystem, errorLogFilterStatus,
        errorLogAgents, errorLogSubsystems, errorLogFiltered,
        errorLogExpandedId, toggleErrorDetail,
        pollErrorLog, startErrorLogPolling, stopErrorLogPolling
    };
}

export { useErrorLog };
