// core.js — Shared utilities: API helper, auth, formatting, confirm/toast
import { ref, computed } from 'vue';

const API_BASE = '/v1';

function useCore() {
    const authenticated = ref(false);
    const sessionToken = ref(null);
    const user = ref(null);
    const permissions = ref({});
    const loginForm = ref({ username: '', password: '' });
    const loginError = ref('');
    const loggingIn = ref(false);

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
            authenticated.value = false;
            sessionToken.value = null;
            permissions.value = {};
            localStorage.removeItem('admin_session');
            throw new Error('Session expired');
        }
        if (response.status === 403) {
            const data = await response.json();
            throw new Error(data.error?.message || 'Insufficient permissions');
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

    // Change password
    const showChangePassword = ref(false);
    const changePasswordForm = ref({ current: '', newPassword: '', confirm: '' });
    const changePasswordError = ref('');
    const changePasswordSaving = ref(false);

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
            showChangePassword.value = false;
            changePasswordForm.value = { current: '', newPassword: '', confirm: '' };
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
        return (ms / 1000).toFixed(2) + ' s';
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
    const fallbackColors = ['#8e6bbf', '#4caf88', '#c9a83e', '#d46a8e'];

    // Strip common prefixes from agent names for display.
    // Full name available via title attribute on hover.
    function shortAgentName(name) {
        if (!name) return '';
        return name.replace(/^llm-memory-api-/, '');
    }

    function agentColor(agent) {
        if (agentColors[agent]) return agentColors[agent];
        let hash = 0;
        for (let i = 0; i < agent.length; i++) {
            hash = agent.charCodeAt(i) + ((hash << 5) - hash);
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

    return {
        authenticated, sessionToken, user, permissions, canDo,
        loginForm, loginError, loggingIn,
        showChangePassword, changePasswordForm, changePasswordError, changePasswordSaving, changePassword,
        login, logout, restoreSession,
        api, showConfirm, executeConfirm, cancelConfirm, confirmPrompt,
        showToast, toast,
        formatDate, formatShortDate, timeAgo, formatBytes, formatDuration, formatTime,
        statusIcon, outcomeIcon, agentColor, shortAgentName, voteQuestion
    };
}

export { useCore };
