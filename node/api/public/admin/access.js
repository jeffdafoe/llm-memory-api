import { ref } from 'vue';

export function useAccess({ api, showToast }) {
    const accessRequests = ref([]);
    const inviteCodes = ref([]);
    const accessSubTab = ref('requests');

    async function loadAccessRequests() {
        const res = await api('/admin/access-requests', {});
        if (res && res.requests) accessRequests.value = res.requests;
    }

    async function loadInviteCodes() {
        const res = await api('/admin/invite-codes', {});
        if (res && res.codes) inviteCodes.value = res.codes;
    }

    async function loadAccess() {
        await Promise.all([loadAccessRequests(), loadInviteCodes()]);
    }

    async function approveRequest(id) {
        const res = await api('/admin/access-requests/approve', { id });
        if (res && res.ok) {
            showToast('Approved — invite code: ' + res.invite_code, 'success');
            await loadAccess();
        }
    }

    async function rejectRequest(id) {
        const res = await api('/admin/access-requests/reject', { id });
        if (res && res.ok) {
            showToast('Request rejected', 'info');
            await loadAccess();
        }
    }

    async function generateCodes(count, expiresDays) {
        const res = await api('/admin/invite-codes/generate', { count, expires_days: expiresDays || undefined });
        if (res && res.ok) {
            showToast(`Generated ${res.codes.length} invite code(s)`, 'success');
            await loadInviteCodes();
        }
    }

    async function deleteCode(id) {
        const res = await api('/admin/invite-codes/delete', { id });
        if (res && res.ok) {
            showToast('Invite code deleted', 'info');
            await loadInviteCodes();
        }
    }

    function copyCode(code) {
        navigator.clipboard.writeText(code);
        showToast('Copied to clipboard', 'info');
    }

    return {
        accessRequests, inviteCodes, accessSubTab,
        loadAccess, loadAccessRequests, loadInviteCodes,
        approveRequest, rejectRequest, generateCodes, copyCode, deleteCode
    };
}
