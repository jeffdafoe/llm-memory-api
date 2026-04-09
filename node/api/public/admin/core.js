// core.js — Shared utilities: API helper, auth, formatting, confirm/toast
import { ref, computed } from 'vue';

const API_BASE = '/v1';

// User role label — displayed in sidebar and profile dialog.
// Hardcoded for now; will be database-driven per user eventually.
const USER_ROLE_LABEL = 'Chipkeeper';

function useCore() {
    const authenticated = ref(false);
    const sessionToken = ref(null);
    const user = ref(null);
    const permissions = ref({});
    const loginForm = ref({ username: '', password: '' });
    const loginError = ref('');
    const loggingIn = ref(false);

    // Callback fired when session expires mid-use (401 or invalid 403).
    // Main app registers this to disconnect WebSocket, stop polling, etc.
    let onSessionExpired = null;

    // Confirm dialog
    const confirmPrompt = ref(null);

    function showConfirm(message, action) {
        // Two calling conventions:
        // 1. showConfirm('msg', callback) — callback-style, runs callback on confirm
        // 2. const ok = await showConfirm('msg') — promise-style, resolves true/false
        if (typeof action === 'function') {
            confirmPrompt.value = { message, action };
            return;
        }
        return new Promise(resolve => {
            confirmPrompt.value = {
                message,
                action: () => resolve(true),
                cancel: () => resolve(false)
            };
        });
    }

    function executeConfirm() {
        if (confirmPrompt.value && confirmPrompt.value.action) {
            confirmPrompt.value.action();
        }
        confirmPrompt.value = null;
    }

    function cancelConfirm() {
        if (confirmPrompt.value && confirmPrompt.value.cancel) {
            confirmPrompt.value.cancel();
        }
        confirmPrompt.value = null;
    }

    // Toast notifications
    const toast = ref(null);
    let toastTimer = null;

    function showToast(text, type, duration, action) {
        if (toastTimer) clearTimeout(toastTimer);
        toast.value = { text, type: type || 'info', action: action || null };
        toastTimer = setTimeout(() => { toast.value = null; }, duration || 5000);
    }

    // Clear session state and notify listeners (e.g. WebSocket disconnect, stop polling)
    function clearSession() {
        authenticated.value = false;
        sessionToken.value = null;
        permissions.value = {};
        localStorage.removeItem('admin_session');
        if (onSessionExpired) onSessionExpired();
    }

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
        if (response.status === 401) {
            clearSession();
            throw new Error('Session expired');
        }
        if (response.status === 403) {
            const data = await response.json();
            const msg = data.error?.message || 'Insufficient permissions';
            // Expired/invalid session token — treat as logout
            if (msg === 'Invalid or expired session token') {
                clearSession();
                throw new Error('Session expired');
            }
            throw new Error(msg);
        }
        const data = await response.json();
        if (!response.ok) {
            throw new Error(data.error?.message || 'Request failed');
        }
        return data;
    }

    // Auth
    async function login(onSuccess) {
        loggingIn.value = true;
        loginError.value = '';
        try {
            // Bypass api() helper to avoid the generic 401 → "Session expired" interceptor
            const response = await fetch(API_BASE + '/admin/login', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(loginForm.value)
            });
            const data = await response.json();
            if (!response.ok) {
                throw new Error(data.error?.message || 'Login failed');
            }
            sessionToken.value = data.session_token;
            user.value = data.user;
            permissions.value = data.permissions || {};
            authenticated.value = true;
            localStorage.setItem('admin_session', JSON.stringify({
                token: data.session_token,
                user: data.user,
                permissions: data.permissions || {},
                expires: data.expires_at
            }));
            loginForm.value = { username: '', password: '' };
            if (onSuccess) onSuccess();
        } catch (err) {
            loginError.value = err.message;
        } finally {
            loggingIn.value = false;
        }
    }

    async function logout(onLogout) {
        try {
            await api('/admin/logout');
        } catch (err) {
            // Ignore — clear local state regardless
        }
        if (onLogout) onLogout();
        authenticated.value = false;
        sessionToken.value = null;
        user.value = null;
        permissions.value = {};
        localStorage.removeItem('admin_session');
    }

    // Profile / Change password / Visibility
    const showProfile = ref(false);
    const showChangePassword = ref(false);
    const changePasswordForm = ref({ current: '', newPassword: '', confirm: '' });
    const changePasswordError = ref('');
    const changePasswordSaving = ref(false);
    const visibleToOthers = ref(false);

    function closeProfile() {
        showProfile.value = false;
        changePasswordForm.value = { current: '', newPassword: '', confirm: '' };
        changePasswordError.value = '';
        changePasswordSaving.value = false;
    }

    async function loadVisibility() {
        try {
            const data = await api('/admin/profile/visibility');
            visibleToOthers.value = data.visible_to_others;
        } catch { /* ignore */ }
    }

    async function toggleVisibleToOthers() {
        const newVal = !visibleToOthers.value;
        if (newVal) {
            // Confirm before enabling
            showConfirm('Enabling this means that other users of llm-memory will be able to search for you when sharing memories.\n\nYour memories still remain private unless you choose to share some.', async () => {
                try {
                    await api('/admin/profile/visibility', { visible_to_others: true });
                    visibleToOthers.value = true;
                    showToast('You are now visible for sharing', 'success');
                } catch (err) {
                    showToast(err.message || 'Failed to update visibility', 'error');
                }
            });
        } else {
            try {
                await api('/admin/profile/visibility', { visible_to_others: false });
                visibleToOthers.value = false;
                showToast('You are now hidden from sharing', 'success');
            } catch (err) {
                showToast(err.message || 'Failed to update visibility', 'error');
            }
        }
    }

    async function changePassword() {
        changePasswordError.value = '';
        if (!changePasswordForm.value.current || !changePasswordForm.value.newPassword) {
            changePasswordError.value = 'All fields are required';
            return;
        }
        if (changePasswordForm.value.newPassword !== changePasswordForm.value.confirm) {
            changePasswordError.value = 'New passwords do not match';
            return;
        }
        if (changePasswordForm.value.newPassword.length < 4) {
            changePasswordError.value = 'Password must be at least 4 characters';
            return;
        }
        changePasswordSaving.value = true;
        try {
            await api('/admin/change-password', {
                current_password: changePasswordForm.value.current,
                new_password: changePasswordForm.value.newPassword
            });
            closeProfile();
            showToast('Password changed', 'success');
        } catch (err) {
            changePasswordError.value = err.message;
        } finally {
            changePasswordSaving.value = false;
        }
    }

    function restoreSession() {
        const saved = localStorage.getItem('admin_session');
        if (saved) {
            try {
                const session = JSON.parse(saved);
                if (new Date(session.expires) > new Date() && session.permissions) {
                    sessionToken.value = session.token;
                    user.value = session.user;
                    permissions.value = session.permissions;
                    authenticated.value = true;
                    return true;
                } else {
                    localStorage.removeItem('admin_session');
                }
            } catch (err) {
                localStorage.removeItem('admin_session');
            }
        }
        return false;
    }

    // Formatting
    function formatDate(dateStr) {
        if (!dateStr) return '—';
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

    function formatBytes(bytes) {
        if (bytes === null || bytes === undefined) return '';
        if (bytes < 1024) return bytes + ' B';
        if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB';
        return (bytes / (1024 * 1024)).toFixed(1) + ' MB';
    }

    function formatDuration(ms) {
        if (ms === null || ms === undefined) return '';
        if (ms < 1000) return ms + ' ms';
        var s = ms / 1000;
        if (s < 10) return s.toFixed(2) + ' s';
        if (s < 60) return s.toFixed(1) + ' s';
        var m = s / 60;
        return m.toFixed(1) + ' m';
    }

    function formatTime(dateStr) {
        if (!dateStr) return '';
        const d = new Date(dateStr);
        return d.toLocaleTimeString();
    }

    // Admin permissions helper — mirrors the backend action hierarchy.
    // permissions map: { resource: [action, ...], ... }
    // canDo('notes', 'read') → true if actor has read, write, or delete on notes (or wildcard).
    const ACTION_RANK = { read: 1, write: 2, delete: 3 };

    function canDo(resource, action) {
        const perms = permissions.value;
        if (!perms) return false;

        // Global wildcard
        if (perms['*'] && perms['*'].includes('*')) return true;

        const requiredRank = ACTION_RANK[action];
        if (!requiredRank) return false;

        // Check grants on specific resource
        const grants = perms[resource];
        if (grants) {
            for (const g of grants) {
                if (g === '*') return true;
                const grantedRank = ACTION_RANK[g];
                if (grantedRank && grantedRank >= requiredRank) return true;
            }
        }

        return false;
    }

    // Discussion display helpers
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

    const outcomeIcons = {
        consensus: 'icon-check-check',
        deadlock: 'icon-lock',
        partial: 'icon-circle-dot',
        abandoned: 'icon-log-out'
    };

    function outcomeIcon(outcome) {
        return outcomeIcons[outcome] || '';
    }

    const agentColors = {
        home: '#5b9bd5',
        work: '#e07b53',
        system: '#888'
    };
    const fallbackColors = [
        '#8e6bbf', '#4caf88', '#c9a83e', '#d46a8e',
        '#e06c75', '#56b6c2', '#c678dd', '#98c379',
        '#d19a66', '#61afef', '#be5046', '#e5c07b'
    ];

    function agentColor(agent) {
        if (!agent) return 'var(--text-secondary)';
        if (agentColors[agent]) return agentColors[agent];
        let hash = 0;
        for (let i = 0; i < agent.length; i++) {
            hash = agent.charCodeAt(i) + ((hash << 5) - hash);
        }
        return fallbackColors[Math.abs(hash) % fallbackColors.length];
    }

    function realmColor(realm) {
        if (!realm) return 'var(--text-muted)';
        let hash = 0;
        for (let i = 0; i < realm.length; i++) {
            hash = realm.charCodeAt(i) + ((hash << 5) - hash);
        }
        return fallbackColors[Math.abs(hash) % fallbackColors.length];
    }

    function voteQuestion(text) {
        const match = text.match(/^(.*?\?)\s*(.+)$/);
        if (!match) return { question: text, choices: [] };
        const choiceMatches = match[2].match(/\d+=\S+/g);
        if (choiceMatches) return { question: match[1], choices: choiceMatches };
        return { question: text, choices: [] };
    }

    // Fade last-seen text from primary to muted over 15 minutes (online threshold)
    function lastSeenColor(agent) {
        if (!agent.last_seen) return 'var(--text-muted)';
        var elapsed = (Date.now() - new Date(agent.last_seen).getTime()) / 1000;
        var threshold = 900; // 15 minutes
        if (elapsed <= 0) return 'var(--text-primary)';
        if (elapsed >= threshold) return 'var(--text-muted)';
        // Interpolate between primary (#e4e4e7) and muted (#71717a)
        var t = elapsed / threshold;
        var r = Math.round(228 - t * (228 - 113));
        var g = Math.round(228 - t * (228 - 113));
        var b = Math.round(231 - t * (231 - 122));
        return 'rgb(' + r + ',' + g + ',' + b + ')';
    }

    return {
        authenticated, sessionToken, user, permissions, canDo,
        loginForm, loginError, loggingIn,
        showProfile, showChangePassword, closeProfile, changePasswordForm, changePasswordError, changePasswordSaving, changePassword, visibleToOthers, loadVisibility, toggleVisibleToOthers,
        login, logout, restoreSession, setOnSessionExpired: (fn) => { onSessionExpired = fn; },
        api, showConfirm, executeConfirm, cancelConfirm, confirmPrompt,
        showToast, toast,
        formatDate, formatShortDate, timeAgo, formatBytes, formatDuration, formatTime,
        statusIcon, outcomeIcon, agentColor, realmColor, voteQuestion, lastSeenColor,
        USER_ROLE_LABEL
    };
}

// Reusable table sort composable.
// Usage:
//   const { sortKey, sortDir, toggleSort, sorted } = useSortable(items, 'name');
//   <th class="sortable" @click="toggleSort('name')">Name <span v-html="sortArrow('name')"></span></th>
//   <tr v-for="item in sorted" ...>
function useSortable(items, defaultKey, defaultDir) {
    const sortKey = ref(defaultKey || '');
    const sortDir = ref(defaultDir || 'asc');

    function toggleSort(key) {
        if (sortKey.value === key) {
            sortDir.value = sortDir.value === 'asc' ? 'desc' : 'asc';
        } else {
            sortKey.value = key;
            sortDir.value = 'asc';
        }
    }

    function sortArrow(key) {
        if (sortKey.value !== key) return '';
        return sortDir.value === 'asc' ? '&#9650;' : '&#9660;';
    }

    const sorted = computed(() => {
        if (!sortKey.value || !items.value) return items.value;
        var key = sortKey.value;
        var dir = sortDir.value === 'asc' ? 1 : -1;
        return [...items.value].sort(function(a, b) {
            var aVal = a[key];
            var bVal = b[key];
            // Nulls/undefined sort last
            if (aVal == null && bVal == null) return 0;
            if (aVal == null) return 1;
            if (bVal == null) return -1;
            // Dates
            if (aVal instanceof Date) return dir * (aVal - bVal);
            // Strings
            if (typeof aVal === 'string') {
                return dir * aVal.localeCompare(bVal, undefined, { sensitivity: 'base' });
            }
            // Numbers / booleans
            if (aVal < bVal) return -dir;
            if (aVal > bVal) return dir;
            return 0;
        });
    });

    return { sortKey, sortDir, toggleSort, sortArrow, sorted };
}

export { useCore, useSortable };
